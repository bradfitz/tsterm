package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bradfitz/tsterm/pkg/shim"
	"github.com/bradfitz/tsterm/pkg/shimproto"
)

// ErrNotFound is returned by Manager lookups when no session matches.
var ErrNotFound = errors.New("session: not found")

// ErrNameTaken is returned by Create when the requested Name is already in
// use by an active session.
var ErrNameTaken = errors.New("session: name already in use")

// Manager owns the daemon's set of live Sessions, indexed by both ID and
// Name. It garbage-collects sessions whose Done channel has closed: those
// stay in the map only until the next mutating call (Create, Kill, List).
//
// Each session's PTY lives in a shim, started by the Manager's Launcher and
// listening on a unix socket in the Manager's directory. Shims outlive the
// Manager; a new Manager finds and restores them with Restore.
//
// A Manager is safe for concurrent use.
type Manager struct {
	dir      string
	launcher shim.Launcher
	logf     func(format string, args ...any)

	mu     sync.Mutex
	byID   map[ID]*Session
	byName map[string]ID
}

// NewManager returns an empty Manager whose shims live in dir, which must
// exist and be private to the user.
func NewManager(dir string, launcher shim.Launcher) *Manager {
	return &Manager{
		dir:      dir,
		launcher: launcher,
		logf:     func(string, ...any) {},
		byID:     map[ID]*Session{},
		byName:   map[string]ID{},
	}
}

// SetLogf sets where the Manager logs. The default is to discard logs.
func (m *Manager) SetLogf(logf func(format string, args ...any)) { m.logf = logf }

// startTimeout bounds how long Create waits for a new shim to start.
const startTimeout = 10 * time.Second

func (m *Manager) socketPath(id ID) string {
	return filepath.Join(m.dir, string(id)+".sock")
}

// Create starts a session per cfg and registers it. If cfg.Name is set and
// already in use, ErrNameTaken is returned.
func (m *Manager) Create(cfg Config) (*Session, error) {
	if cfg.Name != "" && m.ByName(cfg.Name) != nil {
		return nil, ErrNameTaken
	}
	rows, cols := cfg.Rows, cfg.Cols
	if rows <= 0 {
		rows = 24
	}
	if cols <= 0 {
		cols = 80
	}
	id := newID()
	md := meta{Name: cfg.Name, Token: randomBytes(32), Created: time.Now()}
	ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
	defer cancel()
	sock := m.socketPath(id)
	if err := m.launcher.Launch(ctx, shimproto.SpawnConfig{
		ID:      string(id),
		Socket:  sock,
		Command: cfg.PTY.Command,
		Args:    cfg.PTY.Args,
		Env:     withTerm(cfg.PTY.Env),
		Dir:     cfg.PTY.Dir,
		Rows:    rows,
		Cols:    cols,
		NoLogin: cfg.PTY.NoLogin,
		Meta:    encodeMeta(md),
	}); err != nil {
		return nil, fmt.Errorf("session: launching shim: %w", err)
	}
	conn, err := shim.DialWait(ctx, sock)
	if err != nil {
		return nil, fmt.Errorf("session: connecting to new shim: %w", err)
	}
	s, err := attach(conn, id, md, false)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if err := m.register(s); err != nil {
		go s.Close() // lost a race for the name; don't leak the child
		return nil, err
	}
	return s, nil
}

// Term is the TERM of every session. Programs in a session write to
// tstermd's terminal emulator, not to the client's terminal, which only
// redraws the emulator's screen; so TERM describes the emulator, whatever
// terminal the client runs in.
const Term = "xterm-256color"

// withTerm returns env with any TERM replaced by Term.
func withTerm(env []string) []string {
	out := slices.DeleteFunc(slices.Clone(env), func(kv string) bool {
		return strings.HasPrefix(kv, "TERM=")
	})
	return append(out, "TERM="+Term)
}

func (m *Manager) register(s *Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reapLocked()
	if s.Name() != "" {
		if _, ok := m.byName[s.Name()]; ok {
			return ErrNameTaken
		}
		m.byName[s.Name()] = s.ID()
	}
	m.byID[s.ID()] = s
	return nil
}

// Restore attaches to the shims already running in the Manager's
// directory, such as those started by a previous tstermd, and registers
// their sessions. Shims whose child has exited are released. It returns
// the number of sessions restored.
func (m *Manager) Restore(ctx context.Context) (int, error) {
	socks, err := filepath.Glob(filepath.Join(m.dir, "pty-*.sock"))
	if err != nil {
		return 0, err
	}
	n := 0
	for _, sock := range socks {
		if err := m.restore(ctx, sock); err != nil {
			m.logf("session: restoring %s: %v", sock, err)
			continue
		}
		n++
	}
	return n, nil
}

