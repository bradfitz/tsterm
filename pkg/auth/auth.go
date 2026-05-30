// Package auth gates tsterm requests at the Tailscale identity layer.
//
// The current and only policy is "same tailnet user": a request is allowed
// when the Tailscale user that owns the remote peer matches the user that
// owns the local node running the daemon. This lets a single person reach
// all of their own devices and forbids cross-user access on the same
// tailnet without requiring any ACL configuration.
//
// The WhoIsClient interface is the small subset of
// tailscale.com/client/local.Client that the daemon needs, so the host
// tailscaled and tsnet.Server cases share one implementation, and tests can
// substitute a fake.
package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"

	"github.com/bradfitz/tsterm/pkg/proto"

	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
)

// WhoIsClient is the Tailscale LocalAPI surface we depend on.
type WhoIsClient interface {
	WhoIs(ctx context.Context, remoteAddr string) (*apitype.WhoIsResponse, error)
	Status(ctx context.Context) (*ipnstate.Status, error)
}

// ErrUnauthorized indicates the caller's Tailscale identity does not match
// the local node's owner.
var ErrUnauthorized = errors.New("auth: unauthorized")

// SelfUser authorizes requests whose peer is owned by the same tailnet user
// as the local node.
type SelfUser struct {
	lc   WhoIsClient
	self tailcfg.UserID

	// who caches WhoIs results to avoid hitting the LocalAPI on every UDP
	// packet. Entries are not invalidated; for a long-lived process where
	// tailnet identities are stable this is fine. The map is small (one
	// entry per active client address).
	mu  sync.RWMutex
	who map[string]*apitype.WhoIsResponse
}

// NewSelfUser captures the local node's tailnet user identity for later
// comparisons. It returns an error if Status fails or if the local node has
// no associated user (transient during sign-in).
func NewSelfUser(ctx context.Context, lc WhoIsClient) (*SelfUser, error) {
	st, err := lc.Status(ctx)
	if err != nil {
		return nil, err
	}
	if st == nil || st.Self == nil || st.Self.UserID == 0 {
		return nil, errors.New("auth: local node has no UserID")
	}
	return &SelfUser{
		lc:   lc,
		self: st.Self.UserID,
		who:  map[string]*apitype.WhoIsResponse{},
	}, nil
}

// SelfUserID returns the local node's owner UserID.
func (a *SelfUser) SelfUserID() tailcfg.UserID { return a.self }

// Allow returns the WhoIs response for remoteAddr when the peer is owned by
// the same tailnet user as the local node, or ErrUnauthorized otherwise.
func (a *SelfUser) Allow(ctx context.Context, remoteAddr string) (*apitype.WhoIsResponse, error) {
	a.mu.RLock()
	w, ok := a.who[remoteAddr]
	a.mu.RUnlock()
	if !ok {
		var err error
		w, err = a.lc.WhoIs(ctx, remoteAddr)
		if err != nil {
			return nil, err
		}
		a.mu.Lock()
		a.who[remoteAddr] = w
		a.mu.Unlock()
	}
	if w == nil || w.Node == nil || w.Node.User != a.self {
		return nil, ErrUnauthorized
	}
	return w, nil
}

// HTTPMiddleware wraps h to enforce Allow against r.RemoteAddr. On failure
// it responds 403 with a small CBOR error body.
func (a *SelfUser) HTTPMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := a.Allow(r.Context(), r.RemoteAddr)
		if err != nil {
			writeAuthError(w, err)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func writeAuthError(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", proto.ContentType)
	w.WriteHeader(http.StatusForbidden)
	body, _ := proto.Encode(proto.Error{
		Code:    "unauthorized",
		Message: redactErr(err),
	})
	_, _ = w.Write(body)
}

// redactErr strips potentially sensitive details from a Tailscale error
// before returning it to a caller. The control plane is reachable over the
// tailnet but a structured error is more useful than a bare 403.
func redactErr(err error) string {
	s := err.Error()
	if i := strings.Index(s, ":"); i >= 0 && i < 32 {
		// "auth: unauthorized" → keep first segment; drop verbose tail.
		return s[:i]
	}
	if errors.Is(err, ErrUnauthorized) {
		return "unauthorized"
	}
	return "unauthorized"
}
