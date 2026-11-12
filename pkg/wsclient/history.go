package wsclient

import (
	"github.com/bradfitz/tsterm/pkg/proto"
	"github.com/bradfitz/tsterm/pkg/vt"
)

// History is the part of a session's history a client has loaded: the lines
// that scrolled off the top of its screen, numbered from 0, as the web
// viewer loads them. It starts with the newest Chunk lines, loads new ones
// as they come, and older ones when asked (as when scrolled near the top);
// while following new lines, it keeps at most Keep, and skips to the newest
// when more than Skip behind, as when a program floods the screen.
type History struct {
	From  uint64    // the number of Lines[0]
	Lines []vt.Line // loaded
	Total uint64    // how many there are (the screen's History)
	First uint64    // the oldest the server still has

	started bool
	want    string // the request in flight: "newer" or "older"
}

// History loading sizes, as the web viewer's.
const (
	HistoryChunk = 500
	HistoryKeep  = 5000
	HistorySkip  = 3000
)

// To returns the number of the line after the last loaded.
func (h *History) To() uint64 { return h.From + uint64(len(h.Lines)) }

// Reset forgets everything, as for another session.
func (h *History) Reset() { *h = History{} }

// SetTotal notes how many lines of history there are, from the screen.
func (h *History) SetTotal(n uint64) {
	h.Total = n
	if !h.started {
		h.started = true
		h.From = n - min(n, HistoryChunk)
	}
}

// Next returns the next request to make, if any, and marks it in flight:
// new lines, or, if older, older ones. following says whether the view is
// following new lines, at the bottom.
func (h *History) Next(older, following bool) (from uint64, count int, ok bool) {
	if h.want != "" || !h.started {
		return 0, 0, false
	}
	if to := h.To(); to < h.Total {
		if following && h.Total-to > HistorySkip {
			h.Lines = nil
			h.From = h.Total - HistoryChunk
		}
		h.want = "newer"
		return h.To(), int(min(h.Total-h.To(), 2000)), true
	}
	if older && h.From > h.First {
		from := h.From - min(h.From-h.First, HistoryChunk)
		h.want = "older"
		return from, int(h.From - from), true
	}
	return 0, 0, false
}

// Apply adds the lines of a History frame, answering the request in
// flight. It returns how many lines it added before those it had, which a
// view scrolled up must move by to stay in place. following says whether
// the view is following new lines.
func (h *History) Apply(r *proto.History, following bool) (prepended int) {
	want := h.want
	h.want = ""
	h.First = r.First
	switch want {
	case "newer":
		if r.From > h.To() {
			// The server no longer has the lines asked for: start over.
			h.Lines = nil
			h.From = r.From
		}
		if r.From == h.To() {
			h.Lines = append(h.Lines, r.Lines...)
		}
		if following && len(h.Lines) > HistoryKeep {
			drop := len(h.Lines) - HistoryKeep
			h.Lines = append([]vt.Line(nil), h.Lines[drop:]...)
			h.From += uint64(drop)
		}
	case "older":
		if len(r.Lines) == 0 || r.From+uint64(len(r.Lines)) != h.From {
			h.First = h.From // nothing older to be had
			return 0
		}
		h.Lines = append(append([]vt.Line(nil), r.Lines...), h.Lines...)
		h.From = r.From
		return len(r.Lines)
	}
	return 0
}

// WrapLine returns the rows of cells of the history line l, wrapped at cols
// columns, as a terminal shows it: at least one row, each at most cols
// cells, wide characters taking two, the second with Width 0.
func WrapLine(l vt.Line, cols int) [][]vt.Cell {
	cols = max(cols, 1)
	var rows [][]vt.Cell
	var row []vt.Cell
	runs := l.Runs
	if len(runs) == 0 {
		runs = []vt.Run{{Len: len(l.Text)}}
	}
	off := 0
	for _, r := range runs {
		end := min(off+r.Len, len(l.Text))
		for _, g := range graphemes(l.Text[off:end]) {
			w := RuneWidth(g)
			if len(row)+w > cols {
				rows = append(rows, row)
				row = nil
			}
			row = append(row, vt.Cell{Grapheme: g, Width: uint8(w), Style: r.Style, Link: r.Link})
			if w == 2 {
				row = append(row, vt.Cell{Style: r.Style, Link: r.Link})
			}
		}
		off = end
	}
	return append(rows, row)
}
