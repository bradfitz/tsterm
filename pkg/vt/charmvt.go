package vt

import (
	"bytes"
	"image/color"
	"io"
	"slices"
	"strings"
	"sync"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	cvt "github.com/charmbracelet/x/vt"
)

// New returns a Terminal of the given size backed by
// github.com/charmbracelet/x/vt. The implementation is hidden behind the
// Terminal interface so that the backend may be swapped later without
// disturbing callers.
func New(rows, cols int) Terminal {
	return newCharm(rows, cols)
}

// charm wraps a *cvt.Emulator and translates its types into the package's
// public types. All access to the emulator is serialized through mu: the
// emulator itself is not goroutine-safe, and the Terminal contract permits a
// writer goroutine and a reader goroutine.
type charm struct {
	mu   sync.Mutex
	emu  *cvt.Emulator
	rows int
	cols int

	// last is the screen state reported by the most recent Diff. It is the
	// baseline against which the next Diff compares. last.Cells is nil
	// before the first Diff so that the first Diff reports every row as
	// changed, which lets a freshly attached client bootstrap without a
	// separate snapshot step.
	last Screen

	// Mode and event state tracked through callbacks. The upstream emulator
	// does not expose mode reads, so we mirror the state on each callback.
	title       string
	bellSeq     uint64
	altScreen   bool
	cursorVis   bool
	bracketed   bool
	appCursor   bool
	mouseMode   MouseMode
	mouseSGR    bool
	focusEvents bool
	cursorStyle CursorStyle
	cursorBlink bool

	barrier chan struct{} // see syncRepliesLocked
	osc     oscState      // see hideST

	replyMu sync.Mutex
	replies []byte // collected by readReplies, taken by Replies
}

func newCharm(rows, cols int) *charm {
	c := &charm{
		emu:         cvt.NewEmulator(cols, rows),
		rows:        rows,
		cols:        cols,
		cursorVis:   true, // DECTCEM defaults to set
		cursorStyle: CursorBlock,
		cursorBlink: true,
		barrier:     make(chan struct{}),
	}
	go c.readReplies()
	c.emu.SetCallbacks(cvt.Callbacks{
		Bell:      func() { c.bellSeq++ },
		Title:     func(t string) { c.title = unhideST(t) },
		AltScreen: func(on bool) { c.altScreen = on },
		CursorVisibility: func(visible bool) {
			c.cursorVis = visible
		},
		// Despite its documented parameter name, the emulator passes
		// "steady" (not blinking) as the second argument.
		CursorStyle: func(style cvt.CursorStyle, steady bool) {
			c.cursorStyle = mapCursorStyle(style)
			c.cursorBlink = !steady
		},
		EnableMode:  func(m ansi.Mode) { c.applyMode(m, true) },
		DisableMode: func(m ansi.Mode) { c.applyMode(m, false) },
	})
	return c
}

// applyMode mirrors mode set/reset events into the local Modes mirror.
// Only modes we expose to clients are tracked; the rest are ignored.
func (c *charm) applyMode(m ansi.Mode, on bool) {
	switch m.Mode() {
	case 1: // DECCKM, application cursor keys
		c.appCursor = on
	case 25: // DECTCEM, text cursor enable
		c.cursorVis = on
	case 9: // X10 mouse compatibility
		if on {
			c.mouseMode = MouseX10
		} else if c.mouseMode == MouseX10 {
			c.mouseMode = MouseOff
		}
	case 1000: // Normal mouse tracking
		if on {
			c.mouseMode = MouseNormal
		} else if c.mouseMode == MouseNormal {
			c.mouseMode = MouseOff
		}
	case 1002: // Button-event mouse tracking
		if on {
			c.mouseMode = MouseButton
		} else if c.mouseMode == MouseButton {
			c.mouseMode = MouseOff
		}
	case 1003: // Any-event mouse tracking
		if on {
			c.mouseMode = MouseAny
		} else if c.mouseMode == MouseAny {
			c.mouseMode = MouseOff
		}
	case 1006: // SGR mouse encoding
		c.mouseSGR = on
	case 1004: // focus events
		c.focusEvents = on
	case 1047, 1049: // alternate screen
		c.altScreen = on
	case 2004: // bracketed paste
		c.bracketed = on
	}
}

func (c *charm) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err := c.emu.Write(c.hideST(p))
	c.syncRepliesLocked()
	return len(p), err
}

// The emulator's parser ends an OSC string (a title, a hyperlink, and so
// on) at byte 0x9C, the 8-bit C1 String Terminator. But this terminal is
// UTF-8, where, as in xterm, C1 controls are not recognized, and 0x9C is a
// UTF-8 continuation byte, as in "✳" (E2 9C B3), which Claude Code puts in
// its title. Cut off there, the title is invalid UTF-8, and the rest of it
// lands on the screen.
//
// So inside OSC strings, hideST replaces 0x9C with 0xFF, which the parser
// collects like any other byte and which never occurs in UTF-8, and
// unhideST reverses that in the strings that come out of OSC sequences.
//
// TODO(bradfitz): Fix this in github.com/charmbracelet/x/ansi instead.
const hiddenST = 0xFF

