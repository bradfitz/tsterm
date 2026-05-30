//go:build unix

package session

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bradfitz/tsterm/pkg/pty"
)

func makeCfg(script string) Config {
	return Config{
		PTY: pty.Config{
			Command: "/bin/sh",
			Args:    []string{"-c", script},
		},
		Rows: 24,
		Cols: 80,
	}
}

func TestManagerCreateAndList(t *testing.T) {
	m, _ := newTestManager(t)

	s1, err := m.Create(makeCfg("sleep 1"))
	if err != nil {
		t.Fatalf("Create s1: %v", err)
	}
	s2, err := m.Create(makeCfg("sleep 1"))
	if err != nil {
		t.Fatalf("Create s2: %v", err)
	}
	if s1.ID() == s2.ID() {
		t.Errorf("two sessions got identical IDs: %q", s1.ID())
	}
	if bytes.Equal(s1.Token(), s2.Token()) {
		t.Errorf("two sessions got identical tokens")
	}
	for _, s := range []*Session{s1, s2} {
		if !strings.HasPrefix(string(s.ID()), "pty-") || s.Name() != "" {
			t.Errorf("ID %q, name %q; want pty- ID and no name", s.ID(), s.Name())
		}
	}

	list := m.List()
	if len(list) != 2 {
		t.Errorf("List length = %d, want 2", len(list))
	}
}

func TestManagerNameLookup(t *testing.T) {
	m, _ := newTestManager(t)
	cfg := makeCfg("sleep 1")
	cfg.Name = "build"
	s, err := m.Create(cfg)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := m.ByName("build"); got != s {
		t.Errorf("ByName build = %v, want %v", got, s)
	}
	if got, err := m.Resolve("build"); err != nil || got != s {
		t.Errorf("Resolve(name) = (%v,%v), want (%v,nil)", got, err, s)
	}
	if got, err := m.Resolve(string(s.ID())); err != nil || got != s {
		t.Errorf("Resolve(id) = (%v,%v), want (%v,nil)", got, err, s)
	}
}

func TestManagerNameCollision(t *testing.T) {
	m, _ := newTestManager(t)
	cfg := makeCfg("sleep 1")
	cfg.Name = "dup"
	if _, err := m.Create(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(cfg); err != ErrNameTaken {
		t.Errorf("second Create same name: got %v, want ErrNameTaken", err)
	}
}

func TestManagerKill(t *testing.T) {
	m, _ := newTestManager(t)
	s, err := m.Create(makeCfg("sleep 5"))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Kill(string(s.ID())); err != nil {
		t.Errorf("Kill: %v", err)
	}
	if m.Get(s.ID()) != nil {
		t.Errorf("session still present after Kill")
	}
	if _, err := m.Resolve(string(s.ID())); err != ErrNotFound {
		t.Errorf("Resolve after Kill: got %v, want ErrNotFound", err)
	}
}

func TestManagerReapsFinishedSessions(t *testing.T) {
	m, _ := newTestManager(t)
	s, err := m.Create(makeCfg("true"))
	if err != nil {
		t.Fatal(err)
	}
	// Wait for the child to exit and the pump to drain.
	select {
	case <-s.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("session did not finish")
	}
	// List triggers reap; the dead session should be gone.
	list := m.List()
	for _, e := range list {
		if e.ID == s.ID() {
			t.Errorf("dead session still in List")
		}
	}
}

// waitScreen waits until s's screen contains want.
func waitScreen(t *testing.T, s *Session, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(screenText(s), want) {
		if time.Now().After(deadline) {
			t.Fatalf("screen never showed %q; have:\n%s", want, screenText(s))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRestoreAfterDetach(t *testing.T) {
	m, l := newTestManager(t)
	cfg := makeCfg("printf '\\033[1;32mgreen\\033[0m\\n'; stty -echo; cat")
	cfg.Name = "keep"
	s, err := m.Create(cfg)
	if err != nil {
		t.Fatal(err)
	}
	waitScreen(t, s, "green")
	s.WriteInput([]byte("typed\n"))
	waitScreen(t, s, "typed")
	before := s.Snapshot()
	pid, token := s.Pid(), s.Token()

	// tstermd shuts down, leaving the shim running, and a new one starts.
	m.Detach()
	m2 := NewManager(m.dir, l)
	t.Cleanup(func() { m2.Close() })
	n, err := m2.Restore(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("Restore = %d, %v; want 1 session", n, err)
	}
	s2 := m2.ByName("keep")
	if s2 == nil {
		t.Fatal("restored session not found by name")
	}
	if s2.ID() != s.ID() || s2.Pid() != pid || !bytes.Equal(s2.Token(), token) {
		t.Errorf("restored session differs: id %s pid %d", s2.ID(), s2.Pid())
	}
	waitScreen(t, s2, "typed")
	if after := s2.Snapshot(); !after.Equal(before) {
		t.Errorf("restored screen differs:\nbefore:\n%s\nafter:\n%s", screenText(s), screenText(s2))
	}

	// The child is still alive and reachable.
	s2.WriteInput([]byte("again\n"))
	waitScreen(t, s2, "again")
}

func TestRestoreUsesCheckpoint(t *testing.T) {
	m, l := newTestManager(t)
	// Far more output than the checkpoint threshold, then a marker.
	s, err := m.Create(makeCfg("i=0; while [ $i -lt 6000 ]; do echo line-$i-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx; i=$((i+1)); done; echo END; sleep 30"))
	if err != nil {
		t.Fatal(err)
	}
	waitScreen(t, s, "END")
	before := s.Snapshot()
	if s.ckptOffset.Load() == 0 {
		t.Fatalf("no checkpoint after %d bytes of output", s.offset.Load())
	}
	m.Detach()

	m2 := NewManager(m.dir, l)
	t.Cleanup(func() { m2.Close() })
	if n, err := m2.Restore(context.Background()); err != nil || n != 1 {
		t.Fatalf("Restore = %d, %v", n, err)
	}
	s2 := m2.Get(s.ID())
	waitScreen(t, s2, "END")
	if !s2.Snapshot().Equal(before) {
		t.Errorf("restored screen differs:\nbefore:\n%s\nafter:\n%s", screenText(s), screenText(s2))
	}
	if replayed := s2.offset.Load() - s2.ckptOffset.Load(); replayed > checkpointBytes+64<<10 {
		t.Errorf("replayed %d bytes after the checkpoint", replayed)
	}
}

func TestRestoreReleasesExited(t *testing.T) {
	m, l := newTestManager(t)
	s, err := m.Create(makeCfg("sleep 0.3"))
	if err != nil {
		t.Fatal(err)
	}
	m.Detach()
	time.Sleep(time.Second) // the child exits while no Manager is attached
	m2 := NewManager(m.dir, l)
	t.Cleanup(func() { m2.Close() })
	if n, _ := m2.Restore(context.Background()); n != 0 {
		t.Errorf("restored %d sessions, want 0", n)
	}
	if m2.Get(s.ID()) != nil {
		t.Error("exited session was restored")
	}
}
