package vt

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// texts returns the lines' texts.
func texts(lines []Line) []string {
	var s []string
	for _, l := range lines {
		s = append(s, l.Text)
	}
	return s
}

// takeTexts returns the texts of t's new history, failing if any was lost.
func takeTexts(tb testing.TB, t Terminal) []string {
	tb.Helper()
	lines, lost := t.TakeHistory()
	if lost {
		tb.Error("history lost")
	}
	return texts(lines)
}

func numbered(from, to int) []string {
	var s []string
	for i := from; i < to; i++ {
		s = append(s, fmt.Sprintf("line %d", i))
	}
	return s
}

func TestHistoryLines(t *testing.T) {
	g := newTerm(t, 3, 20)
	if got := takeTexts(t, g); len(got) != 0 {
		t.Errorf("history of a new terminal = %q", got)
	}
	for i := range 10 {
		fmt.Fprintf(g, "line %d\r\n", i)
	}
	// The screen holds lines 8 and 9 and the cursor's empty row.
	if got, want := takeTexts(t, g), numbered(0, 8); !slices.Equal(got, want) {
		t.Errorf("history = %q, want %q", got, want)
	}
	if got := takeTexts(t, g); len(got) != 0 {
		t.Errorf("history taken twice: %q", got)
	}
}

func TestHistoryStyles(t *testing.T) {
	g := newTerm(t, 2, 20)
	g.Write([]byte("\x1b[1;31mred\x1b[0m plain\r\n\r\n\r\n"))
	lines, _ := g.TakeHistory()
	if len(lines) == 0 {
		t.Fatal("no history")
	}
	l := lines[0]
	if l.Text != "red plain" {
		t.Fatalf("text = %q", l.Text)
	}
	if len(l.Runs) != 2 || l.Runs[0].Len != 3 || l.Runs[1].Len != 6 {
		t.Fatalf("runs = %+v, want 3 bytes, then 6", l.Runs)
	}
	if s := l.Runs[0].Style; s.Fg != (Color{Kind: ColorIndexed, Index: 1}) || s.Attrs&AttrBold == 0 {
		t.Errorf("style of \"red\" = %+v, want bold red", s)
	}
	if s := l.Runs[1].Style; s != (Style{}) {
		t.Errorf("style of \" plain\" = %+v, want default", s)
	}
}

func TestHistoryJoinsWrappedRows(t *testing.T) {
	g := newTerm(t, 2, 10)
	long := strings.Repeat("abcdefghij", 2) + "xyz" // three rows
	g.Write([]byte(long + "\r\nnext\r\n\r\n\r\n"))
	got := takeTexts(t, g)
	if len(got) < 2 || got[0] != long || got[1] != "next" {
		t.Errorf("history = %q, want %q then \"next\"", got, long)
	}
}

func TestHistoryLinks(t *testing.T) {
	g := newTerm(t, 2, 40)
	g.Write([]byte("see \x1b]8;;https://example.com/\x1b\\here\x1b]8;;\x1b\\ ok\r\n\r\n\r\n"))
	lines, _ := g.TakeHistory()
	if len(lines) == 0 {
		t.Fatal("no history")
	}
	l := lines[0]
	if l.Text != "see here ok" || len(l.Runs) != 3 || l.Runs[1].Link != "https://example.com/" || l.Runs[1].Len != 4 {
		t.Errorf("line = %+v", l)
	}
}

func TestHistoryWideCharacterAtWrap(t *testing.T) {
	g := newTerm(t, 2, 5)
	// 世 doesn't fit in the fifth column, so it wraps, leaving a spacer.
	g.Write([]byte("abcd世e\r\n\r\n\r\n"))
	lines, _ := g.TakeHistory()
	if len(lines) == 0 {
		t.Fatal("no history")
	}
	if l := lines[0]; l.Text != "abcd世e" {
		t.Errorf("text = %q", l.Text)
	}
}

func TestHistoryWaitsOutAltScreen(t *testing.T) {
	g := newTerm(t, 3, 20)
	g.Write([]byte("main 0\r\n"))
	g.Write([]byte("\x1b[?1049h"))
	for i := range 10 {
		fmt.Fprintf(g, "alt %d\r\n", i)
	}
	if got := takeTexts(t, g); len(got) != 0 {
		t.Errorf("history from the alternate screen = %q", got)
	}
	g.Write([]byte("\x1b[?1049lmain 1\r\nmain 2\r\nmain 3\r\n"))
	if got, want := takeTexts(t, g), []string{"main 0", "main 1"}; !slices.Equal(got, want) {
		t.Errorf("history = %q, want %q", got, want)
	}
}

func TestHistoryHugeWrite(t *testing.T) {
	g := newTerm(t, 5, 80)
	const n = 100_000
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, "line %d\r\n", i)
	}
	g.Write([]byte(b.String()))
	got := takeTexts(t, g)
	if want := numbered(0, n-4); !slices.Equal(got, want) {
		t.Errorf("got %d lines (first %q), want %d", len(got), got[:min(len(got), 1)], len(want))
	}
}

