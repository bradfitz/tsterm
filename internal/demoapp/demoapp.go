// Package demoapp is a terminal program that shows off what a terminal can
// do: colors, text styles, wide and combining characters, hyperlinks, the
// title, cursor styles, the bell, desktop notifications, the mouse, paste,
// focus, resizing, and replies to queries. tstermd runs it for sessions
// named DemoName (see cmd/tstermd), and tests run it as a stand-in for a
// full-screen program such as an editor.
//
// When it starts, it writes MainScreenText on the main screen, then switches
// to the alternate screen, turns on input modes, queries the terminal and
// waits for the replies, and draws: a title row (AltScreenText), a status
// row (starting with ReadyText), a row per feature, and an event log at the
// bottom, in a scroll region (see LogRegion). It then reads single-key
// commands:
//
//	n  ask for desktop notifications (OSC 777, then OSC 9)
//	b  ring the bell
//	c  cycle the cursor style
//	t  start or stop animating the title
//	s  log a numbered line ("scroll N")
//	q  restore the main screen and modes, print ByeText, and exit
//
// Other keys, mouse events, pastes, focus changes, and resizes are shown.
//
// A program runs it by calling MainIfRequested first thing in main, or in
// TestMain, and starting itself with Command.
package demoapp

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/term"
)

// Text the program shows.
const (
	MainScreenText = "tsterm demo: main screen"
	AltScreenText  = "tsterm demo"
	ReadyText      = "ready"
	ByeText        = "tsterm demo: bye"
	Title          = "tsterm demo"

	// RepliesText follows ReadyText once the terminal has answered both
	// queries.
	RepliesText = "replies ok"
)

// Notifications, for the n command.
const (
	NotifyTitle = "tsterm demo"
	NotifyBody  = "hello from the tsterm demo"
	Notify9Body = "an OSC 9 notification from the tsterm demo"
)

// EnvVar, when set, makes MainIfRequested run the program.
const EnvVar = "TSTERM_DEMO"

// LogRegion returns the first and last rows, 1-based, of the event log,
// which is a scroll region at the bottom of a rows-row screen.
func LogRegion(rows int) (top, bottom int) {
	n := min(max(rows/4, 3), 8)
	return max(rows-n+1, 4), rows
}

// MainIfRequested runs the program, and exits, if the environment asks for
// it.
func MainIfRequested() {
	if os.Getenv(EnvVar) == "" {
		return
	}
	os.Exit(run(os.Stdin, os.Stdout))
}

// Command returns the command and environment that run the program: the
// running executable, with EnvVar set.
func Command() (path string, env []string) {
	exe, err := os.Executable()
	if err != nil {
		exe = os.Args[0]
	}
	return exe, []string{EnvVar + "=1"}
}

type app struct {
	out        io.Writer
	fd         int // of the terminal, for its size
	rows, cols int

	da, cpr, bg string // replies to queries
	cursor      int    // DECSCUSR shape, 1 to 6
	titleFrame  int    // frame of the title animation, or -1 when still
	focus       string
	lastKey     string
	lastMouse   string
	lastPaste   string
	marks       [][2]int // clicked cells, 0-based x, y
	logs        []string
	scrolls     int
}

func run(in *os.File, out io.Writer) int {
	if old, err := term.MakeRaw(int(in.Fd())); err == nil {
		defer term.Restore(int(in.Fd()), old)
	}
	a := &app{out: out, fd: int(os.Stdout.Fd()), cursor: 1, titleFrame: -1, focus: "unknown"}
	a.rows, a.cols = 24, 80
	if w, h, err := term.GetSize(a.fd); err == nil {
		a.cols, a.rows = w, h
	}

	input := make(chan []byte, 16)
	go func() {
		for {
			b := make([]byte, 4096)
			n, err := in.Read(b)
			if n > 0 {
				input <- b[:n]
			}
			if err != nil {
				close(input)
				return
			}
		}
	}()
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)

	fmt.Fprintf(out, "%s\r\n", MainScreenText)
	fmt.Fprint(out,
		"\x1b[?1049h"+ // alternate screen
			"\x1b]2;"+Title+"\x07"+
			"\x1b[?1002h\x1b[?1006h"+ // mouse button tracking, SGR encoding
			"\x1b[?2004h"+ // bracketed paste
			"\x1b[?1h"+ // application cursor keys
			"\x1b[?1004h") // focus events

	// Ask for device attributes, the cursor position, and the background
	// color, and wait for the first two, as programs that probe their
	// terminal do.
	fmt.Fprint(out, "\x1b[H\x1b[c\x1b[6n\x1b]11;?\x07")
	var p parser
	deadline := time.After(5 * time.Second)
