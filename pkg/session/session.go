// Package session is the substrate that joins a PTY-backed child process to
// a canonical vt.Terminal and fans the resulting state out to multiple
// concurrent watchers.
//
// The PTY and child live in a separate tsterm-shim process (see package
// shim), so that tstermd can restart without killing them. A Session owns:
//
//   - a connection to its shim, the shim's controller;
//   - a vt.Terminal that holds the canonical screen state;
//   - a background pump goroutine that copies output from the shim into the
//     terminal, notifies watchers each time the screen may have changed, and
//     periodically checkpoints the terminal state to the shim;
//   - a set of Watchers that each maintain their own last-seen Screen and
//     compute their own diffs against the canonical state.
//
// Per-watcher diff state is intentional. A slow watcher does not block the
// pump or other watchers; on its next tick it computes a delta from its own
// stale baseline to the latest canonical Screen and catches up in one step.
//
// Input sequencing and ack semantics from the design document live at the
// transport layer; this package only exposes direct WriteInput, Resize, and
// Signal calls.
package session

import (
	"context"
	"crypto/rand"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/bradfitz/tsterm/pkg/pty"
	"github.com/bradfitz/tsterm/pkg/shim"
	"github.com/bradfitz/tsterm/pkg/vt"
)

// ID is a session identifier: "pty-" followed by a random string. IDs are
// what users see; unlike PIDs, they are never reused.
type ID string

// newID returns a new random session ID.
func newID() ID { return ID("pty-" + rand.Text()) }

// Config describes a new session.
type Config struct {
	// Name is the session's human-readable name. Optional.
	Name string

	// PTY is the PTY configuration. Rows and Cols here are authoritative;
	// PTY.Rows and PTY.Cols are ignored.
	PTY pty.Config

	// Rows and Cols are the initial PTY and terminal grid size. Defaults
	// of 24 and 80 apply when either is zero.
	Rows, Cols int
}

// meta is the per-session state that tstermd stores in the shim, so that a
// restarted tstermd can recover it.
type meta struct {
	Name    string    `json:"name,omitzero"`
	Token   []byte    `json:"token"`
	Created time.Time `json:"created"`

	// Clients are the hot-path clients attached to the session, so that
	// a restarted tstermd can bring them up to date unprompted.
	Clients []Client `json:"clients,omitzero"`
}

// Client identifies a hot-path client attached to a session.
type Client struct {
	Addr  string `json:"addr"`  // UDP address, as from netip.AddrPort.String
	Nonce []byte `json:"nonce"` // the client's Hello nonce
}

// Checkpoint policy. The shim keeps all output since the last checkpoint,
// so checkpoints bound both its memory and the replay a restarted tstermd
// must do. A checkpoint can only be taken at a boundary between complete
// characters and escape sequences; output that never reaches one (which
// would take a pathological program) eventually fills the shim's buffer
// and pauses the child.
const (
	checkpointBytes    = 256 << 10
	checkpointInterval = 5 * time.Second
)

// Session is a running PTY child whose output is interpreted by a canonical
// vt.Terminal. Sessions are created and restored by a Manager.
type Session struct {
	id      ID
	name    string
	token   []byte
	created time.Time
	conn    *shim.Conn

	metaMu sync.Mutex
	meta   meta // as stored in the shim
	term   vt.Terminal

	done chan struct{}

	mu       sync.Mutex
	watchers map[uint64]*Watcher
	nextWID  uint64

	// waitErr is set when the pump goroutine returns. It captures the exit
	// status of the child process. closed is true once the done channel
	// has been closed exactly once.
	waitErr error
	closed  atomic.Bool

	// detached is set when tstermd lets go of the session without ending
	// it, so that the pump does not report the closed connection as the
	// session ending.
	detached atomic.Bool

	// Checkpoint state, written only by the pump. The offsets are atomic
	// so that others may read them.
	offset     atomic.Uint64 // output offset the terminal has consumed through
	ckptOffset atomic.Uint64
	ckptTime   time.Time
	scan       vt.BoundaryScanner
}

// attach makes conn the shim's controller and starts a Session for it,
// restoring the terminal state from the shim's latest checkpoint.
func attach(conn *shim.Conn, id ID, m meta) (*Session, error) {
	ckpt, err := conn.Attach()
	if err != nil {
		return nil, err
	}
	term := vt.New(max(conn.Info.Rows, 1), max(conn.Info.Cols, 1))
	if _, err := term.Write(ckpt.Blob); err != nil {
		return nil, err
	}
	s := &Session{
		id:       id,
		name:     m.Name,
		token:    m.Token,
		created:  m.Created,
		conn:     conn,
		meta:     m,
		term:     term,
		done:     make(chan struct{}),
		watchers: map[uint64]*Watcher{},
		ckptTime: time.Now(),
	}
	s.offset.Store(ckpt.Offset)
	s.ckptOffset.Store(ckpt.Offset)
	go s.pump()
	return s, nil
}

