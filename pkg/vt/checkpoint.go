package vt

import (
	"encoding/binary"
	"fmt"
	"strings"
)

// A checkpoint, from Terminal.Checkpoint, holds the terminal's complete
// state in libghostty's snapshot format, plus a Repaint of its screen as a
// fallback: libghostty versions its snapshot format separately from its
// API, so a newer tstermd might not read an older one.
//
// Its encoding is checkpointMagic, a uint32 length and the native
// snapshot, then a uint32 length and the repaint (lengths little endian).
// Checkpoints from before libghostty are a bare repaint.
const checkpointMagic = "tsvt\x01"

func encodeCheckpoint(native, repaint []byte) []byte {
	b := make([]byte, 0, len(checkpointMagic)+8+len(native)+len(repaint))
	b = append(b, checkpointMagic...)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(native)))
	b = append(b, native...)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(repaint)))
	return append(b, repaint...)
}

// Restore returns a terminal restored from a checkpoint. rows and cols are
// the terminal's size, for when only the fallback repaint can be used.
func Restore(checkpoint []byte, rows, cols int) (Terminal, error) {
	repaint := checkpoint
	if rest, ok := strings.CutPrefix(string(checkpoint), checkpointMagic); ok {
		d := decoder{b: []byte(rest)}
		native := d.bytes(int(d.u32()))
		repaint = d.bytes(int(d.u32()))
		if d.err != nil {
			return nil, fmt.Errorf("vt: corrupt checkpoint: %w", d.err)
		}
		if t, err := restoreNative(native); err == nil {
			if r, c := t.Size(); r != rows || c != cols {
				t.Resize(rows, cols)
			}
			return t, nil
		}
		// Fall back to the repaint.
	}
	t, err := New(rows, cols)
	if err != nil {
		return nil, err
	}
	t.Write(repaint)
	t.Replies() // a repaint asks nothing, but be sure
	return t, nil
}

// Repaint returns terminal output that, written to a new Terminal of size
// s.Rows×s.Cols, reproduces s: the visible cells with their styles and
// hyperlinks, the cursor, the title, and the modes in s.Modes. Any terminal
// emulator can load it, which makes it the fallback in checkpoints.
//
// It reproduces only what a Screen holds: not the main screen's contents
// while the alternate screen is active, the scroll region, the saved
// cursor, tab stops, character sets, the current SGR pen, a pending
// autowrap, or scrollback.
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