wait:
	for a.da == "" || a.cpr == "" {
		select {
		case b, ok := <-input:
			if !ok {
				return 1
			}
			for _, ev := range p.feed(b) {
				a.handle(ev)
			}
		case <-deadline:
			break wait
		}
	}
	a.redraw()

	var tick <-chan time.Time
	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()
	for {
		if a.titleFrame >= 0 {
			tick = ticker.C
		} else {
			tick = nil
		}
		select {
		case b, ok := <-input:
			if !ok {
				return 0
			}
			for _, ev := range p.feed(b) {
				if a.handle(ev) {
					a.quit()
					return 0
				}
			}
		case <-p.escTimeout():
			for _, ev := range p.flush() {
				a.handle(ev)
			}
		case <-winch:
			if w, h, err := term.GetSize(a.fd); err == nil {
				a.cols, a.rows = w, h
			}
			a.log(fmt.Sprintf("resized to %dx%d", a.cols, a.rows))
			a.redraw()
		case <-tick:
			a.titleFrame++
			spin := []string{"✳", "✶", "✻", "✽", "✻", "✶"}
			fmt.Fprintf(a.out, "\x1b]2;%s %s\x07", spin[a.titleFrame%len(spin)], Title)
		}
	}
}

// quit restores the terminal and the main screen.
func (a *app) quit() {
	fmt.Fprint(a.out, "\x1b[r"+ // reset the scroll region
		"\x1b[0 q"+ // default cursor
		"\x1b[?1004l\x1b[?1l\x1b[?2004l\x1b[?1006l\x1b[?1002l"+
		"\x1b[?1049l") // back to the main screen
	fmt.Fprintf(a.out, "%s\r\n", ByeText)
}

// handle handles an input event. It reports whether to quit.
func (a *app) handle(ev event) bool {
	switch ev.kind {
	case evDA:
		a.da = ev.text
		return false
	case evCPR:
		a.cpr = ev.text
		return false
	case evOSC:
		if rest, ok := strings.CutPrefix(ev.text, "11;"); ok {
			a.bg = rest
		}
		return false
	}
	if a.da == "" || a.cpr == "" {
		return false // still starting
	}
	switch ev.kind {
	case evKey:
		switch ev.text {
		case "q":
			return true
		case "n":
			fmt.Fprint(a.out, "\x1b]777;notify;"+NotifyTitle+";"+NotifyBody+"\x07")
			fmt.Fprint(a.out, "\x1b]9;"+Notify9Body+"\x07")
			a.log("sent desktop notifications")
			return false
		case "b":
			fmt.Fprint(a.out, "\x07")
			a.log("rang the bell")
			return false
		case "c":
			a.cursor = a.cursor%6 + 1
			fmt.Fprintf(a.out, "\x1b[%d q", a.cursor)
			a.drawStatus()
			return false
		case "t":
			if a.titleFrame >= 0 {
				a.titleFrame = -1
				fmt.Fprint(a.out, "\x1b]2;"+Title+"\x07")
			} else {
				a.titleFrame = 0
			}
			return false
		case "s":
			a.scrolls++
			a.log(fmt.Sprintf("scroll %d", a.scrolls))
			return false
		}
		a.lastKey = keyName(ev.text)
		a.drawFeatures()
		a.log("key " + a.lastKey)
	case evMouse:
		a.lastMouse = ev.text
		if ev.press && ev.button == 0 {
			a.marks = append(a.marks, [2]int{ev.x, ev.y})
			if len(a.marks) > 20 {
				a.marks = a.marks[1:]
			}
			a.log(fmt.Sprintf("click at column %d, row %d", ev.x+1, ev.y+1))
		}
		a.drawFeatures()
	case evPaste:
		a.lastPaste = fmt.Sprintf("%d bytes: %q", len(ev.text), ev.text)
		a.drawFeatures()
		a.log("paste of " + a.lastPaste)
	case evFocus:
		a.focus = ev.text
		a.drawStatus()
		a.log("focus " + ev.text)
	}
	return false
}

