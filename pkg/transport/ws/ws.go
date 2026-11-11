// Package ws implements the read-only WebSocket viewer for tsterm sessions.
//
// On connect, the server sends a CBOR-encoded Snapshot followed by Delta
// frames for the duration of the session. WebSocket runs over TCP, which
// delivers every frame in order, so no keyframes are needed. Inbound frames from the browser are accepted but ignored: the
// first iteration is a viewer only, not an interactive client.
package ws

import (
	"context"
	"errors"
	"net/http"

	"github.com/coder/websocket"

	"github.com/bradfitz/tsterm/pkg/auth"
	"github.com/bradfitz/tsterm/pkg/proto"
	"github.com/bradfitz/tsterm/pkg/session"
)

// Server wires the viewer handler to a session manager. Auth is enforced by
// the surrounding TCP package's middleware; this handler runs only after a
// caller has been authorized.
type Server struct {
	Manager *session.Manager

	// Auth, if non-nil, is consulted again whenever the tailnet's
	// addresses change, and a viewer whose peer is no longer authorized is
	// disconnected. A viewer connection can last indefinitely, unlike the
	// requests the TCP middleware checks.
	Auth *auth.SelfUser

	// Logf, if non-nil, logs per-connection errors. Defaults to no-op.
	Logf func(format string, args ...any)
}

// Handler returns an http.Handler for /v1/sessions/{id}/ws-style routes.
// The session is resolved from r.PathValue("id"), which is populated by the
// TCP package's mux.
func (s *Server) Handler() http.Handler {
	if s.Logf == nil {
		s.Logf = func(string, ...any) {}
	}
	return http.HandlerFunc(s.serve)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, err := s.Manager.Resolve(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Allow same-origin and any tailnet origin; the auth middleware
		// already proved identity, so origin pinning would be redundant
		// for our use case.
		InsecureSkipVerify: true,
	})
	if err != nil {
		s.Logf("ws: accept: %v", err)
		return
	}
	defer conn.Close(websocket.StatusInternalError, "shutting down")

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// Drain inbound frames in the background so a browser that closes the
	// connection (or sends pings) is observed promptly.
	go func() {
		defer cancel()
		for {
			_, _, err := conn.Read(ctx)
			if err != nil {
				return
			}
		}
	}()

	if s.Auth != nil {
		go s.recheck(ctx, cancel, r.RemoteAddr)
	}
	if err := sess.MarkAttached(); err != nil {
		s.Logf("ws: recording attach: %v", err)
	}

	if err := s.pump(ctx, conn, sess); err != nil && !errors.Is(err, context.Canceled) {
		s.Logf("ws: pump: %v", err)
	}
	conn.Close(websocket.StatusNormalClosure, "session ended")
}

// recheck cancels the connection's ctx once remoteAddr is no longer
// authorized.
func (s *Server) recheck(ctx context.Context, cancel context.CancelFunc, remoteAddr string) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.Auth.Changes():
		}
		if err := s.Auth.Allow(ctx, remoteAddr); err != nil {
			s.Logf("ws: disconnecting %s: %v", remoteAddr, err)
			cancel()
			return
		}
	}
}

func (s *Server) pump(ctx context.Context, conn *websocket.Conn, sess *session.Session) error {
	w := sess.Watch(ctx)
	defer func() { <-w.Done() }()

	if err := writeEnv(ctx, conn, proto.Envelope{Snapshot: &proto.Snapshot{S: sess.Snapshot()}}); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-w.Done():
			return writeEnv(ctx, conn, proto.Envelope{Ended: &proto.Ended{Status: "closed"}})
		case _, ok := <-w.Ticks():
			if !ok {
				return writeEnv(ctx, conn, proto.Envelope{Ended: &proto.Ended{Status: "closed"}})
			}
			d := w.Diff()
			if d.Resized || len(d.Regions) > 0 || d.CursorMoved || d.TitleSet || d.ModesChanged {
				if err := writeEnv(ctx, conn, proto.Envelope{Delta: &proto.Delta{D: d}}); err != nil {
					return err
				}
			}
		}
	}
}

func writeEnv(ctx context.Context, conn *websocket.Conn, env proto.Envelope) error {
	b, err := proto.Encode(env)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageBinary, b)
}