func encodeMeta(m meta) []byte {
	b, err := json.Marshal(m)
	if err != nil {
		panic(err) // not possible for this type
	}
	return b
}

func decodeMeta(b []byte) (meta, error) {
	var m meta
	if err := json.Unmarshal(b, &m); err != nil {
		return m, err
	}
	if len(m.Token) == 0 {
		return m, errors.New("no token")
	}
	return m, nil
}

// ID returns the session's identifier.
func (s *Session) ID() ID { return s.id }

// Name returns the session's human-readable name, or the empty string if it
// has none.
func (s *Session) Name() string { return s.name }

// Token returns the per-session opaque capability. Callers must treat it as
// a secret; revealing it allows another caller on the same authorized node
// to take over the UDP attachment.
func (s *Session) Token() []byte { return s.token }

// Created returns the time the session started.
func (s *Session) Created() time.Time { return s.created }

// Clients returns the hot-path clients recorded with SetClients, by this
// tstermd or a previous one.
func (s *Session) Clients() []Client {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	return append([]Client(nil), s.meta.Clients...)
}

// UpdateClients replaces the hot-path clients recorded for the session with
// update's result, and stores them in the shim, where they survive a
// tstermd restart. The update is atomic with respect to other calls.
func (s *Session) UpdateClients(update func([]Client) []Client) error {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	s.meta.Clients = update(append([]Client(nil), s.meta.Clients...))
	return s.conn.SetMeta(encodeMeta(s.meta))
}

// Pid returns the child's process ID.
func (s *Session) Pid() int { return s.conn.Info.Pid }

// WatchersCount returns the number of currently attached watchers.
func (s *Session) WatchersCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.watchers)
}

// Active reports whether the underlying child process is still running.
func (s *Session) Active() bool { return !s.closed.Load() }

// Snapshot returns the current canonical Screen.
func (s *Session) Snapshot() vt.Screen { return s.term.Snapshot() }

// Modes returns the current terminal modes.
func (s *Session) Modes() vt.Modes { return s.term.Modes() }

// Size returns the canonical terminal size.
func (s *Session) Size() (rows, cols int) { return s.term.Size() }

// WriteInput sends client keystrokes (or any other input bytes) to the
// child via the PTY.
func (s *Session) WriteInput(b []byte) (int, error) {
	if err := s.conn.Input(b); err != nil {
		return 0, err
	}
	return len(b), nil
}

// Resize sets the PTY window size and the canonical terminal grid size.
// The child receives SIGWINCH.
func (s *Session) Resize(rows, cols int) error {
	if err := s.conn.Resize(rows, cols); err != nil {
		return err
	}
	s.term.Resize(rows, cols)
	s.notifyWatchers()
	return nil
}

// Signal delivers sig to the child process.
func (s *Session) Signal(sig os.Signal) error {
	ss, ok := sig.(syscall.Signal)
	if !ok {
		return fmt.Errorf("session: unsupported signal %v", sig)
	}
	return s.conn.Signal(ss)
}

// Done returns a channel closed when the child has exited and its output
// has been drained into the canonical terminal, or when the connection to
// the shim is lost.
func (s *Session) Done() <-chan struct{} { return s.done }

// Wait blocks until the child has exited and returns its exit error.
func (s *Session) Wait() error {
	<-s.done
	return s.waitErr
}

// Close terminates the session: it kills the child with SIGKILL and waits
// for the session to end, after which the shim exits. Callers that want
// graceful shutdown should Signal(syscall.SIGTERM) and Wait first.
func (s *Session) Close() error {
	select {
	case <-s.done:
	default:
		_ = s.conn.Signal(syscall.SIGKILL)
		select {
		case <-s.done:
		case <-time.After(5 * time.Second):
		}
	}
	_ = s.conn.Release()
	return s.conn.Close()
}

// Detach disconnects from the shim without ending the session, which keeps
// running for a future tstermd to restore. The Session is unusable after.
func (s *Session) Detach() {
	s.detached.Store(true)
	s.conn.Close()
}

// pump runs in a dedicated goroutine. It copies output from the shim into
// the canonical terminal, notifies watchers after each batch, checkpoints,
// and closes Done when the child exits.
func (s *Session) pump() {
	for {
		out, exited, err := s.conn.Read()
		if err != nil {
			if s.detached.Load() {
				return
			}
			s.shutdown(fmt.Errorf("session: lost connection to shim: %w", err))
			return
		}
		if exited != nil {
			// The shim may exit now; it has nothing more for us.
			_ = s.conn.Release()
			var err error
			if exited.Status != "exit status 0" {
				err = errors.New(exited.Status)
			}
			s.shutdown(err)
			return
		}
		_, _ = s.term.Write(out)
		s.notifyWatchers()
		s.offset.Add(uint64(len(out)))
		s.scan.Write(out)
		s.maybeCheckpoint()
	}
}

