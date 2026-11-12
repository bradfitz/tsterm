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
//
// Deltas come at most every minFrameInterval, each describing all the
// changes since the last. A viewer that connects with ?acks=1 also
// acknowledges each Delta it has shown, with {"ack": Seq}, and the server
// keeps no more than maxInFlight unacknowledged, as Mosh does: a viewer
// that can't keep up gets fewer, fresher frames, rather than a backlog of
// stale ones, which would delay everything after them, such as the result
// of a Ctrl-C.
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
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/bradfitz/tsterm/pkg/auth"
	"github.com/bradfitz/tsterm/pkg/history"
	"github.com/bradfitz/tsterm/pkg/proto"
	"github.com/bradfitz/tsterm/pkg/session"
)

// Server wires the viewer handler to a session manager. Auth is enforced by
// the surrounding TCP package's middleware, which puts the peer in each
// request's context (auth.PeerFrom): the owner may watch, and drive, any
// session; a viewer may watch, read-only, the sessions the owner shares;
// anyone else, nothing.
type Server struct {
	Manager *session.Manager

	// Auth, if non-nil, is consulted again whenever the tailnet's
	// addresses change, and a viewer whose peer is no longer authorized is
	// disconnected. A viewer connection can last indefinitely, unlike the
	// requests the TCP middleware checks.
	Auth auth.Authorizer

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
	var frozen *session.Frozen
	if err != nil {
		if frozen = s.Manager.Frozen(id); frozen == nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
	}
	peer := auth.PeerFrom(r.Context())
	// visible reports whether the peer may still watch the session.
	visible := func() bool {
		switch peer.Access {
		case auth.Owner:
			return true
		case auth.Viewer:
			if frozen != nil {
				f := s.Manager.Frozen(string(frozen.ID))
				return f != nil && f.Shared
			}
			return sess.Shared()
		}
		return false
	}
	if !visible() {
		http.Error(w, "no such session", http.StatusNotFound)
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
	if s.Auth != nil {
		go s.recheck(ctx, cancel, r.RemoteAddr, peer.Access)
	}
	if frozen != nil {
		if err := s.serveFrozen(ctx, conn, frozen, visible); err != nil && !errors.Is(err, context.Canceled) {
			s.Logf("ws: frozen %s: %v", frozen.ID, err)
		}
		conn.Close(websocket.StatusNormalClosure, "")
		return
	}

	// Read inbound frames in the background: input from a writable
	// connection, and otherwise just so that a browser closing the
	// connection is noticed promptly.
	// Only the owner may drive a session, whatever the viewer asks.
	writable := peer.Access == auth.Owner && r.URL.Query().Get("write") == "1"
	fl := &flow{enabled: r.URL.Query().Get("acks") == "1", acked: make(chan struct{}, 1)}
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
			if err := s.handleMessage(ctx, conn, sess, sizer, fl, writable, b); err != nil {
				s.Logf("ws: message from %s: %v", r.RemoteAddr, err)
			}
		}
	}()

	if err := sess.MarkAttached(); err != nil {
		s.Logf("ws: recording attach: %v", err)
	}

	if err := s.pump(ctx, conn, sess, sizer, fl, writable, visible); err != nil && !errors.Is(err, context.Canceled) {
		s.Logf("ws: pump: %v", err)
	}
	conn.Close(websocket.StatusNormalClosure, "session ended")
}

// Flow control of Deltas; see the package comment.
const (
	minFrameInterval = 20 * time.Millisecond
	maxInFlight      = 2

	// maxAckWait is how long the server waits for an acknowledgment
	// before sending anyway, at that rate, as to a background tab that
	// doesn't draw.
	maxAckWait = time.Second
)

// flow is a viewer's acknowledgments of Deltas.
type flow struct {
	enabled bool
	ack     atomic.Uint64 // the latest Seq acknowledged
	acked   chan struct{} // ticks when ack changes
}

func (f *flow) gotAck(seq uint64) {
	for {
		old := f.ack.Load()
		if seq <= old || f.ack.CompareAndSwap(old, seq) {
			break
		}
	}
	select {
	case f.acked <- struct{}{}:
	default:
	}
}

