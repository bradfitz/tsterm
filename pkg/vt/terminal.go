// Package vt defines the canonical server-side terminal model used by tsterm.
//
// A Terminal parses PTY output written to its Write method and exposes the
// resulting cell grid, cursor, and mode state as a Screen snapshot or as
// incremental ScreenDelta updates. Transports (native UDP client, WebSocket
// viewer, recorder) consume the snapshots and deltas; they never see raw PTY
// bytes. Making the terminal model canonical is what lets a session fan out
// to multiple consumers without each one running its own emulator.
//
// The emulator is libghostty-vt; see ghostty.go.
package vt

import (
	"io"
	"strings"
)

// Terminal is the canonical server-side terminal state.
//
// Implementations parse PTY output written via Write and expose the resulting
// screen as either a full Snapshot or, more efficiently, as the diff returned
// by Diff. Terminal must be safe for concurrent use by a writer goroutine
// (driving Write from a PTY reader) and a reader goroutine (calling Snapshot,
// Diff, Modes).
type Terminal interface {
	io.Writer

	// Resize changes the visible grid size. The cursor and existing cell
	// contents follow the emulator's resize behavior.
	Resize(rows, cols int)

	// Snapshot returns a copy of the current full screen state. The returned
	// value owns its own slices and is safe to retain across further writes.
	Snapshot() Screen

	// Diff returns the changes since the last Diff call together with the
	// current cursor, modes, title, and bell counter. The first call after
	// construction returns a delta that describes every row as changed; this
	// lets a freshly attached client bootstrap without a separate snapshot
	// step.
	Diff() ScreenDelta

	// Modes returns the current terminal modes that affect client behavior.
	Modes() Modes

	// Size returns the current grid size in rows and columns.
	Size() (rows, cols int)

	// Replies returns, and clears, the terminal's replies to queries in
	// the output written to it so far, such as cursor position reports
	// and device attributes. Programs send those queries to learn about
	// their terminal and often wait for the answers, so the replies
	// belong on the PTY's input. Every reply caused by a Write or Resize
	// is available once it returns.
	Replies() []byte

	// TakeHistory returns, and forgets, the lines that have scrolled off
	// the top of the primary screen into its history since the last call,
	// oldest first: logical lines, each joining the rows that soft-wrapped,
	// returned only once complete. The emulator's own scrollback is
	// bounded; a caller that keeps every line has all of history. lost
	// reports whether lines were dropped from the scrollback before they
	// could be returned, as when a single huge write outran it.
	TakeHistory() (lines []Line, lost bool)

	// Checkpoint returns the terminal's complete state, for Restore,
	// including which lines TakeHistory has returned.
	Checkpoint() ([]byte, error)

	// Close releases the terminal's resources. Afterwards the final screen
	// can still be read, but Write fails.
	Close() error
}

// Line is a line of history: a logical line of the primary screen, the
// text of its rows joined, without the blank cells at the end. Cells with
// no text in the middle are spaces.
type Line struct {
	Text string `cbor:"t,omitempty"`

	// Runs are the styles and hyperlinks of Text's parts, in order,
	// covering all of it. If there are none, Text is all unstyled.
	Runs []Run `cbor:"r,omitempty"`

	// Continued is set if the next line continues this one: a single
	// line of over a thousand rows is returned in parts.
	Continued bool `cbor:"c,omitempty"`
}

// Run is a part of a Line with one style and hyperlink.
type Run struct {
	Len   int    `cbor:"n"` // in bytes of the Line's Text
	Style Style  `cbor:"s,omitempty"`
	Link  string `cbor:"l,omitempty"`
}

// Screen is a full snapshot of the canonical terminal state at a point in time.
//
// The CBOR field tags are inert in Go code and only consulted by encoders in
// pkg/proto; keeping them on the canonical type avoids a parallel wire-only
// struct and the conversion plumbing that would come with it.
type Screen struct {
	Rows int `cbor:"r"`
	Cols int `cbor:"c"`

	// Cells is indexed Cells[row][col]; len(Cells) == Rows and
	// len(Cells[i]) == Cols.
	Cells [][]Cell `cbor:"x"`

	Cursor  CursorState `cbor:"cur"`
	Title   string      `cbor:"t,omitempty"`
	BellSeq uint64      `cbor:"b,omitempty"`
	Modes   Modes       `cbor:"m"`

	// Notification is the latest desktop notification the program asked
	// for, if any.
	Notification Notification `cbor:"n,omitempty"`

	// History is how many lines of history there are: the number the
	// next line to scroll off the screen will have. A Terminal doesn't
	// number lines; whatever stores its history does (see TakeHistory).
	History uint64 `cbor:"h,omitempty"`
}

// Notification is a program's request for a desktop notification (OSC 9 or
// OSC 777). Like BellSeq, Seq increases with each one, so that a client
// shows each once, and ignores those from before it attached.
type Notification struct {
	Seq   uint64 `cbor:"s,omitempty"`
	Title string `cbor:"t,omitempty"`
	Body  string `cbor:"b,omitempty"`
}

// validUTF8 returns s with invalid UTF-8 replaced, since CBOR text strings
// must be valid UTF-8 and clients reject screens with invalid ones.
func validUTF8(s string) string { return strings.ToValidUTF8(s, "\uFFFD") }