type oscState uint8

const (
	oscNone  oscState = iota
	oscEsc            // after ESC, outside an OSC string
	oscIn             // in an OSC string
	oscInEsc          // after ESC in an OSC string
)

// hideST returns p with 0x9C bytes inside OSC strings replaced by hiddenST,
// tracking OSC strings across calls. It returns p itself if nothing needs
// replacing.
func (c *charm) hideST(p []byte) []byte {
	var out []byte
	for i, b := range p {
		switch c.osc {
		case oscNone:
			if b == 0x1b {
				c.osc = oscEsc
			}
		case oscEsc:
			switch b {
			case ']':
				c.osc = oscIn
			case 0x1b:
			default:
				c.osc = oscNone
			}
		case oscIn:
			switch b {
			case 0x07, 0x18, 0x1a:
				c.osc = oscNone
			case 0x1b:
				c.osc = oscInEsc
			case 0x9c:
				if out == nil {
					out = slices.Clone(p)
				}
				out[i] = hiddenST
			}
		case oscInEsc:
			// ESC \ (ST) ends the string, and any other ESC starts a
			// new sequence, ending it too.
			c.osc = oscNone
			if b == ']' {
				c.osc = oscIn
			} else if b == 0x1b {
				c.osc = oscEsc
			}
		}
	}
	if out == nil {
		return p
	}
	return out
}

// unhideST restores the 0x9C bytes that hideST hid in s.
func unhideST(s string) string {
	return strings.ReplaceAll(s, string([]byte{hiddenST}), string([]byte{0x9c}))
}

func (c *charm) Resize(rows, cols int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if rows == c.rows && cols == c.cols {
		return
	}
	c.rows, c.cols = rows, cols
	c.emu.Resize(cols, rows) // may report the size, in in-band resize mode
	c.syncRepliesLocked()
}

// The emulator writes its replies to queries into an io.Pipe, inside its
// Write method, and that blocks until someone reads them. readReplies
// reads them, forever, so that Write never blocks.
//
// To make every reply caused by a Write available when Write returns,
// syncRepliesLocked sends replyBarrier through the same pipe and waits for
// readReplies to see it: the pipe is ordered, so all earlier replies have
// been collected by then. The barrier is a NUL followed by text that no
// reply contains.
const replyBarrier = "\x00tsterm-reply-barrier\x00"

func (c *charm) readReplies() {
	buf := make([]byte, 4096)
	var pending []byte
	for {
		n, err := c.emu.Read(buf)
		pending = append(pending, buf[:n]...)
		for {
			i := bytes.Index(pending, []byte(replyBarrier))
			if i < 0 {
				break
			}
			c.replyMu.Lock()
			c.replies = append(c.replies, pending[:i]...)
			c.replyMu.Unlock()
			pending = pending[i+len(replyBarrier):]
			c.barrier <- struct{}{}
		}
		if err != nil {
			return
		}
	}
}

func (c *charm) syncRepliesLocked() {
	if _, err := io.WriteString(c.emu.InputPipe(), replyBarrier); err != nil {
		return // closed
	}
	<-c.barrier
}

// Close stops the reply reader. The screen remains readable.
//
// It closes the emulator's reply pipe directly rather than calling the
// emulator's Close, which sets a flag that the emulator's Read checks
// without synchronization.
func (c *charm) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if pw, ok := c.emu.InputPipe().(*io.PipeWriter); ok {
		return pw.Close()
	}
	return c.emu.Close()
}

func (c *charm) Replies() []byte {
	c.replyMu.Lock()
	defer c.replyMu.Unlock()
	r := c.replies
	c.replies = nil
	return r
}

func (c *charm) Size() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rows, c.cols
}

func (c *charm) Modes() Modes {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.snapshotModes()
}

func (c *charm) snapshotModes() Modes {
	return Modes{
		AltScreen:         c.altScreen,
		CursorVisible:     c.cursorVis,
		BracketedPaste:    c.bracketed,
		ApplicationCursor: c.appCursor,
		Mouse:             c.mouseMode,
		MouseSGR:          c.mouseSGR,
		FocusEvents:       c.focusEvents,
	}
}

func (c *charm) Snapshot() Screen {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.snapshotLocked()
}

