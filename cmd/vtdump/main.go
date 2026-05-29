// vtdump reads PTY-style output from stdin, feeds it through a vt.Terminal,
// and prints the resulting cell grid as a framed text rendering. It exists
// to eyeball the canonical terminal model during pkg/vt development and to
// smoke-test recorded byte streams in CI without spinning up a real PTY.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/bradfitz/tsterm/pkg/vt"
)

func main() {
	rows := flag.Int("rows", 24, "terminal rows")
	cols := flag.Int("cols", 80, "terminal cols")
	flag.Parse()

	term := vt.New(*rows, *cols)
	if _, err := io.Copy(term, os.Stdin); err != nil {
		fmt.Fprintf(os.Stderr, "vtdump: read: %v\n", err)
		os.Exit(1)
	}

	s := term.Snapshot()
	m := term.Modes()

	if s.Title != "" {
		fmt.Printf("title: %q\n", s.Title)
	}
	fmt.Printf("size: %dx%d  cursor: (%d,%d) visible=%v style=%d blink=%v\n",
		s.Rows, s.Cols, s.Cursor.Row, s.Cursor.Col, s.Cursor.Visible, s.Cursor.Style, s.Cursor.Blink)
	fmt.Printf("modes: alt=%v bracketed=%v appcursor=%v mouse=%d bell=%d\n",
		m.AltScreen, m.BracketedPaste, m.ApplicationCursor, m.Mouse, s.BellSeq)

	frame(os.Stdout, s)
}

// frame writes the cell grid surrounded by a simple box. Styles are ignored;
// vtdump is for shape and cursor-placement debugging, not color fidelity.
func frame(w io.Writer, s Screen) {
	bar := "+" + strings.Repeat("-", s.Cols) + "+\n"
	fmt.Fprint(w, bar)
	for y := range s.Rows {
		fmt.Fprint(w, "|")
		for _, c := range s.Cells[y] {
			switch {
			case c.Width == 0:
				// Continuation of a wide character: already emitted in the
				// previous column. Skip silently.
			case c.Grapheme == "" || c.Grapheme == " ":
				fmt.Fprint(w, " ")
			default:
				fmt.Fprint(w, c.Grapheme)
			}
		}
		fmt.Fprint(w, "|\n")
	}
	fmt.Fprint(w, bar)
}

// Screen is an alias so callers of frame can write the unqualified name; the
// canonical type lives in pkg/vt.
type Screen = vt.Screen
