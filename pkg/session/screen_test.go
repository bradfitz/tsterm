package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// savedText returns the text of the screen saved in dir.
func savedText(t *testing.T, dir string) string {
	t.Helper()
	ss, err := LoadScreen(dir)
	if err != nil {
		return ""
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

func waitSaved(t *testing.T, dir, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(savedText(t, dir), want) {
		if time.Now().After(deadline) {
			t.Fatalf("saved screen lacks %q:\n%s", want, savedText(t, dir))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestScreenSaved(t *testing.T) {
	old := screenSaveInterval
	screenSaveInterval = 50 * time.Millisecond
	t.Cleanup(func() { screenSaveInterval = old })

	m, _ := newTestManager(t)
	s, err := m.Create(makeCfg("echo first-screen; stty -echo; cat"))
	if err != nil {
		t.Fatal(err)
	}
	dir := m.sessionDir(s.ID())
	waitSaved(t, dir, "first-screen")

	// It's saved again when the screen changes, and only then.
	s.WriteInput([]byte("second-screen\n"))
	waitSaved(t, dir, "second-screen")
	time.Sleep(100 * time.Millisecond) // for a save already underway
	fi, err := os.Stat(filepath.Join(dir, screenFile))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * screenSaveInterval)
	if fi2, _ := os.Stat(filepath.Join(dir, screenFile)); !fi2.ModTime().Equal(fi.ModTime()) {
		t.Error("an unchanged screen was saved again")
	}
}

func TestScreenSavedAtExit(t *testing.T) {
	// With the usual interval, only the save at exit can catch it.
	m, _ := newTestManager(t)
	s, err := m.Create(makeCfg("sleep 0.2; echo last-words"))
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, s, 5*time.Second)
	if got := savedText(t, m.sessionDir(s.ID())); !strings.Contains(got, "last-words") {
		t.Errorf("screen saved at exit:\n%s", got)
	}
	if ss, _ := LoadScreen(m.sessionDir(s.ID())); time.Since(ss.Saved) > time.Minute {
		t.Errorf("saved at %v", ss.Saved)
	}
}
