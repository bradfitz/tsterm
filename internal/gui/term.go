package gui

import (
	"image/color"
	"math"
	"net/url"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	"golang.org/x/text/unicode/norm"

	"github.com/bradfitz/tsterm/pkg/vt"
	"github.com/bradfitz/tsterm/pkg/wsclient"
)

// Term shows a session's history and screen, as the web viewer's terminal
// area does: the history above the screen, scrolled through with the wheel,
// in the session's size, with a checkerboard beyond it; and takes the
// user's typing, pastes, and mouse.
//
// It draws only the rows in view, into a TextGrid that size: the rows of
// history, wrapped at the session's width, and then the screen's, form one
// tall column, which scrollBack says how far up from the bottom to show.
type Term struct {
	widget.BaseWidget
	a *App

	grid    *widget.TextGrid
	under   *canvas.Rectangle // the terminal's background, under the grid
	checker *canvas.Raster
	cursor  *canvas.Rectangle // for cursors a cell's colors can't show
	strikes *fyne.Container   // lines through struck-out text, which TextGrid can't draw
	flash   *canvas.Rectangle // the bell
	thumb   *canvas.Rectangle // where the view is in the history

	scrollBack int     // rows above the bottom of the content
	wheel      float32 // wheel scrolling not yet a whole row
	focused    bool
	blinkOff   bool

	// The wrapped rows of the history lines loaded, from histFrom, at
	// histCols columns.
	histRows  [][]vt.Cell
	histFrom  uint64
	histLines int
	histCols  int

	sel *selection

	shift, ctrl, alt, super bool
	suppress                rune // a rune TypedRune skips, after an Alt shortcut
	suppressAt              time.Time

	mouseButton int    // the button held, for mouse reports, or -1
	mouseCell   [2]int // the last cell reported
	pressAt     fyne.Position
	pressCell   [2]int // where the button went down, in content rows

	styles map[cellStyle]*widget.CustomTextGridStyle
}

// selection is a run of text selected, from one content cell to another.
type selection struct {
	from, to [2]int // row, column; to may be before from
}

func newTerm(a *App) *Term {
	t := &Term{
		a:           a,
		grid:        widget.NewTextGrid(),
		under:       canvas.NewRectangle(wsclient.DefaultBg),
		cursor:      canvas.NewRectangle(color.Transparent),
		flash:       canvas.NewRectangle(color.NRGBA{0xff, 0xff, 0xff, 0x40}),
		thumb:       canvas.NewRectangle(color.NRGBA{0x80, 0x80, 0x80, 0x80}),
		strikes:     container.NewWithoutLayout(),
		mouseButton: -1,
		styles:      map[cellStyle]*widget.CustomTextGridStyle{},
	}
	t.grid.Scroll = fyne.ScrollNone
	t.checker = canvas.NewRasterWithPixels(t.checkerPixel)
	t.cursor.StrokeColor = wsclient.DefaultFg
	t.flash.Hide()
	t.thumb.Hide()
	t.ExtendBaseWidget(t)
	return t
}

// cellSize returns the size of a cell, as the TextGrid computes it.
func (t *Term) cellSize() fyne.Size {
	s := fyne.MeasureText("M", theme.TextSize(), fyne.TextStyle{Monospace: true})
	return fyne.NewSize(float32(math.Round(float64(s.Width))), float32(math.Round(float64(s.Height))))
}

// checkerPixel draws the checkerboard beyond the terminal, as the web
// viewer's.
func (t *Term) checkerPixel(x, y, w, h int) color.Color {
	const sq = 6 // pixels
	if (x/sq+y/sq)%2 == 0 {
		return color.NRGBA{0x26, 0x26, 0x26, 0xff}
	}
	return color.NRGBA{0x1a, 0x1a, 0x1a, 0xff}
}

// fitSize returns the most rows and columns the view holds.
func (t *Term) fitSize() (rows, cols int) {
	cs, s := t.cellSize(), t.Size()
	return max(3, int(s.Height/cs.Height)), max(10, int(s.Width/cs.Width))
}

// contentRows returns the rows of history in view and the screen's.
func (t *Term) contentRows() (hist [][]vt.Cell, screen [][]vt.Cell) {
	st := &t.a.st
	if !st.Screen.Modes.AltScreen {
		t.wrapHistory(st.Screen.Cols)
		hist = t.histRows
	}
	return hist, st.Screen.Cells
}

