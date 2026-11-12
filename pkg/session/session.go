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
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/bradfitz/tsterm/pkg/history"
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

	// LastAttach is when a client last attached to the session to look
	// at it. The shim keeps it, in memory, on tstermd's behalf.
	LastAttach time.Time `json:"lastAttach,omitzero"`

	// Cmd is the command the session runs, if not the login shell.
	Cmd []string `json:"cmd,omitzero"`

	// Named is set once the session has had a name, from the user or
	// auto-naming (see Manager.AutoName), which then leaves it be.
	Named bool `json:"named,omitzero"`

	// Closed is set when the session is out of the user's open set: its
	// tab is closed, though it runs on. Opened is when it last joined
	// the set, which orders the tabs; zero means when it was created.
	Closed bool      `json:"closed,omitzero"`
	Opened time.Time `json:"opened,omitzero"`
}

// Client identifies a hot-path client attached to a session.
type Client struct {
	Addr  string `json:"addr"`  // UDP address, as from netip.AddrPort.String
	Nonce []byte `json:"nonce"` // the client's Hello nonce
}

// Checkpoint policy. The shim keeps all output since the last checkpoint,
// so checkpoints bound both its memory and the replay a restarted tstermd
// must do. A checkpoint holds the terminal's complete state (see
// vt.Terminal.Checkpoint), even partway through an escape sequence.
var (
	checkpointBytes    = uint64(256 << 10) // tests lower it
	checkpointInterval = 5 * time.Second
)

// Session is a running PTY child whose output is interpreted by a canonical
// vt.Terminal. Sessions are created and restored by a Manager.
type Session struct {
	id      ID
	token   []byte
	created time.Time
	conn    *shim.Conn

	metaMu sync.Mutex
	meta   meta // as stored in the shim

	// Activity times. The shim tracks input and output too, so that they
	// survive a tstermd restart; these start from its values.
	activityMu sync.Mutex
	lastInput  time.Time
	lastOutput time.Time
	replayEnd  uint64 // output up to this offset predates attaching
	term       vt.Terminal

	// hist, if non-nil, stores the lines that scroll off term's screen.
	// histMu orders taking them from term and adding them to hist.
	histMu sync.Mutex
	hist   *history.Store

	// dir, if not empty, is the session's directory, which holds its
	// history and its saved screen (see saveScreenLoop).
	dir           string
	logf          func(format string, args ...any)
	screenDirty   atomic.Bool // since the screen was last saved
	stopSaver     chan struct{}
	stopSaverOnce sync.Once
	saverDone     chan struct{}
	infoMu        sync.Mutex // serializes saveInfo
	endedAt       time.Time  // set by shutdown

	done     chan struct{}
	pumpDone chan struct{} // closed when the pump returns

	mu       sync.Mutex
	watchers map[uint64]*Watcher
	nextWID  uint64

	// sizeMu guards leader and every Sizer's wanted size, and serializes
	// the resizes they make.
	sizeMu sync.Mutex
	leader *Sizer // or nil

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
}

