package tcp

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bradfitz/tsterm/pkg/auth"
	"github.com/bradfitz/tsterm/pkg/proto"
	"github.com/bradfitz/tsterm/pkg/pty"
	"github.com/bradfitz/tsterm/pkg/session"
	"github.com/bradfitz/tsterm/pkg/session/sessiontest"

	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
)

// allowAllLC accepts any RemoteAddr as the same tailnet user as the local
// node. It is the most permissive valid auth substrate for transport tests.
type allowAllLC struct{ user tailcfg.UserID }

func (a *allowAllLC) WhoIs(context.Context, string) (*apitype.WhoIsResponse, error) {
	return &apitype.WhoIsResponse{Node: &tailcfg.Node{User: a.user}}, nil
}
func (a *allowAllLC) Status(context.Context) (*ipnstate.Status, error) {
	return &ipnstate.Status{Self: &ipnstate.PeerStatus{UserID: a.user}}, nil
}

// denyAllLC accepts a Status but every WhoIs returns a "different user."
type denyAllLC struct{}

func (denyAllLC) WhoIs(context.Context, string) (*apitype.WhoIsResponse, error) {
	return &apitype.WhoIsResponse{Node: &tailcfg.Node{User: tailcfg.UserID(9999)}}, nil
}
func (denyAllLC) Status(context.Context) (*ipnstate.Status, error) {
	return &ipnstate.Status{Self: &ipnstate.PeerStatus{UserID: tailcfg.UserID(1)}}, nil
}

func newTestServer(t *testing.T, lc auth.WhoIsClient) (*Server, *httptest.Server) {
	t.Helper()
	a, err := auth.NewSelfUser(context.Background(), lc)
	if err != nil {
		t.Fatalf("NewSelfUser: %v", err)
	}
	srv := &Server{
		Manager: sessiontest.NewManager(t),
		Auth:    a,
		UDPPort: 12345,
	}
	hts := httptest.NewServer(srv.Handler())
	t.Cleanup(hts.Close)
	return srv, hts
}

func sleepCfg() proto.CreateSessionReq {
	return proto.CreateSessionReq{
		Cmd: []string{"/bin/sh", "-c", "sleep 5"},
	}
}

func TestUnauthorized(t *testing.T) {
	_, hts := newTestServer(t, denyAllLC{})
	resp, err := http.Get(hts.URL + "/v1/sessions")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

func TestCreateAndList(t *testing.T) {
	_, hts := newTestServer(t, &allowAllLC{user: 42})

	body, err := proto.Encode(sleepCfg())
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("POST", hts.URL+"/v1/sessions", bytes.NewReader(body))
	req.Header.Set("Content-Type", proto.ContentType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("create status = %d, body=%s", resp.StatusCode, raw)
	}
	respBody, _ := io.ReadAll(resp.Body)
	var info proto.SessionInfo
	if err := proto.Decode(respBody, &info); err != nil {
		t.Fatalf("decode info: %v", err)
	}
	if info.ID == "" || len(info.Token) == 0 {
		t.Errorf("info missing fields: %+v", info)
	}
	if info.UDPPort != 12345 {
		t.Errorf("info UDPPort = %d, want 12345", info.UDPPort)
	}

	// List should contain it.
	resp, err = http.Get(hts.URL + "/v1/sessions")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	listBody, _ := io.ReadAll(resp.Body)
	var sums []proto.SessionSummary
	if err := proto.Decode(listBody, &sums); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(sums) != 1 || sums[0].ID != info.ID {
		t.Errorf("list = %+v, want one with ID %q", sums, info.ID)
	}
}

func TestGetAndKill(t *testing.T) {
	srv, hts := newTestServer(t, &allowAllLC{user: 42})

	cfg := sleepCfg()
	cfg.Name = "foo"
	body, _ := proto.Encode(cfg)
	r, _ := http.NewRequest("POST", hts.URL+"/v1/sessions", bytes.NewReader(body))
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	resp, err = http.Get(hts.URL + "/v1/sessions/foo")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("get-by-name status = %d", resp.StatusCode)
	}
	var info proto.SessionInfo
	b, _ := io.ReadAll(resp.Body)
	_ = proto.Decode(b, &info)
	if info.Name != "foo" {
		t.Errorf("info.Name = %q, want foo", info.Name)
	}

	r2, _ := http.NewRequest("DELETE", hts.URL+"/v1/sessions/foo", nil)
	resp, err = http.DefaultClient.Do(r2)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("delete status = %d", resp.StatusCode)
	}
	if srv.Manager.ByName("foo") != nil {
		t.Errorf("session still in manager after delete")
	}
}

