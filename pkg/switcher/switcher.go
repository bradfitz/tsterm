// Package switcher is tsterm's session switcher: a menu of the running
// sessions, which tstermd draws over a terminal client's screen, as GNU
// Screen's C-a " does, for the user to switch to another session without
// reattaching. The client doesn't know about it: it sees only a screen,
// the session's, dimmed and still live, with the menu's box on top, and
// its keys go to the menu while it's open.
//
// The menu is a Bubble Tea program, run without a terminal: its output
// goes into an emulator of its own, the size of the box, whose screen
// Compose draws over the session's.
package switcher

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"

	"github.com/bradfitz/tsterm/pkg/vt"
)

// Item is a session in the menu.
type Item struct {
	ID       string
	Name     string
	What     string // what it runs, such as its command; empty for a shell
	Created  time.Time
	Activity time.Time // its last input or output
	Watchers int
}

// Menu is an open session switcher.
type Menu struct {
	term    vt.Terminal
	prog    *tea.Program
	cancel  context.CancelFunc
	input   chan []byte
	changed chan struct{}
	done    chan struct{}

	n int // sessions when opened, which the box's height fits

	mu     sync.Mutex
	choice string // the session chosen, once done
}

// Open opens a menu of the sessions items returns, which it calls again
// every second while open, for a client whose screen is rows by cols and
// which is looking at the session current.
func Open(items func() []Item, current string, rows, cols int) (*Menu, error) {
	n := len(items())
	br, bc := boxSize(rows, cols, n)
	term, err := vt.New(br, bc)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &Menu{
		term:    term,
		cancel:  cancel,
		input:   make(chan []byte, 64),
		changed: make(chan struct{}, 1),
		done:    make(chan struct{}),
		n:       n,
	}
	inR, inW := io.Pipe()
	mdl := newModel(items, current, br, bc)
	m.prog = tea.NewProgram(mdl,
		tea.WithContext(ctx),
		tea.WithInput(inR),
		tea.WithOutput(termWriter{m, inW}),
		tea.WithWindowSize(bc, br),
		tea.WithEnvironment([]string{"TERM=xterm-256color"}),
		tea.WithColorProfile(colorprofile.TrueColor),
		tea.WithoutSignals(),
		tea.WithoutSignalHandler(),
		tea.WithoutCatchPanics(),
	)
	// Feed typed keys, and the emulator's replies to the program's
	// queries, to the program, without blocking whoever sends them.
	go func() {
		defer inW.Close()
		for {
			select {
			case <-ctx.Done():
				return
			case b := <-m.input:
				if _, err := inW.Write(b); err != nil {
					return
				}
			}
		}
	}()
	go func() {
		defer close(m.done)
		defer term.Close()
		final, err := m.prog.Run()
		cancel()
		inR.Close()
		if fm, ok := final.(*model); ok && err == nil {
			m.mu.Lock()
			m.choice = fm.choice
			m.mu.Unlock()
		}
	}()
	return m, nil
}

// termWriter writes the program's output into the menu's emulator.
type termWriter struct {
	m  *Menu
	in *io.PipeWriter
}

func (w termWriter) Write(p []byte) (int, error) {
	n, err := w.m.term.Write(p)
	if r := w.m.term.Replies(); len(r) > 0 {
		select {
		case w.m.input <- r:
		default:
		}
	}
	select {
	case w.m.changed <- struct{}{}:
	default:
	}
	return n, err
}

// Input types b into the menu.
func (m *Menu) Input(b []byte) {
	select {
	case m.input <- append([]byte(nil), b...):
	case <-m.done:
	}
}

// Resize says the client's screen is now rows by cols.
func (m *Menu) Resize(rows, cols int) {
	br, bc := boxSize(rows, cols, m.n)
	m.term.Resize(br, bc)
	m.prog.Send(tea.WindowSizeMsg{Width: bc, Height: br})
}

// Changed returns a channel that receives when the menu's screen changes.
func (m *Menu) Changed() <-chan struct{} { return m.changed }

// Done returns a channel that's closed once the menu has closed, as when
// the user chooses a session or dismisses it.
func (m *Menu) Done() <-chan struct{} { return m.done }

// Choice returns the session the user chose, once Done, or "" if they
// chose none.
func (m *Menu) Choice() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.choice
}

// Close closes the menu, if it's still open, and waits for it.
func (m *Menu) Close() {
	m.prog.Quit()
	m.cancel()
	<-m.done
}

// boxSize returns the size of the menu's box, listing n sessions, for a
// client screen of rows by cols: tall enough for them, if it fits, and
// not much wider than a session's line needs.
func boxSize(rows, cols, n int) (int, int) {
	// The border takes two rows, the title two, and the help line one;
	// the list's pagination dots one more if it has pages.
	r := min(max(rows-4, min(rows, 8)), max(n+6, 9), 30)
	c := min(max(cols-8, min(cols, 30)), 96)
	return max(r, 1), max(c, 1)
}

