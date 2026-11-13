package switcher

import (
	"strings"
	"testing"
	"time"

	"github.com/bradfitz/tsterm/pkg/vt"
)

func testItems() []Item {
	now := time.Now()
	return []Item{
		{ID: "pty-AAAAAAAAAA", Name: "alpha", What: "vim", Created: now.Add(-3 * time.Hour), Activity: now.Add(-time.Minute)},
		{ID: "pty-BBBBBBBBBB", Name: "bravo", Created: now.Add(-2 * time.Hour), Activity: now.Add(-10 * time.Minute)},
		{ID: "pty-CCCCCCCCCC", Name: "charlie", What: "claude", Created: now.Add(-time.Hour), Activity: now.Add(-30 * time.Second), Watchers: 2},
	}
}

// text returns the screen's rows as text.
func text(s vt.Screen) []string {
	var rows []string
	for _, row := range s.Cells {
		var b strings.Builder
		for _, c := range row {
			if c.Width == 0 {
				continue
			}
			b.WriteString(c.Grapheme)
		}
		rows = append(rows, b.String())
	}
	return rows
}

// waitFor waits for the menu's box to show want, and returns its text.
func waitFor(t *testing.T, m *Menu, want string) string {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		s := strings.Join(text(m.term.Snapshot()), "\n")
		if strings.Contains(s, want) {
			return s
		}
		select {
		case <-m.Changed():
		case <-time.After(50 * time.Millisecond):
		case <-deadline:
			t.Fatalf("menu never showed %q; it shows:\n%s", want, s)
		}
	}
}

func TestMenu(t *testing.T) {
	m, err := Open(testItems, "pty-CCCCCCCCCC", 30, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	s := waitFor(t, m, "charlie")
	t.Logf("menu:\n%s", s)
	if !strings.Contains(s, "alpha") || !strings.Contains(s, "bravo") || !strings.Contains(s, "Sessions") {
		t.Fatalf("menu lacks sessions:\n%s", s)
	}

	// The current session is charlie, so alpha, the most recently used
	// of the others, is selected; down selects bravo.
	m.Input([]byte("\x1b[B"))
	time.Sleep(100 * time.Millisecond)
	m.Input([]byte("\r"))
	select {
	case <-m.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("menu didn't close on enter")
	}
	if got := m.Choice(); got != "pty-BBBBBBBBBB" {
		t.Errorf("Choice = %q, want bravo's", got)
	}
}

func TestMenuSearchAndEscape(t *testing.T) {
	m, err := Open(testItems, "pty-AAAAAAAAAA", 30, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	waitFor(t, m, "charlie")
	m.Input([]byte("/char"))
	s := waitFor(t, m, "char")
	if strings.Contains(s, "bravo") {
		// Filtering lists only matches, once it's applied.
		time.Sleep(200 * time.Millisecond)
	}
	m.Input([]byte("\r")) // finish the search
	time.Sleep(100 * time.Millisecond)
	m.Input([]byte("\r")) // choose
	select {
	case <-m.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("menu didn't close")
	}
	if got := m.Choice(); got != "pty-CCCCCCCCCC" {
		t.Errorf("Choice after searching = %q, want charlie's", got)
	}

	m2, err := Open(testItems, "pty-AAAAAAAAAA", 30, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close()
	waitFor(t, m2, "charlie")
	m2.Input([]byte("q"))
	select {
	case <-m2.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("menu didn't close on q")
	}
	if got := m2.Choice(); got != "" {
		t.Errorf("Choice after q = %q, want none", got)
	}
}

func TestCompose(t *testing.T) {
	base := vt.Screen{Rows: 3, Cols: 6, Cells: [][]vt.Cell{
		cells("abcdef"), cells("ghijkl"), cells("mnopqr"),
	}, Title: "t", Modes: vt.Modes{Mouse: vt.MouseAny, AltScreen: true}}
	box := vt.Screen{Rows: 1, Cols: 2, Cells: [][]vt.Cell{cells("XY")}}
	out := compose(base, box, 4, 8)
	if got := text(out); strings.Join(got, "|") != "abcdef  |ghiXYl  |mnopqr  |        " {
		t.Errorf("composed = %q", got)
	}
	if out.Cells[0][0].Style.Attrs&vt.AttrFaint == 0 || out.Cells[1][3].Style.Attrs&vt.AttrFaint != 0 {
		t.Error("the session isn't dimmed, or the box is")
	}
	if out.Modes.Mouse != vt.MouseOff || !out.Modes.AltScreen || out.Title != "t" {
		t.Errorf("modes, title = %+v, %q", out.Modes, out.Title)
	}
}

func cells(s string) []vt.Cell {
	var row []vt.Cell
	for _, r := range s {
		row = append(row, vt.Cell{Grapheme: string(r), Width: 1})
	}
	return row
}
