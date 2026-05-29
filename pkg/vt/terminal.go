// Package vt defines the canonical server-side terminal model used by tsterm.
//
// A Terminal parses PTY output written to its Write method and exposes the
// resulting cell grid, cursor, and mode state as a Screen snapshot or as
// incremental ScreenDelta updates. Transports (native UDP client, WebSocket
// viewer, recorder) consume the snapshots and deltas; they never see raw PTY
// bytes. Making the terminal model canonical is what lets a session fan out
// to multiple consumers without each one running its own emulator.
package vt

import "io"

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
}

// Equal reports whether s and o describe the same terminal state: the same
// size, cells, cursor, title, bell counter, and modes.
func (s Screen) Equal(o Screen) bool {
	if s.Rows != o.Rows || s.Cols != o.Cols || s.Cursor != o.Cursor ||
		s.Title != o.Title || s.BellSeq != o.BellSeq || s.Modes != o.Modes ||
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
type Modes struct {
	AltScreen         bool      `cbor:"a,omitempty"`
	CursorVisible     bool      `cbor:"v,omitempty"`
	BracketedPaste    bool      `cbor:"p,omitempty"`
	ApplicationCursor bool      `cbor:"k,omitempty"`
	Mouse             MouseMode `cbor:"m,omitempty"`
}

// MouseMode enumerates the mouse reporting modes a terminal may request.
// MouseOff is the zero value and means no mouse reporting.
type MouseMode uint8

const (
	MouseOff MouseMode = iota
	MouseX10
	MouseNormal
	MouseButton
	MouseAny
	MouseSGR
)
