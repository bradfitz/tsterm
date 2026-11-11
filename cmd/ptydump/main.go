// ptydump runs a command under a fresh PTY, pumps the PTY's output into a
// vt.Terminal, waits for the command to finish, and prints the resulting
// framed screen. It is the smallest possible integration smoke for
// pkg/pty + pkg/vt: if a real subshell renders correctly, the seam between
// the two packages is working.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/bradfitz/tsterm/pkg/pty"
	"github.com/bradfitz/tsterm/pkg/vt"
)

func main() {
	rows := flag.Int("rows", 24, "terminal rows")
	cols := flag.Int("cols", 80, "terminal cols")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: ptydump [-rows N] [-cols N] -- command [args...]\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() == 0 {
		flag.Usage()
		os.Exit(2)
	}

	args := flag.Args()
	env := append(os.Environ(), "TERM=xterm-256color")
	p, err := pty.Start(pty.Config{
		Command: args[0],
		Args:    args[1:],
		Env:     env,
		Rows:    *rows,
		Cols:    *cols,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "ptydump: %v\n", err)
		os.Exit(1)
	}

	term, err := vt.New(*rows, *cols)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ptydump: %v\n", err)
		os.Exit(1)
	}
	_, copyErr := io.Copy(term, p)
	waitErr := p.Wait()

	s := term.Snapshot()
	m := term.Modes()

	if s.Title != "" {
		fmt.Printf("title: %q\n", s.Title)
	}
	fmt.Printf("size: %dx%d  cursor: (%d,%d) visible=%v\n",
		s.Rows, s.Cols, s.Cursor.Row, s.Cursor.Col, s.Cursor.Visible)
	fmt.Printf("modes: alt=%v bracketed=%v appcursor=%v mouse=%d bell=%d\n",
		m.AltScreen, m.BracketedPaste, m.ApplicationCursor, m.Mouse, s.BellSeq)
	frame(os.Stdout, s)

	if waitErr != nil {
		fmt.Fprintf(os.Stderr, "ptydump: child: %v\n", waitErr)
		os.Exit(1)
	}
	// copyErr is expected when the slave closes (EIO / EOF). Surface it only
	// when nothing else has gone wrong, so unusual transports show up.
	if copyErr != nil && !isExpectedTTYClose(copyErr) {
		fmt.Fprintf(os.Stderr, "ptydump: copy: %v\n", copyErr)
	}
}

func frame(w io.Writer, s vt.Screen) {
	bar := "+" + strings.Repeat("-", s.Cols) + "+\n"
	fmt.Fprint(w, bar)
	for y := range s.Rows {
		fmt.Fprint(w, "|")
		for _, c := range s.Cells[y] {
			switch {
			case c.Width == 0:
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

// On Linux a PTY master read returns EIO once the slave is gone; on some
// BSDs it surfaces as EOF. Treat either as the normal end-of-session.
func isExpectedTTYClose(err error) bool {
	if err == nil || err == io.EOF {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "input/output error") ||
		strings.Contains(s, "file already closed")
}
