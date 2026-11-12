package session

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// waitFrozen waits for the session id to freeze in m.
func waitFrozen(t *testing.T, m *Manager, id ID) *Frozen {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if f := m.Frozen(string(id)); f != nil {
			return f
		}
		if time.Now().After(deadline) {
			t.Fatalf("session %s never froze", id)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func frozenText(t *testing.T, f *Frozen) string {
	t.Helper()
	ss, err := f.Screen()
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, row := range ss.Screen.Cells {
		for _, c := range row {
			b.WriteString(c.Grapheme)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func TestFrozenAfterExit(t *testing.T) {
	m, _ := newTestManager(t)
	cfg := makeCfg(historyScript(0, 100) + "; echo goodbye; exit 3")
	cfg.Name = "short"
	s, err := m.Create(cfg)
	if err != nil {
		t.Fatal(err)
	}
	f := waitFrozen(t, m, s.ID())
	if f.Status != "exit status 3" || f.Name != "short" || f.Ended.IsZero() {
		t.Errorf("frozen = %+v", f)
	}
	if got := frozenText(t, f); !strings.Contains(got, "goodbye") {
		t.Errorf("frozen screen:\n%s", got)
	}
	h, err := f.OpenHistory()
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if _, lines, _ := h.Read(0, 1); len(lines) != 1 || !strings.HasPrefix(lines[0].Text, "line-0-") {
		t.Errorf("frozen history starts %+v", lines)
	}
	// A frozen session doesn't keep its name from a new one.
	cfg.PTY.Args[1] = "sleep 30"
	if _, err := m.Create(cfg); err != nil {
		t.Errorf("creating a session with a frozen one's name: %v", err)
	}
	if m.ByName("short") == nil || m.Frozen("short") == nil {
		t.Error("name lookups by live and frozen failed")
	}
}

func TestKillFreezesAndRemoveDeletes(t *testing.T) {
	m, _ := newTestManager(t)
	s, err := m.Create(makeCfg("echo killme; sleep 30"))
	if err != nil {
		t.Fatal(err)
	}
	waitScreen(t, s, "killme")
	if err := m.Kill(string(s.ID())); err != nil {
		t.Fatal(err)
	}
	f := waitFrozen(t, m, s.ID())
	if f.Status != "signal: killed" {
		t.Errorf("Status = %q", f.Status)
	}
	if err := m.Remove(string(s.ID())); err != nil {
		t.Fatal(err)
	}
	if m.Frozen(string(s.ID())) != nil {
		t.Error("removed session still frozen")
	}
	if _, err := os.Stat(m.sessionDir(s.ID())); !os.IsNotExist(err) {
		t.Errorf("removed session's directory: %v", err)
	}
	for _, e := range m.List() {
		if e.ID == s.ID() {
			t.Error("removed session listed")
		}
	}
}

func TestFrozenRetention(t *testing.T) {
	m, _ := newTestManager(t)
	var ids []ID
	for range 4 {
		s, err := m.Create(makeCfg("true"))
		if err != nil {
			t.Fatal(err)
		}
		waitFrozen(t, m, s.ID())
		ids = append(ids, s.ID())
	}
	kept := func(i int) bool {
		_, err := os.Stat(m.sessionDir(ids[i]))
		return m.Frozen(string(ids[i])) != nil && err == nil
	}

	// One that ended over a year ago goes.
	m.mu.Lock()
	m.frozen[ids[0]].Ended = time.Now().Add(-maxFrozenAge - time.Hour)
	m.pruneFrozenLocked()
	m.mu.Unlock()
	if kept(0) || !kept(1) {
		t.Errorf("after aging: kept %v, %v", kept(0), kept(1))
	}

	// Over the size limit, the oldest go until it fits.
	old := maxFrozenBytes
	t.Cleanup(func() { maxFrozenBytes = old })
	m.mu.Lock()
	maxFrozenBytes = m.frozen[ids[2]].size + m.frozen[ids[3]].size
	m.pruneFrozenLocked()
	m.mu.Unlock()
	if kept(1) || !kept(2) || !kept(3) {
		t.Errorf("after shrinking: kept %v, %v, %v", kept(1), kept(2), kept(3))
	}
}

func TestClosedSessions(t *testing.T) {
	m, l := newTestManager(t)
	cfg := makeCfg("echo hi; sleep 30")
	cfg.Name = "bg"
	s, err := m.Create(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if closed, opened := s.Closed(); closed || opened.IsZero() {
		t.Errorf("new session: closed %v, opened %v", closed, opened)
	}
	if err := m.SetClosed("bg", true); err != nil {
		t.Fatal(err)
	}
	if !s.Active() {
		t.Fatal("closing ended the session")
	}

	// It stays closed across a tstermd restart.
	m.Detach()
	m2 := NewManager(m.dir, l)
	m2.SetStateDir(m.stateDir)
	t.Cleanup(func() { m2.Close() })
	m2.Restore(context.Background())
	s2 := m2.ByName("bg")
	if s2 == nil {
		t.Fatal("not restored")
	}
	if closed, _ := s2.Closed(); !closed {
		t.Error("restored session isn't closed")
	}
	before := time.Now()
	m2.SetClosed("bg", false)
	if closed, opened := s2.Closed(); closed || opened.Before(before) {
		t.Errorf("reopened: closed %v, opened %v", closed, opened)
	}

	// Frozen, it keeps its name, command, and state, and can be closed.
	m2.Kill("bg")
	f := waitFrozen(t, m2, s.ID())
	if f.Name != "bg" || len(f.Cmd) != 3 || f.Cmd[2] != "echo hi; sleep 30" || f.Closed || f.Created.IsZero() || f.Ended.IsZero() || f.Status != "signal: killed" {
		t.Errorf("frozen = %+v", f)
	}
	if err := m2.SetClosed(string(s.ID()), true); err != nil {
		t.Fatal(err)
	}
	// Its info on disk says so too.
	f2, err := loadFrozen(m2.sessionDir(s.ID()))
	if err != nil || !f2.Closed || f2.Name != "bg" || f2.Status != "signal: killed" {
		t.Errorf("frozen from disk = %+v, %v", f2, err)
	}
}

// TestFrozenAfterLoss checks that a session whose shim went away while no
// tstermd was running, as in a reboot, is found frozen, lost, with its
// last saved screen.
func TestFrozenAfterLoss(t *testing.T) {
	old := screenSaveInterval
	screenSaveInterval = 50 * time.Millisecond
	t.Cleanup(func() { screenSaveInterval = old })
	m, l := newTestManager(t)
	s, err := m.Create(makeCfg("echo before-the-crash; sleep 30"))
	if err != nil {
		t.Fatal(err)
	}
	waitSaved(t, m.sessionDir(s.ID()), "before-the-crash")
	m.Detach()
	l.Close() // the shims go away

	m2 := NewManager(m.dir, l)
	m2.SetStateDir(m.stateDir)
	t.Cleanup(func() { m2.Close() })
	if n, _ := m2.Restore(context.Background()); n != 0 {
		t.Errorf("restored %d live sessions", n)
	}
	f := m2.Frozen(string(s.ID()))
	if f == nil {
		t.Fatal("lost session not found frozen")
	}
	if f.Status != StatusLost || f.Ended.IsZero() {
		t.Errorf("frozen = %+v", f)
	}
	if got := frozenText(t, f); !strings.Contains(got, "before-the-crash") {
		t.Errorf("frozen screen:\n%s", got)
	}
}
