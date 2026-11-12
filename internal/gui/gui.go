// Package gui is tsterm's desktop GUI, built with Fyne, with the web UI's
// features: tabs of the user's open sessions, a terminal view of the one
// shown, with its history, and a page listing all of them. It talks to
// tstermd as the web UI does (see package wsclient). cmd/tsterm-gui runs it.
package gui

import (
	"context"
	"fmt"
	"image/color"
	"log"
	"sort"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/bradfitz/tsterm/pkg/proto"
	"github.com/bradfitz/tsterm/pkg/wsclient"
)

// Preferences.
const (
	prefHost    = "host"    // the tstermd last connected to
	prefAutoFit = "autofit" // whether sessions are sized to the window
)

// App is the GUI: one window, showing one session at a time, or the list
// of sessions.
type App struct {
	fa     fyne.App
	win    fyne.Window
	client *wsclient.Client
	host   string

	// The session shown, or "" for the list.
	id, name string
	st       wsclient.State
	hist     wsclient.History
	conn     *wsclient.Conn
	cancel   context.CancelFunc // ends the connection loop

	writable   bool // typing, rather than view-only
	autoFit    bool
	foreground bool // the window has the focus
	sessions   []proto.SessionSummary
	fitTimer   *time.Timer

	term      *Term
	tabs      *fyne.Container
	tabObjs   map[string]fyne.CanvasObject // the tabs, by session
	otherBtn  *widget.Button               // Other (N), if shown
	tabScroll *container.Scroll
	status    *widget.Label
	modeBtn   *widget.Button
	fitBtn    *widget.Button
	fitOnce   *widget.Button
	closeBtn  *widget.Button
	main      *fyne.Container // the term or the list
	toasts    *fyne.Container
	listPage  fyne.CanvasObject
}

// Run runs the GUI, connecting to the tstermd at host (as "braid" or
// "braid:2200"), or if host is empty, the one last used, or one the user
// says.
func Run(fa fyne.App, host string) {
	fa.Settings().SetTheme(darkTheme{theme.DefaultTheme()})
	a := &App{fa: fa, writable: true, foreground: true}
	a.autoFit = fa.Preferences().BoolWithFallback(prefAutoFit, true)
	a.win = fa.NewWindow("tsterm")
	a.build()
	a.win.Resize(fyne.NewSize(1000, 640))
	lc := fa.Lifecycle()
	lc.SetOnEnteredForeground(func() { a.setForeground(true) })
	lc.SetOnExitedForeground(func() { a.setForeground(false) })

	if host == "" {
		host = fa.Preferences().String(prefHost)
	}
	if host == "" {
		a.askHost()
	} else {
		a.connectHost(host)
	}
	go a.ticker()
	a.win.ShowAndRun()
}

// button returns a button that runs fn, then gives the keyboard back to
// the terminal, if a session is shown: buttons take the focus when
// clicked, which would leave typing going to them.
func (a *App) button(text string, fn func()) *widget.Button {
	return widget.NewButton(text, func() {
		fn()
		if a.id != "" {
			a.win.Canvas().Focus(a.term)
		}
	})
}

// darkTheme is Fyne's theme, always dark, as a terminal is.
type darkTheme struct{ fyne.Theme }

func (t darkTheme) Color(n fyne.ThemeColorName, _ fyne.ThemeVariant) color.Color {
	return t.Theme.Color(n, theme.VariantDark)
}

// Font returns, for the Symbol style, a font for the symbols the terminal
// needs that Fyne's fonts lack (see fonts.go).
func (t darkTheme) Font(s fyne.TextStyle) fyne.Resource {
	if s.Symbol {
		return symbolFont
	}
	return t.Theme.Font(s)
}

