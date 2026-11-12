package session

import (
	"encoding/json/v2"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/bradfitz/tsterm/pkg/history"
)

// A Frozen session is one that has ended, kept so that it can still be
// looked at, read-only: its last screen, and its history. A session
// freezes when its program exits or it is killed. One whose shim is gone
// when tstermd starts, as after a reboot or crash, is found frozen in its
// directory, "lost".
type Frozen struct {
	ID      ID
	Name    string
	Created time.Time

	// Ended is when the session ended, or, if it was lost, when its
	// screen was last saved.
	Ended time.Time

	// Status says how it ended: "exited", the program's exit error (as
	// "exit status 1" or "signal: killed"), or StatusLost.
	Status string

	LastInput, LastOutput, LastAttach time.Time

	Cmd    []string  // the command, or nil for the login shell
	Closed bool      // out of the open set (see Manager.SetClosed)
	Opened time.Time // when it last joined the open set

	dir  string
	size int64 // of dir, roughly
}

// StatusLost is the Status of a session that ended without tstermd seeing
// it end: tstermd, its shim, or the system went away.
const StatusLost = "lost"

// Screen returns the session's last screen.
func (f *Frozen) Screen() (SavedScreen, error) { return LoadScreen(f.dir) }

// OpenHistory opens the session's history, which the caller must close.
func (f *Frozen) OpenHistory() (*history.Store, error) {
	return history.Open(filepath.Join(f.dir, "history"), 0)
}

// infoFile is the name of a session's info in its directory, JSON.
const infoFile = "info"

// info is what a session's directory records about it, besides its
// screen and history.
type info struct {
	Name       string    `json:"name,omitzero"`
	Created    time.Time `json:"created"`
	LastInput  time.Time `json:"lastInput,omitzero"`
	LastOutput time.Time `json:"lastOutput,omitzero"`
	LastAttach time.Time `json:"lastAttach,omitzero"`

	// Ended and Status are set when the session ends (see Frozen).
	Ended  time.Time `json:"ended,omitzero"`
	Status string    `json:"status,omitzero"`

	Cmd    []string  `json:"cmd,omitzero"`
	Closed bool      `json:"closed,omitzero"`
	Opened time.Time `json:"opened,omitzero"`
}

// info returns f's info, to save.
func (f *Frozen) info() info {
	return info{
		Name: f.Name, Created: f.Created,
		LastInput: f.LastInput, LastOutput: f.LastOutput, LastAttach: f.LastAttach,
		Ended: f.Ended, Status: f.Status,
		Cmd: f.Cmd, Closed: f.Closed, Opened: f.Opened,
	}
}

func saveInfoFile(dir string, in info) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return writeFileSync(filepath.Join(dir, infoFile), b)
}

// saveInfo records the session's info in its directory, with ended and
// status if it has ended.
func (s *Session) saveInfo(ended time.Time, status string) {
	if s.dir == "" {
		return
	}
	in := info{
		Name:       s.Name(),
		Created:    s.created,
		LastInput:  s.LastInput(),
		LastOutput: s.LastOutput(),
		LastAttach: s.LastAttach(),
		Ended:      ended,
		Status:     status,
		Cmd:        s.Cmd(),
	}
	in.Closed, in.Opened = s.Closed()
	s.infoMu.Lock()
	defer s.infoMu.Unlock()
	if err := saveInfoFile(s.dir, in); err != nil {
		s.logf("session %s: saving info: %v", s.id, err)
	}
}

// EndStatus returns how the session ended (see Frozen.Status), once Done
// is closed.
func (s *Session) EndStatus() string { return endStatus(s.waitErr) }

// endStatus returns the Status of a session that ended with waitErr.
func endStatus(waitErr error) string {
	if waitErr == nil {
		return "exited"
	}
	return waitErr.Error()
}

// frozen returns the Frozen form of s, which has ended.
func (s *Session) frozen() *Frozen {
	closed, opened := s.Closed()
	return &Frozen{
		Cmd:        s.Cmd(),
		Closed:     closed,
		Opened:     opened,
		ID:         s.id,
		Name:       s.Name(),
		Created:    s.created,
		Ended:      s.endedAt,
		Status:     endStatus(s.waitErr),
		LastInput:  s.LastInput(),
		LastOutput: s.LastOutput(),
		LastAttach: s.LastAttach(),
		dir:        s.dir,
		size:       dirSize(s.dir),
	}
}

