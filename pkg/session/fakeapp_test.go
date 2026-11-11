//go:build unix

package session

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bradfitz/tsterm/internal/fakeapp"
	"github.com/bradfitz/tsterm/pkg/pty"
)

func TestMain(m *testing.M) {
	// In a session's PTY, this binary is the fake full-screen program.
	fakeapp.MainIfRequested()
	os.Exit(m.Run())
}

func fakeappCfg() Config {
	cmd, env := fakeapp.Command()
	return Config{PTY: pty.Config{Command: cmd, Env: env}, Rows: 10, Cols: 40}
}

// rowOf returns the text of row y of s's screen.
func rowOf(s *Session, y int) string {
	rows := strings.Split(screenText(s), "\n")
	if y >= len(rows) {
		return ""
	}
	return rows[y]
}

func TestFakeappQueriesAnswered(t *testing.T) {
	m, _ := newTestManager(t)
	s, err := m.Create(fakeappCfg())
	if err != nil {
		t.Fatal(err)
	}
	waitScreen(t, s, fakeapp.ReadyText)
	if !strings.Contains(screenText(s), fakeapp.RepliesText) {
		t.Errorf("the program's queries went unanswered:\n%s", screenText(s))
	}
	snap := s.Snapshot()
	if snap.Title != fakeapp.Title || !snap.Modes.AltScreen || !snap.Modes.BracketedPaste || !snap.Modes.MouseSGR {
		t.Errorf("title %q, modes %+v", snap.Title, snap.Modes)
	}
}

// TestFullScreenAppAcrossRestart checks that a full-screen program comes
// through a tstermd restart intact: its scroll region still works, and when
// it exits, the main screen behind it is back.
func TestFullScreenAppAcrossRestart(t *testing.T) {
	// Checkpoint after every output chunk, so the restore below comes from
	// a checkpoint rather than from replaying all the output.
	defer func(old uint64) { checkpointBytes = old }(checkpointBytes)
	checkpointBytes = 1

	m, l := newTestManager(t)
	s, err := m.Create(fakeappCfg())
	if err != nil {
		t.Fatal(err)
	}
	waitScreen(t, s, fakeapp.ReadyText)
	s.WriteInput([]byte("s"))
	waitScreen(t, s, "scroll 1")

	// tstermd shuts down, leaving the program running, and a new one starts.
	m.Detach()
	m2 := NewManager(m.dir, l)
	t.Cleanup(func() { m2.Close() })
	if n, err := m2.Restore(context.Background()); err != nil || n != 1 {
		t.Fatalf("Restore = %d, %v", n, err)
	}
	s2 := m2.Get(s.ID())
	waitScreen(t, s2, "scroll 1")
	if s2.ckptOffset.Load() == 0 {
		t.Fatal("restored without a checkpoint")
	}

	// Lines scroll only within the scroll region, leaving the rows above
	// it in place.
	s2.WriteInput([]byte("sss"))
	waitScreen(t, s2, "scroll 4")
	if got := rowOf(s2, 0); !strings.HasPrefix(got, fakeapp.AltScreenText) {
		t.Errorf("row 0 = %q; the scroll region was lost", got)
	}
	if got := rowOf(s2, 1); !strings.HasPrefix(got, fakeapp.ReadyText) {
		t.Errorf("row 1 = %q; the scroll region was lost", got)
	}
	// The newest line is at the bottom of the region, with nothing below
	// it. Without the region, lines would land below it instead.
	if got := rowOf(s2, fakeapp.ScrollBottom-1); !strings.HasPrefix(got, "scroll 4") {
		t.Errorf("bottom of the scroll region = %q, want scroll 4; the scroll region was lost\n%s", got, screenText(s2))
	}
	if got := strings.TrimSpace(rowOf(s2, fakeapp.ScrollBottom)); got != "" {
		t.Errorf("row below the scroll region = %q; the scroll region was lost", got)
	}

	// Quitting returns to the main screen, which must have survived.
	s2.WriteInput([]byte("q"))
	waitScreen(t, s2, fakeapp.ByeText)
	if got := screenText(s2); !strings.Contains(got, fakeapp.MainScreenText) {
		t.Errorf("the main screen was lost:\n%s", got)
	}
}

func TestFakeappNotifications(t *testing.T) {
	m, _ := newTestManager(t)
	s, err := m.Create(fakeappCfg())
	if err != nil {
		t.Fatal(err)
	}
	waitScreen(t, s, fakeapp.ReadyText)
	before := s.Snapshot().Notification
	s.WriteInput([]byte("n"))
	waitFor(t, func() bool { return s.Snapshot().Notification.Body == fakeapp.Notify9Body })
	if n := s.Snapshot().Notification; n.Seq <= before.Seq {
		t.Errorf("notification seq %d not above %d", n.Seq, before.Seq)
	}
}

// waitFor waits up to 5s for cond.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