// inputMsg is a frame from a viewer.
type inputMsg struct {
	Ack uint64 `json:"ack,omitzero"`

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

func (s *Server) handleMessage(ctx context.Context, conn *websocket.Conn, sess *session.Session, sizer *session.Sizer, fl *flow, writable bool, b []byte) error {
	var m inputMsg
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	if m.Ack != 0 {
		fl.gotAck(m.Ack)
	}
	if h := m.History; h != nil {
		if err := sendHistory(ctx, conn, sess.History(), h.From, h.Count); err != nil {
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
// recheck cancels the connection's ctx once remoteAddr no longer has the
// access it connected with.
func (s *Server) recheck(ctx context.Context, cancel context.CancelFunc, remoteAddr string, access auth.Access) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.Auth.Changes():
		}
		p, err := s.Auth.Authorize(ctx, remoteAddr)
		if err == nil && p.Access < access {
			err = errors.New("less access than before")
		}
		if err != nil {
			s.Logf("ws: disconnecting %s: %v", remoteAddr, err)
			cancel()
			return
		}
	}
}

// pump streams sess to conn while visible, which a viewer stops being
// when the owner stops sharing it.
func (s *Server) pump(ctx context.Context, conn *websocket.Conn, sess *session.Session, sizer *session.Sizer, fl *flow, writable bool, visible func() bool) error {
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
	var (
		seq      uint64    // of the last Delta sent
		lastSend time.Time // when it was sent
		dirty    bool      // the screen may have changed since
		timer    = time.NewTimer(0)
	)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-w.Done():
			return s.ended(ctx, conn, sess)
		case _, ok := <-w.Ticks():
			if !ok {
				return s.ended(ctx, conn, sess)
			}
			if !visible() {
				return writeEnv(ctx, conn, proto.Envelope{Ended: &proto.Ended{Status: "no longer shared"}})
			}
			if err := sendSizing(); err != nil {
				return err
			}
			dirty = true
		case <-fl.acked:
		case <-timer.C:
		}
		if !dirty {
			continue
		}
		// Wait for the frame interval, and for the viewer to catch up.
		wait := minFrameInterval - time.Since(lastSend)
		if fl.enabled && seq-fl.ack.Load() >= maxInFlight {
			wait = max(wait, maxAckWait-time.Since(lastSend))
		}
		if wait > 0 {
			timer.Reset(wait)
			continue
		}
		dirty = false
		d := w.Diff()
		if !d.Changed() {
			continue
		}
		seq++
		lastSend = time.Now()
		if err := writeEnv(ctx, conn, proto.Envelope{Delta: &proto.Delta{Seq: seq, D: d}}); err != nil {
			return err
		}
	}
}

// sendHistory sends up to count lines of history h, which may be nil,
// from line from.
func sendHistory(ctx context.Context, conn *websocket.Conn, h *history.Store, from uint64, count int) error {
	reply := &proto.History{From: from}
	if h != nil {
		first, lines, err := h.Read(from, min(max(count, 0), maxHistoryLines))
		if err != nil {
			return err
		}
		reply.First, reply.From, reply.Lines = h.First(), first, lines
	}
	return writeEnv(ctx, conn, proto.Envelope{History: reply})
}

// ended tells the viewer that sess has ended, with its final screen, and
// keeps the connection open, for its history, until the viewer closes it.
func (s *Server) ended(ctx context.Context, conn *websocket.Conn, sess *session.Session) error {
	<-sess.Done()
	// The final screen, which the last delta may not have caught up to.
	if err := writeEnv(ctx, conn, proto.Envelope{Snapshot: &proto.Snapshot{S: sess.Snapshot()}}); err != nil {
		return err
	}
	if err := writeEnv(ctx, conn, proto.Envelope{Ended: &proto.Ended{Status: sess.EndStatus()}}); err != nil {
		return err
	}
	<-ctx.Done()
	return nil
}

// serveFrozen shows the viewer a frozen session: its last screen, then
// Ended, and then its history, as the viewer asks for it, while visible.
func (s *Server) serveFrozen(ctx context.Context, conn *websocket.Conn, f *session.Frozen, visible func() bool) error {
	ss, err := f.Screen()
	if err != nil {
		s.Logf("ws: frozen %s has no screen: %v", f.ID, err)
	}
	h, err := f.OpenHistory()
	if err != nil {
		s.Logf("ws: frozen %s's history: %v", f.ID, err)
		h = nil
	} else {
		defer h.Close()
	}
	if err := writeEnv(ctx, conn, proto.Envelope{Snapshot: &proto.Snapshot{S: ss.Screen}}); err != nil {
		return err
	}
	if err := writeEnv(ctx, conn, proto.Envelope{Ended: &proto.Ended{Status: f.Status}}); err != nil {
		return err
	}
	for {
		typ, b, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		var m inputMsg
		if typ != websocket.MessageText || json.Unmarshal(b, &m) != nil || m.History == nil {
			continue
		}
		if !visible() {
			return nil
		}
		if err := sendHistory(ctx, conn, h, m.History.From, m.History.Count); err != nil {
			return err
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
