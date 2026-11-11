package ghosttyvt

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/bradfitz/tsterm/pkg/vt"
)

func newTerm(tb testing.TB, rows, cols int) *Terminal {
	tb.Helper()
	t, err := New(rows, cols)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { t.Close() })
	return t
}

func text(s vt.Screen) string {
	var sb strings.Builder
	for _, row := range s.Cells {
		for _, c := range row {
			if c.Width != 0 {
				sb.WriteString(c.Grapheme)
			}
		}
		sb.WriteString("|\n")
	}
	return sb.String()
}

// TestMatchesCharm feeds the same output to this terminal and to the
// current emulator (charmbracelet/x/vt) and compares their screens.
func TestMatchesCharm(t *testing.T) {
	tests := []struct {
		name, out string
	}{
		{"text", "hello\r\nworld"},
		{"styles", "\x1b[1;31mbold red\x1b[0m \x1b[3;4:3;38;2;10;20;30;48;5;200mfancy\x1b[0m \x1b[7;9;2mrev\x1b[0m"},
		{"wide", "a世界b\r\n🙂x"},
		{"link", "see \x1b]8;;https://example.com/\x1b\\here\x1b]8;;\x1b\\ ok"},
		{"title", "\x1b]2;my title\x07$ "},
		{"title with 0x9C", "\x1b]0;✳ Claude Code\x07ok"},
		{"modes", "\x1b[?1h\x1b[?2004h\x1b[?1000h\x1b[?1006h\x1b[?1004hmodes"},
		{"cursor", "\x1b[5 q\x1b[?25l\x1b[3;7Hcur"},
		{"erase", "abcdef\x1b[3D\x1b[K\r\nxyz\x1b[1;1H\x1b[2J"},
		{"scroll", strings.Repeat("line\r\n", 10)},
		{"alt screen", "main text\x1b[?1049h\x1b[2;2Halt text"},
		{"bell", "a\x07b\x07"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := newTerm(t, 4, 12)
			c := vt.New(4, 12)
			defer c.Close()
			g.Write([]byte(tt.out))
			c.Write([]byte(tt.out))
			gs, cs := g.Snapshot(), c.Snapshot()
			if !gs.Equal(cs) {
				t.Errorf("screens differ: %s", screenDiff(gs, cs))
			}
		})
	}
}

// screenDiff describes the first differences between a (ghostty) and b
// (charm).
func screenDiff(a, b vt.Screen) string {
	var d []string
	if a.Rows != b.Rows || a.Cols != b.Cols {
		d = append(d, fmt.Sprintf("size %dx%d vs %dx%d", a.Cols, a.Rows, b.Cols, b.Rows))
	}
	if a.Cursor != b.Cursor {
		d = append(d, fmt.Sprintf("cursor %+v vs %+v", a.Cursor, b.Cursor))
	}
	if a.Modes != b.Modes {
		d = append(d, fmt.Sprintf("modes %+v vs %+v", a.Modes, b.Modes))
	}
	if a.Title != b.Title {
		d = append(d, fmt.Sprintf("title %q vs %q", a.Title, b.Title))
	}
	if a.BellSeq != b.BellSeq {
		d = append(d, fmt.Sprintf("bell %d vs %d", a.BellSeq, b.BellSeq))
	}
	n := 0
	for y := range min(len(a.Cells), len(b.Cells)) {
		for x := range min(len(a.Cells[y]), len(b.Cells[y])) {
			if a.Cells[y][x] != b.Cells[y][x] && n < 3 {
				d = append(d, fmt.Sprintf("cell %d,%d %+v vs %+v", y, x, a.Cells[y][x], b.Cells[y][x]))
				n++
			}
		}
	}
	return strings.Join(d, "; ")
}

func TestReplies(t *testing.T) {
	g := newTerm(t, 5, 20)
	g.Write([]byte("ab\x1b[c\x1b[6n\x1b]11;?\x07"))
	r := string(g.Replies())
	for _, want := range []string{"\x1b[?62;22c", "\x1b[1;3R", "\x1b]11;"} {
		if !strings.Contains(r, want) {
			t.Errorf("replies %q lack %q", r, want)
		}
	}
	if r := g.Replies(); len(r) != 0 {
		t.Errorf("Replies not cleared: %q", r)
	}
}