// loadFrozen returns the frozen session in directory dir.
func loadFrozen(dir string) (*Frozen, error) {
	b, err := os.ReadFile(filepath.Join(dir, infoFile))
	if err != nil {
		return nil, err
	}
	var in info
	if err := json.Unmarshal(b, &in); err != nil {
		return nil, err
	}
	f := &Frozen{
		ID:         ID(filepath.Base(dir)),
		Name:       in.Name,
		Created:    in.Created,
		Ended:      in.Ended,
		Status:     in.Status,
		LastInput:  in.LastInput,
		LastOutput: in.LastOutput,
		LastAttach: in.LastAttach,
		Cmd:        in.Cmd,
		Closed:     in.Closed,
		Opened:     in.Opened,
		dir:        dir,
		size:       dirSize(dir),
	}
	if f.Opened.IsZero() {
		f.Opened = f.Created
	}
	if f.Status == "" {
		// It never ended, as far as anyone saw.
		f.Status = StatusLost
		if ss, err := LoadScreen(dir); err == nil {
			f.Ended = ss.Saved
		} else if fi, err := os.Stat(filepath.Join(dir, infoFile)); err == nil {
			f.Ended = fi.ModTime()
		}
	}
	return f, nil
}

// dirSize returns the bytes the files under dir take.
func dirSize(dir string) int64 {
	var n int64
	filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

// Retention of frozen sessions: those that ended within maxFrozenAge, as
// long as they take no more than maxFrozenBytes together, the oldest going
// first. Live sessions are always kept. Tests lower these.
var (
	maxFrozenAge         = 365 * 24 * time.Hour
	maxFrozenBytes int64 = 10 << 30
)

// loadFrozenLocked adds the frozen sessions in the Manager's state
// directory: those without a live session. The caller must hold m.mu.
func (m *Manager) loadFrozenLocked() {
	if m.stateDir == "" {
		return
	}
	ents, err := os.ReadDir(m.stateDir)
	if err != nil {
		return
	}
	for _, e := range ents {
		id := ID(e.Name())
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "pty-") || m.byID[id] != nil || m.frozen[id] != nil {
			continue
		}
		f, err := loadFrozen(filepath.Join(m.stateDir, e.Name()))
		if err != nil {
			m.logf("session: loading frozen %s: %v", id, err)
			continue
		}
		m.frozen[id] = f
	}
	m.pruneFrozenLocked()
}

// pruneFrozenLocked deletes the oldest frozen sessions beyond the limits.
// The caller must hold m.mu.
func (m *Manager) pruneFrozenLocked() {
	all := make([]*Frozen, 0, len(m.frozen))
	var total int64
	for _, f := range m.frozen {
		all = append(all, f)
		total += f.size
	}
	slices.SortFunc(all, func(a, b *Frozen) int { return a.Ended.Compare(b.Ended) })
	for len(all) > 0 && (time.Since(all[0].Ended) > maxFrozenAge || total > maxFrozenBytes) {
		f := all[0]
		all = all[1:]
		total -= f.size
		m.removeFrozenLocked(f)
	}
}

func (m *Manager) removeFrozenLocked(f *Frozen) {
	delete(m.frozen, f.ID)
	if err := os.RemoveAll(f.dir); err != nil {
		m.logf("session: removing %s: %v", f.dir, err)
	}
}

// Frozen returns the frozen session with the given ID, or else the newest
// with the given name, or nil.
func (m *Manager) Frozen(idOrName string) *Frozen {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reapLocked()
	if f := m.frozen[ID(idOrName)]; f != nil {
		return f
	}
	var newest *Frozen
	for _, f := range m.frozen {
		if f.Name == idOrName && idOrName != "" && (newest == nil || f.Ended.After(newest.Ended)) {
			newest = f
		}
	}
	return newest
}

// SetClosed moves the session, live or frozen, with the given ID or name
// out of the user's open set, the sessions they have tabs for, or into it.
// A closed session runs on; it's just out of the way.
func (m *Manager) SetClosed(idOrName string, closed bool) error {
	if s, err := m.Resolve(idOrName); err == nil {
		return s.setClosed(closed)
	}
	f := m.Frozen(idOrName)
	if f == nil {
		return ErrNotFound
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if f.Closed == closed {
		return nil
	}
	f.Closed = closed
	if !closed {
		f.Opened = time.Now()
	}
	return saveInfoFile(f.dir, f.info())
}

// Remove deletes the frozen session with the given ID or name, and all
// it kept.
func (m *Manager) Remove(idOrName string) error {
	f := m.Frozen(idOrName)
	if f == nil {
		return ErrNotFound
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.frozen[f.ID] == f {
		m.removeFrozenLocked(f)
	}
	return nil
}
