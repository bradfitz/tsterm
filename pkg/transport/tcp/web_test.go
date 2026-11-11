//go:build unix

package tcp

import (
	"encoding/json/v2"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bradfitz/tsterm/pkg/proto"
	"github.com/bradfitz/tsterm/pkg/pty"
	"github.com/bradfitz/tsterm/pkg/session"
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
		Name: "build<b>",
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
	if strings.Contains(body, "build<b>") || !strings.Contains(body, "build&lt;b&gt;") {
		t.Errorf("session name not HTML-escaped: %s", body)
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