// snapshotLocked builds a Screen from the current emulator state. The caller
// must hold c.mu.
func (c *charm) snapshotLocked() Screen {
	cells := make([][]Cell, c.rows)
	for y := range c.rows {
		row := make([]Cell, c.cols)
		x := 0
		for x < c.cols {
			cell := c.emu.CellAt(x, y)
			if cell == nil {
				row[x] = Cell{Grapheme: " ", Width: 1}
				x++
				continue
			}
			row[x] = convertCell(cell)
			w := max(row[x].Width, 1)
			// Fill continuation columns for wide cells with width-zero
			// placeholders so callers can address the grid by (row, col).
			for k := uint8(1); k < w && int(k)+x < c.cols; k++ {
				row[x+int(k)] = Cell{Grapheme: "", Width: 0, Style: row[x].Style}
			}
			x += int(w)
		}
		cells[y] = row
	}
	pos := c.emu.CursorPosition()
	return Screen{
		Rows:    c.rows,
		Cols:    c.cols,
		Cells:   cells,
		Cursor:  CursorState{Row: pos.Y, Col: pos.X, Visible: c.cursorVis, Style: c.cursorStyle, Blink: c.cursorBlink},
		Title:   strings.ToValidUTF8(c.title, "\uFFFD"),
		BellSeq: c.bellSeq,
		Modes:   c.snapshotModes(),
	}
}

func (c *charm) Diff() ScreenDelta {
	c.mu.Lock()
	defer c.mu.Unlock()
	cur := c.snapshotLocked()
	delta := Diff(c.last, cur)
	c.last = cur
	return delta
}

// diffRows returns whole-row CellRuns for every row in cur that differs from
// the corresponding row in prev. Rows present only in cur are emitted in
// full; rows present only in prev are ignored (the row count is communicated
// separately via ScreenDelta.Rows/Cols and Resized).
func diffRows(prev, cur [][]Cell, cols int) []CellRun {
	var out []CellRun
	for y := range cur {
		if y < len(prev) && rowEqual(prev[y], cur[y]) {
			continue
		}
		row := make([]Cell, cols)
		copy(row, cur[y])
		out = append(out, CellRun{Row: y, Col: 0, Cells: row})
	}
	return out
}

func rowEqual(a, b []Cell) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func mapCursorStyle(s cvt.CursorStyle) CursorStyle {
	switch s {
	case cvt.CursorUnderline:
		return CursorUnderline
	case cvt.CursorBar:
		return CursorBar
	default:
		return CursorBlock
	}
}

func convertCell(c *uv.Cell) Cell {
	out := Cell{
		Grapheme: strings.ToValidUTF8(c.Content, "\uFFFD"),
		Width:    uint8(c.Width),
		Style:    convertStyle(c.Style),
		Link:     strings.ToValidUTF8(unhideST(linkURI(c.Link)), "\uFFFD"),
	}
	if out.Grapheme == "" {
		out.Grapheme = " "
	}
	if out.Width == 0 {
		out.Width = 1
	}
	return out
}

// linkURI returns l's URI. The emulator parses OSC 8, which is
// "8;params;URI", into URL=params and Params=URI, the reverse of what the
// field names say.
//
// TODO(bradfitz): Fix this in github.com/charmbracelet/x/vt, which also
// drops links whose URI contains ';'.
func linkURI(l uv.Link) string { return l.Params }

func convertStyle(s uv.Style) Style {
	out := Style{
		Fg:        convertColor(s.Fg),
		Bg:        convertColor(s.Bg),
		Underline: mapUnderline(s.Underline),
	}
	if s.Attrs&uv.AttrBold != 0 {
		out.Attrs |= AttrBold
	}
	if s.Attrs&uv.AttrFaint != 0 {
		out.Attrs |= AttrFaint
	}
	if s.Attrs&uv.AttrItalic != 0 {
		out.Attrs |= AttrItalic
	}
	if s.Attrs&uv.AttrBlink != 0 {
		out.Attrs |= AttrBlink
	}
	if s.Attrs&uv.AttrRapidBlink != 0 {
		out.Attrs |= AttrRapidBlink
	}
	if s.Attrs&uv.AttrReverse != 0 {
		out.Attrs |= AttrReverse
	}
	if s.Attrs&uv.AttrConceal != 0 {
		out.Attrs |= AttrConceal
	}
	if s.Attrs&uv.AttrStrikethrough != 0 {
		out.Attrs |= AttrStrikethrough
	}
	return out
}

func mapUnderline(u ansi.Underline) UnderlineStyle {
	switch u {
	case ansi.UnderlineSingle:
		return UnderlineSingle
	case ansi.UnderlineDouble:
		return UnderlineDouble
	case ansi.UnderlineCurly:
		return UnderlineCurly
	case ansi.UnderlineDotted:
		return UnderlineDotted
	case ansi.UnderlineDashed:
		return UnderlineDashed
	default:
		return UnderlineNone
	}
}

func convertColor(c color.Color) Color {
	switch v := c.(type) {
	case nil:
		return Color{}
	case ansi.BasicColor:
		return Color{Kind: ColorIndexed, Index: uint8(v)}
	case ansi.IndexedColor:
		return Color{Kind: ColorIndexed, Index: uint8(v)}
	case ansi.RGBColor:
		return Color{Kind: ColorRGB, R: v.R, G: v.G, B: v.B}
	}
	r, g, b, a := c.RGBA()
	if a == 0 {
		return Color{}
	}
	return Color{Kind: ColorRGB, R: uint8(r >> 8), G: uint8(g >> 8), B: uint8(b >> 8)}
}