// wrapHistory updates histRows for the history loaded, at cols columns.
func (t *Term) wrapHistory(cols int) {
	h := &t.a.hist
	if t.histCols == cols && t.histFrom == h.From && t.histLines == len(h.Lines) {
		return
	}
	// Rewrap only what's changed, if the width hasn't: lines are added
	// at the ends, and dropped from the start.
	if t.histCols == cols && h.From <= t.histFrom && h.To() >= t.histFrom+uint64(t.histLines) && t.histLines > 0 {
		var before, after [][]vt.Cell
		for _, l := range h.Lines[:t.histFrom-h.From] {
			before = append(before, wsclient.WrapLine(l, cols)...)
		}
		for _, l := range h.Lines[int(t.histFrom-h.From)+t.histLines:] {
			after = append(after, wsclient.WrapLine(l, cols)...)
		}
		t.histRows = append(append(before, t.histRows...), after...)
	} else {
		t.histRows = t.histRows[:0]
		for _, l := range h.Lines {
			t.histRows = append(t.histRows, wsclient.WrapLine(l, cols)...)
		}
	}
	t.histFrom, t.histLines, t.histCols = h.From, len(h.Lines), cols
}

// wrappedRows returns how many rows lines take at the current width.
func (t *Term) wrappedRows(lines []vt.Line) int {
	n := 0
	for _, l := range lines {
		n += len(wsclient.WrapLine(l, max(1, t.a.st.Screen.Cols)))
	}
	return n
}

// pinned reports whether the view is at the bottom, following new output.
func (t *Term) pinned() bool { return t.scrollBack == 0 }

// nearTop reports whether the view is near the top of the history loaded.
func (t *Term) nearTop() bool {
	hist, screen := t.contentRows()
	viewRows, _ := t.fitSize()
	top := len(hist) + len(screen) - viewRows - t.scrollBack
	return top < 2*viewRows
}

// scrollBy scrolls the view n rows up (or down, for negative n).
func (t *Term) scrollBy(n int) {
	hist, screen := t.contentRows()
	viewRows, _ := t.fitSize()
	t.scrollBack = min(max(0, t.scrollBack+n), max(0, len(hist)+len(screen)-viewRows))
	t.refreshView()
	if n > 0 {
		t.a.loadHistory()
	}
}

// scrollToBottom shows the screen again, as typing does.
func (t *Term) scrollToBottom() {
	if t.scrollBack != 0 {
		t.scrollBack = 0
		t.refreshView()
	}
}

// view returns the rows in view, the content row of the first, and how
// many history rows there are.
func (t *Term) view() (rows [][]vt.Cell, top, nhist int) {
	hist, screen := t.contentRows()
	viewRows, _ := t.fitSize()
	total := len(hist) + len(screen)
	t.scrollBack = min(t.scrollBack, max(0, total-viewRows))
	top = max(0, total-viewRows-t.scrollBack)
	for i := top; i < min(total, top+viewRows); i++ {
		if i < len(hist) {
			rows = append(rows, hist[i])
		} else {
			rows = append(rows, screen[i-len(hist)])
		}
	}
	return rows, top, len(hist)
}

// cellStyle is what TextGrid styles are made of, to share them.
type cellStyle struct {
	fg, bg     color.RGBA
	defaultBg  bool
	bold       bool
	italic     bool
	underlined bool
}

func (t *Term) style(st vt.Style, invert bool) *widget.CustomTextGridStyle {
	fg, bg, defaultBg := wsclient.Colors(st)
	if invert {
		fg, bg, defaultBg = bg, fg, false
	}
	k := cellStyle{fg: fg, bg: bg, defaultBg: defaultBg,
		bold: st.Attrs&vt.AttrBold != 0, italic: st.Attrs&vt.AttrItalic != 0, underlined: st.Underline != vt.UnderlineNone}
	if s, ok := t.styles[k]; ok {
		return s
	}
	s := &widget.CustomTextGridStyle{
		TextStyle: fyne.TextStyle{Monospace: true, Bold: k.bold, Italic: k.italic, Underline: k.underlined},
		FGColor:   fg,
	}
	if !defaultBg {
		s.BGColor = bg
	}
	t.styles[k] = s
	return s
}

