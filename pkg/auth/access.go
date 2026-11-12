package auth

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/ipn"
)

// Access is what a peer may do with tstermd.
type Access int

const (
	// NoAccess is no access at all.
	NoAccess Access = iota

	// Viewer is a person other than the owner, reaching tstermd's
	// sharing node: they may look, read-only, at the sessions the owner
	// shares.
	Viewer

	// Owner is the owner of the machine's Tailscale node: they may do
	// everything.
	Owner
)

func (a Access) String() string {
	switch a {
	case Viewer:
		return "viewer"
	case Owner:
		return "owner"
	}
	return "none"
}

// Peer is who made a request, and what they may do.
type Peer struct {
	Access Access
	Login  string // their Tailscale login name, if known
	Name   string // their display name, if known

	// AsOther is set if the peer is the owner, but seeing what other
	// people see, as a Viewer (see Sharing.SetViewAsOther). They may
	// still switch back.
	AsOther bool
}

// An Authorizer decides what the peer at a remote address may do.
type Authorizer interface {
	// Authorize returns the peer at remoteAddr, an "ip:port" or "ip",
	// or ErrUnauthorized, or another error, if it may do nothing.
	Authorize(ctx context.Context, remoteAddr string) (Peer, error)

	// Changes returns a channel that is closed the next time a peer's
	// access may have changed. Holders of long-lived authorizations, as
	// attached clients, should then authorize again.
	Changes() <-chan struct{}
}

// Authorize implements Authorizer: the owner of the local node's peers
// are the Owner, and nobody else has access.
func (a *SelfUser) Authorize(ctx context.Context, remoteAddr string) (Peer, error) {
	if err := a.Allow(ctx, remoteAddr); err != nil {
		return Peer{}, err
	}
	return Peer{Access: Owner, Login: a.SelfLogin()}, nil
}

type peerKey struct{}

// WithPeer returns a context holding the peer p.
func WithPeer(ctx context.Context, p Peer) context.Context {
	return context.WithValue(ctx, peerKey{}, p)
}

// PeerFrom returns the peer that Middleware put in ctx, or a Peer with no
// access.
func PeerFrom(ctx context.Context) Peer {
	p, _ := ctx.Value(peerKey{}).(Peer)
	return p
}

// Middleware wraps h to authorize each request with a, refusing those of
// peers with no access, and putting the peer in the request's context (see
// PeerFrom), for h to check what they may do.
func Middleware(a Authorizer, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := a.Authorize(r.Context(), r.RemoteAddr)
		if err == nil && p.Access == NoAccess {
			err = ErrUnauthorized
		}
		if err != nil {
			writeAuthError(w, r, err)
			return
		}
		h.ServeHTTP(w, r.WithContext(WithPeer(r.Context(), p)))
	})
}

// Sharing authorizes requests to tstermd's sharing node: a tsnet node of
// its own, usually tagged, so that people the owner's machines are closed
// to may reach it. Its peers are known to its own LocalAPI, LC, and
// compared with the owner of the machine's tailscaled, Owner:
//
//   - a person whose login matches the owner's is the Owner;
//   - another person is a Viewer;
//   - a tagged device, which has no person behind it, has no access, even
//     if its tags' owner is the owner.
type Sharing struct {
	Owner *SelfUser
	LC    WhoIsClient

	viewAsOther atomic.Bool

	mu      sync.Mutex
	changed chan struct{}
}

// SetViewAsOther sets whether the owner, on the sharing node, sees what
// other people see: they're then a Viewer, with Peer.AsOther set, until
// it's unset. It wakes Changes, so that their connections authorize again.
func (s *Sharing) SetViewAsOther(v bool) {
	if s.viewAsOther.Swap(v) == v {
		return
	}
	s.mu.Lock()
	ch := s.changed
	s.mu.Unlock()
	if ch != nil {
		s.signal(ch)
	}
}

// ViewAsOther reports whether the owner sees what others see.
func (s *Sharing) ViewAsOther() bool { return s.viewAsOther.Load() }

// Authorize implements Authorizer.
func (s *Sharing) Authorize(ctx context.Context, remoteAddr string) (Peer, error) {
	w, err := s.LC.WhoIs(ctx, remoteAddr)
	if err != nil {
		return Peer{}, err
	}
	if w == nil || w.Node == nil || w.UserProfile == nil {
		return Peer{}, ErrUnauthorized
	}
	if w.Node.IsTagged() {
		return Peer{}, ErrUnauthorized
	}
	login := w.UserProfile.LoginName
	if login == "" {
		return Peer{}, ErrUnauthorized
	}
	p := Peer{Access: Viewer, Login: login, Name: w.UserProfile.DisplayName}
	if self := s.Owner.SelfLogin(); self != "" && strings.EqualFold(login, self) {
		p.Access = Owner
		if s.viewAsOther.Load() {
			p.Access, p.AsOther = Viewer, true
		}
	}
	return p, nil
}

// Changes implements Authorizer: it's closed when the owner's tailnet
// changes, or the sharing node's, as Watch reports.
func (s *Sharing) Changes() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.changed == nil {
		s.changed = make(chan struct{})
		ownerChanges := s.Owner.Changes()
		ch := s.changed
		go func() {
			<-ownerChanges
			s.signal(ch)
		}()
	}
	return s.changed
}

// signal closes ch, if it's still the current Changes channel.
func (s *Sharing) signal(ch chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.changed == ch {
		close(ch)
		s.changed = nil
	}
}

// Watch watches the sharing node's IPN bus, through lc, until ctx is done,
// waking Changes when its peers change.
func (s *Sharing) Watch(ctx context.Context, lc *local.Client, logf func(format string, args ...any)) {
	for ctx.Err() == nil {
		w, err := lc.WatchIPNBus(ctx, ipn.NotifyPeerChanges)
		if err == nil {
			for {
				n, err := w.Next()
				if err != nil {
					break
				}
				if len(n.PeersChanged) > 0 || len(n.PeersRemoved) > 0 {
					s.mu.Lock()
					ch := s.changed
					s.mu.Unlock()
					if ch != nil {
						s.signal(ch)
					}
				}
			}
			w.Close()
		}
		if ctx.Err() != nil {
			return
		}
		logf("auth: sharing node's IPN bus watch ended (%v); retrying", err)
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
}
