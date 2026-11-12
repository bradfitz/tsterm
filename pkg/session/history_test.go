package session

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// checkHistory checks that s's history is lines line-0 through
// line-(n-1), as historyScript prints them, and returns how many it has.
func checkHistory(t *testing.T, s *Session, n int) {
	t.Helper()
	h := s.History()
	if h == nil {
		t.Fatal("no history")
	}
	deadline := time.Now().Add(5 * time.Second)
	for h.Next() < uint64(n) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	first, lines, err := h.Read(0, n+100)
	if err != nil {
		t.Fatal(err)
	}
	if first != 0 || len(lines) != n {
		t.Fatalf("history has %d lines from %d, want %d from 0", len(lines), first, n)
	}
	for i, l := range lines {
		if want := fmt.Sprintf("line-%d-%s", i, pad); l.Text != want {
			t.Fatalf("line %d = %q, want %q", i, l.Text, want)
		}
	}
}

const pad = "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"

// historyScript prints lines line-from through line-(to-1).
func historyScript(from, to int) string {
	return fmt.Sprintf("i=%d; while [ $i -lt %d ]; do echo line-$i-%s; i=$((i+1)); done", from, to, pad)
}

func TestSessionHistory(t *testing.T) {
	m, _ := newTestManager(t)
	s, err := m.Create(makeCfg(historyScript(0, 1000) + "; echo END; sleep 30"))
	if err != nil {
		t.Fatal(err)
	}
	waitScreen(t, s, "END")
	// The screen holds the last 22 lines, END, and the cursor's row.
	checkHistory(t, s, 1000-22)
	if got := s.Snapshot().History; got != 1000-22 {
		t.Errorf("Snapshot().History = %d, want %d", got, 1000-22)
	}
}

// TestHistoryAcrossRestart checks that a restarted tstermd, which replays
// the output since the last checkpoint, keeps the history whole: no line
// missing or repeated.
func TestHistoryAcrossRestart(t *testing.T) {
	m, l := newTestManager(t)
	// Far more output than the checkpoint threshold, so that there's a
	// checkpoint partway, and output after it to replay.
	s, err := m.Create(makeCfg(historyScript(0, 6000) + "; echo END; stty -echo; cat"))
	if err != nil {
		t.Fatal(err)
	}
	waitScreen(t, s, "END")
	if s.ckptOffset.Load() == 0 || s.ckptOffset.Load() == s.offset.Load() {
		t.Fatalf("checkpoint at %d of %d bytes; want one partway", s.ckptOffset.Load(), s.offset.Load())
	}
	checkHistory(t, s, 6000-22)
	m.Detach()

	m2 := NewManager(m.dir, l)
	m2.SetStateDir(m.stateDir)
	t.Cleanup(func() { m2.Close() })
	if n, err := m2.Restore(context.Background()); err != nil || n != 1 {
		t.Fatalf("Restore = %d, %v", n, err)
	}
	s2 := m2.Get(s.ID())
	waitScreen(t, s2, "END")
	checkHistory(t, s2, 6000-22)

	// And it continues.
	for i := range 30 {
		fmt.Fprintf(writer{s2}, "line-%d-%s\n", 6000+i, pad)
	}
	waitScreen(t, s2, fmt.Sprintf("line-6029-%s", pad))
	// cat echoes each line, so the history now also has END and the
	// lines that were on the screen.
	if h := s2.History(); h.Next() < 6000 {
		t.Errorf("history has %d lines after more output", h.Next())
	}
}

// writer types into a session.
type writer struct{ s *Session }

func (w writer) Write(b []byte) (int, error) { return w.s.WriteInput(b) }
