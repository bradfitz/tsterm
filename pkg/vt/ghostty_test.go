package vt

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
)

func screenText(s Screen) string {
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

func restore(tb testing.TB, ckpt []byte, rows, cols int) Terminal {
	tb.Helper()
	t, err := Restore(ckpt, rows, cols)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { t.Close() })
	return t
}

func checkpoint(tb testing.TB, t Terminal) []byte {
	tb.Helper()
	b, err := t.Checkpoint()
	if err != nil {
		tb.Fatal(err)
	}
	return b
}

// TestCheckpointFidelity checks that restoring a checkpoint and then
// continuing yields the same screen as never stopping, including state that
// a Repaint can't reproduce.
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
			restored := restore(t, checkpoint(t, first), 4, 12)
			restored.Write([]byte(tt.after))

			w, r := whole.Snapshot(), restored.Snapshot()
			w.BellSeq, r.BellSeq = 0, 0
			if !w.Equal(r) {
				t.Errorf("restored and continued differs from uninterrupted\nuninterrupted:\n%s%+v\nrestored:\n%s%+v",
					screenText(w), w.Cursor, screenText(r), r.Cursor)
			}
		})
	}
}

func TestCheckpointKeepsScrollback(t *testing.T) {
	g := newTerm(t, 3, 20)
	for i := range 500 {
		fmt.Fprintf(g, "line %d\r\n", i)
	}
	ckpt := checkpoint(t, g)
	t.Logf("checkpoint with 500 lines of scrollback: %d bytes", len(ckpt))
	if r := restore(t, ckpt, 3, 20); !r.Snapshot().Equal(g.Snapshot()) {
		t.Error("restored screen differs")
	}
}

// TestCheckpointFallback checks that a checkpoint whose native snapshot
// can't be read, as from an incompatible libghostty, restores the screen
// from its repaint.
func TestCheckpointFallback(t *testing.T) {
	g := newTerm(t, 3, 20)
	g.Write([]byte("\x1b[1;32mgreen\x1b[0m text\r\nsecond"))
	ckpt := checkpoint(t, g)

	// Corrupt the native snapshot, keeping the envelope intact.
	native := binary.LittleEndian.Uint32(ckpt[len(checkpointMagic):])
	bad := bytes.Clone(ckpt)
	for i := range int(native) {
		bad[len(checkpointMagic)+4+i] = 0
	}
	want, got := g.Snapshot(), restore(t, bad, 3, 20).Snapshot()
	want.BellSeq, got.BellSeq = 0, 0
	if !got.Equal(want) {
		t.Errorf("fallback screen differs:\n%s\nwant:\n%s", screenText(got), screenText(want))
	}
}

// TestRestoreBareRepaint checks that a checkpoint from before libghostty,
// a bare repaint, still restores.
func TestRestoreBareRepaint(t *testing.T) {
	g := newTerm(t, 2, 10)
	g.Write([]byte("old style"))
	r := restore(t, g.Snapshot().Repaint(), 2, 10)
	if !strings.HasPrefix(screenText(r.Snapshot()), "old style") {
		t.Errorf("screen = %q", screenText(r.Snapshot()))
	}
}

func TestCorruptCheckpoint(t *testing.T) {
	if _, err := Restore([]byte(checkpointMagic+"\xff\xff\xff\xff"), 2, 10); err == nil {
		t.Error("Restore of a truncated checkpoint succeeded")
	}
}

func TestNotifications(t *testing.T) {
	g := newTerm(t, 2, 20)
	if n := g.Snapshot().Notification; n != (Notification{}) {
		t.Errorf("notification before any = %+v", n)
	}
	g.Write([]byte("\x1b]777;notify;Build;done\x07"))
	n1 := g.Snapshot().Notification
	if n1.Title != "Build" || n1.Body != "done" || n1.Seq == 0 {
		t.Errorf("OSC 777 notification = %+v", n1)
	}
	g.Write([]byte("\x1b]9;just a body\x07"))
	n2 := g.Snapshot().Notification
	if n2.Body != "just a body" || n2.Seq <= n1.Seq {
		t.Errorf("OSC 9 notification = %+v (previous seq %d)", n2, n1.Seq)
	}
	// Sequence numbers keep increasing across a restore, as across a
	// tstermd restart.
	r := restore(t, checkpoint(t, g), 2, 20)
	r.Write([]byte("\x1b]9;after restore\x07"))
	if n3 := r.Snapshot().Notification; n3.Body != "after restore" || n3.Seq <= n2.Seq {
		t.Errorf("notification after restore = %+v (previous seq %d)", n3, n2.Seq)
	}
}

func TestBellSeqIncreasesAcrossRestore(t *testing.T) {
	g := newTerm(t, 1, 5)
	g.Write([]byte("\x07\x07"))
	before := g.Snapshot().BellSeq
	r := restore(t, checkpoint(t, g), 1, 5)
	r.Write([]byte("\x07"))
	if after := r.Snapshot().BellSeq; after <= before {
		t.Errorf("BellSeq after restore = %d, not above %d", after, before)
	}
}

func TestTerminalsAreIsolated(t *testing.T) {
	a, b := newTerm(t, 2, 10), newTerm(t, 2, 10)
	a.Write([]byte("AAA"))
	b.Write([]byte("BBB"))
	if !strings.HasPrefix(screenText(a.Snapshot()), "AAA") || !strings.HasPrefix(screenText(b.Snapshot()), "BBB") {
		t.Errorf("a=%q b=%q", screenText(a.Snapshot()), screenText(b.Snapshot()))
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

func BenchmarkWrite(b *testing.B) {
	term := newTerm(b, 60, 200)
	data := output(1 << 20)
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		term.Write(data)
	}
}

func BenchmarkSnapshot200x60(b *testing.B) {
	term := newTerm(b, 60, 200)
	term.Write(output(1 << 20))
	for b.Loop() {
		term.Snapshot()
	}
}

func BenchmarkCheckpoint200x60(b *testing.B) {
	term := newTerm(b, 60, 200)
	term.Write(output(4 << 20))
	for b.Loop() {
		term.Checkpoint()
	}
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
