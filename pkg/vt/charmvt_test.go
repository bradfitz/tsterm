package vt

import (
	"strings"
	"testing"
)

// writeString feeds s to t and fails the test if Write returns an error or a
// short count.
func writeString(tb testing.TB, t Terminal, s string) {
	tb.Helper()
	n, err := t.Write([]byte(s))
	if err != nil {
		tb.Fatalf("Write(%q): %v", s, err)
	}
	if n != len(s) {
		tb.Fatalf("Write(%q): short write %d/%d", s, n, len(s))
	}
}

// rowText returns the visible text of row y in screen s, joining grapheme
// clusters and substituting a single space for zero-width continuation cells.
func rowText(s Screen, y int) string {
	var b strings.Builder
	for _, c := range s.Cells[y] {
		if c.Width == 0 {
			continue
		}
		if c.Grapheme == "" {
			b.WriteString(" ")
			continue
		}
		b.WriteString(c.Grapheme)
	}
	return b.String()
}

func TestPlainASCII(t *testing.T) {
	term := New(2, 10)
	writeString(t, term, "hello")
	s := term.Snapshot()
	if got := rowText(s, 0); got != "hello     " {
		t.Errorf("row 0 = %q, want %q", got, "hello     ")
	}
	if s.Cursor.Row != 0 || s.Cursor.Col != 5 {
		t.Errorf("cursor = (%d,%d), want (0,5)", s.Cursor.Row, s.Cursor.Col)
	}
}

func TestCRLFAdvancesRow(t *testing.T) {
	term := New(3, 5)
	writeString(t, term, "ab\r\ncd")
	s := term.Snapshot()
	if got := rowText(s, 0); got != "ab   " {
		t.Errorf("row 0 = %q", got)
	}
	if got := rowText(s, 1); got != "cd   " {
		t.Errorf("row 1 = %q", got)
	}
	if s.Cursor.Row != 1 || s.Cursor.Col != 2 {
		t.Errorf("cursor = (%d,%d), want (1,2)", s.Cursor.Row, s.Cursor.Col)
	}
}

func TestCSICursorMoves(t *testing.T) {
	term := New(5, 10)
	// CUP to row 3, col 5, then write "X".
	writeString(t, term, "\x1b[3;5HX")
	s := term.Snapshot()
	if s.Cursor.Row != 2 || s.Cursor.Col != 5 {
		t.Errorf("cursor after CUP+X = (%d,%d), want (2,5)", s.Cursor.Row, s.Cursor.Col)
	}
	if s.Cells[2][4].Grapheme != "X" {
		t.Errorf("cell (2,4) = %q, want %q", s.Cells[2][4].Grapheme, "X")
	}
}

func TestEraseInLine(t *testing.T) {
	term := New(1, 8)
	// Fill the row, then erase from column 4 to end.
	writeString(t, term, "ABCDEFGH")
	writeString(t, term, "\x1b[1;5H\x1b[K") // CUP to (1,5), then EL
	s := term.Snapshot()
	if got := rowText(s, 0); got != "ABCD    " {
		t.Errorf("row 0 = %q, want %q", got, "ABCD    ")
	}
}

func TestEraseInDisplay(t *testing.T) {
	term := New(2, 4)
	writeString(t, term, "abcd\r\nefgh")
	writeString(t, term, "\x1b[H\x1b[2J") // CUP home, ED 2 (whole screen)
	s := term.Snapshot()
	for y := range s.Rows {
		if got := rowText(s, y); strings.TrimSpace(got) != "" {
			t.Errorf("row %d after ED 2 = %q, want blank", y, got)
		}
	}
}

func TestSGRAttributesAndColors(t *testing.T) {
	term := New(1, 10)
	// Bold + reverse + red indexed + truecolor on background.
	writeString(t, term, "\x1b[1;7;31;48;2;10;20;30mA\x1b[0mB")
	s := term.Snapshot()
	a := s.Cells[0][0]
	if a.Style.Attrs&AttrBold == 0 || a.Style.Attrs&AttrReverse == 0 {
		t.Errorf("A attrs = %b, want bold+reverse set", a.Style.Attrs)
	}
	if a.Style.Fg.Kind != ColorIndexed || a.Style.Fg.Index != 1 {
		t.Errorf("A fg = %+v, want indexed 1 (red)", a.Style.Fg)
	}
	if a.Style.Bg.Kind != ColorRGB || a.Style.Bg.R != 10 || a.Style.Bg.G != 20 || a.Style.Bg.B != 30 {
		t.Errorf("A bg = %+v, want rgb(10,20,30)", a.Style.Bg)
	}
	b := s.Cells[0][1]
	if b.Style.Attrs != 0 || b.Style.Fg.Kind != ColorDefault || b.Style.Bg.Kind != ColorDefault {
		t.Errorf("B should be plain after SGR reset, got %+v", b.Style)
	}
}

