// Package ws implements the WebSocket viewer for tsterm sessions, which the
// web UI uses.
//
// On connect, the server sends a CBOR-encoded Snapshot followed by Delta
// frames for the duration of the session. WebSocket runs over TCP, which
// delivers every frame in order, so no keyframes are needed.
//
// A connection opened with ?write=1 may also drive the session, by sending
// JSON text frames: {"input": "..."} to type, {"resize": {"rows": R,
// "cols": C}} to say what size it wants the terminal (0 by 0 for no
// preference), and {"lead": true} to make that size apply, as when the
// user focuses its window; typing does too. Only the client the user used
// last sets the size (see session.Sizer), and the server sends a Sizing
// frame to say whether that's this one. Other connections are view-only,
// but any may send {"history": {"from": N, "count": K}} to get up to K
// lines of history from line N in a History frame,
// and their inbound frames are ignored. (Today only the session's owner can
// connect at all; view-only connections are what links shared with others
// will get.)
package ws

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
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

	// Read inbound frames in the background: input from a writable
	// connection, and otherwise just so that a browser closing the
	// connection is noticed promptly.
	writable := r.URL.Query().Get("write") == "1"
	sizer := sess.NewSizer("a browser")
	defer sizer.Close()
	go func() {
		defer cancel()
		for {
			typ, b, err := conn.Read(ctx)
			if err != nil {
				return
			}
			if typ != websocket.MessageText {
				continue
			}
			if err := s.handleMessage(ctx, conn, sess, sizer, writable, b); err != nil {
				s.Logf("ws: message from %s: %v", r.RemoteAddr, err)
			}
		}
	}()

	if s.Auth != nil {
		go s.recheck(ctx, cancel, r.RemoteAddr)
	}
	if err := sess.MarkAttached(); err != nil {
		s.Logf("ws: recording attach: %v", err)
	}

	if err := s.pump(ctx, conn, sess, sizer, writable); err != nil && !errors.Is(err, context.Canceled) {
		s.Logf("ws: pump: %v", err)
	}
	conn.Close(websocket.StatusNormalClosure, "session ended")
}

// inputMsg is a frame from a viewer.
type inputMsg struct {
	History *struct {
		From  uint64 `json:"from"`
		Count int    `json:"count"`
	} `json:"history,omitzero"`

	Input  string `json:"input,omitzero"`
	Resize *struct {
		Rows int `json:"rows"`
		Cols int `json:"cols"`
	} `json:"resize,omitzero"`
	Lead bool `json:"lead,omitzero"`
}

// maxDim bounds a requested terminal dimension.
const maxDim = 1000

// maxHistoryLines bounds the lines of history sent in reply to a request.
const maxHistoryLines = 2000

func (s *Server) handleMessage(ctx context.Context, conn *websocket.Conn, sess *session.Session, sizer *session.Sizer, writable bool, b []byte) error {
	var m inputMsg
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	if h := m.History; h != nil {
		if err := sendHistory(ctx, conn, sess, h.From, h.Count); err != nil {
			return err
		}
	}
	if !writable {
		return nil
	}
	// Record the wanted size before taking the lead, which applies it.
	if rs := m.Resize; rs != nil {
		noPref := rs.Rows == 0 && rs.Cols == 0
		if !noPref && (rs.Rows < 1 || rs.Cols < 1 || rs.Rows > maxDim || rs.Cols > maxDim) {
			return fmt.Errorf("bad size %dx%d", rs.Cols, rs.Rows)
		}
		if err := sizer.Want(rs.Rows, rs.Cols); err != nil {
			return err
		}
	}
	if m.Lead || m.Input != "" {
		// Whoever types is who the user is using.
		if err := sizer.Lead(); err != nil {
			return err
		}
	}
	if m.Input != "" {
		if _, err := sess.WriteInput([]byte(m.Input)); err != nil {
			return err
		}
	}
	return nil
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

func (s *Server) pump(ctx context.Context, conn *websocket.Conn, sess *session.Session, sizer *session.Sizer, writable bool) error {
	w := sess.Watch(ctx)
	defer func() { <-w.Done() }()

	if err := writeEnv(ctx, conn, proto.Envelope{Snapshot: &proto.Snapshot{S: sess.Snapshot()}}); err != nil {
		return err
	}
	// sendSizing tells a writable viewer about changes in who sets the
	// size. The watcher ticks when the leader changes.
	var sizing *proto.Sizing
	sendSizing := func() error {
		if !writable {
			return nil
		}
		leads, other := sizer.Leader()
		if sizing != nil && sizing.Leader == leads && sizing.Other == other {
			return nil
		}
		sizing = &proto.Sizing{Leader: leads, Other: other}
		return writeEnv(ctx, conn, proto.Envelope{Sizing: sizing})
	}
	if err := sendSizing(); err != nil {
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
			if err := sendSizing(); err != nil {
				return err
			}
			d := w.Diff()
			if d.Changed() {
				if err := writeEnv(ctx, conn, proto.Envelope{Delta: &proto.Delta{D: d}}); err != nil {
					return err
				}
			}
		}
	}
}

// sendHistory sends up to count lines of sess's history from line from.
func sendHistory(ctx context.Context, conn *websocket.Conn, sess *session.Session, from uint64, count int) error {
	reply := &proto.History{From: from}
	if h := sess.History(); h != nil {
		first, lines, err := h.Read(from, min(max(count, 0), maxHistoryLines))
		if err != nil {
			return err
		}
		reply.First, reply.From, reply.Lines = h.First(), first, lines
	}
	return writeEnv(ctx, conn, proto.Envelope{History: reply})
}

func writeEnv(ctx context.Context, conn *websocket.Conn, env proto.Envelope) error {
	b, err := proto.Encode(env)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageBinary, b)
}
