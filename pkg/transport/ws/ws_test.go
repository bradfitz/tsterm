//go:build unix

package ws

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/bradfitz/tsterm/pkg/proto"
	"github.com/bradfitz/tsterm/pkg/pty"
	"github.com/bradfitz/tsterm/pkg/session"
	"github.com/bradfitz/tsterm/pkg/session/sessiontest"
)

// mountTestServer puts the ws.Server behind a mux that supplies the {id}
// path parameter, matching how the TCP transport routes in production.
func mountTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	mgr := sessiontest.NewManager(t)
	srv := &Server{
		Manager: mgr,
	}
	mux := http.NewServeMux()
	mux.Handle("/v1/sessions/{id}/ws", srv.Handler())
	hts := httptest.NewServer(mux)
	t.Cleanup(hts.Close)
	return srv, hts
}

func dialWS(t *testing.T, url string) (*websocket.Conn, context.Context, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		cancel()
		t.Fatalf("Dial: %v", err)
	}
	return conn, ctx, cancel
}

func readEnv(t *testing.T, conn *websocket.Conn, ctx context.Context) proto.Envelope {
	t.Helper()
	mt, b, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if mt != websocket.MessageBinary {
		t.Fatalf("message type = %v, want binary", mt)
	}
	var env proto.Envelope
	if err := proto.Decode(b, &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return env
}

func TestViewerReceivesSnapshot(t *testing.T) {
	srv, hts := mountTestServer(t)
	sess, err := srv.Manager.Create(session.Config{
		PTY: pty.Config{Command: "/bin/sh", Args: []string{"-c", "printf hello; sleep 5"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	url := "ws://" + hts.Listener.Addr().String() + "/v1/sessions/" + string(sess.ID()) + "/ws"
	conn, ctx, cancel := dialWS(t, url)
	defer cancel()
	defer conn.Close(websocket.StatusNormalClosure, "test done")

	env := readEnv(t, conn, ctx)
	if env.Snapshot == nil {
		t.Fatalf("first frame = %+v, want Snapshot", env)
	}
	if env.Snapshot.S.Rows == 0 || env.Snapshot.S.Cols == 0 {
		t.Errorf("snapshot size = %dx%d, want non-zero", env.Snapshot.S.Rows, env.Snapshot.S.Cols)
	}
}

func TestViewerReceivesDeltaAfterWrite(t *testing.T) {
	srv, hts := mountTestServer(t)
	sess, err := srv.Manager.Create(session.Config{
		PTY: pty.Config{Command: "/bin/cat"},
	})
	if err != nil {
		t.Fatal(err)
	}
	url := "ws://" + hts.Listener.Addr().String() + "/v1/sessions/" + string(sess.ID()) + "/ws"
	conn, ctx, cancel := dialWS(t, url)
	defer cancel()
	defer conn.Close(websocket.StatusNormalClosure, "test done")

	// Snapshot first.
	_ = readEnv(t, conn, ctx)

	// Send input via the session; cat echoes it.
	if _, err := sess.WriteInput([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		env := readEnv(t, conn, ctx)
		if env.Delta != nil && len(env.Delta.D.Regions) > 0 {
			return
		}
	}
	t.Fatal("did not receive a Delta within deadline")
}

func TestViewerNotFound(t *testing.T) {
	_, hts := mountTestServer(t)
	url := "ws://" + hts.Listener.Addr().String() + "/v1/sessions/nonesuch/ws"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, url, nil)
	if err == nil {
		t.Fatalf("expected dial error for missing session, got nil")
	}
	if resp == nil {
		return
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// screenHas waits up to d for sess's screen to show want.
func screenHas(sess *session.Session, want string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		for _, row := range sess.Snapshot().Cells {
			var line string
			for _, c := range row {
				line += c.Grapheme
			}
			if strings.Contains(line, want) {
				return true
			}
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWritableViewer(t *testing.T) {
	srv, hts := mountTestServer(t)
	sess, err := srv.Manager.Create(session.Config{PTY: pty.Config{Command: "/bin/cat"}})
	if err != nil {
		t.Fatal(err)
	}
	base := "ws://" + hts.Listener.Addr().String() + "/v1/sessions/" + string(sess.ID()) + "/ws"

	// A view-only connection's input is ignored.
	ro, ctx, cancel := dialWS(t, base)
	defer cancel()
	readEnv(t, ro, ctx)
	ro.Write(ctx, websocket.MessageText, []byte(`{"input":"ignored"}`))
	ro.Close(websocket.StatusNormalClosure, "")

	rw, ctx, cancel := dialWS(t, base+"?write=1")
	defer cancel()
	defer rw.Close(websocket.StatusNormalClosure, "")
	readEnv(t, rw, ctx)
	if err := rw.Write(ctx, websocket.MessageText, []byte(`{"input":"typed"}`)); err != nil {
		t.Fatal(err)
	}
	if !screenHas(sess, "typed", 3*time.Second) {
		t.Fatal("input from a writable viewer did not reach the session")
	}
	// The ignored input was sent first, so it would be on screen by now.
	if screenHas(sess, "ignored", 0) {
		t.Error("input from a view-only viewer reached the session")
	}

	if err := rw.Write(ctx, websocket.MessageText, []byte(`{"resize":{"rows":30,"cols":100}}`)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if r, c := sess.Size(); r == 30 && c == 100 {
			break
		}
		if time.Now().After(deadline) {
			r, c := sess.Size()
			t.Fatalf("size = %dx%d after resize, want 100x30", c, r)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Absurd sizes are refused.
	rw.Write(ctx, websocket.MessageText, []byte(`{"resize":{"rows":0,"cols":100000}}`))
	time.Sleep(100 * time.Millisecond)
	if r, c := sess.Size(); r != 30 || c != 100 {
		t.Errorf("size = %dx%d after a bad resize", c, r)
	}
}

// readSizing reads frames until a Sizing frame, and returns it.
func readSizing(t *testing.T, conn *websocket.Conn, ctx context.Context) proto.Sizing {
	t.Helper()
	for {
		if env := readEnv(t, conn, ctx); env.Sizing != nil {
			return *env.Sizing
		}
	}
}

// waitSize waits for sess to be rows by cols.
func waitSize(t *testing.T, sess *session.Session, rows, cols int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		r, c := sess.Size()
		if r == rows && c == cols {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("size = %dx%d, want %dx%d", r, c, rows, cols)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestSizeLeader checks that of two viewers that want different sizes,
// only the last one used sets the size, and that each is told whether it
// does.
func TestSizeLeader(t *testing.T) {
	srv, hts := mountTestServer(t)
	sess, err := srv.Manager.Create(session.Config{PTY: pty.Config{Command: "/bin/cat"}})
	if err != nil {
		t.Fatal(err)
	}
	url := "ws://" + hts.Listener.Addr().String() + "/v1/sessions/" + string(sess.ID()) + "/ws?write=1"
	a, ctx, cancel := dialWS(t, url)
	defer cancel()
	defer a.Close(websocket.StatusNormalClosure, "")
	b, _, cancelB := dialWS(t, url)
	defer cancelB()
	defer b.Close(websocket.StatusNormalClosure, "")
	if s := readSizing(t, a, ctx); s.Leader {
		t.Errorf("a's first Sizing = %+v, before anyone wants a size", s)
	}
	readSizing(t, b, ctx)

	send := func(c *websocket.Conn, msg string) {
		t.Helper()
		if err := c.Write(ctx, websocket.MessageText, []byte(msg)); err != nil {
			t.Fatal(err)
		}
	}
	send(a, `{"resize":{"rows":30,"cols":100},"lead":true}`)
	waitSize(t, sess, 30, 100)
	if s := readSizing(t, a, ctx); !s.Leader {
		t.Errorf("a's Sizing after leading = %+v", s)
	}
	if s := readSizing(t, b, ctx); s.Leader || s.Other != "a browser" {
		t.Errorf("b's Sizing after a leads = %+v", s)
	}

	// b's size waits until b is used: here, typed into.
	send(b, `{"resize":{"rows":40,"cols":120}}`)
	time.Sleep(100 * time.Millisecond)
	waitSize(t, sess, 30, 100)
	send(b, `{"input":"x"}`)
	waitSize(t, sess, 40, 120)
	if s := readSizing(t, a, ctx); s.Leader {
		t.Errorf("a's Sizing after b typed = %+v", s)
	}
	if s := readSizing(t, b, ctx); !s.Leader {
		t.Errorf("b's Sizing after typing = %+v", s)
	}

	// And a's comes back when a is used again.
	send(a, `{"lead":true}`)
	waitSize(t, sess, 30, 100)
}

// TestBellAndNotificationAlone checks that a bell and a notification reach
// viewers even when nothing on the screen changes with them.
func TestBellAndNotificationAlone(t *testing.T) {
	srv, hts := mountTestServer(t)
	sess, err := srv.Manager.Create(session.Config{PTY: pty.Config{
		Command: "/bin/sh",
		Args:    []string{"-c", `sleep 0.5; printf '\a'; sleep 0.5; printf '\033]9;hi\007'; sleep 5`},
	}})
	if err != nil {
		t.Fatal(err)
	}
	conn, ctx, cancel := dialWS(t, "ws://"+hts.Listener.Addr().String()+"/v1/sessions/"+string(sess.ID())+"/ws")
	defer cancel()
	defer conn.Close(websocket.StatusNormalClosure, "")
	conn.SetReadLimit(1 << 20)
	readEnv(t, conn, ctx) // the snapshot
	var rang, notified bool
	for !rang || !notified {
		env := readEnv(t, conn, ctx)
		if d := env.Delta; d != nil {
			rang = rang || d.D.Rang
			notified = notified || (d.D.NotificationSet && d.D.Notification.Body == "hi")
		}
	}
}
