//go:build unix

package session

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bradfitz/tsterm/internal/demoapp"
	"github.com/bradfitz/tsterm/pkg/pty"
)

func TestMain(m *testing.M) {
	// In a session's PTY, this binary is the fake full-screen program.
	demoapp.MainIfRequested()
	os.Exit(m.Run())
}

func demoCfg() Config {
	cmd, env := demoapp.Command()
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

func TestDemoappQueriesAnswered(t *testing.T) {
	m, _ := newTestManager(t)
	s, err := m.Create(demoCfg())
	if err != nil {
		t.Fatal(err)
	}
	waitScreen(t, s, demoapp.ReadyText)
	if !strings.Contains(screenText(s), demoapp.RepliesText) {
		t.Errorf("the program's queries went unanswered:\n%s", screenText(s))
	}
	snap := s.Snapshot()
	if snap.Title != demoapp.Title || !snap.Modes.AltScreen || !snap.Modes.BracketedPaste || !snap.Modes.MouseSGR {
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
	s, err := m.Create(demoCfg())
	if err != nil {
		t.Fatal(err)
	}
	waitScreen(t, s, demoapp.ReadyText)
	s.WriteInput([]byte("s"))
	waitScreen(t, s, "scroll 1")

	// tstermd shuts down, leaving the program running, and a new one starts.
	m.Detach()
	m2 := NewManager(m.dir, l)
	m2.SetStateDir(m.stateDir)
	t.Cleanup(func() { m2.Close() })
	if n, err := m2.Restore(context.Background()); err != nil || n != 1 {
		t.Fatalf("Restore = %d, %v", n, err)
	}
	s2 := m2.Get(s.ID())
	waitScreen(t, s2, "scroll 1")
	if s2.ckptOffset.Load() == 0 {
		t.Fatal("restored without a checkpoint")
	}

	// Lines scroll only within the log's scroll region, leaving the rows
	// above it in place. Without the region, a newline on the bottom row
	// would scroll the whole screen.
	s2.WriteInput([]byte("sss"))
	waitScreen(t, s2, "scroll 4")
	rows, _ := s2.Size()
	top, bottom := demoapp.LogRegion(rows)
	if got := rowOf(s2, bottom-1); !strings.HasPrefix(got, "scroll 4") {
		t.Errorf("bottom row = %q, want scroll 4\n%s", got, screenText(s2))
	}
	if got := rowOf(s2, top-2); !strings.HasPrefix(got, "── events") {
		t.Errorf("separator row = %q; the scroll region was lost\n%s", got, screenText(s2))
	}
	if got := rowOf(s2, 0); !strings.Contains(got, demoapp.AltScreenText) {
		t.Errorf("title row = %q; the scroll region was lost", got)
	}
	if got := rowOf(s2, 1); !strings.HasPrefix(got, demoapp.ReadyText) {
		t.Errorf("status row = %q; the scroll region was lost", got)
	}

	// Quitting returns to the main screen, which must have survived.
	s2.WriteInput([]byte("q"))
	waitScreen(t, s2, demoapp.ByeText)
	if got := screenText(s2); !strings.Contains(got, demoapp.MainScreenText) {
		t.Errorf("the main screen was lost:\n%s", got)
	}
}

func TestDemoappNotifications(t *testing.T) {
	m, _ := newTestManager(t)
	s, err := m.Create(demoCfg())
	if err != nil {
		t.Fatal(err)
	}
	waitScreen(t, s, demoapp.ReadyText)
	before := s.Snapshot().Notification
	s.WriteInput([]byte("n"))
	waitFor(t, func() bool { return s.Snapshot().Notification.Body == demoapp.Notify9Body })
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

func TestDemoSessions(t *testing.T) {
	m, _ := newTestManager(t)
	if _, err := m.Create(Config{Name: DemoName}); err != ErrNoDemo {
		t.Errorf("demo without a demo program: %v, want ErrNoDemo", err)
	}
	m.SetDemo(func() pty.Config { return demoCfg().PTY })
	a, err := m.Create(Config{Name: DemoName, Rows: 10, Cols: 40})
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.Create(Config{Name: DemoName, Rows: 10, Cols: 40})
	if err != nil {
		t.Fatal(err)
	}
	if a.Name() != DemoName || b.Name() != DemoName+"2" {
		t.Errorf("demo names %q, %q", a.Name(), b.Name())
	}
	waitScreen(t, a, demoapp.ReadyText)

	// Other reserved names can't be taken, by creating or renaming.
	if _, err := m.Create(Config{Name: ReservedPrefix + "_mine"}); err != ErrBadName {
		t.Errorf("creating a reserved name: %v, want ErrBadName", err)
	}
	s, err := m.Create(makeCfg("sleep 30"))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Rename(string(s.ID()), DemoName+"9"); err != ErrBadName {
		t.Errorf("renaming to a reserved name: %v, want ErrBadName", err)
	}
	// A demo session can be renamed to an ordinary name.
	if err := m.Rename(string(a.ID()), "mydemo"); err != nil {
		t.Errorf("renaming a demo: %v", err)
	}
}
