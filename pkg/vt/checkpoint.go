package vt

import (
	"fmt"
	"strings"
)

// Repaint returns terminal output that, written to a new Terminal of size
// s.Rows×s.Cols, reproduces s: the visible cells with their styles and
// hyperlinks, the cursor, the title, and the modes in s.Modes.
//
// A Repaint serves as a checkpoint of terminal state that survives a
// restart of the process holding the Terminal, and that any terminal
// emulator can load, including a newer version of this package's.
//
// Some state cannot be read from the emulator and is not reproduced: the
// main screen's contents while the alternate screen is active, the scroll
// region, the saved cursor, tab stops, character sets, the current SGR pen,
// and a pending autowrap. Programs that use these generally redraw on their
// next resize.
//
// TODO(bradfitz): Capture the missing state. That needs either an exported
// checkpoint method in github.com/charmbracelet/x/vt (sent upstream) or a
// fork of it.
func (s Screen) Repaint() []byte {
	var sb strings.Builder
	if s.Modes.AltScreen {
		sb.WriteString("\x1b[?1049h")
	}
	var style Style
	var link string
	for y, row := range s.Cells {
		fmt.Fprintf(&sb, "\x1b[%d;1H", y+1)
		for _, c := range row {
			if c.Width == 0 {
				continue // continuation of a wide cell
			}
			WriteStyleChange(&sb, style, c.Style)
			style = c.Style
			if c.Link != link {
				// OSC 8 hyperlink; an empty URL ends the link.
				sb.WriteString("\x1b]8;;" + c.Link + "\x1b\\")
				link = c.Link
			}
			g := c.Grapheme
			if g == "" {
				g = " "
			}
			sb.WriteString(g)
		}
	}
	if link != "" {
		sb.WriteString("\x1b]8;;\x1b\\")
	}
	sb.WriteString("\x1b[0m")

	if s.Title != "" {
		sb.WriteString("\x1b]2;" + s.Title + "\x07")
	}
	if s.Modes.ApplicationCursor {
		sb.WriteString("\x1b[?1h")
	}
	if s.Modes.BracketedPaste {
		sb.WriteString("\x1b[?2004h")
	}
	if m := s.Modes.Mouse.DECMode(); m != 0 {
		fmt.Fprintf(&sb, "\x1b[?%dh", m)
	}
	if s.Modes.MouseSGR {
		sb.WriteString("\x1b[?1006h")
	}
	if s.Modes.FocusEvents {
		sb.WriteString("\x1b[?1004h")
	}

	// DECSCUSR numbers each shape's blinking variant first.
	shape := 1
	switch s.Cursor.Style {
	case CursorUnderline:
		shape = 3
	case CursorBar:
		shape = 5
	}
	if !s.Cursor.Blink {
		shape++
	}
	fmt.Fprintf(&sb, "\x1b[%d q", shape)
	if !s.Cursor.Visible {
		sb.WriteString("\x1b[?25l")
	}
	fmt.Fprintf(&sb, "\x1b[%d;%dH", s.Cursor.Row+1, s.Cursor.Col+1)
	return []byte(sb.String())
}

// BoundaryScanner follows just enough of the terminal output grammar to
// report whether the output seen so far ends between complete characters
// and escape sequences. A checkpoint is only valid at such a boundary: if
// it were taken partway through a sequence, the rest of the sequence would
// be replayed without its beginning.
//
// The zero value is at a boundary.
type BoundaryScanner struct {
	state    scanState
	utf8Need int // continuation bytes still expected
}

type scanState uint8

const (
	scanGround    scanState = iota
	scanEsc                 // after ESC
	scanEscInter            // after ESC and an intermediate byte
	scanCSI                 // in a control sequence
	scanString              // in OSC, DCS, SOS, PM, or APC
	scanStringEsc           // after ESC in a string, which may begin ST
)

// Write scans p. It never fails.
func (s *BoundaryScanner) Write(p []byte) (int, error) {
	for _, b := range p {
		s.scan(b)
	}
	return len(p), nil
}

// AtBoundary reports whether the output so far ends at a boundary.
func (s *BoundaryScanner) AtBoundary() bool {
	return s.state == scanGround && s.utf8Need == 0
}

func (s *BoundaryScanner) scan(b byte) {
	switch {
	case b == 0x18 || b == 0x1a: // CAN and SUB abort any sequence
		s.state, s.utf8Need = scanGround, 0
		return
	case b == 0x1b && s.state != scanString:
		s.state, s.utf8Need = scanEsc, 0
		return
	}
	switch s.state {
	case scanGround:
		switch {
		case b >= 0xc0:
			s.utf8Need = 1
			if b >= 0xe0 {
				s.utf8Need = 2
			}
			if b >= 0xf0 {
				s.utf8Need = 3
			}
		case b >= 0x80:
			if s.utf8Need > 0 {
				s.utf8Need--
			}
		default:
			s.utf8Need = 0
		}
	case scanEsc:
		switch {
		case b == '[':
			s.state = scanCSI
		case b == ']' || b == 'P' || b == 'X' || b == '^' || b == '_':
			s.state = scanString
		case b >= 0x20 && b <= 0x2f:
			s.state = scanEscInter
		case b >= 0x30 && b <= 0x7e:
			s.state = scanGround
		}
	case scanEscInter:
		if b >= 0x30 && b <= 0x7e {
			s.state = scanGround
		}
	case scanCSI:
		if b >= 0x40 && b <= 0x7e {
			s.state = scanGround
		}
	case scanString:
		switch b {
		case 0x07: // BEL ends OSC
			s.state = scanGround
		case 0x1b:
			s.state = scanStringEsc
		}
	case scanStringEsc:
		if b == '\\' {
			s.state = scanGround
		} else {
			// An ESC not forming ST ends the string and starts a new
			// sequence, as in a terminal.
			s.state = scanEsc
			s.scan(b)
		}
	}
}