// Compose returns base, the client's session's screen, dimmed, and with
// the menu's box centered over it, on a screen rows by cols, the client's.
func (m *Menu) Compose(base vt.Screen, rows, cols int) vt.Screen {
	return compose(base, m.term.Snapshot(), rows, cols)
}

func compose(base, box vt.Screen, rows, cols int) vt.Screen {
	out := vt.Screen{
		Rows:         rows,
		Cols:         cols,
		Cells:        make([][]vt.Cell, rows),
		Title:        base.Title,
		BellSeq:      base.BellSeq,
		Notification: base.Notification,
		History:      base.History,
		// The menu takes no mouse or pastes, whatever the session
		// wants, and has no cursor of its own.
		Modes: vt.Modes{AltScreen: base.Modes.AltScreen},
	}
	for y := range rows {
		row := make([]vt.Cell, cols)
		for x := range row {
			if y < len(base.Cells) && x < len(base.Cells[y]) {
				row[x] = dim(base.Cells[y][x])
			} else {
				row[x] = vt.Cell{Grapheme: " ", Width: 1}
			}
		}
		out.Cells[y] = row
	}
	top, left := (rows-box.Rows)/2, (cols-box.Cols)/2
	for y, brow := range box.Cells {
		row := out.Cells[top+y]
		// A wide character of the session's cut by the box's edges
		// becomes a space.
		if left > 0 && row[left-1].Width == 2 {
			row[left-1] = vt.Cell{Grapheme: " ", Width: 1, Style: row[left-1].Style}
		}
		if r := left + box.Cols; r < cols && row[r].Width == 0 {
			row[r] = vt.Cell{Grapheme: " ", Width: 1, Style: row[r].Style}
		}
		copy(row[left:], brow)
	}
	return out
}

// dim returns c faded into the background.
func dim(c vt.Cell) vt.Cell {
	c.Link = ""
	c.Style.Attrs |= vt.AttrFaint
	c.Style.Attrs &^= vt.AttrBold | vt.AttrBlink | vt.AttrRapidBlink
	c.Style.Fg = darken(c.Style.Fg)
	c.Style.Bg = darken(c.Style.Bg)
	return c
}

func darken(c vt.Color) vt.Color {
	if c.Kind == vt.ColorRGB {
		c.R, c.G, c.B = c.R/2, c.G/2, c.B/2
	}
	return c
}

// Sort orders.
const (
	byActivity = iota
	byName
	byCreated
	numSorts
)

var sortNames = [numSorts]string{"recent activity", "name", "newest"}

// model is the menu's Bubble Tea model.
type model struct {
	items   func() []Item
	current string
	sort    int
	list    list.Model
	choice  string
	first   bool // not yet shown with items
}

// listItem is an Item in the list.
type listItem struct {
	Item
	current bool
}

func (i listItem) FilterValue() string { return i.Name + " " + i.What + " " + i.ID }

var (
	accent    = lipgloss.Color("#7aa2f7")
	dimText   = lipgloss.Color("#8a8f98")
	boxStyle  = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(accent).Padding(0, 1)
	selStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#ffffff")).Background(lipgloss.Color("#3b4261")).Bold(true)
	nameStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#e0e0e0"))
	infoStyle = lipgloss.NewStyle().Foreground(dimText)
	curStyle  = lipgloss.NewStyle().Foreground(accent)
	helpStyle = lipgloss.NewStyle().Foreground(dimText)
)

var (
	sortKey  = key.NewBinding(key.WithKeys("m"))
	closeKey = key.NewBinding(key.WithKeys("q", "esc", "ctrl+c"))
)

func newModel(items func() []Item, current string, rows, cols int) *model {
	l := list.New(nil, delegate{}, 0, 0)
	l.Title = "Sessions"
	l.Styles.Title = lipgloss.NewStyle().Foreground(accent).Bold(true)
	l.Styles.TitleBar = lipgloss.NewStyle().PaddingBottom(1)
	l.SetShowStatusBar(false)
	l.SetShowHelp(false)
	l.SetShowPagination(true)
	l.DisableQuitKeybindings() // closeKey's, instead
	m := &model{items: items, current: current, list: l, first: true}
	m.resize(rows, cols)
	m.refresh()
	return m
}

// resize fits the list in a box rows by cols.
func (m *model) resize(rows, cols int) {
	// The border and padding take two rows and four columns, and the
	// help line one row.
	m.list.SetSize(max(cols-4, 1), max(rows-3, 1))
}

