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
	old := maxFrozen
	maxFrozen = 2
	t.Cleanup(func() { maxFrozen = old })
	m, _ := newTestManager(t)
	var ids []ID
	for range 3 {
		s, err := m.Create(makeCfg("true"))
		if err != nil {
			t.Fatal(err)
		}
		waitFrozen(t, m, s.ID())
		ids = append(ids, s.ID())
	}
	if m.Frozen(string(ids[0])) != nil {
		t.Error("oldest frozen session kept")
	}
	if m.Frozen(string(ids[1])) == nil || m.Frozen(string(ids[2])) == nil {
		t.Error("newest frozen sessions not kept")
	}
	if _, err := os.Stat(m.sessionDir(ids[0])); !os.IsNotExist(err) {
		t.Errorf("oldest frozen session's directory: %v", err)
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
