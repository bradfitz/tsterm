// Package fakeapp is a stand-in for a full-screen terminal program, such as
// an editor, for tests. It exercises what such programs do to a terminal,
// so tests can check that sessions handle it without depending on any
// installed program.
//
// When it starts, it writes MainScreenText on the main screen, then switches
// to the alternate screen, sets a title, a scroll region, and input modes,
// queries the terminal and waits for its replies, and shows ReadyText. It
// then reads single-key commands:
//
//	s  print a numbered line at the bottom of the scroll region
//	n  ask for two desktop notifications (OSC 777 and OSC 9)
//	q  restore the main screen and modes, print ByeText, and exit
//
// A test binary runs it by calling MainIfRequested from TestMain and
// starting itself with Command.
package fakeapp

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"time"

	"golang.org/x/term"
)

// Text the program shows.
const (
	MainScreenText = "fakeapp: main screen"
	AltScreenText  = "fakeapp: alt screen"
	ReadyText      = "fakeapp: ready"
	ByeText        = "fakeapp: bye"
	Title          = "fakeapp"

	// RepliesText follows ReadyText once the terminal has answered both
	// queries.
	RepliesText = "replies ok"
)

// Notifications, for the n command.
const (
	NotifyTitle = "fakeapp"
	NotifyBody  = "hello from fakeapp"
	Notify9Body = "osc 9 from fakeapp"
)

// The scroll region, as 1-based rows.
const (
	ScrollTop    = 3
	ScrollBottom = 5
)

// EnvVar, when set, makes MainIfRequested run the program.
const EnvVar = "TSTERM_FAKEAPP"

// MainIfRequested runs the program, and exits, if the environment asks for
// it. Tests call it first thing in TestMain.
func MainIfRequested() {
	if os.Getenv(EnvVar) == "" {
		return
	}
	os.Exit(run(os.Stdin, os.Stdout))
}

// Command returns the command and environment that run the program: this
// test binary, with EnvVar set.
func Command() (path string, env []string) {
	exe, err := os.Executable()
	if err != nil {
		exe = os.Args[0]
	}
	return exe, []string{EnvVar + "=1"}
}

func run(in *os.File, out io.Writer) int {
	if old, err := term.MakeRaw(int(in.Fd())); err == nil {
		defer term.Restore(int(in.Fd()), old)
	}
	keys := make(chan byte, 64)
	go func() {
		b := make([]byte, 256)
		for {
			n, err := in.Read(b)
			for _, c := range b[:n] {
				keys <- c
			}
			if err != nil {
				close(keys)
				return
			}
		}
	}()

	fmt.Fprintf(out, "%s\r\n", MainScreenText)
	fmt.Fprint(out,
		"\x1b[?1049h"+ // alternate screen
			"\x1b]2;"+Title+"\x07"+
			"\x1b[?1002h\x1b[?1006h"+ // mouse button tracking, SGR encoding
			"\x1b[?2004h"+ // bracketed paste
			"\x1b[?1h"+ // application cursor keys
			"\x1b[?1004h"+ // focus events
			"\x1b[H\x1b[2J"+
			AltScreenText+
			fmt.Sprintf("\x1b[%d;%dr", ScrollTop, ScrollBottom))

	// Ask for device attributes and the cursor position, and wait for both
	// replies, as programs that probe their terminal do.
	fmt.Fprint(out, "\x1b[c\x1b[6n")
	var replies []byte
	deadline := time.After(5 * time.Second)
	ok := false
wait:
	for !ok {
		select {
		case c, open := <-keys:
			if !open {
				return 1
			}
			replies = append(replies, c)
			ok = bytes.Contains(replies, []byte("\x1b[?")) && bytes.Contains(replies, []byte("R"))
		case <-deadline:
			break wait
		}
	}
	status := RepliesText
	if !ok {
		status = fmt.Sprintf("replies missing: %q", replies)
	}
	fmt.Fprintf(out, "\x1b[2;1H%s %s", ReadyText, status)

	lines := 0
	for c := range keys {
		switch c {
		case 's':
			lines++
			fmt.Fprintf(out, "\x1b[%d;1H\r\nscroll %d", ScrollBottom, lines)
		case 'n':
			fmt.Fprint(out, "\x1b]777;notify;"+NotifyTitle+";"+NotifyBody+"\x07")
			fmt.Fprint(out, "\x1b]9;"+Notify9Body+"\x07")
		case 'q':
			fmt.Fprint(out, "\x1b[r"+ // reset the scroll region
				"\x1b[?1004l\x1b[?1l\x1b[?2004l\x1b[?1006l\x1b[?1002l"+
				"\x1b[?1049l") // back to the main screen
			fmt.Fprintf(out, "%s\r\n", ByeText)
			return 0
		}
	}
	return 0
}
