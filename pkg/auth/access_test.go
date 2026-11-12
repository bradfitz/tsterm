package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/tailcfg"
)

func who(user tailcfg.UserID, login string, tags ...string) *apitype.WhoIsResponse {
	return &apitype.WhoIsResponse{
		Node:        &tailcfg.Node{User: user, Tags: tags},
		UserProfile: &tailcfg.UserProfile{ID: user, LoginName: login},
	}
}

func TestSharing(t *testing.T) {
	owner := newAuth(t, &fakeLC{self: 1, selfLogin: "bob@example.com"})
	s := &Sharing{Owner: owner, LC: &fakeLC{
		who: map[string]*apitype.WhoIsResponse{
			// The sharing node's own user IDs needn't match tailscaled's.
			"100.64.0.1:1": who(77, "bob@example.com"),
			"100.64.0.2:1": who(78, "Bob@Example.com"),
			"100.64.0.3:1": who(79, "alice@example.com"),
			"100.64.0.4:1": who(77, "bob@example.com", "tag:server"), // tagged, owned by bob
			"100.64.0.5:1": {Node: &tailcfg.Node{User: 80}},          // no profile
		},
		whoErr: map[string]error{"100.64.0.6:1": errors.New("boom")},
	}}
	for addr, want := range map[string]Access{
		"100.64.0.1:1": Owner,
		"100.64.0.2:1": Owner, // logins are case-insensitive
		"100.64.0.3:1": Viewer,
		"100.64.0.4:1": NoAccess,
		"100.64.0.5:1": NoAccess,
		"100.64.0.6:1": NoAccess,
		"100.64.0.7:1": NoAccess, // unknown
	} {
		p, err := s.Authorize(context.Background(), addr)
		if got := p.Access; got != want || (want == NoAccess) != (err != nil) {
			t.Errorf("Authorize(%s) = %v, %v; want %v", addr, got, err, want)
		}
	}
}

func TestSharingOwnerWithoutLogin(t *testing.T) {
	// If tailscaled's owner's login is unknown, nobody is the owner.
	s := &Sharing{Owner: newAuth(t, &fakeLC{self: 1}), LC: &fakeLC{
		who: map[string]*apitype.WhoIsResponse{"100.64.0.1:1": who(1, "")},
	}}
	if p, err := s.Authorize(context.Background(), "100.64.0.1:1"); err == nil || p.Access != NoAccess {
		t.Errorf("Authorize = %v, %v", p.Access, err)
	}
}

func TestMiddlewarePeer(t *testing.T) {
	owner := newAuth(t, &fakeLC{self: 1, selfLogin: "bob@example.com"})
	s := &Sharing{Owner: owner, LC: &fakeLC{who: map[string]*apitype.WhoIsResponse{
		"100.64.0.3:1": who(79, "alice@example.com"),
	}}}
	var got Peer
	h := Middleware(s, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = PeerFrom(r.Context()) }))
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "100.64.0.3:1"
	h.ServeHTTP(httptest.NewRecorder(), r)
	if got.Access != Viewer || got.Login != "alice@example.com" {
		t.Errorf("peer = %+v", got)
	}
	r.RemoteAddr = "100.64.0.9:1"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Errorf("unknown peer: %d", rec.Code)
	}
}