// attach makes conn the shim's controller and starts a Session for it,
// restoring the terminal state from the shim's latest checkpoint. restored
// says whether the shim had a previous controller, a previous tstermd, in
// which case output already produced was seen, and its queries answered,
// by that tstermd.
//
// If dir isn't empty, the session keeps its history and saves its screen
// there.
func attach(conn *shim.Conn, id ID, m meta, restored bool, dir string, logf func(string, ...any)) (*Session, error) {
	ckpt, err := conn.Attach()
	if err != nil {
		return nil, err
	}
	rows, cols := max(conn.Info.Rows, 1), max(conn.Info.Cols, 1)
	blob, histLines, hasHistLines := decodeCheckpoint(ckpt.Blob)
	var term vt.Terminal
	if len(blob) > 0 {
		term, err = vt.Restore(blob, rows, cols)
	} else {
		term, err = vt.New(rows, cols)
	}
	if err != nil {
		return nil, err
	}
	var hist *history.Store
	if dir != "" {
		if hist, err = history.Open(filepath.Join(dir, "history"), 0); err != nil {
			term.Close()
			return nil, err
		}
		// Replaying the output since the checkpoint will add the lines
		// stored since then again.
		if hasHistLines && hist.Next() > histLines {
			if err := hist.Truncate(histLines); err != nil {
				hist.Close()
				term.Close()
				return nil, err
			}
		}
	}
	s := &Session{
		id:         id,
		token:      m.Token,
		created:    m.Created,
		conn:       conn,
		meta:       m,
		lastInput:  conn.Info.LastInput,
		lastOutput: conn.Info.LastOutput,
		replayEnd:  replayEnd(conn, restored),
		term:       term,
		hist:       hist,
		dir:        dir,
		logf:       logf,
		stopSaver:  make(chan struct{}),
		saverDone:  make(chan struct{}),
		done:       make(chan struct{}),
		pumpDone:   make(chan struct{}),
		watchers:   map[uint64]*Watcher{},
		ckptTime:   time.Now(),
	}
	s.offset.Store(ckpt.Offset)
	s.ckptOffset.Store(ckpt.Offset)
	go s.pump()
	if dir != "" {
		s.screenDirty.Store(true)
		go s.saveScreenLoop()
	}
	return s, nil
}

// The session's checkpoint, which the shim keeps, is sessionCkptMagic, the
// number of history lines stored (a u64, little endian), then the
// terminal's checkpoint. Older ones are just the terminal's.
const sessionCkptMagic = "tsss\x01"

func encodeCheckpoint(term []byte, histLines uint64) []byte {
	b := make([]byte, 0, len(sessionCkptMagic)+8+len(term))
	b = append(b, sessionCkptMagic...)
	b = binary.LittleEndian.AppendUint64(b, histLines)
	return append(b, term...)
}

func decodeCheckpoint(b []byte) (term []byte, histLines uint64, ok bool) {
	rest, ok := bytes.CutPrefix(b, []byte(sessionCkptMagic))
	if !ok || len(rest) < 8 {
		return b, 0, false
	}
	return rest[8:], binary.LittleEndian.Uint64(rest), true
}