// Layout: row 1 is the title, row 2 the status, rows 3 through the row
// before the log's separator the features, then the separator and the log.

func (a *app) redraw() {
	fmt.Fprint(a.out, "\x1b[r\x1b[0m\x1b[H\x1b[2J")
	fmt.Fprintf(a.out, "\x1b[1;1H\x1b[1;97;44m%s\x1b[K\x1b[0m", a.fit(" "+AltScreenText+" · n notify · b bell · c cursor · t title · s log · click · paste · q quit"))
	a.drawStatus()
	a.drawFeatures()
	top, bottom := LogRegion(a.rows)
	fmt.Fprintf(a.out, "\x1b[%d;1H\x1b[2m%s\x1b[0m", top-1, a.fit("── events "+strings.Repeat("─", max(a.cols-10, 0))))
	fmt.Fprintf(a.out, "\x1b[%d;%dr", top, bottom)
	n := bottom - top + 1
	start := max(len(a.logs)-n, 0)
	for i, l := range a.logs[start:] {
		fmt.Fprintf(a.out, "\x1b[%d;1H%s", top+i, a.fit(l))
	}
	a.home()
}

func (a *app) home() {
	_, bottom := LogRegion(a.rows)
	fmt.Fprintf(a.out, "\x1b[%d;1H", bottom)
}

func (a *app) drawStatus() {
	status := ReadyText + " " + RepliesText
	if a.da == "" || a.cpr == "" {
		status = ReadyText + fmt.Sprintf(" replies missing (DA %q, CPR %q)", a.da, a.cpr)
	}
	shapes := []string{"", "blinking block", "block", "blinking underline", "underline", "blinking bar", "bar"}
	status += fmt.Sprintf(" · size %dx%d · cursor %s · focus %s · terminal %s · background %s",
		a.cols, a.rows, shapes[a.cursor], a.focus, a.da, orNone(a.bg))
	fmt.Fprintf(a.out, "\x1b[2;1H\x1b[0m%s\x1b[K", a.fit(status))
	a.home()
}

func orNone(s string) string {
	if s == "" {
		return "not reported"
	}
	return s
}

func (a *app) drawFeatures() {
	top, _ := LogRegion(a.rows)
	var palette strings.Builder
	for i := range 16 {
		fmt.Fprintf(&palette, "\x1b[48;5;%dm \x1b[0m", i)
	}
	palette.WriteString(" ")
	for i := 16; i < 232; i += 9 {
		fmt.Fprintf(&palette, "\x1b[48;5;%dm \x1b[0m", i)
	}
	palette.WriteString(" ")
	for i := range 24 {
		fmt.Fprintf(&palette, "\x1b[48;2;%d;%d;%dm \x1b[0m", 255-i*10, i*10, 128)
	}
	rows := []string{
		"colors   " + palette.String(),
		"styles   \x1b[1mbold\x1b[0m \x1b[2mfaint\x1b[0m \x1b[3mitalic\x1b[0m \x1b[4munderline\x1b[0m \x1b[4:2mdouble\x1b[0m " +
			"\x1b[4:3;58;2;255;0;0mcurly\x1b[0m \x1b[4:4mdotted\x1b[0m \x1b[4:5mdashed\x1b[0m \x1b[9mstrike\x1b[0m " +
			"\x1b[53moverline\x1b[0m \x1b[7mreverse\x1b[0m \x1b[5mblink\x1b[0m \x1b[38;2;255;165;0mtruecolor\x1b[0m",
		"unicode  wide 世界 · emoji 🙂👍🏽 · combining é · boxes ┌─┬─┐ ▁▂▃▄▅▆▇█ · braille ⣿⣶⣤",
		"link     \x1b]8;;https://tailscale.com/\x1b\\a hyperlink to tailscale.com\x1b]8;;\x1b\\",
		"keys     last: " + orNone(a.lastKey),
		"mouse    last: " + orNone(a.lastMouse) + " (click anywhere)",
		"paste    last: " + orNone(a.lastPaste),
	}
	for i, r := range rows {
		y := 3 + i
		if y >= top-1 {
			break
		}
		fmt.Fprintf(a.out, "\x1b[%d;1H\x1b[0m\x1b[K%s", y, a.fitStyled(r))
	}
	for _, m := range a.marks {
		if m[1]+1 >= 3 && m[1]+1 < top-1 {
			fmt.Fprintf(a.out, "\x1b[%d;%dH\x1b[1;31m×\x1b[0m", m[1]+1, m[0]+1)
		}
	}
	a.home()
}

