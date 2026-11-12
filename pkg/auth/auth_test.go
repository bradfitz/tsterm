package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/bradfitz/tsterm/pkg/proto"

	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
)

type fakeLC struct {
	self      tailcfg.UserID
	selfLogin string
	who       map[string]*apitype.WhoIsResponse
	whoErr    map[string]error
	statusErr error
}

func (f *fakeLC) WhoIs(_ context.Context, addr string) (*apitype.WhoIsResponse, error) {
	if err, ok := f.whoErr[addr]; ok {
		return nil, err
	}
	w, ok := f.who[addr]
	if !ok {
		return nil, errors.New("no such peer")
	}
	return w, nil
}

func (f *fakeLC) Status(context.Context) (*ipnstate.Status, error) {
	if f.statusErr != nil {
		return nil, f.statusErr
	}
	return &ipnstate.Status{
		Self: &ipnstate.PeerStatus{UserID: f.self},
		User: map[tailcfg.UserID]tailcfg.UserProfile{f.self: {LoginName: f.selfLogin}},
	}, nil
}

func newAuth(t *testing.T, lc *fakeLC) *SelfUser {
	t.Helper()
	a, err := NewSelfUser(context.Background(), lc)
	if err != nil {
		t.Fatalf("NewSelfUser: %v", err)
	}
	return a
}

func TestAllowSameUser(t *testing.T) {
	const me tailcfg.UserID = 1234
	lc := &fakeLC{
		self: me,
		who: map[string]*apitype.WhoIsResponse{
			"100.64.0.5:51234": {Node: &tailcfg.Node{User: me}},
		},
	}
	a := newAuth(t, lc)
	if err := a.Allow(context.Background(), "100.64.0.5:51234"); err != nil {
		t.Errorf("Allow same-user: %v", err)
	}
}

func TestDenyDifferentUser(t *testing.T) {
	const me, other tailcfg.UserID = 1234, 5678
	lc := &fakeLC{
		self: me,
		who: map[string]*apitype.WhoIsResponse{
			"100.64.0.6:1": {Node: &tailcfg.Node{User: other}},
		},
	}
	a := newAuth(t, lc)
	err := a.Allow(context.Background(), "100.64.0.6:1")
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("Allow different-user: got %v, want ErrUnauthorized", err)
	}
}

func TestDenyWhoIsError(t *testing.T) {
	lc := &fakeLC{
		self:   1,
		whoErr: map[string]error{"100.64.0.7:1": errors.New("boom")},
	}
	a := newAuth(t, lc)
	err := a.Allow(context.Background(), "100.64.0.7:1")
	if err == nil || errors.Is(err, ErrUnauthorized) {
		t.Errorf("expected propagated WhoIs error, got %v", err)
	}
}

func TestDenyMissingNode(t *testing.T) {
	lc := &fakeLC{
		self: 1,
		who:  map[string]*apitype.WhoIsResponse{"100.64.0.8:1": {Node: nil}},
	}
	a := newAuth(t, lc)
	err := a.Allow(context.Background(), "100.64.0.8:1")
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("missing-node should be unauthorized, got %v", err)
	}
}

func TestStatusNoUserIDFailsConstruction(t *testing.T) {
	if _, err := NewSelfUser(context.Background(), &fakeLC{self: 0}); err == nil {
		t.Errorf("expected error when Self.UserID is 0")
	}
}

func TestHTTPMiddlewareAllowed(t *testing.T) {
	const me tailcfg.UserID = 7
	lc := &fakeLC{
		self: me,
		who:  map[string]*apitype.WhoIsResponse{"1.2.3.4:5": {Node: &tailcfg.Node{User: me}}},
	}
	a := newAuth(t, lc)
	called := false
	h := a.HTTPMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(204)
	}))
	srv := httptest.NewServer(h)
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL, nil)
	req.RemoteAddr = "1.2.3.4:5"
	// Use a recorder so RemoteAddr is honored verbatim.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 204 {
		t.Errorf("status = %d, want 204; body=%q", rec.Code, rec.Body.String())
	}
	if !called {
		t.Error("wrapped handler was not called")
	}
}

