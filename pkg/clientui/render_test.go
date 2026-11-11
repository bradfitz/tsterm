package clientui

import (
	"bytes"
	"strings"
	"testing"

	"github.com/bradfitz/tsterm/pkg/proto"
	"github.com/bradfitz/tsterm/pkg/vt"
)

// makeScreen builds a Screen with rows×cols cells, all populated with text.
func makeScreen(rows, cols int, text string) vt.Screen {
	cells := make([][]vt.Cell, rows)
	for y := range cells {
		row := make([]vt.Cell, cols)
		for x := range row {
			row[x] = vt.Cell{Grapheme: " ", Width: 1}
		}
		cells[y] = row
	}
	for i, r := range text {
		if i >= cols || rows == 0 {
			break
		}
		cells[0][i] = vt.Cell{Grapheme: string(r), Width: 1}
	}
	return vt.Screen{
		Rows: rows, Cols: cols, Cells: cells,
		Cursor: vt.CursorState{Row: 0, Col: len(text), Visible: true},
	}
}

func TestSnapshotEmitsClearHomeAndCells(t *testing.T) {
	var buf bytes.Buffer
	r := New(&buf)
	r.Snapshot(proto.Snapshot{S: makeScreen(2, 5, "hi")})
	out := buf.String()
	if !strings.HasPrefix(out, "\x1b[2J\x1b[H\x1b[0m") {
		t.Errorf("missing clear+home+reset: %q", out)
	}
	if !strings.Contains(out, "hi") {
		t.Errorf("missing text: %q", out)
	}
	// Final cursor placement at (0,2) → ANSI 1-based "1;3H".
	if !strings.Contains(out, "\x1b[1;3H") {
		t.Errorf("missing final cursor placement \\x1b[1;3H: %q", out)
	}
}

func TestApplySingleRowChange(t *testing.T) {
	var buf bytes.Buffer
	r := New(&buf)
	r.Snapshot(proto.Snapshot{S: makeScreen(2, 5, "ab")})
	buf.Reset()
	row := []vt.Cell{
		{Grapheme: "X", Width: 1},
		{Grapheme: "Y", Width: 1},
		{Grapheme: " ", Width: 1},
		{Grapheme: " ", Width: 1},
		{Grapheme: " ", Width: 1},
	}
	r.Apply(proto.Delta{D: vt.ScreenDelta{
		Rows: 2, Cols: 5,
		Cursor: vt.CursorState{Row: 0, Col: 2, Visible: true},
		Regions: []vt.CellRun{
			{Row: 0, Col: 0, Cells: row},
		},
	}})
	out := buf.String()
	// Expect a CUP to (0,0) → "\x1b[1;1H" then XY.
	if !strings.Contains(out, "\x1b[1;1HXY") {
		t.Errorf("did not emit row update: %q", out)
	}
}

func TestApplyResizedDoesFullRepaint(t *testing.T) {
	var buf bytes.Buffer
	r := New(&buf)
	r.Snapshot(proto.Snapshot{S: makeScreen(2, 5, "")})
	buf.Reset()

	// A resize delta describing every row.
	row0 := []vt.Cell{{Grapheme: "A", Width: 1}, {Grapheme: "B", Width: 1}}
	row1 := []vt.Cell{{Grapheme: "C", Width: 1}, {Grapheme: "D", Width: 1}}
	r.Apply(proto.Delta{D: vt.ScreenDelta{
		Rows: 2, Cols: 2, Resized: true,
		Cursor: vt.CursorState{Row: 1, Col: 1, Visible: true},
		Regions: []vt.CellRun{
			{Row: 0, Col: 0, Cells: row0},
			{Row: 1, Col: 0, Cells: row1},
		},
	}})
	out := buf.String()
	if !strings.HasPrefix(out, "\x1b[2J\x1b[H\x1b[0m") {
		t.Errorf("resize should clear+home+reset: %q", out)
	}
	if !strings.Contains(out, "AB") || !strings.Contains(out, "CD") {
		t.Errorf("resize did not redraw both rows: %q", out)
	}
}