// Equal reports whether s and o describe the same terminal state: the same
// size, cells, cursor, title, bell counter, and modes.
func (s Screen) Equal(o Screen) bool {
	if s.Rows != o.Rows || s.Cols != o.Cols || s.Cursor != o.Cursor ||
		s.Title != o.Title || s.BellSeq != o.BellSeq || s.Modes != o.Modes ||
		s.Notification != o.Notification || s.History != o.History ||
		len(s.Cells) != len(o.Cells) {
		return false
	}
	for y := range s.Cells {
		if !rowEqual(s.Cells[y], o.Cells[y]) {
			return false
		}
	}
	return true
}

// Cell is a single grid cell.
//
// Grapheme holds the cell's grapheme cluster as UTF-8 text. For a wide
// character that occupies two columns, the first cell has Width == 2 and the
// following cell has Width == 0 and an empty Grapheme; the continuation cell
// is preserved so callers can address the grid by (row, col) without a sparse
// representation.
type Cell struct {
	Grapheme string `cbor:"g,omitempty"`
	Width    uint8  `cbor:"w,omitempty"`
	Style    Style  `cbor:"s,omitempty"`
	Link     string `cbor:"l,omitempty"`
}

// Style describes the rendition of a cell.
type Style struct {
	Fg        Color          `cbor:"f,omitempty"`
	Bg        Color          `cbor:"b,omitempty"`
	Underline UnderlineStyle `cbor:"u,omitempty"`
	Attrs     Attr           `cbor:"a,omitempty"`
}

// Attr is a bitset of text attributes.
type Attr uint16

// Attribute bits. The set mirrors what xterm and modern emulators expose via
// SGR; not all attributes are honored by every renderer, but the canonical
// model preserves them so renderers may choose.
const (
	AttrBold Attr = 1 << iota
	AttrFaint
	AttrItalic
	AttrBlink
	AttrRapidBlink
	AttrReverse
	AttrConceal
	AttrStrikethrough
)

// UnderlineStyle enumerates the underline rendering modes supported by SGR.
// UnderlineNone is the zero value and means no underline.
type UnderlineStyle uint8

const (
	UnderlineNone UnderlineStyle = iota
	UnderlineSingle
	UnderlineDouble
	UnderlineCurly
	UnderlineDotted
	UnderlineDashed
)

// Color is a tagged color value. The zero value (Kind == ColorDefault) means
// the terminal's default foreground or background color; renderers substitute
// their own defaults at display time.
//
// Color is a value type rather than image/color.Color because the wire format
// needs to be compact and stable, and because a value type is cheaper to copy
// into per-cell snapshots.
type Color struct {
	Kind  ColorKind `cbor:"k,omitempty"`
	Index uint8     `cbor:"i,omitempty"`
	R     uint8     `cbor:"r,omitempty"`
	G     uint8     `cbor:"g,omitempty"`
	B     uint8     `cbor:"b,omitempty"`
}

// ColorKind tags a Color.
type ColorKind uint8

const (
	// ColorDefault is the terminal's default color. R, G, B, and Index are
	// unused.
	ColorDefault ColorKind = iota

	// ColorIndexed is an ANSI palette color in [0, 255]. Index holds the
	// value; R, G, B are unused. Indices 0..15 are the basic ANSI colors;
	// 16..255 are the 256-color extended palette.
	ColorIndexed

	// ColorRGB is a 24-bit truecolor. R, G, B hold the channel values;
	// Index is unused.
	ColorRGB
)

// CursorState is the current cursor position and presentation.
type CursorState struct {
	Row     int         `cbor:"r"`
	Col     int         `cbor:"c"`
	Visible bool        `cbor:"v,omitempty"`
	Style   CursorStyle `cbor:"s,omitempty"`
	Blink   bool        `cbor:"b,omitempty"`
}

// CursorStyle enumerates the cursor shape values reported by DECSCUSR.
type CursorStyle uint8

const (
	CursorBlock CursorStyle = iota
	CursorUnderline
	CursorBar
)

// Modes describes terminal modes that change how a client should interpret
// input or render the screen.
//
// Several of them are requests by the program for particular input, such
// as mouse events, which only the client's own terminal can produce. A
// client mirrors those onto its terminal.
type Modes struct {
	AltScreen         bool      `cbor:"a,omitempty"`
	CursorVisible     bool      `cbor:"v,omitempty"`
	BracketedPaste    bool      `cbor:"p,omitempty"`  // 2004: mark pastes
	ApplicationCursor bool      `cbor:"k,omitempty"`  // 1 (DECCKM): cursor keys send SS3 sequences
	Mouse             MouseMode `cbor:"m,omitempty"`  // which mouse events to report
	MouseSGR          bool      `cbor:"ms,omitempty"` // 1006: report mouse events in SGR encoding
	FocusEvents       bool      `cbor:"f,omitempty"`  // 1004: report focus in and out
}

// MouseMode enumerates the mouse tracking modes a terminal may request,
// each a DEC private mode. MouseOff is the zero value and means no mouse
// reporting. How events are encoded is separate; see Modes.MouseSGR.
type MouseMode uint8

const (
	MouseOff    MouseMode = iota
	MouseX10              // 9: button presses
	MouseNormal           // 1000: presses and releases
	MouseButton           // 1002: and motion while a button is down
	MouseAny              // 1003: and all motion
)

// DECMode returns the DEC private mode number that enables m, or 0 for
// MouseOff.
func (m MouseMode) DECMode() int {
	switch m {
	case MouseX10:
		return 9
	case MouseNormal:
		return 1000
	case MouseButton:
		return 1002
	case MouseAny:
		return 1003
	}
	return 0
}