// refresh gets the sessions again, keeping the selected one selected.
func (m *model) refresh() tea.Cmd {
	var sel string
	if it, ok := m.list.SelectedItem().(listItem); ok {
		sel = it.ID
	}
	all := m.items()
	switch m.sort {
	case byActivity:
		slices.SortStableFunc(all, func(a, b Item) int { return b.Activity.Compare(a.Activity) })
	case byName:
		slices.SortStableFunc(all, func(a, b Item) int { return cmp.Compare(strings.ToLower(label(a)), strings.ToLower(label(b))) })
	case byCreated:
		slices.SortStableFunc(all, func(a, b Item) int { return b.Created.Compare(a.Created) })
	}
	items := make([]list.Item, len(all))
	for i, it := range all {
		items[i] = listItem{it, it.ID == m.current}
	}
	cmd := m.list.SetItems(items)
	if m.first && len(all) > 0 {
		// At first, select the session used most recently other than
		// this one, to switch back and forth quickly, as with Alt-Tab.
		m.first = false
		sel = ""
		var best time.Time
		for _, it := range all {
			if it.ID != m.current && (sel == "" || it.Activity.After(best)) {
				sel, best = it.ID, it.Activity
			}
		}
	}
	if m.list.FilterState() == list.Unfiltered {
		for i, it := range m.list.Items() {
			if it.(listItem).ID == sel {
				m.list.Select(i)
			}
		}
	}
	return cmd
}

type tickMsg struct{}

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return tickMsg{} })
}

func (m *model) Init() tea.Cmd { return tick() }

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tickMsg:
		return m, tea.Batch(m.refresh(), tick())
	case tea.WindowSizeMsg:
		m.resize(msg.Height, msg.Width)
		return m, nil
	case tea.KeyPressMsg:
		if m.list.SettingFilter() {
			break // the filter's keys
		}
		switch {
		case msg.String() == "enter":
			if it, ok := m.list.SelectedItem().(listItem); ok {
				m.choice = it.ID
			}
			return m, tea.Quit
		case key.Matches(msg, closeKey) && (m.list.FilterState() == list.Unfiltered || msg.String() != "esc"):
			return m, tea.Quit
		case key.Matches(msg, sortKey):
			m.sort = (m.sort + 1) % numSorts
			return m, m.refresh()
		}
	}
	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	return m, cmd
}

func (m *model) View() tea.View {
	help := helpStyle.Render(fmt.Sprintf("↑↓ move · enter switch · / search · m sort (%s) · esc close", sortNames[m.sort]))
	if m.list.SettingFilter() {
		help = helpStyle.Render("type to search · enter done · esc clear")
	} else if m.list.FilterState() == list.FilterApplied {
		help = helpStyle.Render("enter switch · esc clear search")
	}
	w, h := m.list.Width()+4, m.list.Height()+3
	body := lipgloss.JoinVertical(lipgloss.Left, m.list.View(), help)
	v := tea.NewView(boxStyle.Width(w).Height(h).Render(body))
	v.AltScreen = true
	return v
}

// label is how the menu names a session: its name, or the start of its ID.
func label(it Item) string {
	if it.Name != "" {
		return it.Name
	}
	return it.ID[:min(len(it.ID), len("pty-")+5)]
}

// delegate draws the list's items, a line each.
type delegate struct{}

func (delegate) Height() int                         { return 1 }
func (delegate) Spacing() int                        { return 0 }
func (delegate) Update(tea.Msg, *list.Model) tea.Cmd { return nil }
func (delegate) Render(w io.Writer, m list.Model, index int, li list.Item) {
	it := li.(listItem)
	width := m.Width()
	mark := "  "
	if it.current {
		mark = "● "
	}
	info := fmt.Sprintf("%s · %s", ago(it.Activity), it.What)
	if it.What == "" {
		info = ago(it.Activity)
	}
	if it.Watchers > 0 {
		info += fmt.Sprintf(" · %d watching", it.Watchers)
	}
	name := label(it.Item)
	nameW := min(max(width/3, 12), 32)
	name = truncate(name, nameW)
	info = truncate(info, max(width-nameW-4, 0))
	line := fmt.Sprintf("%s%-*s  %s", mark, nameW, name, info)
	line = truncate(line, width)
	if index == m.Index() {
		fmt.Fprint(w, selStyle.Width(width).Render(line))
		return
	}
	markS := mark
	if it.current {
		markS = curStyle.Render(mark)
	}
	fmt.Fprint(w, markS+nameStyle.Render(fmt.Sprintf("%-*s", nameW, name))+"  "+infoStyle.Render(info))
}

// truncate shortens s to at most n columns, ending it with "…" if cut.
func truncate(s string, n int) string {
	if lipgloss.Width(s) <= n {
		return s
	}
	if n <= 1 {
		return "…"[:min(n, len("…"))]
	}
	r := []rune(s)
	for len(r) > 0 && lipgloss.Width(string(r))+1 > n {
		r = r[:len(r)-1]
	}
	return string(r) + "…"
}

// ago describes how long ago t was.
func ago(t time.Time) string {
	if t.IsZero() {
		return "idle"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}