func (a *App) build() {
	a.term = newTerm(a)
	a.status = widget.NewLabel("")
	a.status.Importance = widget.LowImportance
	a.modeBtn = a.button("", a.toggleWritable)
	a.fitBtn = a.button("⇲ auto-fit", func() { a.setAutoFit(!a.autoFit) })
	a.fitOnce = a.button("fit once", func() { a.fit(true, !a.autoFit) })
	a.closeBtn = a.button("✕ close", func() { a.closeTab(a.id) })
	a.closeBtn.Hide()
	a.tabs = container.NewHBox()
	a.tabScroll = container.NewHScroll(a.tabs)
	sessionsBtn := a.button("☰ sessions", a.showList)
	right := container.NewHBox(a.status, a.fitOnce, a.fitBtn, a.modeBtn, a.closeBtn)
	top := container.NewBorder(nil, nil, sessionsBtn, right, a.tabScroll)
	a.toasts = container.NewVBox()
	a.main = container.NewStack(a.term)
	content := container.NewStack(a.main, container.NewHBox(layout.NewSpacer(), container.NewVBox(a.toasts, layout.NewSpacer())))
	a.win.SetContent(container.NewBorder(top, nil, nil, nil, content))
	a.showMode()
}

// askHost asks the user which tstermd to use.
func (a *App) askHost() {
	e := widget.NewEntry()
	e.SetPlaceHolder("machine or machine:port")
	d := dialog.NewForm("Connect to tstermd", "Connect", "Quit",
		[]*widget.FormItem{widget.NewFormItem("Host", e)},
		func(ok bool) {
			if !ok || strings.TrimSpace(e.Text) == "" {
				a.fa.Quit()
				return
			}
			a.connectHost(strings.TrimSpace(e.Text))
		}, a.win)
	showForm(a.win, d, e)
}

// showForm shows the form d, ready for typing into e, which Enter submits.
func showForm(win fyne.Window, d *dialog.FormDialog, e *widget.Entry) {
	e.OnSubmitted = func(string) { d.Submit() }
	d.Resize(fyne.NewSize(420, d.MinSize().Height))
	d.Show()
	win.Canvas().Focus(e)
}

// connectHost starts using the tstermd at host: it shows the session the
// user last had open there, or the list.
func (a *App) connectHost(host string) {
	a.host = host
	a.client = wsclient.NewClient(host)
	a.status.SetText("connecting to " + host + "…")
	go func() {
		list, err := a.client.List(context.Background())
		fyne.Do(func() {
			if err != nil {
				dialog.ShowError(fmt.Errorf("can't reach tstermd at %s: %w", host, err), a.win)
				a.status.SetText("can't reach " + host)
				return
			}
			a.fa.Preferences().SetString(prefHost, host)
			a.sessions = list
			a.renderTabs()
			if open := openSessions(list); len(open) > 0 {
				s := open[len(open)-1]
				a.switchTo(s.ID, s.Name)
			} else {
				a.showList()
				a.showStatus()
			}
		})
	}()
}

// ticker refreshes the tabs, and blinks the cursor.
func (a *App) ticker() {
	blink := time.NewTicker(600 * time.Millisecond)
	tabs := time.NewTicker(5 * time.Second)
	for {
		select {
		case <-blink.C:
			fyne.Do(func() {
				if a.id != "" {
					a.term.blink()
				}
			})
		case <-tabs.C:
			a.refreshSessions()
		}
	}
}

// refreshSessions gets the session list, for the tabs and the list page.
func (a *App) refreshSessions() {
	if a.client == nil {
		return
	}
	list, err := a.client.List(context.Background())
	if err != nil {
		return
	}
	fyne.Do(func() {
		a.sessions = list
		a.renderTabs()
		if a.id == "" && a.listPage != nil {
			a.showList()
		}
	})
}

// openSessions returns the sessions in the open set, the tabs, in the
// order they were opened.
func openSessions(list []proto.SessionSummary) []proto.SessionSummary {
	var open []proto.SessionSummary
	for _, s := range list {
		if !s.Closed {
			open = append(open, s)
		}
	}
	sort.SliceStable(open, func(i, j int) bool { return openedAt(open[i]).Before(openedAt(open[j])) })
	return open
}

func openedAt(s proto.SessionSummary) time.Time {
	if !s.Opened.IsZero() {
		return s.Opened
	}
	return s.Created
}

