// Package clientui renders canonical screen state into ANSI/SGR byte
// sequences for a user's terminal.
//
// The renderer is stateful: it holds the most recently rendered Screen and
// the SGR style it last left the terminal in, so it can emit minimal
// transitions on the next update. It is meant to drive os.Stdout for the
// tsterm CLI; tests render to a bytes.Buffer and inspect the result.
//
// No prediction or local echo is performed; that is the next slice.
package clientui

import (
	"fmt"
	"io"
	"strings"

	"github.com/bradfitz/tsterm/pkg/proto"
	"github.com/bradfitz/tsterm/pkg/vt"
)

// Renderer applies vt-derived state to a writer as ANSI escape sequences.
type Renderer struct {
	w      io.Writer
	last   vt.Screen
	style  vt.Style
	cur    vt.CursorState
	status string // overlay on the top row, if non-empty
}

// New returns a Renderer that writes to w.
func New(w io.Writer) *Renderer {
	return &Renderer{w: w}
}

// Snapshot replaces the renderer's state with s and redraws every row.
// Use it on initial attach and whenever a keyframe arrives.
func (r *Renderer) Snapshot(snap proto.Snapshot) {
	s := snap.S
	var sb strings.Builder
	// Clear, home, reset.
	sb.WriteString("\x1b[2J\x1b[H\x1b[0m")
	r.style = vt.Style{}
	for y := range s.Rows {
		moveTo(&sb, y, 0)
		writeRow(&sb, &r.style, s.Cells[y])
	}
	r.last = s
	r.cur = s.Cursor
	r.writeStatus(&sb)
	moveTo(&sb, s.Cursor.Row, s.Cursor.Col)
	writeCursorVisibility(&sb, s.Cursor.Visible)
	_, _ = io.WriteString(r.w, sb.String())
}

// Apply applies d to the renderer's state, emitting cursor-positioned per-
// row updates.
func (r *Renderer) Apply(delta proto.Delta) {
	d := delta.D
	if d.Resized {
		// Resized deltas carry every row, so the cleanest path is a full
		// repaint. Build a Screen from the delta and feed it through
		// Snapshot.
		s := r.last
		s.Rows, s.Cols = d.Rows, d.Cols
		s.Cells = make([][]vt.Cell, d.Rows)
		for _, run := range d.Regions {
			if run.Row >= 0 && run.Row < d.Rows {
				row := make([]vt.Cell, d.Cols)
				copy(row, run.Cells)
				s.Cells[run.Row] = row
			}
		}
		// Some rows may not be in Regions if Diff omitted them (shouldn't
		// happen on a resize); fill with blanks.
		for i := range s.Cells {
			if s.Cells[i] == nil {
				s.Cells[i] = blankRow(d.Cols)
			}
		}
		s.Cursor = d.Cursor
		if d.TitleSet {
			s.Title = d.Title
		}
		if d.ModesChanged {
			s.Modes = d.Modes
		}
		r.Snapshot(proto.Snapshot{S: s})
		return
	}

	var sb strings.Builder
	for _, run := range d.Regions {
		moveTo(&sb, run.Row, run.Col)
		writeRow(&sb, &r.style, run.Cells)
		r.applyRun(run)
	}
	if r.status != "" && len(d.Regions) > 0 {
		r.writeStatus(&sb)
		moveTo(&sb, d.Cursor.Row, d.Cursor.Col)
	} else if d.CursorMoved || d.Cursor != r.cur {
		moveTo(&sb, d.Cursor.Row, d.Cursor.Col)
	}
	if d.Cursor.Visible != r.cur.Visible {
		writeCursorVisibility(&sb, d.Cursor.Visible)
	}
	if d.TitleSet {
		// OSC 2 sets the window title; harmless if the user's terminal
		// ignores it.
		sb.WriteString("\x1b]2;")
		sb.WriteString(d.Title)
		sb.WriteString("\x07")
		r.last.Title = d.Title
	}
	if d.BellSeq > r.last.BellSeq {
		sb.WriteString("\x07")
		r.last.BellSeq = d.BellSeq
	}
	r.cur = d.Cursor
	r.last.Cursor = d.Cursor
	if d.ModesChanged {
		r.last.Modes = d.Modes
	}
	_, _ = io.WriteString(r.w, sb.String())
}

// SetStatus overlays msg in reverse video across the top row of the screen.
// The overlay persists across updates until SetStatus("") restores the
// row's real contents.
func (r *Renderer) SetStatus(msg string) {
	if msg == r.status {
		return
	}
	r.status = msg
	var sb strings.Builder
	if msg == "" {
		if len(r.last.Cells) > 0 {
			moveTo(&sb, 0, 0)
			writeRow(&sb, &r.style, r.last.Cells[0])
		}
	} else {
		r.writeStatus(&sb)
	}
	moveTo(&sb, r.cur.Row, r.cur.Col)
	_, _ = io.WriteString(r.w, sb.String())
}

// writeStatus draws the status overlay, if any, leaving the cursor on the
// top row and the SGR state reset.
func (r *Renderer) writeStatus(sb *strings.Builder) {
	if r.status == "" {
		return
	}
	text := []rune(r.status)
	if cols := r.last.Cols; cols > 0 {
		if len(text) > cols {
			text = text[:cols]
		}
		for len(text) < cols {
			text = append(text, ' ')
		}
	}
	moveTo(sb, 0, 0)
	sb.WriteString("\x1b[0;7m")
	sb.WriteString(string(text))
	sb.WriteString("\x1b[0m")
	r.style = vt.Style{}
}

// Close emits a reset so the terminal returns to a sensible state for the
// shell that follows. It also restores cursor visibility.
func (r *Renderer) Close() {
	_, _ = io.WriteString(r.w, "\x1b[0m\x1b[?25h")
}

// applyRun mirrors a CellRun into the renderer's last Screen so subsequent
// diffs compare against a freshly-applied baseline.
func (r *Renderer) applyRun(run vt.CellRun) {
	if run.Row >= len(r.last.Cells) {
		return
	}
	row := r.last.Cells[run.Row]
	if row == nil {
		row = blankRow(r.last.Cols)
		r.last.Cells[run.Row] = row
	}
	for i, c := range run.Cells {
		col := run.Col + i
		if col >= 0 && col < len(row) {
			row[col] = c
		}
	}
}

func blankRow(cols int) []vt.Cell {
	r := make([]vt.Cell, cols)
	for i := range r {
		r[i] = vt.Cell{Grapheme: " ", Width: 1}
	}
	return r
}

func moveTo(sb *strings.Builder, row, col int) {
	// ANSI CUP is 1-based.
	fmt.Fprintf(sb, "\x1b[%d;%dH", row+1, col+1)
}

func writeCursorVisibility(sb *strings.Builder, visible bool) {
	if visible {
		sb.WriteString("\x1b[?25h")
	} else {
		sb.WriteString("\x1b[?25l")
	}
}

// writeRow emits cells starting at the current cursor position. It mutates
// *style as it goes so callers can keep a running SGR state across rows.
func writeRow(sb *strings.Builder, style *vt.Style, cells []vt.Cell) {
	for _, c := range cells {
		if c.Width == 0 {
			// Continuation of a wide cell already emitted; skip.
			continue
		}
		vt.WriteStyleChange(sb, *style, c.Style)
		*style = c.Style
		g := c.Grapheme
		if g == "" {
			g = " "
		}
		sb.WriteString(g)
	}
}
