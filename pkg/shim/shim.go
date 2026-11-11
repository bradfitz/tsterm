// Package shim implements tsterm-shim, the small per-session process that
// owns a PTY and its child, and the tstermd side of talking to it. See
// package shimproto for the protocol and the reasons for the split.
package shim

import (
	"bufio"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/bradfitz/tsterm/pkg/pty"
	"github.com/bradfitz/tsterm/pkg/shimproto"
)

// Options tunes a shim. The zero value is the production configuration.
type Options struct {
	// BufferLimit is how much output, counted from the latest checkpoint,
	// the shim retains before it stops reading from the PTY. Defaults to
	// 16 MiB.
	BufferLimit int

	// Linger is how long a shim whose child has exited waits for a
	// controller to collect the exit before giving up and exiting anyway.
	// Defaults to 24 hours.
	Linger time.Duration

	// Logf, if non-nil, receives diagnostic logs.
	Logf func(format string, args ...any)
}

// drainTimeout is how long after the child exits the shim waits for the
// PTY to reach EOF. Grandchildren that still hold the terminal open can
// delay EOF indefinitely, so the shim reports the exit after this long
// regardless.
const drainTimeout = 500 * time.Millisecond

// outputChunk bounds the size of an Output frame.
const outputChunk = 64 << 10

