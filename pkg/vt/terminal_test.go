package vt

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// newTerm returns a new terminal, closed when the test ends.
func newTerm(tb testing.TB, rows, cols int) Terminal {
	tb.Helper()
	t, err := New(rows, cols)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { t.Close() })
	return t
}

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
	term := newTerm(t, 2, 10)
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
	term := newTerm(t, 3, 5)
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
	term := newTerm(t, 5, 10)
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
	term := newTerm(t, 1, 8)
	// Fill the row, then erase from column 4 to end.
	writeString(t, term, "ABCDEFGH")
	writeString(t, term, "\x1b[1;5H\x1b[K") // CUP to (1,5), then EL
	s := term.Snapshot()
	if got := rowText(s, 0); got != "ABCD    " {
		t.Errorf("row 0 = %q, want %q", got, "ABCD    ")
	}
}

func TestEraseInDisplay(t *testing.T) {
	term := newTerm(t, 2, 4)
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
	term := newTerm(t, 1, 10)
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
	term := newTerm(t, 1, 5)
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
	term := newTerm(t, 1, 5)
	before := term.Snapshot().BellSeq
	writeString(t, term, "\x07")
	if got := term.Snapshot().BellSeq; got <= before {
		t.Errorf("bell seq = %d, want > %d", got, before)
	}
}

func TestAltScreenMode(t *testing.T) {
	term := newTerm(t, 2, 4)
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
	term := newTerm(t, 1, 4)
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
	term := newTerm(t, 1, 4)
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
	term := newTerm(t, 3, 5)
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
	term := newTerm(t, 2, 3)
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
	term := newTerm(t, 2, 4)
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
	term := newTerm(t, 3, 10)
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

func TestQueriesAreAnswered(t *testing.T) {
	term := newTerm(t, 5, 20)
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Before replies were drained, the first query blocked Write
		// forever.
		term.Write([]byte("ab\x1b[c\x1b[6n\x1b]11;?\x07"))
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Write blocked on a terminal query")
	}
	r := string(term.Replies())
	for _, want := range []string{
		"\x1b[?",    // device attributes
		"\x1b[1;3R", // cursor at row 1, column 3
		"\x1b]11;",  // background color
	} {
		if !strings.Contains(r, want) {
			t.Errorf("replies %q lack %q", r, want)
		}
	}
	if r := term.Replies(); len(r) != 0 {
		t.Errorf("Replies not cleared: %q", r)
	}
	term.Write([]byte("no queries here"))
	if r := term.Replies(); len(r) != 0 {
		t.Errorf("replies without queries: %q", r)
	}
}

func TestWriteAfterClose(t *testing.T) {
	term, err := New(2, 10)
	if err != nil {
		t.Fatal(err)
	}
	term.Write([]byte("final"))
	term.Close()
	if _, err := term.Write([]byte("hi")); err == nil {
		t.Error("Write after Close succeeded")
	}
	if got := rowText(term.Snapshot(), 0); !strings.HasPrefix(got, "final") {
		t.Errorf("screen after Close = %q, want the final screen", got)
	}
}

func TestMouseModes(t *testing.T) {
	term := newTerm(t, 2, 10)
	check := func(when string, want Modes) {
		t.Helper()
		m := term.Modes()
		if m.Mouse != want.Mouse || m.MouseSGR != want.MouseSGR || m.FocusEvents != want.FocusEvents {
			t.Errorf("%s: mouse %v sgr %v focus %v; want %v %v %v",
				when, m.Mouse, m.MouseSGR, m.FocusEvents, want.Mouse, want.MouseSGR, want.FocusEvents)
		}
	}
	term.Write([]byte("\x1b[?1002h\x1b[?1006h\x1b[?1004h"))
	// Before the encoding was separate, SGR replaced the tracking mode.
	check("after enabling", Modes{Mouse: MouseButton, MouseSGR: true, FocusEvents: true})
	term.Write([]byte("\x1b[?1006l"))
	check("after disabling SGR", Modes{Mouse: MouseButton, FocusEvents: true})
	term.Write([]byte("\x1b[?1000l")) // a different mode: no effect
	check("after disabling another mode", Modes{Mouse: MouseButton, FocusEvents: true})
	term.Write([]byte("\x1b[?1002l\x1b[?1004l"))
	check("after disabling all", Modes{})
}

func TestTitleWith9CByte(t *testing.T) {
	// "✳" is E2 9C B3; 0x9C is also the C1 String Terminator. This is the
	// title Claude Code sets.
	const title = "✳ Claude Code"
	for _, osc := range []string{
		"\x1b]0;" + title + "\x07",
		"\x1b]2;" + title + "\x1b\\",
	} {
		term := newTerm(t, 2, 20)
		// Split across writes, to exercise the cross-call tracking.
		mid := len(osc) / 2
		term.Write([]byte(osc[:mid]))
		term.Write([]byte(osc[mid:] + "ok"))
		s := term.Snapshot()
		if s.Title != title {
			t.Errorf("title = %q, want %q", s.Title, title)
		}
		if got := s.Cells[0][0].Grapheme + s.Cells[0][1].Grapheme + s.Cells[0][2].Grapheme; got != "ok " {
			t.Errorf("screen starts %q, want %q; the title leaked onto it", got, "ok ")
		}
		if s.BellSeq != 0 {
			t.Errorf("BellSeq = %d; the title's BEL rang", s.BellSeq)
		}
		term.Close()
	}
}

func TestHyperlinkWith9CByte(t *testing.T) {
	term := newTerm(t, 1, 10)
	const url = "https://example.com/✓" // ✓ is E2 9C 93
	term.Write([]byte("\x1b]8;;" + url + "\x1b\\L\x1b]8;;\x1b\\"))
	if got := term.Snapshot().Cells[0][0].Link; got != url {
		t.Errorf("link = %q, want %q", got, url)
	}
}

func TestSnapshotStringsAreValidUTF8(t *testing.T) {
	term := newTerm(t, 1, 10)
	term.Write([]byte("\x1b]2;bad\xe2title\x07a\xffb"))
	s := term.Snapshot()
	if !utf8.ValidString(s.Title) {
		t.Errorf("title %q is not valid UTF-8", s.Title)
	}
	for _, c := range s.Cells[0] {
		if !utf8.ValidString(c.Grapheme) {
			t.Errorf("grapheme %q is not valid UTF-8", c.Grapheme)
		}
	}
}