func TestNameConflict(t *testing.T) {
	_, hts := newTestServer(t, &allowAllLC{user: 42})
	cfg := sleepCfg()
	cfg.Name = "dup"
	body, _ := proto.Encode(cfg)
	for i := range 2 {
		req, _ := http.NewRequest("POST", hts.URL+"/v1/sessions", bytes.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if i == 0 && resp.StatusCode != http.StatusCreated {
			t.Fatalf("first create status = %d", resp.StatusCode)
		}
		if i == 1 && resp.StatusCode != http.StatusConflict {
			t.Errorf("second create status = %d, want 409", resp.StatusCode)
		}
	}
}

func TestServeContextShutdown(t *testing.T) {
	a, err := auth.NewSelfUser(context.Background(), &allowAllLC{user: 1})
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{Manager: sessiontest.NewManager(t), Auth: a}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, ln, srv) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve returned %v, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("Serve did not return after cancel")
	}
}

func TestCloseAndPurge(t *testing.T) {
	srv, hts := newTestServer(t, &allowAllLC{user: 42})
	cfg := sleepCfg()
	cfg.Name = "bar"
	sess, err := srv.Manager.Create(session.Config{Name: "bar", PTY: pty.Config{Command: cfg.Cmd[0], Args: cfg.Cmd[1:]}})
	if err != nil {
		t.Fatal(err)
	}
	do := func(method, path, body string) int {
		t.Helper()
		r, _ := http.NewRequest(method, hts.URL+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json")
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	summary := func() proto.SessionSummary {
		t.Helper()
		r, _ := http.NewRequest("GET", hts.URL+"/v1/sessions", nil)
		r.Header.Set("Accept", "application/json")
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var list []proto.SessionSummary
		b, _ := io.ReadAll(resp.Body)
		json.Unmarshal(b, &list)
		for _, s := range list {
			if s.ID == string(sess.ID()) {
				return s
			}
		}
		t.Fatal("session not listed")
		return proto.SessionSummary{}
	}

	if code := do("PATCH", "/v1/sessions/bar", `{"closed": true}`); code != 200 {
		t.Fatalf("PATCH closed: %d", code)
	}
	if s := summary(); !s.Closed || !s.Active || len(s.Cmd) != 3 {
		t.Errorf("closed session listed as %+v", s)
	}
	do("PATCH", "/v1/sessions/bar", `{"closed": false}`)
	if s := summary(); s.Closed {
		t.Error("reopened session still closed")
	}

	// DELETE ends it, keeping it frozen, and again is a no-op.
	for range 2 {
		if code := do("DELETE", "/v1/sessions/"+string(sess.ID()), ""); code != http.StatusNoContent {
			t.Fatalf("DELETE: %d", code)
		}
	}
	if s := summary(); s.Active || s.Status == "" || s.Name != "bar" {
		t.Errorf("ended session listed as %+v", s)
	}
	// Ended, it can still be closed.
	if code := do("PATCH", "/v1/sessions/"+string(sess.ID()), `{"closed": true}`); code != http.StatusNoContent {
		t.Errorf("PATCH closed on an ended session: %d", code)
	}
	if s := summary(); !s.Closed {
		t.Error("ended session not closed")
	}
	// Only purging deletes it.
	if code := do("DELETE", "/v1/sessions/"+string(sess.ID())+"?purge=1", ""); code != http.StatusNoContent {
		t.Fatalf("purge: %d", code)
	}
	if srv.Manager.Frozen(string(sess.ID())) != nil {
		t.Error("purged session still frozen")
	}
}
