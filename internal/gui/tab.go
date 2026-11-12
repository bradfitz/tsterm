package gui

import (
	"image/color"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// currentTab is the tab of the session shown, which a double-click renames,
// as in the web UI. Other tabs are buttons, which switch to theirs.
type currentTab struct {
	widget.BaseWidget
	text     string
	onRename func()
}

func newCurrentTab(text string, onRename func()) *currentTab {
	t := &currentTab{text: text, onRename: onRename}
	t.ExtendBaseWidget(t)
	return t
}

// DoubleTapped implements fyne.DoubleTappable.
func (t *currentTab) DoubleTapped(*fyne.PointEvent) { t.onRename() }

// CreateRenderer implements fyne.Widget.
func (t *currentTab) CreateRenderer() fyne.WidgetRenderer {
	bg := canvas.NewRectangle(theme.Color(theme.ColorNamePrimary))
	bg.CornerRadius = theme.InputRadiusSize()
	l := canvas.NewText(t.text, color.White)
	l.TextStyle.Bold = true
	return widget.NewSimpleRenderer(container.NewStack(bg, container.NewPadded(container.NewCenter(l))))
}
