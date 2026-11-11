//go:build unix

package tcp

import (
	"encoding/json/v2"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bradfitz/tsterm/internal/demoapp"
	"github.com/bradfitz/tsterm/pkg/proto"
	"github.com/bradfitz/tsterm/pkg/pty"
	"github.com/bradfitz/tsterm/pkg/session"
	"github.com/bradfitz/tsterm/pkg/vt"
)

func get(t *testing.T, url, accept string) (int, string, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("Content-Type"), string(b)
}

func TestWebIndexAndViewer(t *testing.T) {
	srv, hts := newTestServer(t, &allowAllLC{user: 1})
	code, _, body := get(t, hts.URL+"/", "text/html")
	if code != 200 || !strings.Contains(body, "No active sessions") {
		t.Errorf("empty index: %d %q", code, body)
	}

	sess, err := srv.Manager.Create(session.Config{
		Name: "build.v2_x-y",
		PTY:  pty.Config{Command: "/bin/sh", Args: []string{"-c", "sleep 5"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	code, ctype, body := get(t, hts.URL+"/", "text/html")
	if code != 200 || !strings.HasPrefix(ctype, "text/html") {
		t.Fatalf("index: %d %s", code, ctype)
	}
	if !strings.Contains(body, `href="/s/`+string(sess.ID())+`"`) {
		t.Errorf("index lacks a link to the session viewer: %s", body)
	}
	if !strings.Contains(body, ">build.v2_x-y<") {
		t.Errorf("index lacks the session's name: %s", body)
	}

	code, _, body = get(t, hts.URL+"/s/"+string(sess.ID()), "text/html")
	if code != 200 || !strings.Contains(body, `data-id="`+string(sess.ID())+`"`) || !strings.Contains(body, "/static/viewer.js") {
		t.Errorf("viewer page: %d %s", code, body)
	}
	if code, _, _ := get(t, hts.URL+"/s/pty-NOPE", "text/html"); code != 404 {
		t.Errorf("viewer for unknown session: %d, want 404", code)
	}
	for _, f := range []string{"/static/viewer.js", "/static/tsterm.css"} {
		if code, _, body := get(t, hts.URL+f, ""); code != 200 || len(body) == 0 {
			t.Errorf("%s: %d, %d bytes", f, code, len(body))
		}
	}
}

func TestWebForbiddenIsHTMLForBrowsers(t *testing.T) {
	_, hts := newTestServer(t, denyAllLC{})
	code, ctype, body := get(t, hts.URL+"/", "text/html,application/xhtml+xml")
	if code != 403 || !strings.HasPrefix(ctype, "text/html") || !strings.Contains(body, "owner") {
		t.Errorf("browser 403: %d %s %q", code, ctype, body)
	}
	// API clients still get CBOR.
	if _, ctype, _ := get(t, hts.URL+"/v1/sessions", ""); ctype != "application/cbor" {
		t.Errorf("API 403 content type = %q", ctype)
	}
}

func TestListJSONSortedByActivity(t *testing.T) {
	srv, hts := newTestServer(t, &allowAllLC{user: 1})
	var ids []string
	for range 3 {
		sess, err := srv.Manager.Create(session.Config{PTY: pty.Config{Command: "/bin/cat"}})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, string(sess.ID()))
	}
	// Type into the first session last, making it the most recently active.
	time.Sleep(10 * time.Millisecond)
	for _, i := range []int{1, 2, 0} {
		s, _ := srv.Manager.Resolve(ids[i])
		s.WriteInput([]byte("x"))
		time.Sleep(10 * time.Millisecond)
	}
	code, ctype, body := get(t, hts.URL+"/v1/sessions?sort=activity", "application/json")
	if code != 200 || ctype != "application/json" {
		t.Fatalf("list: %d %s %s", code, ctype, body)
	}
	var list []proto.SessionSummary
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range list {
		got = append(got, s.ID)
		// cat's echo makes output follow the input.
		if s.LastInput.IsZero() || s.LastActivity.Before(s.LastInput) {
			t.Errorf("%s: lastInput %v, lastActivity %v", s.ID, s.LastInput, s.LastActivity)
		}
	}
	if want := []string{ids[0], ids[2], ids[1]}; !slices.Equal(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
	if !strings.Contains(body, `"lastActivity":`) {
		t.Errorf("JSON lacks lastActivity: %s", body)
	}
	if code, _, _ := get(t, hts.URL+"/v1/sessions?sort=bogus", "application/json"); code != 400 {
		t.Errorf("bad sort: %d, want 400", code)
	}
}

func patchName(t *testing.T, url, name string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("PATCH", url, strings.NewReader(`{"name":"`+name+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestRename(t *testing.T) {
	srv, hts := newTestServer(t, &allowAllLC{user: 1})
	a, err := srv.Manager.Create(session.Config{PTY: pty.Config{Command: "/bin/cat"}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := srv.Manager.Create(session.Config{Name: "taken", PTY: pty.Config{Command: "/bin/cat"}})
	if err != nil {
		t.Fatal(err)
	}
	url := hts.URL + "/v1/sessions/" + string(a.ID())
	if code, body := patchName(t, url, "build"); code != 200 || !strings.Contains(body, `"name":"build"`) {
		t.Fatalf("rename: %d %s", code, body)
	}
	if got, err := srv.Manager.Resolve("build"); err != nil || got != a {
		t.Errorf("new name does not resolve: %v %v", got, err)
	}
	// Renaming again frees the old name.
	if code, _ := patchName(t, url, "build2"); code != 200 {
		t.Fatalf("second rename: %d", code)
	}
	if _, err := srv.Manager.Resolve("build"); err == nil {
		t.Error("old name still resolves")
	}
	for _, tt := range []struct {
		name string
		code int
	}{
		{"taken", 409},
		{"pty-sneaky", 400},
		{"has space", 400},
		{"build2", 200}, // its own name
		{"", 200},       // removes the name
	} {
		if code, body := patchName(t, url, tt.name); code != tt.code {
			t.Errorf("rename to %q: %d %s, want %d", tt.name, code, body, tt.code)
		}
	}
	if a.Name() != "" || b.Name() != "taken" {
		t.Errorf("names now %q and %q", a.Name(), b.Name())
	}
	if code, _ := patchName(t, hts.URL+"/v1/sessions/pty-NOPE", "x"); code != 404 {
		t.Errorf("rename of unknown session: %d, want 404", code)
	}
	// The pages offer renaming.
	_, _, body := get(t, hts.URL+"/", "text/html")
	if !strings.Contains(body, `class="rename" data-id="`+string(a.ID())+`"`) {
		t.Errorf("index has no rename cell: %s", body)
	}
}

func TestMain(m *testing.M) {
	// For the demo session, this binary is the demo program.
	demoapp.MainIfRequested()
	os.Exit(m.Run())
}

func TestCreateDemo(t *testing.T) {
	srv, hts := newTestServer(t, &allowAllLC{user: 1})
	post := func() (int, string) {
		req, _ := http.NewRequest("POST", hts.URL+"/v1/sessions", strings.NewReader(`{"name":"_tsterm_demo","rows":10,"cols":40}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := post(); code != 404 || !strings.Contains(body, "no_demo") {
		t.Errorf("demo without a demo program: %d %s", code, body)
	}
	srv.Manager.SetDemo(func() pty.Config {
		cmd, env := demoapp.Command()
		return pty.Config{Command: cmd, Env: env}
	})
	code, body := post()
	if code != 201 || !strings.Contains(body, `"name":"_tsterm_demo"`) {
		t.Fatalf("creating the demo: %d %s", code, body)
	}
	sess, err := srv.Manager.Resolve("_tsterm_demo")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(rowsText(sess.Snapshot().Cells), demoapp.ReadyText) {
		if time.Now().After(deadline) {
			t.Fatalf("demo never became ready:\n%s", rowsText(sess.Snapshot().Cells))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if code, body := post(); code != 201 || !strings.Contains(body, `"name":"_tsterm_demo2"`) {
		t.Errorf("second demo: %d %s", code, body)
	}
	if code, _ := patchName(t, hts.URL+"/v1/sessions/"+string(sess.ID()), "_tsterm_mine"); code != 400 {
		t.Errorf("renaming to a reserved name: %d, want 400", code)
	}
}

func rowsText(cells [][]vt.Cell) string {
	var sb strings.Builder
	for _, row := range cells {
		for _, c := range row {
			sb.WriteString(c.Grapheme)
		}
		sb.WriteByte('\n')
	}
	return sb.String()
}