// Run runs a shim: it listens on cfg.Socket, starts cfg's command under a
// new PTY, and serves controllers until the child has exited and a
// controller has released it, or until ctx is done, in which case the child
// is killed.
func Run(ctx context.Context, cfg shimproto.SpawnConfig, opts Options) error {
	if opts.BufferLimit <= 0 {
		opts.BufferLimit = 16 << 20
	}
	if opts.Linger <= 0 {
		opts.Linger = 24 * time.Hour
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	rows, cols := cfg.Rows, cfg.Cols
	if rows <= 0 {
		rows = 24
	}
	if cols <= 0 {
		cols = 80
	}

	// IDs are unique, so an existing socket is stale.
	_ = os.Remove(cfg.Socket)
	ln, err := net.Listen("unix", cfg.Socket)
	if err != nil {
		return err
	}
	defer os.Remove(cfg.Socket)
	defer ln.Close()

	p, err := pty.Start(pty.Config{
		Command: cfg.Command,
		Args:    cfg.Args,
		Env:     cfg.Env,
		Dir:     cfg.Dir,
		Rows:    rows,
		Cols:    cols,
		NoLogin: cfg.NoLogin,
	})
	if err != nil {
		return err
	}

	s := &server{
		opts:     opts,
		id:       cfg.ID,
		pty:      p,
		started:  time.Now(),
		rows:     rows,
		cols:     cols,
		meta:     cfg.Meta,
		readDone: make(chan struct{}),
		waitDone: make(chan struct{}),
	}
	s.cond = sync.NewCond(&s.mu)
	go s.readPTY()
	go s.waitChild()
	go s.accept(ln)

	s.waitForExit(ctx)
	if ctx.Err() != nil {
		_ = p.Signal(syscall.SIGKILL)
	}
	select {
	case <-s.waitDone:
	case <-time.After(5 * time.Second):
	}
	_ = p.Close()
	s.mu.Lock()
	if s.ctrl != nil {
		s.ctrl.c.Close()
	}
	s.mu.Unlock()
	return nil
}

type server struct {
	opts     Options
	id       string
	pty      *pty.PTY
	started  time.Time
	readDone chan struct{} // closed when the PTY reaches EOF
	waitDone chan struct{} // closed once the child is reaped

	mu   sync.Mutex
	cond *sync.Cond // broadcast on any change to the fields below

	// ckpt is the latest checkpoint; buf holds the output that follows it,
	// so the output stream so far ends at offset ckpt.Offset+len(buf).
	ckpt shimproto.Checkpoint
	buf  []byte

	rows, cols int
	meta       []byte
	lastInput  time.Time
	lastOutput time.Time
	exited     bool
	exitStatus string
	ctrl       *conn // current controller, if any
	released   bool  // a controller has released the exited child
	lingerUp   bool  // Linger has passed with the child exited and no controller
	lingerT    *time.Timer
	done       bool // Run's ctx is done
}

func (s *server) end() uint64 { return s.ckpt.Offset + uint64(len(s.buf)) }

// readPTY copies PTY output into buf, pausing while buf is at its limit.
func (s *server) readPTY() {
	defer close(s.readDone)
	b := make([]byte, 32<<10)
	for {
		s.mu.Lock()
		for len(s.buf) >= s.opts.BufferLimit && !s.exited && !s.done {
			s.cond.Wait()
		}
		s.mu.Unlock()
		n, err := s.pty.Read(b)
		if n > 0 {
			s.mu.Lock()
			if !s.exited {
				s.buf = append(s.buf, b[:n]...)
				s.lastOutput = time.Now()
				s.cond.Broadcast()
			}
			s.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

// waitChild reaps the child and records its exit once its output has
// drained.
func (s *server) waitChild() {
	defer close(s.waitDone)
	err := s.pty.Wait()
	select {
	case <-s.readDone:
	case <-time.After(drainTimeout):
	}
	status := "exit status 0"
	if err != nil {
		status = err.Error()
	}
	s.mu.Lock()
	s.exited = true
	s.exitStatus = status
	s.armLingerLocked()
	s.cond.Broadcast()
	s.mu.Unlock()
	s.opts.Logf("shim %s: child %s", s.id, status)
}

// armLingerLocked starts the Linger timer if the child has exited and no
// controller is attached.
func (s *server) armLingerLocked() {
	if !s.exited || s.ctrl != nil || s.lingerT != nil {
		return
	}
	s.lingerT = time.AfterFunc(s.opts.Linger, func() {
		s.mu.Lock()
		s.lingerUp = true
		s.cond.Broadcast()
		s.mu.Unlock()
	})
}

// waitForExit blocks until the shim should exit.
func (s *server) waitForExit(ctx context.Context) {
	stop := context.AfterFunc(ctx, func() {
		s.mu.Lock()
		s.done = true
		s.cond.Broadcast()
		s.mu.Unlock()
	})
	defer stop()
	s.mu.Lock()
	defer s.mu.Unlock()
	for !s.done && !(s.exited && (s.released || s.lingerUp)) {
		s.cond.Wait()
	}
}

func (s *server) accept(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		if err := checkPeer(c); err != nil {
			s.opts.Logf("shim %s: rejecting connection: %v", s.id, err)
			c.Close()
			continue
		}
		go s.serve(&conn{c: c})
	}
}

// conn is one connection to the shim.
type conn struct {
	c    net.Conn
	sent uint64 // output offset sent so far, once attached
}

func (s *server) info() shimproto.Info {
	s.mu.Lock()
	defer s.mu.Unlock()
	return shimproto.Info{
		Version:    shimproto.Version,
		ID:         s.id,
		Pid:        s.pty.Pid(),
		Rows:       s.rows,
		Cols:       s.cols,
		Started:    s.started,
		Exited:     s.exited,
		ExitStatus: s.exitStatus,
		LastInput:  s.lastInput,
		LastOutput: s.lastOutput,
		OutputEnd:  s.end(),
		Meta:       s.meta,
	}
}

func (s *server) serve(c *conn) {
	defer c.c.Close()
	defer func() {
		s.mu.Lock()
		if s.ctrl == c {
			s.ctrl = nil
			s.armLingerLocked()
			s.cond.Broadcast()
		}
		s.mu.Unlock()
	}()
	if err := shimproto.WriteJSON(c.c, shimproto.TypeInfo, s.info()); err != nil {
		return
	}
	r := bufio.NewReader(c.c)
	for {
		typ, payload, err := shimproto.ReadFrame(r)
		if err != nil {
			return
		}
		if typ == shimproto.TypeAttach {
			s.attach(c)
			continue
		}
		s.mu.Lock()
		isCtrl := s.ctrl == c
		s.mu.Unlock()
		if !isCtrl {
			continue // only the controller may drive the session
		}
		if err := s.handle(typ, payload); err != nil {
			s.opts.Logf("shim %s: frame type %d: %v", s.id, typ, err)
		}
	}
}

// attach makes c the controller and starts streaming to it.
func (s *server) attach(c *conn) {
	s.mu.Lock()
	if old := s.ctrl; old != nil && old != c {
		old.c.Close()
	}
	s.ctrl = c
	if s.lingerT != nil {
		s.lingerT.Stop()
		s.lingerT = nil
		s.lingerUp = false
	}
	c.sent = s.ckpt.Offset
	ckpt := s.ckpt
	s.cond.Broadcast()
	s.mu.Unlock()

	if err := shimproto.WriteJSON(c.c, shimproto.TypeCheckpoint, ckpt); err != nil {
		c.c.Close()
		return
	}
	go s.stream(c)
}

// stream sends output, then the exit, to the controller c.
func (s *server) stream(c *conn) {
	exitSent := false
	for {
		s.mu.Lock()
		for s.ctrl == c && c.sent >= s.end() && (exitSent || !s.exited) {
			s.cond.Wait()
		}
		if s.ctrl != c {
			s.mu.Unlock()
			return
		}
		c.sent = max(c.sent, s.ckpt.Offset)
		var typ shimproto.Type
		var payload []byte
		if c.sent < s.end() {
			start := c.sent - s.ckpt.Offset
			n := min(uint64(len(s.buf))-start, outputChunk)
			typ, payload = shimproto.TypeOutput, append([]byte(nil), s.buf[start:start+n]...)
			c.sent += n
		} else {
			typ, exitSent = shimproto.TypeExited, true
			payload, _ = json.Marshal(shimproto.Exited{Status: s.exitStatus})
		}
		s.mu.Unlock()
		if err := shimproto.WriteFrame(c.c, typ, payload); err != nil {
			c.c.Close()
			return
		}
	}
}

func (s *server) handle(typ shimproto.Type, payload []byte) error {
	switch typ {
	case shimproto.TypeInput:
		s.mu.Lock()
		s.lastInput = time.Now()
		s.mu.Unlock()
		_, err := s.pty.Write(payload)
		return err
	case shimproto.TypeResize:
		var m shimproto.Resize
		if err := shimproto.Decode(payload, &m); err != nil {
			return err
		}
		if m.Rows <= 0 || m.Cols <= 0 {
			return errors.New("bad size")
		}
		if err := s.pty.Resize(m.Rows, m.Cols); err != nil {
			return err
		}
		s.mu.Lock()
		s.rows, s.cols = m.Rows, m.Cols
		s.mu.Unlock()
	case shimproto.TypeSignal:
		var m shimproto.Signal
		if err := shimproto.Decode(payload, &m); err != nil {
			return err
		}
		return s.pty.Signal(syscall.Signal(m.Signal))
	case shimproto.TypeSetCheckpoint:
		var m shimproto.Checkpoint
		if err := shimproto.Decode(payload, &m); err != nil {
			return err
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if m.Offset < s.ckpt.Offset || m.Offset > s.end() {
			return fmt.Errorf("checkpoint offset %d outside [%d, %d]", m.Offset, s.ckpt.Offset, s.end())
		}
		s.buf = append([]byte(nil), s.buf[m.Offset-s.ckpt.Offset:]...)
		s.ckpt = m
		s.cond.Broadcast() // the reader may have room now
	case shimproto.TypeSetMeta:
		var m shimproto.Meta
		if err := shimproto.Decode(payload, &m); err != nil {
			return err
		}
		s.mu.Lock()
		s.meta = m.Meta
		s.mu.Unlock()
	case shimproto.TypeRelease:
		s.mu.Lock()
		s.released = true
		s.cond.Broadcast()
		s.mu.Unlock()
	}
	// Unknown types are ignored, for forward compatibility.
	return nil
}