func (m *Manager) restore(ctx context.Context, sock string) error {
	dctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	conn, err := shim.Dial(dctx, sock)
	if err != nil {
		if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT) {
			// Nobody is listening: the shim is gone.
			os.Remove(sock)
		}
		return err
	}
	if conn.Info.Exited {
		// The child exited while no tstermd was running. Let the
		// shim go.
		if _, err := conn.Attach(); err == nil {
			conn.Release()
		}
		conn.Close()
		return fmt.Errorf("child had exited (%s)", conn.Info.ExitStatus)
	}
	md, err := decodeMeta(conn.Info.Meta)
	if err != nil {
		conn.Close()
		return fmt.Errorf("bad metadata: %w", err)
	}
	s, err := attach(conn, ID(conn.Info.ID), md, true)
	if err != nil {
		conn.Close()
		return err
	}
	if err := m.register(s); err != nil {
		s.Detach()
		return err
	}
	return nil
}

// Get returns the session with the given ID, or nil.
func (m *Manager) Get(id ID) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reapLocked()
	return m.byID[id]
}

// ByName returns the session with the given Name, or nil.
func (m *Manager) ByName(name string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reapLocked()
	id, ok := m.byName[name]
	if !ok {
		return nil
	}
	return m.byID[id]
}

// Resolve looks up a session by either ID (exact match) or Name. It returns
// ErrNotFound if neither matches.
func (m *Manager) Resolve(idOrName string) (*Session, error) {
	if s := m.Get(ID(idOrName)); s != nil {
		return s, nil
	}
	if s := m.ByName(idOrName); s != nil {
		return s, nil
	}
	return nil, ErrNotFound
}

// Kill closes the session with the given ID or Name.
func (m *Manager) Kill(idOrName string) error {
	s, err := m.Resolve(idOrName)
	if err != nil {
		return err
	}
	_ = s.Close()
	m.mu.Lock()
	delete(m.byID, s.ID())
	delete(m.byName, s.Name())
	m.mu.Unlock()
	return nil
}

// Sessions returns the live sessions, in no particular order.
func (m *Manager) Sessions() []*Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reapLocked()
	out := make([]*Session, 0, len(m.byID))
	for _, s := range m.byID {
		out = append(out, s)
	}
	return out
}

// Summary is the manager's view of a session as exposed over the wire by
// transports. The proto package mirrors this type field-for-field.
type Summary struct {
	ID       ID
	Name     string
	Rows     int
	Cols     int
	Created  time.Time
	Active   bool
	Watchers int

	LastInput  time.Time // keyboard input sent to the child
	LastOutput time.Time // output from the child, changing the screen
	LastAttach time.Time // a client attached to look at the session
}

// LastActivity returns the later of LastInput and LastOutput.
func (s Summary) LastActivity() time.Time {
	if s.LastInput.After(s.LastOutput) {
		return s.LastInput
	}
	return s.LastOutput
}

// List returns the current set of sessions, sorted by creation time.
func (m *Manager) List() []Summary {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reapLocked()
	out := make([]Summary, 0, len(m.byID))
	for _, s := range m.byID {
		rows, cols := s.Size()
		out = append(out, Summary{
			ID:       s.ID(),
			Name:     s.Name(),
			Rows:     rows,
			Cols:     cols,
			Created:  s.Created(),
			Active:   s.Active(),
			Watchers: s.WatchersCount(),

			LastInput:  s.LastInput(),
			LastOutput: s.LastOutput(),
			LastAttach: s.LastAttach(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

// Close terminates every session and clears the manager. Already-exited
// sessions are reaped without sending signals.
func (m *Manager) Close() error {
	for _, s := range m.takeAll() {
		_ = s.Close()
	}
	return nil
}

// Detach disconnects from every session without ending any, and clears the
// manager. The sessions keep running in their shims for a future Manager to
// Restore. tstermd calls it when shutting down.
func (m *Manager) Detach() {
	for _, s := range m.takeAll() {
		s.Detach()
	}
}

func (m *Manager) takeAll() []*Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	sessions := make([]*Session, 0, len(m.byID))
	for _, s := range m.byID {
		sessions = append(sessions, s)
	}
	m.byID = map[ID]*Session{}
	m.byName = map[string]ID{}
	return sessions
}

// reapLocked removes sessions whose Done channel has closed. The caller
// must hold m.mu.
func (m *Manager) reapLocked() {
	for id, s := range m.byID {
		select {
		case <-s.Done():
			delete(m.byID, id)
			delete(m.byName, s.Name())
		default:
		}
	}
}
