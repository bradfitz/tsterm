package vt

// Diff returns a ScreenDelta describing the changes from prev to cur. The
// resulting delta is whole-row: any row that differs between prev and cur
// appears as a full CellRun for that row.
//
// If prev is the zero Screen (Rows == 0) the delta reports every row of
// cur, every mode, the title, and any nonzero bell counter as changed.
// That lets a freshly attached client bootstrap from a single Diff call
// without a separate snapshot step.
func Diff(prev, cur Screen) ScreenDelta {
	first := prev.Rows == 0
	d := ScreenDelta{
		Rows:    cur.Rows,
		Cols:    cur.Cols,
		Resized: first || prev.Rows != cur.Rows || prev.Cols != cur.Cols,
		Cursor:  cur.Cursor,
		BellSeq: cur.BellSeq,
		Modes:   cur.Modes,
	}
	d.CursorMoved = first || prev.Cursor != cur.Cursor
	d.TitleSet = first || prev.Title != cur.Title
	if d.TitleSet {
		d.Title = cur.Title
	}
	d.ModesChanged = first || prev.Modes != cur.Modes
	if d.NotificationSet = first || prev.Notification != cur.Notification; d.NotificationSet {
		d.Notification = cur.Notification
	}
	d.Rang = !first && cur.BellSeq != prev.BellSeq
	d.History = cur.History
	d.HistoryChanged = first || cur.History != prev.History
	d.Regions = diffRows(prev.Cells, cur.Cells, cur.Cols)
	return d
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

// ScreenDelta is the diff returned by Terminal.Diff between two observation
// points.
//
// A consumer that already has a prior Screen can apply Regions and the
// metadata fields and arrive at the new screen. A consumer with no prior
// state should request a Snapshot instead. The first Diff after Terminal
// construction reports every row as changed for exactly this reason: a
// freshly attached client receives a fully-populated delta and does not need
// a separate snapshot path.
//
// Regions is sorted by Row and then Col. For the initial release Diff emits
// whole changed rows: every CellRun spans an entire row. The model accepts
// arbitrary runs so we can tighten to minimal runs later without changing
// the wire shape consumers depend on.
type ScreenDelta struct {
	Rows    int  `cbor:"r"`
	Cols    int  `cbor:"c"`
	Resized bool `cbor:"rz,omitempty"`

	Regions []CellRun `cbor:"x,omitempty"`

	Cursor      CursorState `cbor:"cur"`
	CursorMoved bool        `cbor:"cm,omitempty"`

	Title    string `cbor:"t,omitempty"`
	TitleSet bool   `cbor:"ts,omitempty"`

	BellSeq uint64 `cbor:"b,omitempty"`
	Rang    bool   `cbor:"br,omitempty"` // BellSeq changed

	Modes        Modes `cbor:"m"`
	ModesChanged bool  `cbor:"mc,omitempty"`

	Notification    Notification `cbor:"n,omitempty"`
	NotificationSet bool         `cbor:"ns,omitempty"`

	History        uint64 `cbor:"h,omitempty"`
	HistoryChanged bool   `cbor:"hc,omitempty"`
}

// Changed reports whether d describes any change at all.
func (d ScreenDelta) Changed() bool {
	return d.Resized || len(d.Regions) > 0 || d.CursorMoved || d.TitleSet ||
		d.ModesChanged || d.NotificationSet || d.Rang || d.HistoryChanged
}

// CellRun is a horizontal span of cells starting at (Row, Col) and continuing
// rightward for len(Cells) columns.
type CellRun struct {
	Row   int    `cbor:"r"`
	Col   int    `cbor:"c"`
	Cells []Cell `cbor:"x"`
}