func TestHTTPMiddlewareDeniedReturnsCBORError(t *testing.T) {
	const me, other tailcfg.UserID = 7, 8
	lc := &fakeLC{
		self: me,
		who:  map[string]*apitype.WhoIsResponse{"1.2.3.4:5": {Node: &tailcfg.Node{User: other}}},
	}
	a := newAuth(t, lc)
	h := a.HTTPMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("wrapped handler must not run for denied request")
	}))
	req, _ := http.NewRequest("GET", "/", nil)
	req.RemoteAddr = "1.2.3.4:5"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "cbor") {
		t.Errorf("content-type = %q, want cbor", ct)
	}
	var e proto.Error
	if err := proto.Decode(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("decode CBOR error body: %v", err)
	}
	if e.Code != "unauthorized" {
		t.Errorf("error code = %q, want %q", e.Code, "unauthorized")
	}
}

func TestDenyUnparseableAddr(t *testing.T) {
	a := newAuth(t, &fakeLC{self: 1})
	if err := a.Allow(context.Background(), "not-an-addr"); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("err = %v, want ErrUnauthorized", err)
	}
}

// busStatus returns an initial IPN bus status for self, owned by user 1,
// and the given peers.
func busStatus(peers ...*ipnstate.PeerStatus) ipn.Notify {
	st := &ipnstate.Status{
		Self: &ipnstate.PeerStatus{NodeID: 1, UserID: 1, TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.64.0.1")}},
		Peer: map[key.NodePublic]*ipnstate.PeerStatus{},
	}
	for _, p := range peers {
		st.Peer[key.NewNode().Public()] = p
	}
	return ipn.Notify{InitialStatus: st}
}

func TestIPNBusDrivesDecisions(t *testing.T) {
	// The fake WhoIs knows nothing, so every decision below must come
	// from the bus.
	a := newAuth(t, &fakeLC{self: 1})
	ctx := context.Background()
	mine := netip.MustParseAddr("100.64.0.2")
	mine6 := netip.MustParseAddr("fd7a:115c:a1e0::2")
	theirs := netip.MustParseAddr("100.64.0.3")
	a.ApplyNotify(busStatus(
		&ipnstate.PeerStatus{NodeID: 2, UserID: 1, TailscaleIPs: []netip.Addr{mine, mine6}},
		&ipnstate.PeerStatus{NodeID: 3, UserID: 99, TailscaleIPs: []netip.Addr{theirs}},
	))
	for addr, want := range map[string]bool{
		"100.64.0.1:5":          true, // self
		"100.64.0.2:5":          true,
		"[fd7a:115c:a1e0::2]:5": true,
		"[::ffff:100.64.0.2]:5": true, // v4-mapped
		"100.64.0.3:5":          false,
	} {
		if got := a.Allow(ctx, addr) == nil; got != want {
			t.Errorf("Allow(%s) = %v, want %v", addr, got, want)
		}
	}

	// A node changing addresses wakes Changes and moves authorization.
	changes := a.Changes()
	a.ApplyNotify(ipn.Notify{PeersChanged: []*tailcfg.Node{{
		ID:        3,
		User:      99,
		Addresses: []netip.Prefix{netip.PrefixFrom(mine, 32)},
	}}})
	select {
	case <-changes:
	default:
		t.Fatal("Changes not closed after an address change")
	}
	if a.Allow(ctx, "100.64.0.2:5") == nil {
		t.Error("address claimed by another user's node is still allowed")
	}
	// Once the old node gives the address up, it is the new owner's.
	a.ApplyNotify(ipn.Notify{PeersChanged: []*tailcfg.Node{{
		ID:        2,
		User:      1,
		Addresses: []netip.Prefix{netip.PrefixFrom(mine6, 128)},
	}}})
	if a.Allow(ctx, "100.64.0.2:5") == nil {
		t.Error("address now owned by another user is still allowed")
	}
	if a.Allow(ctx, "[fd7a:115c:a1e0::2]:5") != nil {
		t.Error("node's remaining address no longer allowed")
	}

	// Removing that node makes its address unknown again, which falls
	// back to WhoIs (and so is denied here).
	a.ApplyNotify(ipn.Notify{PeersRemoved: []tailcfg.NodeID{3}})
	if a.Allow(ctx, "100.64.0.3:5") == nil {
		t.Error("removed node's address still allowed")
	}
}

func TestIPNBusNoChangeNoWake(t *testing.T) {
	a := newAuth(t, &fakeLC{self: 1})
	a.ApplyNotify(busStatus())
	changes := a.Changes()
	// A redundant update must not wake waiters.
	a.ApplyNotify(ipn.Notify{PeersChanged: []*tailcfg.Node{{
		ID: 1, User: 1, Addresses: []netip.Prefix{netip.MustParsePrefix("100.64.0.1/32")},
	}}})
	select {
	case <-changes:
		t.Error("Changes closed without a change")
	default:
	}
}