// cellRune returns the rune a TextGrid can show of grapheme g: one, so
// combining marks are composed if they can be.
func cellRune(g string) rune {
	if g == "" {
		return ' '
	}
	if len(g) > 1 {
		g = norm.NFC.String(g)
	}
	r, _ := utf8.DecodeRuneInString(g)
	return r
}

// refreshView redraws the rows in view.
func (t *Term) refreshView() {
	rows, top, nhist := t.view()
	st := &t.a.st
	_, viewCols := t.fitSize()
	cols := min(st.Screen.Cols, viewCols)
	cur := st.Screen.Cursor
	cursorRow := -1
	if cur.Visible && st.Ended == "" && !t.blinkOff {
		cursorRow = nhist + cur.Row - top
	}
	block := t.focused && t.a.foreground && cur.Style == vt.CursorBlock
	var selFrom, selTo [2]int
	if t.sel != nil {
		selFrom, selTo = t.sel.from, t.sel.to
		if less(selTo, selFrom) {
			selFrom, selTo = selTo, selFrom
		}
	}
	grid := make([]widget.TextGridRow, len(rows))
	cs := t.cellSize()
	var strikes []fyne.CanvasObject
	strike := func(y, x0, x1 int, c color.Color) {
		l := canvas.NewLine(c)
		l.StrokeWidth = 1
		mid := float32(y)*cs.Height + cs.Height*0.55
		l.Position1 = fyne.NewPos(float32(x0)*cs.Width, mid)
		l.Position2 = fyne.NewPos(float32(x1)*cs.Width, mid)
		strikes = append(strikes, l)
	}
	for y, row := range rows {
		struck := -1 // where struck-out text started in this row
		cells := make([]widget.TextGridCell, cols)
		for x := range cols {
			var c vt.Cell
			if x < len(row) {
				c = row[x]
			} else {
				c.Width = 1
			}
			invert := y == cursorRow && x == cur.Col && block
			if t.sel != nil {
				p := [2]int{top + y, x}
				invert = invert != (!less(p, selFrom) && less(p, selTo))
			}
			r := cellRune(c.Grapheme)
			if c.Width == 0 {
				r = ' ' // the second column of a wide character
			}
			cells[x] = widget.TextGridCell{Rune: r, Style: t.style(c.Style, invert)}
			on := c.Style.Attrs&vt.AttrStrikethrough != 0
			if on && struck < 0 {
				struck = x
			} else if !on && struck >= 0 {
				fg, _, _ := wsclient.Colors(row[struck].Style)
				strike(y, struck, x, fg)
				struck = -1
			}
		}
		if struck >= 0 {
			fg, _, _ := wsclient.Colors(row[struck].Style)
			strike(y, struck, cols, fg)
		}
		grid[y] = widget.TextGridRow{Cells: cells}
	}
	t.grid.Rows = grid
	t.grid.Refresh()
	t.strikes.Objects = strikes
	t.strikes.Refresh()

	size := fyne.NewSize(float32(cols)*cs.Width, float32(len(rows))*cs.Height)
	t.grid.Resize(size)
	t.under.Resize(size)

	// Cursors a cell's colors can't show.
	if cursorRow >= 0 && cursorRow < len(rows) && cur.Col < cols && !block {
		pos := fyne.NewPos(float32(cur.Col)*cs.Width, float32(cursorRow)*cs.Height)
		t.cursor.FillColor, t.cursor.StrokeWidth = color.Transparent, 0
		switch {
		case !t.focused || !t.a.foreground:
			t.cursor.StrokeWidth = 1 // hollow
			t.cursor.Move(pos)
			t.cursor.Resize(cs)
		case cur.Style == vt.CursorUnderline:
			t.cursor.FillColor = wsclient.DefaultFg
			t.cursor.Move(pos.AddXY(0, cs.Height-2))
			t.cursor.Resize(fyne.NewSize(cs.Width, 2))
		default: // a bar
			t.cursor.FillColor = wsclient.DefaultFg
			t.cursor.Move(pos)
			t.cursor.Resize(fyne.NewSize(2, cs.Height))
		}
		t.cursor.Show()
		t.cursor.Refresh()
	} else {
		t.cursor.Hide()
	}

	// Where the view is in the history, if there's any to scroll to.
	total := nhist + len(st.Screen.Cells)
	if viewRows, _ := t.fitSize(); total > viewRows {
		h := t.Size().Height
		thumbH := max(12, h*float32(viewRows)/float32(total))
		y := (h - thumbH) * float32(top) / float32(max(1, total-viewRows))
		t.thumb.Move(fyne.NewPos(t.Size().Width-5, y))
		t.thumb.Resize(fyne.NewSize(4, thumbH))
		t.thumb.Show()
	} else {
		t.thumb.Hide()
	}
	t.thumb.Refresh()
	t.under.Refresh()
}

