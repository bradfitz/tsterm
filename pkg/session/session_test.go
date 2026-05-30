//go:build unix

package session

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bradfitz/tsterm/pkg/pty"
	"github.com/bradfitz/tsterm/pkg/shim"
	"github.com/bradfitz/tsterm/pkg/vt"
)

// newTestManager returns a Manager whose shims run in-process, in a fresh
// short directory (unix socket paths have a length limit).
func newTestManager(t *testing.T) (*Manager, *shim.InProcess) {
	t.Helper()
	dir, err := os.MkdirTemp("", "sess")
	if err != nil {
		t.Fatal(err)
	}
	l := &shim.InProcess{}
	m := NewManager(dir, l)
	t.Cleanup(func() {
		m.Close()
		l.Close()
		os.RemoveAll(dir)
	})
	return m, l
}

func startEcho(t *testing.T, script string) *Session {
	t.Helper()
	m, _ := newTestManager(t)
	s, err := m.Create(Config{
		PTY: pty.Config{
			Command: "/bin/sh",
			Args:    []string{"-c", script},
		},
		Rows: 24,
		Cols: 80,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return s
}

// waitDone waits for s.Done to close, or fails after d.
func waitDone(t *testing.T, s *Session, d time.Duration) {
	t.Helper()
	select {
	case <-s.Done():
	case <-time.After(d):
		t.Fatalf("session did not finish within %v", d)
	}
}

func screenText(s *Session) string {
	snap := s.Snapshot()
	var b strings.Builder
	for y := range snap.Rows {
		for _, c := range snap.Cells[y] {
			if c.Width == 0 {
				continue
			}
			if c.Grapheme == "" {
				b.WriteString(" ")
			} else {
				b.WriteString(c.Grapheme)
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

func TestSessionEcho(t *testing.T) {
	s := startEcho(t, "echo hello")
	defer s.Close()

	waitDone(t, s, 3*time.Second)
	if got := screenText(s); !strings.Contains(got, "hello") {
		t.Errorf("session screen text = %q, want to contain %q", got, "hello")
	}
	if err := s.Wait(); err != nil {
		t.Errorf("Wait: %v", err)
	}
}

func TestWatcherBootstrap(t *testing.T) {
	s := startEcho(t, "echo bootstrap; sleep 0.1")
	defer s.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := s.Watch(ctx)

	// First tick is delivered immediately by Watch so the caller can compute
	// a bootstrap delta without polling.
	select {
	case <-w.Ticks():
	case <-time.After(2 * time.Second):
		t.Fatalf("bootstrap tick not delivered")
	}
	d := w.Diff()
	if !d.ModesChanged {
		t.Errorf("bootstrap diff should set ModesChanged")
	}
	if d.Rows == 0 || d.Cols == 0 {
		t.Errorf("bootstrap diff size = %dx%d, want non-zero", d.Rows, d.Cols)
	}
	if len(d.Regions) == 0 {
		t.Errorf("bootstrap diff should describe every row, got 0 regions")
	}
}

func TestWatcherSeesOutput(t *testing.T) {
	s := startEcho(t, "echo watched")
	defer s.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := s.Watch(ctx)

	// The output may arrive before or after the bootstrap diff, so look
	// for it in both.
	<-w.Ticks()
	got := screenTextFromDiff(w.Diff())

	// Wait for the session to end and consume any final ticks.
	waitDone(t, s, 3*time.Second)
	drainTicks(w, 100*time.Millisecond)
	got += screenTextFromDiff(w.Diff())
	if !strings.Contains(got, "watched") {
		t.Errorf("watcher diff text = %q, want to contain %q", got, "watched")
	}
}

func TestMultipleWatchersConverge(t *testing.T) {
	s := startEcho(t, "echo a; echo b; echo c")
	defer s.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w1 := s.Watch(ctx)
	w2 := s.Watch(ctx)

	waitDone(t, s, 3*time.Second)
	drainTicks(w1, 100*time.Millisecond)
	drainTicks(w2, 100*time.Millisecond)

	// Each watcher reports a single bootstrap diff that contains the full
	// final screen, because we never called Diff before now.
	d1 := w1.Diff()
	d2 := w2.Diff()
	t1 := screenTextFromDiff(d1)
	t2 := screenTextFromDiff(d2)
	if t1 != t2 {
		t.Errorf("watchers disagree:\n w1=%q\n w2=%q", t1, t2)
	}
	for _, want := range []string{"a", "b", "c"} {
		if !strings.Contains(t1, want) {
			t.Errorf("text missing %q: %q", want, t1)
		}
	}
}

func TestWatcherCancellationClosesChannel(t *testing.T) {
	s := startEcho(t, "sleep 0.5")
	defer s.Close()

	ctx, cancel := context.WithCancel(context.Background())
	w := s.Watch(ctx)
	cancel()

	select {
	case <-w.Done():
	case <-time.After(2 * time.Second):
		t.Fatalf("watcher Done not closed after cancel")
	}
	// Ticks channel should also be closed.
	select {
	case _, ok := <-w.Ticks():
		if ok {
			// still might receive a residual tick; loop until closed
			for range w.Ticks() {
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Ticks channel not closed after cancel")
	}
}

func TestSessionResize(t *testing.T) {
	m, _ := newTestManager(t)
	s, err := m.Create(Config{
		PTY:  pty.Config{Command: "/bin/cat"},
		Rows: 10,
		Cols: 20,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer s.Close()
	if r, c := s.Size(); r != 10 || c != 20 {
		t.Errorf("initial size = %dx%d, want 10x20", r, c)
	}
	if err := s.Resize(40, 132); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if r, c := s.Size(); r != 40 || c != 132 {
		t.Errorf("size after resize = %dx%d, want 40x132", r, c)
	}
}

func TestWriteInputRoundtrip(t *testing.T) {
	m, _ := newTestManager(t)
	s, err := m.Create(Config{PTY: pty.Config{Command: "/bin/cat"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer s.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := s.Watch(ctx)
	<-w.Ticks() // bootstrap

	if _, err := s.WriteInput([]byte("hi\n")); err != nil {
		t.Fatalf("WriteInput: %v", err)
	}
	// Poll until the watcher sees the echo.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-w.Ticks():
		case <-time.After(50 * time.Millisecond):
		}
		txt := screenText(s)
		if strings.Contains(txt, "hi") {
			return
		}
	}
	t.Fatalf("did not observe echoed input within deadline, got %q", screenText(s))
}

func drainTicks(w *Watcher, d time.Duration) {
	deadline := time.NewTimer(d)
	defer deadline.Stop()
	for {
		select {
		case _, ok := <-w.Ticks():
			if !ok {
				return
			}
		case <-deadline.C:
			return
		}
	}
}

// screenTextFromDiff reconstructs a plain-text rendering from a delta's
// regions. The tests pass it bootstrap deltas, which describe every row.
func screenTextFromDiff(d vt.ScreenDelta) string {
	var b strings.Builder
	for _, r := range d.Regions {
		for _, c := range r.Cells {
			if c.Width == 0 {
				continue
			}
			if c.Grapheme == "" {
				b.WriteString(" ")
			} else {
				b.WriteString(c.Grapheme)
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}