func TestStatusOverlay(t *testing.T) {
	var buf bytes.Buffer
	r := New(&buf)
	r.Snapshot(proto.Snapshot{S: makeScreen(2, 8, "top")})
	buf.Reset()

	r.SetStatus("lost")
	if out := buf.String(); !strings.Contains(out, "\x1b[0;7mlost    \x1b[0m") {
		t.Errorf("status not drawn padded in reverse video: %q", out)
	}
	buf.Reset()

	// Updates while the status is shown keep it on top.
	row := []vt.Cell{{Grapheme: "X", Width: 1}}
	r.Apply(proto.Delta{D: vt.ScreenDelta{
		Rows: 2, Cols: 8,
		Regions: []vt.CellRun{{Row: 0, Col: 0, Cells: row}},
		Cursor:  vt.CursorState{Row: 0, Col: 3, Visible: true},
	}})
	if out := buf.String(); !strings.Contains(out, "lost") {
		t.Errorf("status not redrawn after update: %q", out)
	}
	buf.Reset()

	r.SetStatus("")
	out := buf.String()
	if !strings.Contains(out, "Xop") || strings.Contains(out, "lost") {
		t.Errorf("clearing status did not restore row 0: %q", out)
	}
}

func TestModesMirroredToLocalTerminal(t *testing.T) {
	var buf bytes.Buffer
	r := New(&buf)
	s := makeScreen(2, 5, "")
	s.Modes = vt.Modes{Mouse: vt.MouseButton, MouseSGR: true, BracketedPaste: true}
	r.Snapshot(proto.Snapshot{S: s})
	out := buf.String()
	for _, want := range []string{"\x1b[?1002h", "\x1b[?1006h", "\x1b[?2004h"} {
		if !strings.Contains(out, want) {
			t.Errorf("snapshot output lacks %q: %q", want, out)
		}
	}
	if strings.Contains(out, "\x1b[?1004") || strings.Contains(out, "\x1b[?1h") {
		t.Errorf("snapshot set modes the session does not have: %q", out)
	}

	// The program switches to any-motion tracking and drops SGR.
	buf.Reset()
	r.Apply(proto.Delta{D: vt.ScreenDelta{
		Rows: 2, Cols: 5, ModesChanged: true,
		Modes: vt.Modes{Mouse: vt.MouseAny, BracketedPaste: true, FocusEvents: true},
	}})
	out = buf.String()
	for _, want := range []string{"\x1b[?1002l", "\x1b[?1003h", "\x1b[?1006l", "\x1b[?1004h"} {
		if !strings.Contains(out, want) {
			t.Errorf("delta output lacks %q: %q", want, out)
		}
	}
	if strings.Contains(out, "2004") {
		t.Errorf("unchanged bracketed paste re-sent: %q", out)
	}

	// Leaving turns it all off.
	buf.Reset()
	r.Close()
	out = buf.String()
	for _, want := range []string{"\x1b[?1003l", "\x1b[?2004l", "\x1b[?1004l"} {
		if !strings.Contains(out, want) {
			t.Errorf("Close output lacks %q: %q", want, out)
		}
	}
}

func TestNotificationsPassedToLocalTerminal(t *testing.T) {
	var buf bytes.Buffer
	r := New(&buf)
	// A notification that predates attaching, in the first full screen,
	// is not shown.
	s := makeScreen(2, 5, "")
	s.Notification = vt.Notification{Seq: 10, Title: "old", Body: "old"}
	r.Snapshot(proto.Snapshot{S: s})
	if strings.Contains(buf.String(), "old") {
		t.Errorf("notification from before attaching shown: %q", buf.String())
	}

	notify := func(n vt.Notification) string {
		buf.Reset()
		r.Apply(proto.Delta{D: vt.ScreenDelta{Rows: 2, Cols: 5, NotificationSet: true, Notification: n}})
		return buf.String()
	}
	if got := notify(vt.Notification{Seq: 11, Title: "Build; CI", Body: "done"}); !strings.Contains(got, "\x1b]777;notify;Build, CI;done\x07") {
		t.Errorf("titled notification: %q", got)
	}
	if got := notify(vt.Notification{Seq: 12, Body: "just a body"}); !strings.Contains(got, "\x1b]9;just a body\x07") {
		t.Errorf("untitled notification: %q", got)
	}
	// Control characters, which could inject escape sequences, are dropped.
	if got := notify(vt.Notification{Seq: 13, Body: "evil\x07\x1b]2;pwned\x07"}); strings.Contains(got, "\x1b]2") || !strings.Contains(got, "\x1b]9;evil]2;pwned\x07") {
		t.Errorf("unsanitized notification: %q", got)
	}
	// Repeats of the same notification are not shown again.
	if got := notify(vt.Notification{Seq: 13, Body: "again"}); strings.Contains(got, "again") {
		t.Errorf("repeated notification shown: %q", got)
	}
}