// label names a session for its tab: its name, or the start of its ID.
func label(id, name string) string {
	if name != "" {
		return name
	}
	return id[:min(len(id), len("pty-")+4)]
}

// Showing a session.

// switchTo shows the session id, connecting to it, and reopens it if it
// was closed, as visiting it in the web UI does.
func (a *App) switchTo(id, name string) {
	a.disconnect()
	a.id, a.name = id, name
	a.st = wsclient.State{}
	a.hist.Reset()
	a.term.scrollBack, a.term.sel = 0, nil
	a.term.histRows, a.term.histLines = nil, 0
	a.main.Objects = []fyne.CanvasObject{a.term}
	a.main.Refresh()
	a.win.Canvas().Focus(a.term)
	a.setTitle()
	a.showMode()
	for _, s := range a.sessions {
		if s.ID == id && s.Closed {
			go func() {
				a.client.SetClosed(context.Background(), id, false)
				a.refreshSessions()
			}()
		}
	}
	a.renderTabs()
	a.connect()
}

func (a *App) disconnect() {
	if a.cancel != nil {
		a.cancel()
		a.cancel = nil
	}
	if a.conn != nil {
		a.conn.Close()
		a.conn = nil
	}
}

// connect starts the connection loop for the session shown: it connects,
// applies what the server sends, and reconnects after losing it.
func (a *App) connect() {
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	id, writable, client := a.id, a.writable, a.client
	a.status.SetText("connecting…")
	go func() {
		for ctx.Err() == nil {
			conn, err := client.Dial(ctx, id, writable)
			if err != nil {
				if ctx.Err() == nil {
					fyne.Do(func() { a.status.SetText("disconnected; retrying…") })
					log.Printf("tsterm-gui: connecting to %s: %v", id, err)
				}
				sleepCtx(ctx, 2*time.Second)
				continue
			}
			fyne.Do(func() {
				if ctx.Err() != nil {
					conn.Close()
					return
				}
				a.conn = conn
				a.st.Ended = ""
				a.showStatus()
				a.fit(a.foreground, !a.autoFit && false)
			})
			for {
				env, err := conn.Recv(ctx)
				if err != nil {
					break
				}
				fyne.Do(func() {
					if a.conn == conn {
						a.apply(env)
					}
				})
			}
			conn.Close()
			if ctx.Err() == nil {
				fyne.Do(func() { a.status.SetText("disconnected; retrying…") })
				sleepCtx(ctx, 2*time.Second)
			}
		}
	}()
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// apply applies a frame from the server.
func (a *App) apply(env proto.Envelope) {
	ch := a.st.Apply(env)
	t := a.term
	if ch.Lines != nil {
		before := t.scrollBack
		added := a.hist.Apply(ch.Lines, t.pinned())
		if added > 0 && !t.pinned() {
			// Keep the view where it is.
			t.scrollBack = before + t.wrappedRows(a.hist.Lines[:added])
		}
		a.loadHistory()
	}
	if ch.History {
		a.hist.SetTotal(a.st.Screen.History)
		a.loadHistory()
	}
	if ch.Title {
		a.setTitle()
	}
	if ch.Ended || ch.Sizing {
		a.showStatus()
		a.showMode()
	}
	if ch.Rang {
		t.ring()
	}
	if n := ch.Notify; n != nil {
		a.notify(n.Title, n.Body)
	}
	if ch.All || len(ch.Rows) > 0 || ch.Cursor || ch.Lines != nil || ch.History || ch.Modes || ch.Ended {
		t.refreshView()
	}
	if ch.Seq != 0 {
		conn, seq := a.conn, ch.Seq
		conn.Ack(seq)
	}
}

// loadHistory asks for the next history needed, if any.
func (a *App) loadHistory() {
	if a.conn == nil {
		return
	}
	if from, count, ok := a.hist.Next(a.term.nearTop(), a.term.pinned()); ok {
		conn := a.conn
		conn.History(from, count)
	}
}

// canType reports whether typing reaches the session.
func (a *App) canType() bool { return a.writable && a.st.Ended == "" && a.conn != nil }

// send sends input to the session, as mouse reports.
func (a *App) send(s string) {
	if a.canType() {
		conn := a.conn
		conn.Input(s)
	}
}

// typeInput sends what the user typed, and, as typing in a terminal does,
// scrolls back down to the screen.
func (a *App) typeInput(s string) {
	if s == "" || !a.canType() {
		return
	}
	a.send(s)
	a.term.sel = nil
	a.term.scrollToBottom()
}

func (a *App) setTitle() {
	title := a.st.Screen.Title
	if title == "" {
		title = label(a.id, a.name)
	}
	if a.id == "" {
		title = "sessions"
	}
	a.win.SetTitle(title + " · tsterm")
}

// showStatus describes the connection: live, view-only, who sizes it, or
// how it ended.
func (a *App) showStatus() {
	switch {
	case a.id == "":
		a.status.SetText(a.host)
	case a.st.Ended != "":
		switch a.st.Ended {
		case "exited":
			a.status.SetText("ended")
		case proto.StatusLost:
			a.status.SetText("ended (lost)")
		default:
			a.status.SetText("ended (" + a.st.Ended + ")")
		}
	case a.conn == nil:
		a.status.SetText("connecting…")
	case !a.writable:
		a.status.SetText("view-only")
	case a.autoFit && a.st.Sizing.Leader:
		a.status.SetText("live, sizing to window")
	case a.autoFit && a.st.Sizing.Other != "":
		a.status.SetText("live, sized by " + a.st.Sizing.Other)
	default:
		a.status.SetText("live")
	}
}

// showMode shows the buttons that apply.
func (a *App) showMode() {
	show := func(o fyne.CanvasObject, on bool) {
		if on {
			o.Show()
		} else {
			o.Hide()
		}
	}
	live := a.id != "" && a.st.Ended == ""
	if a.writable {
		a.modeBtn.SetText("⌨ typing")
	} else {
		a.modeBtn.SetText("👁 view-only")
	}
	show(a.modeBtn, live)
	show(a.fitBtn, live && a.writable)
	show(a.fitOnce, live && a.writable && !a.autoFit)
	show(a.closeBtn, a.id != "" && a.st.Ended != "")
	if a.autoFit {
		a.fitBtn.Importance = widget.SuccessImportance
	} else {
		a.fitBtn.Importance = widget.MediumImportance
	}
	a.fitBtn.Refresh()
	a.showStatus()
}

func (a *App) toggleWritable() {
	a.writable = !a.writable
	a.showMode()
	if a.id != "" {
		a.disconnect()
		a.connect()
	}
	a.win.Canvas().Focus(a.term)
}

// Sizing.

// setAutoFit sets whether sessions are sized to the window.
func (a *App) setAutoFit(on bool) {
	a.autoFit = on
	a.fa.Preferences().SetBool(prefAutoFit, on)
	if on {
		a.fit(true, false)
	} else if a.conn != nil {
		conn := a.conn
		conn.Resize(0, 0, false)
	}
	a.showMode()
}

// fit tells the server the size the view holds, if auto-fit is on or once;
// lead says the user is using this window now, so the size applies.
func (a *App) fit(lead, once bool) {
	if a.conn == nil || !a.canType() || !(a.autoFit || once) {
		return
	}
	rows, cols := a.term.fitSize()
	a.conn.Resize(rows, cols, lead)
	if once {
		a.conn.Resize(0, 0, false)
	}
}

// termResized is called when the terminal view changes size.
func (a *App) termResized() {
	if a.fitTimer != nil {
		a.fitTimer.Stop()
	}
	a.fitTimer = time.AfterFunc(100*time.Millisecond, func() {
		fyne.Do(func() { a.fit(a.foreground, false) })
	})
}

// setForeground notes the window gaining or losing the focus: the cursor
// shows it, programs that asked are told, and focusing a window is using
// it, so its size applies.
func (a *App) setForeground(on bool) {
	a.foreground = on
	if a.canType() && a.st.Screen.Modes.FocusEvents {
		if on {
			a.send("\x1b[I")
		} else {
			a.send("\x1b[O")
		}
	}
	if on {
		a.fit(true, false)
	}
	a.term.refreshView()
}

// Notifications.

// notify shows a program's notification: in the window, while it's in
// front, and otherwise as a system notification.
func (a *App) notify(title, body string) {
	if title == "" {
		title = label(a.id, a.name)
	}
	if !a.foreground {
		a.fa.SendNotification(fyne.NewNotification(title, body))
		return
	}
	t := canvas.NewText(title, theme.Color(theme.ColorNameForeground))
	t.TextStyle.Bold = true
	b := widget.NewLabel(body)
	b.Wrapping = fyne.TextWrapWord
	card := container.NewStack(canvas.NewRectangle(color.NRGBA{0x30, 0x30, 0x30, 0xf0}), container.NewPadded(container.NewVBox(t, b)))
	box := container.NewGridWrap(fyne.NewSize(320, 80), card)
	a.toasts.Add(box)
	time.AfterFunc(8*time.Second, func() { fyne.Do(func() { a.toasts.Remove(box) }) })
}

// Tabs.

// renderTabs shows the tabs: the open sessions, Other, and +.
func (a *App) renderTabs() {
	var objs []fyne.CanvasObject
	open := openSessions(a.sessions)
	found := false
	for _, s := range open {
		found = found || s.ID == a.id
	}
	if !found && a.id != "" {
		open = append(open, proto.SessionSummary{ID: a.id, Name: a.name, Active: a.st.Ended == ""})
	}
	a.tabObjs = map[string]fyne.CanvasObject{}
	for _, s := range open {
		o := a.tab(s)
		a.tabObjs[s.ID] = o
		objs = append(objs, o)
	}
	other := 0
	for _, s := range a.sessions {
		if s.Closed && s.Active {
			other++
		}
	}
	a.otherBtn = nil
	if other > 0 {
		a.otherBtn = a.button(fmt.Sprintf("Other (%d)", other), a.showList)
		objs = append(objs, a.otherBtn)
	}
	add := a.button("+", func() { a.newSession("") })
	objs = append(objs, add)
	a.tabs.Objects = objs
	a.tabs.Refresh()
}

// tab returns a session's tab: its name, which shows it, and a button
// that closes it, which leaves it running.
func (a *App) tab(s proto.SessionSummary) fyne.CanvasObject {
	name := label(s.ID, s.Name)
	if !s.Active {
		name += " (ended)"
	}
	x := a.button("×", func() { a.closeTab(s.ID) })
	x.Importance = widget.LowImportance
	if s.ID == a.id {
		cur := newCurrentTab(name, func() { a.rename(s.ID, a.name) })
		return container.NewHBox(cur, x)
	}
	b := a.button(name, func() { a.switchTo(s.ID, s.Name) })
	b.Importance = widget.LowImportance
	return container.NewHBox(b, x)
}

// closeTab closes a session's tab, which leaves it running. If it's the
// session shown, the window shows the next tab's, to the right or else
// the left, or else the list.
func (a *App) closeTab(id string) {
	open := openSessions(a.sessions)
	next := -1
	for i, s := range open {
		if s.ID == id {
			if i+1 < len(open) {
				next = i + 1
			} else if i > 0 {
				next = i - 1
			}
		}
	}
	go func() {
		if err := a.client.SetClosed(context.Background(), id, true); err != nil {
			fyne.Do(func() { dialog.ShowError(err, a.win) })
			return
		}
		a.refreshSessions()
	}()
	alive := false
	for i := range a.sessions {
		if a.sessions[i].ID == id {
			a.sessions[i].Closed = true
			alive = a.sessions[i].Active
		}
	}
	// Where its tab was, before the tabs change.
	var g ghost
	if o := a.tabObjs[id]; o != nil && alive {
		g = ghost{from: a.fa.Driver().AbsolutePositionForObject(o), size: o.Size(), text: tabText(o), ok: true}
	}
	if id != a.id {
		a.renderTabs()
		a.flyToOther(g)
		return
	}
	defer a.flyToOther(g)
	if next >= 0 {
		a.switchTo(open[next].ID, open[next].Name)
	} else {
		a.showList()
	}
}

// ghost is where a closed tab was, and what it said.
type ghost struct {
	from fyne.Position
	size fyne.Size
	text string
	ok   bool
}

// tabText returns what a tab says.
func tabText(tab fyne.CanvasObject) string {
	if box, ok := tab.(*fyne.Container); ok && len(box.Objects) > 0 {
		switch o := box.Objects[0].(type) {
		case *widget.Button:
			return o.Text
		case *currentTab:
			return o.text
		}
	}
	return ""
}

// flyToOther animates a copy of a closed tab, g, from where it was into
// Other, to show where its session went: it's not ended, just out of the
// way. The tab bar must already show Other.
func (a *App) flyToOther(g ghost) {
	if !g.ok || a.otherBtn == nil || !a.fa.Settings().ShowAnimations() {
		return
	}
	from, size := g.from, g.size
	to := a.fa.Driver().AbsolutePositionForObject(a.otherBtn).Add(fyne.NewPos(a.otherBtn.Size().Width/2, a.otherBtn.Size().Height/2))
	bg := canvas.NewRectangle(theme.Color(theme.ColorNameButton))
	bg.CornerRadius = theme.InputRadiusSize()
	txt := canvas.NewText(g.text, theme.Color(theme.ColorNameForeground))
	ghost := container.NewStack(bg, container.NewCenter(txt))
	layer := container.NewWithoutLayout(ghost)
	ghost.Move(from)
	ghost.Resize(size)
	overlays := a.win.Canvas().Overlays()
	overlays.Add(layer)
	anim := fyne.NewAnimation(450*time.Millisecond, func(f float32) {
		s := 1 - 0.65*f
		cur := fyne.NewSize(size.Width*s, size.Height*s)
		center := fyne.NewPos(from.X+size.Width/2+(to.X-from.X-size.Width/2)*f, from.Y+size.Height/2+(to.Y-from.Y-size.Height/2)*f)
		ghost.Move(fyne.NewPos(center.X-cur.Width/2, center.Y-cur.Height/2))
		ghost.Resize(cur)
		if f >= 1 {
			overlays.Remove(layer)
		}
	})
	anim.Curve = fyne.AnimationEaseIn
	anim.Start()
}

// newSession starts a session, named name unless it's empty, and shows it.
func (a *App) newSession(name string) {
	go func() {
		info, err := a.client.Create(context.Background(), proto.CreateSessionReq{Name: name})
		fyne.Do(func() {
			if err != nil {
				dialog.ShowError(err, a.win)
				return
			}
			a.sessions = append(a.sessions, proto.SessionSummary{ID: info.ID, Name: info.Name, Active: true, Created: time.Now()})
			a.switchTo(info.ID, info.Name)
			a.fit(true, true) // nothing else owns its size yet
		})
		a.refreshSessions()
	}()
}

// rename asks for a new name for the session id.
func (a *App) rename(id, name string) {
	e := widget.NewEntry()
	e.SetText(name)
	e.SetPlaceHolder("no name")
	d := dialog.NewForm("Rename session", "Rename", "Cancel", []*widget.FormItem{widget.NewFormItem("Name", e)},
		func(ok bool) {
			if !ok {
				return
			}
			newName := strings.TrimSpace(e.Text)
			go func() {
				err := a.client.Rename(context.Background(), id, newName)
				fyne.Do(func() {
					if err != nil {
						dialog.ShowError(err, a.win)
						return
					}
					if id == a.id {
						a.name = newName
						a.setTitle()
						a.win.Canvas().Focus(a.term)
					}
				})
				a.refreshSessions()
			}()
		}, a.win)
	showForm(a.win, d, e)
}