// log adds a line to the event log, scrolling it within its region.
func (a *app) log(s string) {
	a.logs = append(a.logs, s)
	if len(a.logs) > 100 {
		a.logs = a.logs[1:]
	}
	_, bottom := LogRegion(a.rows)
	fmt.Fprintf(a.out, "\x1b[%d;1H\r\n\x1b[0m%s", bottom, a.fit(s))
	a.home()
}

// fit truncates plain text to the screen width.
func (a *app) fit(s string) string {
	if utf8.RuneCountInString(s) <= a.cols {
		return s
	}
	return string([]rune(s)[:max(a.cols, 0)])
}

// fitStyled truncates text with escape sequences to the screen width,
// counting only printed characters (roughly: wide characters count once).
func (a *app) fitStyled(s string) string {
	var b strings.Builder
	n := 0
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			j := i + 1
			if j < len(s) && s[j] == ']' { // OSC, to ST
				end := strings.Index(s[j:], "\x1b\\")
				if end < 0 {
					break
				}
				j += end + 2
			} else {
				for j < len(s) && !(s[j] >= 0x40 && s[j] <= 0x7e && j > i+1) {
					j++
				}
				j++
			}
			b.WriteString(s[i:min(j, len(s))])
			i = j
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if n >= a.cols-1 {
			break
		}
		b.WriteRune(r)
		if r < 0x300 || r > 0x36f { // combining marks take no column
			n++
		}
		i += size
	}
	return b.String() + "\x1b[0m"
}

// keyName describes a key's bytes.
func keyName(k string) string {
	names := map[string]string{
		"\r": "Enter", "\t": "Tab", "\x7f": "Backspace", "\x1b": "Escape",
		"\x1bOA": "Up", "\x1bOB": "Down", "\x1bOC": "Right", "\x1bOD": "Left",
		"\x1b[A": "Up", "\x1b[B": "Down", "\x1b[C": "Right", "\x1b[D": "Left",
		"\x1bOH": "Home", "\x1bOF": "End", "\x1b[H": "Home", "\x1b[F": "End",
		"\x1b[5~": "PageUp", "\x1b[6~": "PageDown", "\x1b[3~": "Delete",
		"\x1bOP": "F1", "\x1bOQ": "F2", "\x1bOR": "F3", "\x1bOS": "F4",
	}
	if n, ok := names[k]; ok {
		return fmt.Sprintf("%s (%q)", n, k)
	}
	if len(k) == 1 && k[0] < 0x20 {
		return fmt.Sprintf("Ctrl-%c (%q)", k[0]+0x40, k)
	}
	return strconv.Quote(k)
}

// Input events.

type eventKind int

const (
	evKey eventKind = iota
	evMouse
	evPaste
	evFocus
	evDA
	evCPR
	evOSC
)

type event struct {
	kind   eventKind
	text   string
	x, y   int // mouse, 0-based
	button int
	press  bool
}

// parser splits the input into events.
type parser struct {
	buf     []byte
	pasting bool
	paste   bytes.Buffer
	escAt   time.Time // when buf began with an unfinished escape
}

func (p *parser) feed(b []byte) []event {
	p.buf = append(p.buf, b...)
	var evs []event
	for len(p.buf) > 0 {
		ev, n := p.next()
		if n == 0 {
			// Only a lone ESC might be the Escape key; anything else
			// unfinished (a sequence, a paste, a UTF-8 character) just
			// waits for more input.
			if !p.pasting && len(p.buf) == 1 && p.buf[0] == 0x1b && p.escAt.IsZero() {
				p.escAt = time.Now()
			}
			break
		}
		p.escAt = time.Time{}
		p.buf = p.buf[n:]
		if ev != nil {
			evs = append(evs, *ev)
		}
	}
	return evs
}