// maybeCheckpoint sends the shim a checkpoint if one is due and possible.
func (s *Session) maybeCheckpoint() {
	offset := s.offset.Load()
	pending := offset - s.ckptOffset.Load()
	if pending == 0 || !s.scan.AtBoundary() {
		return
	}
	if pending < checkpointBytes && time.Since(s.ckptTime) < checkpointInterval {
		return
	}
	// Only the pump writes to the terminal, so the snapshot corresponds
	// exactly to offset.
	if err := s.conn.SetCheckpoint(offset, s.term.Snapshot().Repaint()); err != nil {
		return // the next Read will report the broken connection
	}
	s.ckptOffset.Store(offset)
	s.ckptTime = time.Now()
}

// shutdown is called by the pump on exit. It records the exit error, then
// closes done and all watcher channels.
func (s *Session) shutdown(waitErr error) {
	if !s.closed.CompareAndSwap(false, true) {
		return
	}
	s.waitErr = waitErr
	close(s.done)

	s.mu.Lock()
	for _, w := range s.watchers {
		w.closeOnce()
	}
	s.watchers = nil
	s.mu.Unlock()
}

// notifyWatchers sends a non-blocking tick to each watcher. If a watcher
// already has a pending tick the new one is dropped: a single pending tick
// is enough to tell the watcher "the canonical screen may have changed,"
// because the watcher computes its diff against its own baseline.
func (s *Session) notifyWatchers() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, w := range s.watchers {
		select {
		case w.ticks <- struct{}{}:
		default:
		}
	}
}

// Watch returns a Watcher that delivers tick notifications until ctx is
// canceled or the session ends.
func (s *Session) Watch(ctx context.Context) *Watcher {
	s.mu.Lock()
	id := s.nextWID
	s.nextWID++
	w := &Watcher{
		id:      id,
		session: s,
		ticks:   make(chan struct{}, 1),
		closed:  make(chan struct{}),
	}
	// Deliver an initial tick so the watcher's first Diff returns the
	// bootstrap delta without the caller having to poll.
	select {
	case w.ticks <- struct{}{}:
	default:
	}
	if s.closed.Load() {
		// Session already shut down: the pump will not notify anyone.
		// Close the watcher's ticks channel so the watcher's range loop
		// exits cleanly after the bootstrap tick is consumed.
		s.mu.Unlock()
		w.closeOnce()
		return w
	}
	s.watchers[id] = w
	s.mu.Unlock()

	go func() {
		<-ctx.Done()
		s.removeWatcher(w)
	}()
	return w
}

func (s *Session) removeWatcher(w *Watcher) {
	s.mu.Lock()
	if s.watchers != nil {
		delete(s.watchers, w.id)
	}
	s.mu.Unlock()
	w.closeOnce()
}

// Watcher is a per-subscriber view onto a session. Each Watcher maintains
// its own last-seen Screen so that its Diff calls return changes relative
// to what it last reported, independent of other watchers.
//
// The zero Watcher is unusable; obtain one from Session.Watch.
type Watcher struct {
	id      uint64
	session *Session
	ticks   chan struct{}
	last    vt.Screen

	closeMu   sync.Mutex
	closed    chan struct{}
	closedSet bool
}

// Ticks returns a channel that receives a value each time the canonical
// screen may have changed. The channel is closed when the session ends or
// when the watcher's context is canceled. Multiple unread ticks coalesce
// to one.
func (w *Watcher) Ticks() <-chan struct{} { return w.ticks }

// Done returns a channel that is closed when this watcher is no longer
// receiving ticks (either the session ended or the watcher was canceled).
func (w *Watcher) Done() <-chan struct{} { return w.closed }

// Diff returns a ScreenDelta describing changes since this watcher's last
// Diff call. The first Diff returns a bootstrap delta describing every row
// of the current canonical Screen, so a new client can render without a
// separate snapshot step.
func (w *Watcher) Diff() vt.ScreenDelta {
	cur := w.session.Snapshot()
	d := vt.Diff(w.last, cur)
	w.last = cur
	return d
}

// Snapshot returns the current canonical Screen without affecting the
// watcher's diff baseline. It is intended for catching up a renderer that
// has lost state but does not want to rewrite its baseline.
func (w *Watcher) Snapshot() vt.Screen { return w.session.Snapshot() }

func (w *Watcher) closeOnce() {
	w.closeMu.Lock()
	defer w.closeMu.Unlock()
	if w.closedSet {
		return
	}
	w.closedSet = true
	close(w.ticks)
	close(w.closed)
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	rand.Read(b) // never fails
	return b
}
