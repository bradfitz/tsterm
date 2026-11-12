package wsclient

import (
	"github.com/bradfitz/tsterm/pkg/proto"
	"github.com/bradfitz/tsterm/pkg/vt"
)

// State is a session's screen as a Conn's frames describe it.
type State struct {
	Screen vt.Screen

	// Ended, if not empty, says how the session ended; it's then
	// read-only, with its last screen and history.
	Ended string

	// Sizing says whether this client sets the session's size, and if
	// not, who does (see proto.Sizing).
	Sizing proto.Sizing

	lastBell, lastNotification uint64
}

// Change describes what a frame changed.
type Change struct {
	All     bool         // everything, as on a snapshot or resize
	Rows    map[int]bool // rows whose cells changed, unless All
	Cursor  bool
	Title   bool
	Modes   bool
	Rang    bool             // the bell rang
	Notify  *vt.Notification // a new notification
	History bool             // Screen.History changed
	Ended   bool
	Sizing  bool

	// Seq, if not zero, is the Delta's, to Ack once it's shown.
	Seq uint64

	// Lines is a History frame's.
	Lines *proto.History
}

// Apply applies a frame from the server.
func (s *State) Apply(env proto.Envelope) Change {
	var ch Change
	switch {
	case env.Snapshot != nil:
		s.Screen = env.Snapshot.S
		// A bell or notification from before the snapshot is old news.
		s.lastBell = s.Screen.BellSeq
		s.lastNotification = s.Screen.Notification.Seq
		ch = Change{All: true, Cursor: true, Title: true, Modes: true, History: true}
	case env.Delta != nil:
		ch = s.applyDelta(env.Delta.D)
		ch.Seq = env.Delta.Seq
	case env.Sizing != nil:
		s.Sizing = *env.Sizing
		ch.Sizing = true
	case env.Ended != nil:
		s.Ended = env.Ended.Status
		if s.Ended == "" {
			s.Ended = "exited"
		}
		ch.Ended = true
	case env.History != nil:
		ch.Lines = env.History
	}
	return ch
}

func (s *State) applyDelta(d vt.ScreenDelta) Change {
	ch := Change{Rows: map[int]bool{}}
	sc := &s.Screen
	if d.Resized || sc.Rows != d.Rows || sc.Cols != d.Cols {
		cells := make([][]vt.Cell, d.Rows)
		for y := range cells {
			if y < len(sc.Cells) {
				cells[y] = sc.Cells[y]
			}
		}
		sc.Cells, sc.Rows, sc.Cols = cells, d.Rows, d.Cols
		ch.All = true
	}
	for _, run := range d.Regions {
		if run.Row < 0 || run.Row >= len(sc.Cells) {
			continue
		}
		row := sc.Cells[run.Row]
		if need := run.Col + len(run.Cells); len(row) < need {
			row = append(row, make([]vt.Cell, need-len(row))...)
		} else {
			row = append([]vt.Cell(nil), row...)
		}
		copy(row[run.Col:], run.Cells)
		sc.Cells[run.Row] = row
		ch.Rows[run.Row] = true
	}
	if d.CursorMoved || sc.Cursor != d.Cursor {
		ch.Rows[sc.Cursor.Row] = true
		ch.Rows[d.Cursor.Row] = true
		sc.Cursor = d.Cursor
		ch.Cursor = true
	}
	if d.TitleSet {
		sc.Title = d.Title
		ch.Title = true
	}
	if d.ModesChanged || sc.Modes != d.Modes {
		sc.Modes = d.Modes
		ch.Modes = true
	}
	if d.NotificationSet {
		sc.Notification = d.Notification
		if d.Notification.Seq > s.lastNotification {
			s.lastNotification = d.Notification.Seq
			if !d.Resized {
				n := d.Notification
				ch.Notify = &n
			}
		}
	}
	if d.BellSeq > s.lastBell {
		s.lastBell = d.BellSeq
		ch.Rang = !d.Resized
	}
	sc.BellSeq = d.BellSeq
	if d.HistoryChanged {
		sc.History = d.History
		ch.History = true
	}
	return ch
}