// escTimeout returns a channel that fires once a lone ESC has waited long
// enough to be the Escape key rather than the start of a sequence.
func (p *parser) escTimeout() <-chan time.Time {
	if p.escAt.IsZero() {
		return nil
	}
	return time.After(time.Until(p.escAt.Add(50 * time.Millisecond)))
}

// flush treats a pending lone ESC as the Escape key.
func (p *parser) flush() []event {
	if len(p.buf) == 0 {
		return nil
	}
	ev := event{kind: evKey, text: string(p.buf[:1])}
	p.buf = p.buf[1:]
	p.escAt = time.Time{}
	return append([]event{ev}, p.feed(nil)...)
}

// next parses one event from the start of buf, returning the event (or
// nil for none) and the bytes consumed, or 0 if more input is needed.
func (p *parser) next() (*event, int) {
	b := p.buf
	if p.pasting {
		if i := bytes.Index(b, []byte("\x1b[201~")); i >= 0 {
			p.paste.Write(b[:i])
			p.pasting = false
			ev := &event{kind: evPaste, text: p.paste.String()}
			p.paste.Reset()
			return ev, i + 6
		}
		// Keep what might be the start of the end marker.
		keep := min(len(b), 5)
		p.paste.Write(b[:len(b)-keep])
		if len(b) > keep {
			return nil, len(b) - keep
		}
		return nil, 0
	}
	if b[0] != 0x1b {
		_, size := utf8.DecodeRune(b)
		if !utf8.FullRune(b) {
			return nil, 0
		}
		return &event{kind: evKey, text: string(b[:size])}, size
	}
	if len(b) < 2 {
		return nil, 0
	}
	switch b[1] {
	case '[':
		// CSI: parameters, then a final byte.
		i := 2
		for i < len(b) && !(b[i] >= 0x40 && b[i] <= 0x7e) {
			i++
		}
		if i >= len(b) {
			return nil, 0
		}
		seq := string(b[:i+1])
		params := string(b[2:i])
		switch final := b[i]; {
		case seq == "\x1b[200~":
			p.pasting = true
			return nil, i + 1
		case seq == "\x1b[I":
			return &event{kind: evFocus, text: "in"}, i + 1
		case seq == "\x1b[O":
			return &event{kind: evFocus, text: "out"}, i + 1
		case strings.HasPrefix(params, "?") && final == 'c':
			return &event{kind: evDA, text: params[1:]}, i + 1
		case final == 'R':
			return &event{kind: evCPR, text: params}, i + 1
		case strings.HasPrefix(params, "<") && (final == 'M' || final == 'm'):
			f := strings.Split(params[1:], ";")
			if len(f) == 3 {
				btn, _ := strconv.Atoi(f[0])
				x, _ := strconv.Atoi(f[1])
				y, _ := strconv.Atoi(f[2])
				action := "release"
				if final == 'M' {
					action = "press"
					if btn&32 != 0 {
						action = "drag"
					}
				}
				if btn&64 != 0 {
					action = "wheel"
				}
				return &event{
					kind: evMouse, x: x - 1, y: y - 1, button: btn & 3,
					press: final == 'M' && btn&(32|64) == 0,
					text:  fmt.Sprintf("%s button %d at column %d, row %d", action, btn&3, x, y),
				}, i + 1
			}
		}
		return &event{kind: evKey, text: seq}, i + 1
	case ']':
		// OSC, ended by BEL or ST.
		if i := bytes.IndexByte(b, 0x07); i >= 0 {
			return &event{kind: evOSC, text: string(b[2:i])}, i + 1
		}
		if i := bytes.Index(b, []byte("\x1b\\")); i >= 0 {
			return &event{kind: evOSC, text: string(b[2:i])}, i + 2
		}
		return nil, 0
	case 'O':
		if len(b) < 3 {
			return nil, 0
		}
		return &event{kind: evKey, text: string(b[:3])}, 3
	}
	// Alt plus a key.
	_, size := utf8.DecodeRune(b[1:])
	return &event{kind: evKey, text: string(b[:1+size])}, 1 + size
}