// TestCheckpointFidelity checks that restoring a checkpoint and then
// continuing yields the same screen as never stopping, including the state
// the charm emulator cannot checkpoint.
func TestCheckpointFidelity(t *testing.T) {
	tests := []struct {
		name, before, after string
	}{
		// The main screen must survive while the alternate one is up.
		{"alt screen", "main screen text\x1b[?1049h\x1b[Halt", "\x1b[?1049l"},
		// The scroll region must survive: these lines scroll only rows 2-3.
		{"scroll region", "top\r\n\x1b[2;3r\x1b[2;1H", "a\r\nb\r\nc\r\nd"},
		// The saved cursor must survive.
		{"saved cursor", "\x1b[3;5H\x1b7\x1b[1;1Hx", "\x1b8saved"},
		// The pen: text after the checkpoint must stay red.
		{"pen", "\x1b[31mred ", "still red"},
		// Mid-sequence: the checkpoint splits an SGR sequence.
		{"unfinished sequence", "plain \x1b[3", "1mred"},
		// Pending wrap at the last column.
		{"pending wrap", strings.Repeat("x", 12), "y"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			whole := newTerm(t, 4, 12)
			whole.Write([]byte(tt.before + tt.after))

			first := newTerm(t, 4, 12)
			first.Write([]byte(tt.before))
			ckpt, err := first.Checkpoint()
			if err != nil {
				t.Fatal(err)
			}
			restored, err := Restore(ckpt)
			if err != nil {
				t.Fatal(err)
			}
			defer restored.Close()
			restored.Write([]byte(tt.after))

			if w, r := whole.Snapshot(), restored.Snapshot(); !w.Equal(r) {
				t.Errorf("restored and continued differs from uninterrupted\nuninterrupted:\n%s%+v\nrestored:\n%s%+v",
					text(w), w.Cursor, text(r), r.Cursor)
			}
		})
	}
}

func TestCheckpointKeepsScrollback(t *testing.T) {
	g := newTerm(t, 3, 20)
	for i := range 500 {
		fmt.Fprintf(g, "line %d\r\n", i)
	}
	ckpt, err := g.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("checkpoint of 500 lines of scrollback: %d bytes", len(ckpt))
	r, err := Restore(ckpt)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if !r.Snapshot().Equal(g.Snapshot()) {
		t.Error("restored screen differs")
	}
}

func TestRestoreGarbage(t *testing.T) {
	if _, err := Restore([]byte("not a checkpoint")); err == nil {
		t.Error("Restore of garbage succeeded")
	}
}

func TestTerminalsAreIsolated(t *testing.T) {
	a, b := newTerm(t, 2, 10), newTerm(t, 2, 10)
	a.Write([]byte("AAA"))
	b.Write([]byte("BBB"))
	if !strings.HasPrefix(text(a.Snapshot()), "AAA") || !strings.HasPrefix(text(b.Snapshot()), "BBB") {
		t.Errorf("a=%q b=%q", text(a.Snapshot()), text(b.Snapshot()))
	}
	a.Write([]byte("\x1b[6n"))
	if len(b.Replies()) != 0 || len(a.Replies()) == 0 {
		t.Error("replies went to the wrong terminal")
	}
}

// output returns n bytes of plausible terminal output: colored text,
// cursor motion, and line ends.
func output(n int) []byte {
	r := rand.New(rand.NewPCG(1, 2))
	var b bytes.Buffer
	for b.Len() < n {
		switch r.IntN(6) {
		case 0:
			fmt.Fprintf(&b, "\x1b[%d;%dm", 30+r.IntN(8), 40+r.IntN(8))
		case 1:
			b.WriteString("\r\n")
		case 2:
			fmt.Fprintf(&b, "\x1b[%d;%dH", 1+r.IntN(60), 1+r.IntN(200))
		default:
			b.WriteString("the quick brown fox jumps over the lazy dog ")
		}
	}
	return b.Bytes()
}

type terminal interface {
	Write([]byte) (int, error)
	Snapshot() vt.Screen
}

func benchWrite(b *testing.B, term terminal) {
	data := output(1 << 20)
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		term.Write(data)
	}
}

func benchSnapshot(b *testing.B, term terminal) {
	term.Write(output(1 << 20))
	for b.Loop() {
		term.Snapshot()
	}
}

func BenchmarkWrite(b *testing.B) {
	b.Run("ghostty", func(b *testing.B) { benchWrite(b, newTerm(b, 60, 200)) })
	b.Run("charm", func(b *testing.B) { benchWrite(b, vt.New(60, 200)) })
}

func BenchmarkSnapshot200x60(b *testing.B) {
	b.Run("ghostty", func(b *testing.B) { benchSnapshot(b, newTerm(b, 60, 200)) })
	b.Run("charm", func(b *testing.B) { benchSnapshot(b, vt.New(60, 200)) })
}

func BenchmarkNew(b *testing.B) {
	newTerm(b, 24, 80) // compile the module outside the loop
	for b.Loop() {
		t, err := New(24, 80)
		if err != nil {
			b.Fatal(err)
		}
		t.Close()
	}
}
