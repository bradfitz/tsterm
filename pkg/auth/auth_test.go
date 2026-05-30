package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bradfitz/tsterm/pkg/proto"

	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
)

type fakeLC struct {
	self  tailcfg.UserID
	who   map[string]*apitype.WhoIsResponse
	whoErr map[string]error
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
	if _, err := a.Allow(context.Background(), "100.64.0.5:51234"); err != nil {
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
	_, err := a.Allow(context.Background(), "100.64.0.6:1")
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("Allow different-user: got %v, want ErrUnauthorized", err)
	}
}

func TestDenyWhoIsError(t *testing.T) {
	lc := &fakeLC{
		self:   1,
		whoErr: map[string]error{"x": errors.New("boom")},
	}
	a := newAuth(t, lc)
	_, err := a.Allow(context.Background(), "x")
	if err == nil || errors.Is(err, ErrUnauthorized) {
		t.Errorf("expected propagated WhoIs error, got %v", err)
	}
}

func TestDenyMissingNode(t *testing.T) {
	lc := &fakeLC{
		self: 1,
		who:  map[string]*apitype.WhoIsResponse{"y": {Node: nil}},
	}
	a := newAuth(t, lc)
	_, err := a.Allow(context.Background(), "y")
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
