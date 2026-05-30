//go:build unix

package ws

import (
	"context"
	"net/http"
	"net/http/httptest"
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