func TestHistoryClearedScrollback(t *testing.T) {
	g := newTerm(t, 2, 20)
	g.Write([]byte("a\r\nb\r\nc\r\n"))
	takeTexts(t, g)
	// Erasing the scrollback loses nothing already taken.
	g.Write([]byte("\x1b[3J"))
	if lines, lost := g.TakeHistory(); len(lines) != 0 || lost {
		t.Errorf("after ED 3: %q, lost %v", texts(lines), lost)
	}
	g.Write([]byte("d\r\ne\r\nf\r\n"))
	if got := takeTexts(t, g); !slices.Contains(got, "e") || slices.Contains(got, "a") {
		t.Errorf("history after ED 3 = %q", got)
	}
}

// TestHistoryAcrossCheckpoint checks that a terminal restored from a
// checkpoint returns the lines the original hadn't returned yet, and none it
// had.
func TestHistoryAcrossCheckpoint(t *testing.T) {
	g := newTerm(t, 3, 20)
	for i := range 10 {
		fmt.Fprintf(g, "line %d\r\n", i)
	}
	if got, want := takeTexts(t, g), numbered(0, 8); !slices.Equal(got, want) {
		t.Fatalf("history = %q, want %q", got, want)
	}
	for i := 10; i < 15; i++ {
		fmt.Fprintf(g, "line %d\r\n", i)
	}
	// Lines 8 through 12 have scrolled off, untaken.
	r := restore(t, checkpoint(t, g), 3, 20)
	for i := 15; i < 18; i++ {
		fmt.Fprintf(r, "line %d\r\n", i)
	}
	if got, want := takeTexts(t, r), numbered(8, 16); !slices.Equal(got, want) {
		t.Errorf("history after restore = %q, want %q", got, want)
	}
}

// TestHistoryAcrossCheckpointOnAltScreen is TestHistoryAcrossCheckpoint
// with the checkpoint taken while the alternate screen is up.
func TestHistoryAcrossCheckpointOnAltScreen(t *testing.T) {
	g := newTerm(t, 3, 20)
	for i := range 10 {
		fmt.Fprintf(g, "line %d\r\n", i)
	}
	takeTexts(t, g)
	g.Write([]byte("\x1b[?1049halt\r\n"))
	r := restore(t, checkpoint(t, g), 3, 20)
	r.Write([]byte("\x1b[?1049lline 10\r\nline 11\r\n"))
	if got, want := takeTexts(t, r), numbered(8, 10); !slices.Equal(got, want) {
		t.Errorf("history after restore = %q, want %q", got, want)
	}
}

func BenchmarkWriteWithHistory(b *testing.B) {
	term := newTerm(b, 60, 200)
	data := output(1 << 20)
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		term.Write(data)
		term.TakeHistory()
	}
}

// catOutput returns n bytes of what cat prints for a text file: lines of
// up to 79 characters.
func catOutput(n int) []byte {
	var b strings.Builder
	for i := 0; b.Len() < n; i++ {
		b.WriteString(strings.Repeat("the quick brown fox jumps over the lazy dog ", 2)[:20+i%60])
		b.WriteString("\r\n")
	}
	return []byte(b.String())
}

func BenchmarkWriteCat(b *testing.B) {
	term := newTerm(b, 50, 80)
	data := catOutput(1 << 20)
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		term.Write(data)
		term.TakeHistory()
	}
}

func TestHistoryErasedBackground(t *testing.T) {
	g := newTerm(t, 2, 10)
	g.Write([]byte("\x1b[44mbar\x1b[K\x1b[0m\r\n\r\n\r\n"))
	lines, _ := g.TakeHistory()
	if len(lines) == 0 {
		t.Fatal("no history")
	}
	l := lines[0]
	blue := Style{Bg: Color{Kind: ColorIndexed, Index: 4}}
	if l.Text != "bar       " || len(l.Runs) != 1 || l.Runs[0].Style != blue {
		t.Errorf("line = %+v, want 10 cells on blue", l)
	}
}

func TestParseLine(t *testing.T) {
	red := Color{Kind: ColorIndexed, Index: 1}
	tests := []struct {
		in   string
		want Line
	}{
		{"plain", Line{Text: "plain"}},
		{"trailing   ", Line{Text: "trailing"}},
		{"gap\x1b[3Cafter", Line{Text: "gap   after"}},
		{"\x1b[31mred\x1b[0m   ", Line{Text: "red", Runs: []Run{{Len: 3, Style: Style{Fg: red}}}}},
		{"\x1b[38;2;1;2;3mrgb", Line{Text: "rgb", Runs: []Run{{Len: 3, Style: Style{Fg: Color{Kind: ColorRGB, R: 1, G: 2, B: 3}}}}}},
		{"\x1b[38:2::1:2:3;4:3mx", Line{Text: "x", Runs: []Run{{Len: 1, Style: Style{Fg: Color{Kind: ColorRGB, R: 1, G: 2, B: 3}, Underline: UnderlineCurly}}}}},
		{"a\x1b[41m\x1b[2X\x1b[2C", Line{Text: "a  ", Runs: []Run{{Len: 1}, {Len: 2, Style: Style{Bg: red}}}}},
		{"\x1b]8;;http://x/\x1b\\l\x1b]8;;\x1b\\ m", Line{Text: "l m", Runs: []Run{{Len: 1, Link: "http://x/"}, {Len: 2}}}},
	}
	for _, tt := range tests {
		if got := parseLine([]byte(tt.in)); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("parseLine(%q) = %+v, want %+v", tt.in, got, tt.want)
		}
	}
}