func less(a, b [2]int) bool { return a[0] < b[0] || a[0] == b[0] && a[1] < b[1] }

// ring flashes the terminal, for the bell.
func (t *Term) ring() {
	t.flash.Resize(t.under.Size())
	t.flash.Show()
	t.flash.Refresh()
	time.AfterFunc(150*time.Millisecond, func() {
		fyne.Do(func() { t.flash.Hide() })
	})
}

// blink toggles a blinking cursor.
func (t *Term) blink() {
	cur := t.a.st.Screen.Cursor
	if !cur.Blink || !t.focused || !t.a.foreground {
		if t.blinkOff {
			t.blinkOff = false
			t.refreshView()
		}
		return
	}
	t.blinkOff = !t.blinkOff
	t.refreshView()
}

// CreateRenderer implements fyne.Widget.
func (t *Term) CreateRenderer() fyne.WidgetRenderer {
	return &termRenderer{t: t}
}

type termRenderer struct{ t *Term }

func (r *termRenderer) Layout(size fyne.Size) {
	r.t.checker.Resize(size)
	r.t.refreshView()
	r.t.a.termResized()
}

func (r *termRenderer) MinSize() fyne.Size {
	cs := r.t.cellSize()
	return fyne.NewSize(cs.Width*10, cs.Height*3)
}

func (r *termRenderer) Refresh() { r.t.refreshView() }

func (r *termRenderer) Objects() []fyne.CanvasObject {
	t := r.t
	return []fyne.CanvasObject{t.checker, t.under, t.grid, t.strikes, t.cursor, t.flash, t.thumb}
}

func (r *termRenderer) Destroy() {}

// Keyboard.

// FocusGained implements fyne.Focusable.
func (t *Term) FocusGained() {
	t.focused = true
	t.refreshView()
}

// FocusLost implements fyne.Focusable.
func (t *Term) FocusLost() {
	t.focused = false
	t.shift, t.ctrl, t.alt, t.super = false, false, false, false
	t.refreshView()
}

// AcceptsTab implements fyne.Tabbable: Tab is for the program.
func (t *Term) AcceptsTab() bool { return true }

// TypedRune implements fyne.Focusable.
func (t *Term) TypedRune(r rune) {
	if r == t.suppress && time.Since(t.suppressAt) < 100*time.Millisecond {
		t.suppress = 0
		return
	}
	t.a.typeInput(string(r))
}

// keyNames maps Fyne's key names to the web's, which wsclient uses.
var keyNames = map[fyne.KeyName]string{
	fyne.KeyReturn: "Enter", fyne.KeyEnter: "Enter", fyne.KeyBackspace: "Backspace",
	fyne.KeyTab: "Tab", fyne.KeyEscape: "Escape",
	fyne.KeyUp: "ArrowUp", fyne.KeyDown: "ArrowDown", fyne.KeyLeft: "ArrowLeft", fyne.KeyRight: "ArrowRight",
	fyne.KeyHome: "Home", fyne.KeyEnd: "End", fyne.KeyInsert: "Insert", fyne.KeyDelete: "Delete",
	fyne.KeyPageUp: "PageUp", fyne.KeyPageDown: "PageDown",
	fyne.KeyF1: "F1", fyne.KeyF2: "F2", fyne.KeyF3: "F3", fyne.KeyF4: "F4", fyne.KeyF5: "F5", fyne.KeyF6: "F6",
	fyne.KeyF7: "F7", fyne.KeyF8: "F8", fyne.KeyF9: "F9", fyne.KeyF10: "F10", fyne.KeyF11: "F11", fyne.KeyF12: "F12",
}

