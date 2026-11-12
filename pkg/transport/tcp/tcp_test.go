package tcp

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
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

// viewerAuth authorizes every peer as a viewer, as tstermd's sharing node
// does people other than the owner.
type viewerAuth struct{}

func (viewerAuth) Authorize(context.Context, string) (auth.Peer, error) {
	return auth.Peer{Access: auth.Viewer, Login: "other@example.com"}, nil
}
func (viewerAuth) Changes() <-chan struct{} { return nil }

// TestViewerAccess checks that a viewer sees only the sessions the owner
// shares, and may change nothing.
func TestViewerAccess(t *testing.T) {
	srv := &Server{Manager: sessiontest.NewManager(t), Auth: viewerAuth{}, ShareURL: func() string { return "https://share.example/" }}
	hts := httptest.NewServer(srv.Handler())
	t.Cleanup(hts.Close)
	mk := func(name string) *session.Session {
		cfg := sleepCfg()
		s, err := srv.Manager.Create(session.Config{Name: name, PTY: pty.Config{Command: cfg.Cmd[0], Args: cfg.Cmd[1:]}})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	secret, open := mk("secret"), mk("open")
	if err := srv.Manager.SetShared("open", true); err != nil {
		t.Fatal(err)
	}
	do := func(method, path, body string) (int, string) {
		t.Helper()
		r, _ := http.NewRequest(method, hts.URL+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json")
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	code, body := do("GET", "/v1/sessions", "")
	var list []proto.SessionSummary
	if err := json.Unmarshal([]byte(body), &list); code != 200 || err != nil {
		t.Fatalf("list: %d, %v", code, err)
	}
	if len(list) != 1 || list[0].ID != string(open.ID()) || !list[0].Shared {
		t.Errorf("viewer's list = %+v, want just the shared session", list)
	}
	if code, _ := do("GET", "/s/"+string(secret.ID()), ""); code != http.StatusNotFound {
		t.Errorf("viewer page of an unshared session: %d, want 404", code)
	}
	if code, _ := do("GET", "/s/"+string(open.ID()), ""); code != 200 {
		t.Errorf("viewer page of a shared session: %d, want 200", code)
	}
	r, _ := http.NewRequest("GET", hts.URL+"/", nil)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if page := string(b); strings.Contains(page, "secret") || !strings.Contains(page, ">open<") || strings.Contains(page, `id="new"`) || strings.Contains(page, `class="pencil"`) || strings.Contains(page, `class="share`) {
		t.Errorf("viewer's index page shows too much:\n%s", page)
	}

	for _, tc := range []struct{ method, path, body string }{
		{"POST", "/v1/sessions", `{}`},
		{"GET", "/v1/sessions/open", ""},
		{"DELETE", "/v1/sessions/open", ""},
		{"PATCH", "/v1/sessions/open", `{"shared": true}`},
		{"PATCH", "/v1/sessions/secret", `{"shared": true}`},
		{"PATCH", "/v1/sessions/open", `{"name": "mine"}`},
	} {
		if code, _ := do(tc.method, tc.path, tc.body); code != http.StatusForbidden {
			t.Errorf("viewer %s %s: %d, want 403", tc.method, tc.path, code)
		}
	}
	if secret.Shared() || open.Name() != "open" {
		t.Error("a viewer changed a session")
	}
	select {
	case <-open.Done():
		t.Error("a viewer killed a session")
	default:
	}
}

// fakeShareNode is a tcp.ShareNodeControl that pretends to start nodes.
type fakeShareNode struct {
	st  proto.ShareNode
	req proto.StartShareNodeReq
}

func (f *fakeShareNode) Status(context.Context) proto.ShareNode { return f.st }
func (f *fakeShareNode) Start(_ context.Context, req proto.StartShareNodeReq) error {
	if f.st.State != "off" {
		return errors.New("already started")
	}
	f.req = req
	f.st = proto.ShareNode{State: "login", Hostname: req.Hostname, LoginURL: "https://login.example/"}
	return nil
}
func (f *fakeShareNode) Destroy(context.Context) error {
	f.st = proto.ShareNode{State: "off"}
	return nil
}

func TestShareNodeAPI(t *testing.T) {
	fake := &fakeShareNode{st: proto.ShareNode{State: "off", Hostname: "braid-shell"}}
	for _, acc := range []auth.Access{auth.Owner, auth.Viewer} {
		srv := &Server{Manager: sessiontest.NewManager(t), Auth: fixedAuth(acc), ShareNode: fake}
		hts := httptest.NewServer(srv.Handler())
		t.Cleanup(hts.Close)
		do := func(method, body string) (int, string) {
			t.Helper()
			r, _ := http.NewRequest(method, hts.URL+"/v1/share", strings.NewReader(body))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Accept", "application/json")
			resp, err := http.DefaultClient.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			return resp.StatusCode, string(b)
		}
		if acc == auth.Viewer {
			for _, m := range []string{"GET", "POST", "DELETE"} {
				if code, _ := do(m, "{}"); code != http.StatusForbidden {
					t.Errorf("viewer %s /v1/share: %d, want 403", m, code)
				}
			}
			continue
		}
		if code, body := do("GET", ""); code != 200 || !strings.Contains(body, `"state":"off"`) {
			t.Errorf("GET: %d %s", code, body)
		}
		if code, body := do("POST", `{"hostname":"x-shell","tags":["tag:a"],"authKey":"tskey-secret"}`); code != 200 || !strings.Contains(body, `"state":"login"`) || strings.Contains(body, "tskey") {
			t.Errorf("POST: %d %s", code, body)
		}
		if fake.req.Hostname != "x-shell" || fake.req.AuthKey != "tskey-secret" || len(fake.req.Tags) != 1 {
			t.Errorf("Start got %+v", fake.req)
		}
		if code, _ := do("POST", `{}`); code != http.StatusBadRequest {
			t.Errorf("second POST: %d, want 400", code)
		}
		if code, _ := do("DELETE", ""); code != http.StatusNoContent || fake.st.State != "off" {
			t.Errorf("DELETE: %d, state %q", code, fake.st.State)
		}
		r, _ := http.Get(hts.URL + "/")
		b, _ := io.ReadAll(r.Body)
		r.Body.Close()
		if !strings.Contains(string(b), `id="sharenode"`) {
			t.Error("owner's index page has no sharing node section")
		}
	}
}

// fixedAuth authorizes every peer with the same access.
type fixedAuth auth.Access

func (f fixedAuth) Authorize(context.Context, string) (auth.Peer, error) {
	return auth.Peer{Access: auth.Access(f)}, nil
}
func (fixedAuth) Changes() <-chan struct{} { return nil }
