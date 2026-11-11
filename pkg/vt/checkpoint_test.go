package vt

import (
	"strings"
	"testing"
)

// roundTrip writes out to a new rows×cols terminal, checkpoints it with
// Repaint, loads the checkpoint into another new terminal, and reports
// whether the two screens match.
func roundTrip(t *testing.T, rows, cols int, out string) (orig, restored Screen) {
	t.Helper()
	a := newTerm(t, rows, cols)
	a.Write([]byte(out))
	orig = a.Snapshot()
	b := newTerm(t, rows, cols)
	b.Write(orig.Repaint())
	restored = b.Snapshot()
	// The bell counter is an event count, not state, and is not restored.
	orig.BellSeq, restored.BellSeq = 0, 0
	return orig, restored
}

func TestRepaintRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		out  string
	}{
		{"empty", ""},
		{"text", "hello\r\nworld"},
		{"styles", "\x1b[1;31mbold red\x1b[0m \x1b[3;4:3;38;2;10;20;30;48;5;200mfancy\x1b[0m \x1b[7;9;2mrev\x1b[0m"},
		{"wide", "a世界b\r\n🙂x"},
		{"link", "see \x1b]8;;https://example.com/\x1b\\here\x1b]8;;\x1b\\ ok"},
		{"title", "\x1b]2;my title\x07$ "},
		{"modes", "\x1b[?1h\x1b[?2004h\x1b[?1000hmodes"},
		{"mouse sgr focus", "\x1b[?1002h\x1b[?1006h\x1b[?1004hmouse"},
		{"cursor", "\x1b[5 q\x1b[?25l\x1b[3;7Hcur"},
		{"steady block", "\x1b[2 qx"},
		{"full last row", strings.Repeat("x", 10*4)},
		{"alt screen", "main text\x1b[?1049h\x1b[2;2Halt text"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			orig, restored := roundTrip(t, 4, 10, tt.out)
			if tt.name == "link" && orig.Cells[0][4].Link != "https://example.com/" {
				t.Fatalf("original screen has no link to round-trip: %+v", orig.Cells[0][4])
			}
			if !orig.Equal(restored) {
				t.Errorf("restored screen differs\norig:     %+v\nrestored: %+v", orig, restored)
			}
		})
	}
}

func TestRepaintNotEmptyScreenChanges(t *testing.T) {
	// Sanity check that roundTrip can fail: a checkpoint of one screen
	// must not match a different one.
	a := newTerm(t, 2, 5)
	a.Write([]byte("abc"))
	b := newTerm(t, 2, 5)
	b.Write([]byte("abd"))
	c := newTerm(t, 2, 5)
	c.Write(a.Snapshot().Repaint())
	if c.Snapshot().Equal(b.Snapshot()) {
		t.Fatal("checkpoint of one screen matches another")
	}
}