// keyFor returns the wsclient.Key for a Fyne key name, with modifiers.
func keyFor(name fyne.KeyName, shift, alt, ctrl bool) (wsclient.Key, bool) {
	k := wsclient.Key{Shift: shift, Alt: alt, Ctrl: ctrl}
	if n, ok := keyNames[name]; ok {
		k.Name = n
		return k, true
	}
	if name == fyne.KeySpace {
		k.Rune = ' '
		return k, true
	}
	s := string(name)
	if utf8.RuneCountInString(s) != 1 {
		return k, false
	}
	r, _ := utf8.DecodeRuneInString(s)
	if r >= 'A' && r <= 'Z' && !shift {
		r += 'a' - 'A'
	}
	k.Rune = r
	return k, true
}

// TypedKey implements fyne.Focusable, for keys that aren't characters.
func (t *Term) TypedKey(ev *fyne.KeyEvent) {
	if t.shift && (ev.Name == fyne.KeyPageUp || ev.Name == fyne.KeyPageDown) {
		// Shift-PageUp and -PageDown scroll the history, as terminals do.
		viewRows, _ := t.fitSize()
		if ev.Name == fyne.KeyPageDown {
			viewRows = -viewRows
		}
		t.scrollBy(viewRows - 1)
		return
	}
	k, ok := keyFor(ev.Name, t.shift, false, false)
	if !ok || k.Name == "" {
		return // characters come as TypedRune
	}
	t.a.typeInput(wsclient.KeyBytes(k, t.a.st.Screen.Modes.ApplicationCursor))
}

// KeyDown implements desktop.Keyable, to track the modifiers.
func (t *Term) KeyDown(ev *fyne.KeyEvent) { t.setModifier(ev.Name, true) }

// KeyUp implements desktop.Keyable.
func (t *Term) KeyUp(ev *fyne.KeyEvent) { t.setModifier(ev.Name, false) }

func (t *Term) setModifier(name fyne.KeyName, down bool) {
	switch name {
	case desktop.KeyShiftLeft, desktop.KeyShiftRight:
		t.shift = down
	case desktop.KeyControlLeft, desktop.KeyControlRight:
		t.ctrl = down
	case desktop.KeyAltLeft, desktop.KeyAltRight:
		t.alt = down
	case desktop.KeySuperLeft, desktop.KeySuperRight:
		t.super = down
	}
}

// mac says whether copying and pasting are Cmd-C and Cmd-V, as on macOS,
// rather than Ctrl-Shift-C and Ctrl-Shift-V, as in Linux terminals, whose
// Ctrl-C is for the program.
var mac = runtime.GOOS == "darwin"

// TypedShortcut implements fyne.Shortcutable. Keys typed with Ctrl, Alt,
// or Cmd come as shortcuts.
func (t *Term) TypedShortcut(s fyne.Shortcut) {
	ctrlKey := func(r rune) { t.a.typeInput(wsclient.KeyBytes(wsclient.Key{Rune: r, Ctrl: true}, false)) }
	switch s := s.(type) {
	case *fyne.ShortcutCopy:
		if mac || s.Secondary { // Cmd-C, or Ctrl-Insert
			t.copySelection()
		} else {
			ctrlKey('c')
		}
		return
	case *fyne.ShortcutPaste:
		if mac || s.Secondary { // Cmd-V, or Shift-Insert
			t.paste()
		} else {
			ctrlKey('v')
		}
		return
	case *fyne.ShortcutCut:
		if !mac && !s.Secondary {
			ctrlKey('x')
		}
		return
	case *fyne.ShortcutUndo:
		if !mac {
			ctrlKey('z')
		}
		return
	case *fyne.ShortcutRedo:
		if !mac {
			ctrlKey('y')
		}
		return
	case *fyne.ShortcutSelectAll:
		if !mac {
			ctrlKey('a')
		}
		return
	case *desktop.CustomShortcut:
		mod := s.Modifier
		shift, ctrl, alt := mod&fyne.KeyModifierShift != 0, mod&fyne.KeyModifierControl != 0, mod&fyne.KeyModifierAlt != 0
		if mod&fyne.KeyModifierSuper != 0 {
			return // Cmd and the Windows key are the system's
		}
		if ctrl && shift && !alt && !mac {
			switch s.KeyName {
			case fyne.KeyC:
				t.copySelection()
				return
			case fyne.KeyV:
				t.paste()
				return
			}
		}
		k, ok := keyFor(s.KeyName, shift, alt, ctrl)
		if !ok {
			return
		}
		if alt && k.Rune != 0 {
			// The character may come as TypedRune too.
			t.suppress, t.suppressAt = k.Rune, time.Now()
		}
		t.a.typeInput(wsclient.KeyBytes(k, t.a.st.Screen.Modes.ApplicationCursor))
	}
}

