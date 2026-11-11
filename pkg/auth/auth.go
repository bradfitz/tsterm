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
//
// Peer identities come from the IPN bus (see Watch), which reports every
// change to the tailnet's nodes and their addresses, so a decision never
// rests on stale identity. Addresses the bus has not described yet fall
// back to a WhoIs lookup.
package auth

import (
	"context"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/bradfitz/tsterm/pkg/proto"

	"tailscale.com/client/local"
	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/ipn"
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
	lc WhoIsClient

	mu   sync.Mutex
	self tailcfg.UserID

	// nodes is the tailnet as last reported on the IPN bus, including the
	// local node, or nil before the first report. owner is derived from
	// it: the owning user of each Tailscale address.
	nodes map[tailcfg.NodeID]node
	owner map[netip.Addr]tailcfg.UserID

	// changed is closed, and replaced, whenever owner changes.
	changed chan struct{}
}

// node is what SelfUser tracks about each tailnet node.
type node struct {
	user  tailcfg.UserID
	addrs []netip.Addr
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
		lc:      lc,
		self:    st.Self.UserID,
		changed: make(chan struct{}),
	}, nil
}

// SelfUserID returns the local node's owner UserID.
func (a *SelfUser) SelfUserID() tailcfg.UserID {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.self
}

// Allow reports whether the peer at remoteAddr, an "ip:port" or "ip", is
// owned by the same tailnet user as the local node. It returns
// ErrUnauthorized if not.
func (a *SelfUser) Allow(ctx context.Context, remoteAddr string) error {
	ip, err := parseIP(remoteAddr)
	if err != nil {
		return ErrUnauthorized
	}
	a.mu.Lock()
	self := a.self
	user, known := a.owner[ip]
	a.mu.Unlock()
	if !known {
		// Not (yet) on the IPN bus: perhaps a brand-new peer, or the bus
		// is not being watched. Ask tailscaled.
		w, err := a.lc.WhoIs(ctx, remoteAddr)
		if err != nil {
			return err
		}
		if w == nil || w.Node == nil {
			return ErrUnauthorized
		}
		user = w.Node.User
	}
	if user != self {
		return ErrUnauthorized
	}
	return nil
}

func parseIP(remoteAddr string) (netip.Addr, error) {
	if ap, err := netip.ParseAddrPort(remoteAddr); err == nil {
		return ap.Addr().Unmap(), nil
	}
	ip, err := netip.ParseAddr(remoteAddr)
	return ip.Unmap(), err
}

// Changes returns a channel that is closed the next time the set of
// addresses and their owners changes. Holders of long-lived authorizations
// (attached clients) should then call Allow again.
func (a *SelfUser) Changes() <-chan struct{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.changed
}

// ApplyNotify updates the tailnet's nodes from an IPN bus message, as
// requested with NotifyInitialStatus and NotifyPeerChanges.
func (a *SelfUser) ApplyNotify(n ipn.Notify) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if st := n.InitialStatus; st != nil {
		a.nodes = map[tailcfg.NodeID]node{}
		if st.Self != nil {
			a.setStatusLocked(st.Self)
			if st.Self.UserID != 0 {
				a.self = st.Self.UserID
			}
		}
		for _, ps := range st.Peer {
			a.setStatusLocked(ps)
		}
	}
	if a.nodes == nil {
		return // deltas are meaningless before the initial status
	}
	if n.SelfChange != nil {
		a.setNodeLocked(n.SelfChange)
		if n.SelfChange.User != 0 {
			a.self = n.SelfChange.User
		}
	}
	for _, nd := range n.PeersChanged {
		a.setNodeLocked(nd)
	}
	for _, id := range n.PeersRemoved {
		delete(a.nodes, id)
	}
	if n.InitialStatus != nil || n.SelfChange != nil || len(n.PeersChanged) > 0 || len(n.PeersRemoved) > 0 {
		a.rebuildLocked()
	}
}

func (a *SelfUser) setStatusLocked(ps *ipnstate.PeerStatus) {
	a.nodes[ps.NodeID] = node{user: ps.UserID, addrs: ps.TailscaleIPs}
}

func (a *SelfUser) setNodeLocked(nd *tailcfg.Node) {
	var addrs []netip.Addr
	for _, pfx := range nd.Addresses {
		if pfx.IsSingleIP() {
			addrs = append(addrs, pfx.Addr())
		}
	}
	a.nodes[nd.ID] = node{user: nd.User, addrs: addrs}
}

// rebuildLocked recomputes owner and, if it changed, wakes Changes.
func (a *SelfUser) rebuildLocked() {
	owner := map[netip.Addr]tailcfg.UserID{}
	for _, nd := range a.nodes {
		for _, ip := range nd.addrs {
			owner[ip.Unmap()] = nd.user
		}
	}
	if a.owner != nil && maps.Equal(owner, a.owner) {
		return
	}
	a.owner = owner
	close(a.changed)
	a.changed = make(chan struct{})
}

// Watch keeps a's view of the tailnet current by watching lc's IPN bus,
// until ctx is done. It reconnects if the bus connection breaks, as when
// tailscaled restarts.
func (a *SelfUser) Watch(ctx context.Context, lc *local.Client, logf func(format string, args ...any)) {
	for ctx.Err() == nil {
		w, err := lc.WatchIPNBus(ctx, ipn.NotifyInitialStatus|ipn.NotifyPeerChanges)
		if err == nil {
			for {
				n, err := w.Next()
				if err != nil {
					break
				}
				a.ApplyNotify(n)
			}
			w.Close()
		}
		if ctx.Err() != nil {
			return
		}
		logf("auth: IPN bus watch ended (%v); retrying", err)
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
}

// HTTPMiddleware wraps h to enforce Allow against r.RemoteAddr. On failure
// it responds 403 with a small CBOR error body.
func (a *SelfUser) HTTPMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := a.Allow(r.Context(), r.RemoteAddr); err != nil {
			writeAuthError(w, r, err)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func writeAuthError(w http.ResponseWriter, r *http.Request, err error) {
	if strings.Contains(r.Header.Get("Accept"), "text/html") {
		// A browser, presumably visiting the web UI.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `<!doctype html><title>tsterm: forbidden</title>`+
			`<p>Only the owner of this machine's Tailscale node may use tsterm here.`)
		return
	}
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
