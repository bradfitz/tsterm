package gui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"

	"github.com/bradfitz/tsterm/pkg/proto"
)

// maxEnded bounds the ended sessions the list shows, as the web's does.
const maxEnded = 100

// showList shows the list of sessions, as the web UI's index page: those
// running, with buttons to reopen the closed ones' tabs, and those ended,
// kept to look at, with buttons to delete them for good.
func (a *App) showList() {
	if a.id != "" {
		a.disconnect()
		a.id, a.name = "", ""
		a.st.Ended = ""
		a.setTitle()
		a.showMode()
		a.renderTabs()
	}
	nameEntry := widget.NewEntry()
	nameEntry.SetPlaceHolder("name (optional)")
	newBtn := widget.NewButton("New session", func() { a.newSession(strings.TrimSpace(nameEntry.Text)) })
	newBtn.Importance = widget.HighImportance
	demoBtn := widget.NewButton("Try the demo", func() { a.newSession("_tsterm_demo") })
	head := container.NewBorder(nil, nil, nil, container.NewHBox(newBtn, demoBtn), nameEntry)

	var running, ended []proto.SessionSummary
	for _, s := range a.sessions {
		if s.Active {
			running = append(running, s)
		} else {
			ended = append(ended, s)
		}
	}
	sort.SliceStable(running, func(i, j int) bool { return running[i].LastActivity.After(running[j].LastActivity) })
	sort.SliceStable(ended, func(i, j int) bool { return ended[i].Ended.After(ended[j].Ended) })

	items := []fyne.CanvasObject{head, heading("Running")}
	if len(running) == 0 {
		items = append(items, widget.NewLabel("No running sessions."))
	}
	for _, s := range running {
		items = append(items, a.runningRow(s))
	}
	if len(ended) > 0 {
		items = append(items, heading("Ended"))
		for _, s := range ended[:min(len(ended), maxEnded)] {
			items = append(items, a.endedRow(s))
		}
		if n := len(ended) - maxEnded; n > 0 {
			items = append(items, dim(fmt.Sprintf("And %d older.", n)))
		}
		items = append(items, dim("Ended sessions keep their last screen and history, to look at, for a year."))
	}
	items = append(items, dim("Closing a session's tab leaves it running, here."))
	page := container.NewVScroll(container.NewPadded(container.NewVBox(items...)))
	a.listPage = page
	a.main.Objects = []fyne.CanvasObject{page}
	a.main.Refresh()
}

func heading(s string) fyne.CanvasObject {
	l := widget.NewLabel(s)
	l.TextStyle.Bold = true
	return l
}

func dim(s string) fyne.CanvasObject {
	l := widget.NewLabel(s)
	l.Importance = widget.LowImportance
	l.Wrapping = fyne.TextWrapWord
	return l
}

// dimWord is dim, for a word or two that mustn't wrap.
func dimWord(s string) fyne.CanvasObject {
	l := widget.NewLabel(s)
	l.Importance = widget.LowImportance
	return l
}

// ago describes how long ago t was.
func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

// openButton returns a button showing the session s.
func (a *App) openButton(s proto.SessionSummary) *widget.Button {
	b := widget.NewButton(label(s.ID, s.Name), func() { a.switchTo(s.ID, s.Name) })
	b.Alignment = widget.ButtonAlignLeading
	return b
}

func (a *App) runningRow(s proto.SessionSummary) fyne.CanvasObject {
	info := fmt.Sprintf("active %s · %d×%d · started %s · %d watching", ago(s.LastActivity), s.Cols, s.Rows, ago(s.Created), s.Watchers)
	if s.Closed {
		info = "tab closed · " + info
	}
	rename := widget.NewButton("rename", func() { a.rename(s.ID, s.Name) })
	var tabBtn fyne.CanvasObject = dimWord("open")
	if s.Closed {
		tabBtn = widget.NewButton("reopen", func() { a.reopen(s.ID) })
	}
	return container.NewBorder(nil, nil, a.openButton(s), container.NewHBox(rename, tabBtn), dim(info))
}

func (a *App) endedRow(s proto.SessionSummary) fyne.CanvasObject {
	cmd := "shell"
	if len(s.Cmd) > 0 {
		cmd = strings.Join(s.Cmd, " ")
	}
	info := fmt.Sprintf("%s · started %s · ended %s · %s", cmd, ago(s.Created), ago(s.Ended), s.Status)
	var tabBtn fyne.CanvasObject = dimWord("open")
	if s.Closed {
		tabBtn = widget.NewButton("reopen", func() { a.reopen(s.ID) })
	}
	purge := widget.NewButton("delete forever", func() {
		dialog.ShowConfirm("Delete for good?", "Delete this session's screen and history for good?", func(ok bool) {
			if !ok {
				return
			}
			go func() {
				err := a.client.Purge(context.Background(), s.ID)
				if err != nil {
					fyne.Do(func() { dialog.ShowError(err, a.win) })
				}
				a.refreshSessions()
			}()
		}, a.win)
	})
	purge.Importance = widget.DangerImportance
	return container.NewBorder(nil, nil, a.openButton(s), container.NewHBox(tabBtn, purge), dim(info))
}

// reopen gives the session id a tab again.
func (a *App) reopen(id string) {
	go func() {
		if err := a.client.SetClosed(context.Background(), id, false); err != nil {
			fyne.Do(func() { dialog.ShowError(err, a.win) })
		}
		a.refreshSessions()
	}()
}