// paste types the clipboard's text, marked as pasted if the program asked.
func (t *Term) paste() {
	text := fyne.CurrentApp().Clipboard().Content()
	if text != "" {
		t.a.typeInput(wsclient.Paste(text, t.a.st.Screen.Modes.BracketedPaste))
	}
}

// copySelection copies the text selected to the clipboard.
func (t *Term) copySelection() {
	if text := t.selectedText(); text != "" {
		fyne.CurrentApp().Clipboard().SetContent(text)
	}
}

// selectedText returns the selection's text, a line per row, without
// their blank ends.
func (t *Term) selectedText() string {
	if t.sel == nil {
		return ""
	}
	from, to := t.sel.from, t.sel.to
	if less(to, from) {
		from, to = to, from
	}
	hist, screen := t.contentRows()
	var b strings.Builder
	for y := from[0]; y <= to[0] && y < len(hist)+len(screen); y++ {
		var row []vt.Cell
		if y < len(hist) {
			row = hist[y]
		} else {
			row = screen[y-len(hist)]
		}
		x0, x1 := 0, len(row)
		if y == from[0] {
			x0 = min(from[1], len(row))
		}
		if y == to[0] {
			x1 = min(to[1], len(row))
		}
		var line strings.Builder
		for _, c := range row[x0:max(x0, x1)] {
			if c.Width != 0 {
				line.WriteString(c.Grapheme)
			}
		}
		b.WriteString(strings.TrimRight(line.String(), " "))
		if y < to[0] {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// Mouse.

// cellAt returns the content row and column at the position p.
func (t *Term) cellAt(p fyne.Position) (row, col int) {
	cs := t.cellSize()
	_, top, _ := t.view()
	return top + max(0, int(p.Y/cs.Height)), max(0, int(p.X/cs.Width))
}

// screenCell returns the screen cell at p, if it's on the screen.
func (t *Term) screenCell(p fyne.Position) (x, y int, ok bool) {
	row, col := t.cellAt(p)
	hist, _ := t.contentRows()
	y = row - len(hist)
	st := &t.a.st.Screen
	return col, y, y >= 0 && y < st.Rows && col < st.Cols
}

// mouseWanted reports whether the program gets the mouse: it asked, and
// Shift isn't held, which keeps the mouse for selecting text.
func (t *Term) mouseWanted(mod fyne.KeyModifier) bool {
	return t.a.canType() && t.a.st.Screen.Modes.Mouse != vt.MouseOff && mod&fyne.KeyModifierShift == 0
}

func (t *Term) report(button, x, y int, release bool, mod fyne.KeyModifier) {
	m := t.a.st.Screen.Modes
	t.a.send(wsclient.MouseReport(button, x, y, release,
		mod&fyne.KeyModifierShift != 0, mod&fyne.KeyModifierAlt != 0, mod&fyne.KeyModifierControl != 0, m.MouseSGR))
}

func mouseButton(b desktop.MouseButton) int {
	switch b {
	case desktop.MouseButtonTertiary:
		return wsclient.MouseMiddle
	case desktop.MouseButtonSecondary:
		return wsclient.MouseRight
	}
	return wsclient.MouseLeft
}

// MouseDown implements desktop.Mouseable.
func (t *Term) MouseDown(ev *desktop.MouseEvent) {
	t.a.win.Canvas().Focus(t)
	t.pressAt = ev.Position
	if t.mouseWanted(ev.Modifier) {
		if x, y, ok := t.screenCell(ev.Position); ok {
			t.mouseButton = mouseButton(ev.Button)
			t.mouseCell = [2]int{x, y}
			t.report(t.mouseButton, x, y, false, ev.Modifier)
		}
		return
	}
	if ev.Button == desktop.MouseButtonPrimary {
		row, col := t.cellAt(ev.Position)
		t.pressCell = [2]int{row, col}
		if t.sel != nil {
			t.sel = nil
			t.refreshView()
		}
	}
}

// MouseUp implements desktop.Mouseable.
func (t *Term) MouseUp(ev *desktop.MouseEvent) {
	if t.mouseButton >= 0 {
		x, y, _ := t.screenCell(ev.Position)
		if t.a.st.Screen.Modes.Mouse >= vt.MouseNormal { // X10 mode reports presses only
			t.report(t.mouseButton, x, y, true, ev.Modifier)
		}
		t.mouseButton = -1
		return
	}
	// A click on a hyperlink, not a drag, opens it.
	if t.sel == nil && ev.Button == desktop.MouseButtonPrimary {
		if c, ok := t.contentCell(t.cellAt(ev.Position)); ok && c.Link != "" {
			if u, err := url.Parse(c.Link); err == nil && (u.Scheme == "http" || u.Scheme == "https") {
				fyne.CurrentApp().OpenURL(u)
			}
		}
	}
}

// contentCell returns the cell at a content row and column.
func (t *Term) contentCell(row, col int) (vt.Cell, bool) {
	hist, screen := t.contentRows()
	var r []vt.Cell
	switch {
	case row < len(hist):
		r = hist[row]
	case row-len(hist) < len(screen):
		r = screen[row-len(hist)]
	}
	if col < len(r) {
		return r[col], true
	}
	return vt.Cell{}, false
}

// Dragged implements fyne.Draggable: it selects text, or reports drags to
// a program that wants them.
func (t *Term) Dragged(ev *fyne.DragEvent) {
	if t.mouseButton >= 0 {
		m := t.a.st.Screen.Modes.Mouse
		if m != vt.MouseButton && m != vt.MouseAny {
			return
		}
		x, y, ok := t.screenCell(ev.Position)
		if ok && [2]int{x, y} != t.mouseCell {
			t.mouseCell = [2]int{x, y}
			t.report(wsclient.MouseMotion+t.mouseButton, x, y, false, 0)
		}
		return
	}
	row, col := t.cellAt(ev.Position)
	if t.sel == nil {
		t.sel = &selection{from: t.pressCell}
	}
	t.sel.to = [2]int{row, col}
	t.refreshView()
}

// DragEnd implements fyne.Draggable.
func (t *Term) DragEnd() {
	if t.mouseButton >= 0 {
		x, y := t.mouseCell[0], t.mouseCell[1]
		if t.a.st.Screen.Modes.Mouse >= vt.MouseNormal {
			t.report(t.mouseButton, x, y, true, 0)
		}
		t.mouseButton = -1
	}
}

// MouseIn implements desktop.Hoverable.
func (t *Term) MouseIn(*desktop.MouseEvent) {}

// MouseOut implements desktop.Hoverable.
func (t *Term) MouseOut() {}

// MouseMoved implements desktop.Hoverable, for programs that want all
// motion.
func (t *Term) MouseMoved(ev *desktop.MouseEvent) {
	if !t.mouseWanted(ev.Modifier) || t.a.st.Screen.Modes.Mouse != vt.MouseAny {
		return
	}
	if x, y, ok := t.screenCell(ev.Position); ok && [2]int{x, y} != t.mouseCell {
		t.mouseCell = [2]int{x, y}
		t.report(wsclient.MouseMotion+3, x, y, false, ev.Modifier) // 3: no button
	}
}

// Scrolled implements fyne.Scrollable: the wheel scrolls the history, or
// goes to a program that wants the mouse.
func (t *Term) Scrolled(ev *fyne.ScrollEvent) {
	dy := ev.Scrolled.DY
	if dy == 0 {
		return
	}
	if t.mouseWanted(0) && !t.shift {
		if x, y, ok := t.screenCell(ev.Position); ok {
			b := wsclient.MouseWheelUp
			if dy < 0 {
				b = wsclient.MouseWheelDown
			}
			t.report(b, x, y, false, 0)
		}
		return
	}
	// About three rows a notch of a wheel, which Fyne makes 10; less
	// from a trackpad, whose small moves add up.
	t.wheel += dy * 3 / 10
	n := int(t.wheel)
	t.wheel -= float32(n)
	if n != 0 {
		t.scrollBy(n)
	}
}