func TestOSCTitle(t *testing.T) {
	term := New(1, 5)
	writeString(t, term, "\x1b]2;hello world\x07")
	s := term.Snapshot()
	if s.Title != "hello world" {
		t.Errorf("title = %q, want %q", s.Title, "hello world")
	}
	d := term.Diff()
	if !d.TitleSet || d.Title != "hello world" {
		t.Errorf("diff title: set=%v title=%q", d.TitleSet, d.Title)
	}
	// A second Diff with no further title change should not flag TitleSet.
	d2 := term.Diff()
	if d2.TitleSet {
		t.Errorf("second diff should not flag TitleSet")
	}
}

func TestBellIncrements(t *testing.T) {
	term := New(1, 5)
	before := term.Snapshot().BellSeq
	writeString(t, term, "\x07")
	if got := term.Snapshot().BellSeq; got <= before {
		t.Errorf("bell seq = %d, want > %d", got, before)
	}
}

func TestAltScreenMode(t *testing.T) {
	term := New(2, 4)
	writeString(t, term, "\x1b[?1049h")
	if !term.Modes().AltScreen {
		t.Errorf("Modes().AltScreen = false after DECSET 1049")
	}
	if !term.Snapshot().Modes.AltScreen {
		t.Errorf("Snapshot().Modes.AltScreen = false after DECSET 1049")
	}
	writeString(t, term, "\x1b[?1049l")
	if term.Modes().AltScreen {
		t.Errorf("Modes().AltScreen = true after DECRST 1049")
	}
}

func TestBracketedPasteMode(t *testing.T) {
	term := New(1, 4)
	writeString(t, term, "\x1b[?2004h")
	if !term.Modes().BracketedPaste {
		t.Errorf("BracketedPaste = false after enable")
	}
	writeString(t, term, "\x1b[?2004l")
	if term.Modes().BracketedPaste {
		t.Errorf("BracketedPaste = true after disable")
	}
}

func TestWideCharacter(t *testing.T) {
	term := New(1, 4)
	// CJK fullwidth character occupies two columns.
	writeString(t, term, "中A")
	s := term.Snapshot()
	if s.Cells[0][0].Width != 2 || s.Cells[0][0].Grapheme != "中" {
		t.Errorf("cell (0,0) = %+v, want width 2 '中'", s.Cells[0][0])
	}
	if s.Cells[0][1].Width != 0 {
		t.Errorf("cell (0,1) width = %d, want 0 (continuation)", s.Cells[0][1].Width)
	}
	if s.Cells[0][2].Grapheme != "A" {
		t.Errorf("cell (0,2) = %q, want %q", s.Cells[0][2].Grapheme, "A")
	}
}

func TestResizeReportsResized(t *testing.T) {
	term := New(3, 5)
	_ = term.Diff() // drain initial delta
	term.Resize(4, 8)
	d := term.Diff()
	if !d.Resized {
		t.Errorf("diff after resize: Resized = false")
	}
	if d.Rows != 4 || d.Cols != 8 {
		t.Errorf("diff size = %dx%d, want 4x8", d.Rows, d.Cols)
	}
	if r, c := term.Size(); r != 4 || c != 8 {
		t.Errorf("Size() = %dx%d, want 4x8", r, c)
	}
}

func TestDiffChurnQuiescence(t *testing.T) {
	term := New(2, 3)
	writeString(t, term, "ab\r\ncd")
	_ = term.Diff() // bootstrap delta
	d := term.Diff()
	if len(d.Regions) != 0 {
		t.Errorf("idle Diff returned %d regions, want 0", len(d.Regions))
	}
	if d.CursorMoved {
		t.Errorf("idle Diff reported CursorMoved")
	}
	if d.TitleSet {
		t.Errorf("idle Diff reported TitleSet")
	}
	if d.ModesChanged {
		t.Errorf("idle Diff reported ModesChanged")
	}
}

func TestFirstDiffBootstraps(t *testing.T) {
	term := New(2, 4)
	writeString(t, term, "hi")
	d := term.Diff()
	if len(d.Regions) != 2 {
		t.Errorf("first Diff returned %d rows, want 2", len(d.Regions))
	}
	if !d.ModesChanged {
		t.Errorf("first Diff should report ModesChanged so a new client picks up modes")
	}
}

func TestScreenEqual(t *testing.T) {
	term := New(3, 10)
	a := term.Snapshot()
	if !a.Equal(term.Snapshot()) {
		t.Fatal("identical snapshots not Equal")
	}
	if a.Equal(Screen{}) {
		t.Fatal("snapshot Equal to zero Screen")
	}
	term.Write([]byte("hi"))
	b := term.Snapshot()
	if a.Equal(b) {
		t.Fatal("snapshots differing in cells and cursor reported Equal")
	}
	term.Write([]byte("\x07"))
	if b.Equal(term.Snapshot()) {
		t.Fatal("snapshots differing only in BellSeq reported Equal")
	}
}