// replayEnd returns the output offset up to which attaching replays output
// that a previous controller already handled.
func replayEnd(conn *shim.Conn, restored bool) uint64 {
	if !restored {
		// A new session's early output, produced before we attached,
		// is ours to handle.
		return 0
	}
	return conn.Info.OutputEnd
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
func (s *Session) Name() string {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	return s.meta.Name
}

// setName sets the session's name, storing it in the shim. The Manager
// keeps names unique.
func (s *Session) setName(name string) error {
	s.metaMu.Lock()
	s.meta.Name = name
	if name != "" {
		s.meta.Named = true
	}
	err := s.conn.SetMeta(encodeMeta(s.meta))
	s.metaMu.Unlock()
	if err == nil && s.Active() {
		s.saveInfo(time.Time{}, "")
	}
	return err
}

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

// MarkAttached records that a client attached to the session to look at
// it, for LastAttach.
func (s *Session) MarkAttached() error {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	s.meta.LastAttach = time.Now()
	return s.conn.SetMeta(encodeMeta(s.meta))
}

// Cmd returns the command the session runs, or nil for the login shell.
func (s *Session) Cmd() []string {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	return s.meta.Cmd
}

// Closed reports whether the session is out of the user's open set (see
// Manager.SetClosed), and when it last joined it.
func (s *Session) Closed() (closed bool, opened time.Time) {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	opened = s.meta.Opened
	if opened.IsZero() {
		opened = s.meta.Created
	}
	return s.meta.Closed, opened
}

// setClosed moves the session out of the user's open set, or into it.
func (s *Session) setClosed(closed bool) error {
	s.metaMu.Lock()
	if s.meta.Closed == closed {
		s.metaMu.Unlock()
		return nil
	}
	s.meta.Closed = closed
	if !closed {
		s.meta.Opened = time.Now()
	}
	err := s.conn.SetMeta(encodeMeta(s.meta))
	s.metaMu.Unlock()
	if err == nil && s.Active() {
		s.saveInfo(time.Time{}, "")
	}
	return err
}

// LastAttach returns when a client last attached, or the zero time.
func (s *Session) LastAttach() time.Time {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	return s.meta.LastAttach
}

// LastInput returns when keyboard input was last sent to the child, or the
// zero time.
func (s *Session) LastInput() time.Time {
	s.activityMu.Lock()
	defer s.activityMu.Unlock()
	return s.lastInput
}

// LastOutput returns when the child last produced output, which is when
// the screen last changed, or the zero time.
func (s *Session) LastOutput() time.Time {
	s.activityMu.Lock()
	defer s.activityMu.Unlock()
	return s.lastOutput
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
func (s *Session) Snapshot() vt.Screen {
	scr := s.term.Snapshot()
	if s.hist != nil {
		scr.History = s.hist.Next()
	}
	return scr
}

// History returns the session's history, or nil if it isn't kept.
func (s *Session) History() *history.Store { return s.hist }

// takeHistory moves the lines that have scrolled off the terminal's screen
// to the session's history.
func (s *Session) takeHistory() {
	s.histMu.Lock()
	defer s.histMu.Unlock()
	s.takeHistoryLocked()
}

func (s *Session) takeHistoryLocked() {
	lines, lost := s.term.TakeHistory()
	if s.hist == nil || len(lines) == 0 && !lost {
		return
	}
	if lost {
		// The terminal's scrollback overflowed before these lines
		// could be read. Say so where they would have been.
		lines = append([]vt.Line{lostLine}, lines...)
	}
	_ = s.hist.Append(lines...)
}

// lostLine stands in for lines missing from history.
var lostLine = vt.Line{
	Text: "[tsterm: lines missing here]",
	Runs: []vt.Run{{Len: len("[tsterm: lines missing here]"), Style: vt.Style{Attrs: vt.AttrFaint}}},
}

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
	s.activityMu.Lock()
	s.lastInput = time.Now()
	s.activityMu.Unlock()
	return len(b), nil
}

// Resize sets the PTY window size and the canonical terminal grid size.
// The child receives SIGWINCH.
func (s *Session) Resize(rows, cols int) error {
	if err := s.conn.Resize(rows, cols); err != nil {
		return err
	}
	s.term.Resize(rows, cols)
	s.takeHistory() // shrinking can scroll lines off
	s.screenDirty.Store(true)
	if r := s.term.Replies(); len(r) > 0 {
		_ = s.conn.Input(r) // the size, in in-band resize mode
	}
	s.notifyWatchers()
	return nil
}

// A Sizer is one client's say in the session's size. Clients disagree
// about the size when their windows do, so only one Sizer, the leader,
// sets it: the one whose client the user used last. A client claims the
// lead (Lead) when the user interacts with it, as by typing or focusing
// its window, and records the size it wants (Want) whenever that changes.
// The session then has the leader's wanted size, if it wants one.
type Sizer struct {
	s          *Session
	label      string
	rows, cols int // wanted; zero for no preference
}

// NewSizer returns a Sizer for a client, described by label (such as
// "browser") to the other clients.
func (s *Session) NewSizer(label string) *Sizer {
	return &Sizer{s: s, label: label}
}

// Want records the size z's client wants, rows by cols, or no preference
// if they're zero, and applies it if z leads. With no leader, z takes the
// lead.
func (z *Sizer) Want(rows, cols int) error {
	s := z.s
	s.sizeMu.Lock()
	defer s.sizeMu.Unlock()
	z.rows, z.cols = rows, cols
	if s.leader == nil {
		s.leader = z
		defer s.notifyWatchers()
	}
	if s.leader != z {
		return nil
	}
	return z.applyLocked()
}

// Lead makes z the leader, applying its wanted size.
func (z *Sizer) Lead() error {
	s := z.s
	s.sizeMu.Lock()
	defer s.sizeMu.Unlock()
	if s.leader != z {
		s.leader = z
		defer s.notifyWatchers() // so others learn they lost the lead
	}
	return z.applyLocked()
}

// applyLocked resizes the session to z's wanted size, if it has one.
func (z *Sizer) applyLocked() error {
	if z.rows <= 0 || z.cols <= 0 {
		return nil
	}
	if rows, cols := z.s.Size(); rows == z.rows && cols == z.cols {
		return nil
	}
	return z.s.Resize(z.rows, z.cols)
}

// Leader reports whether z leads, and otherwise the label of the Sizer
// that does, if any.
func (z *Sizer) Leader() (leads bool, other string) {
	s := z.s
	s.sizeMu.Lock()
	defer s.sizeMu.Unlock()
	if s.leader == z {
		return true, ""
	}
	if s.leader != nil {
		return false, s.leader.label
	}
	return false, ""
}

// Close gives up z's lead, if it has it, as when its client goes away.
func (z *Sizer) Close() {
	s := z.s
	s.sizeMu.Lock()
	defer s.sizeMu.Unlock()
	if s.leader == z {
		s.leader = nil
		s.notifyWatchers()
	}
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

// detachTimeout bounds how long Detach waits for the shim to finish
// handling what was sent to it.
const detachTimeout = 2 * time.Second

// Detach disconnects from the shim without ending the session, which keeps
// running for a future tstermd to restore. The Session is unusable after.
//
// Detach waits for the shim to handle everything already sent to it, such
// as a rename stored in its metadata, so that a tstermd that starts next
// finds it.
func (s *Session) Detach() {
	s.detached.Store(true)
	if s.conn.CloseWrite() == nil {
		select {
		case <-s.pumpDone: // the shim closed its end
		case <-time.After(detachTimeout):
		}
	}
	s.conn.Close()
	s.takeHistory()
	s.stopSaving()
	s.term.Close()
	if s.hist != nil {
		_ = s.hist.Close()
	}
}

// pump runs in a dedicated goroutine. It copies output from the shim into
// the canonical terminal, notifies watchers after each batch, checkpoints,
// and closes Done when the child exits.
func (s *Session) pump() {
	defer close(s.pumpDone)
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
		s.takeHistory()
		s.screenDirty.Store(true)
		s.notifyWatchers()
		replies := s.term.Replies()
		if offset := s.offset.Add(uint64(len(out))); offset > s.replayEnd {
			// New output, not a replay of output from before we
			// attached, whose time the shim already reported.
			s.activityMu.Lock()
			s.lastOutput = time.Now()
			s.activityMu.Unlock()
			// Answer the program's queries, such as cursor position
			// reports. Queries in replayed output were answered by
			// the tstermd that first saw them; answering them again
			// would feed stale replies to the program as input.
			if len(replies) > 0 {
				_ = s.conn.Input(replies)
			}
		}
		s.maybeCheckpoint()
	}
}

// maybeCheckpoint sends the shim a checkpoint if one is due and possible.
func (s *Session) maybeCheckpoint() {
	offset := s.offset.Load()
	pending := offset - s.ckptOffset.Load()
	if pending == 0 {
		return
	}
	if pending < checkpointBytes && time.Since(s.ckptTime) < checkpointInterval {
		return
	}
	// Only the pump writes to the terminal, so the checkpoint
	// corresponds exactly to offset. Under histMu, the history stored
	// corresponds exactly to the lines the checkpoint says the terminal
	// has returned.
	s.histMu.Lock()
	s.takeHistoryLocked()
	term, err := s.term.Checkpoint()
	var histLines uint64
	if s.hist != nil {
		histLines = s.hist.Next()
		_ = s.hist.Flush()
	}
	s.histMu.Unlock()
	if err != nil {
		return
	}
	ckpt := encodeCheckpoint(term, histLines)
	if err := s.conn.SetCheckpoint(offset, ckpt); err != nil {
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
	s.endedAt = time.Now()
	s.takeHistory()
	s.stopSaving()
	s.saveInfo(s.endedAt, endStatus(waitErr))
	s.term.Close()
	if s.hist != nil {
		_ = s.hist.Close()
	}
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
