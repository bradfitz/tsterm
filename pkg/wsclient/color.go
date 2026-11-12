package wsclient

import (
	"image/color"

	"github.com/bradfitz/tsterm/pkg/vt"
)

// Palette is xterm's 256 colors, as the web viewer shows them.
var Palette = func() [256]color.RGBA {
	var p [256]color.RGBA
	base := []uint32{0x000000, 0xcd0000, 0x00cd00, 0xcdcd00, 0x0000ee, 0xcd00cd, 0x00cdcd, 0xe5e5e5,
		0x7f7f7f, 0xff0000, 0x00ff00, 0xffff00, 0x5c5cff, 0xff00ff, 0x00ffff, 0xffffff}
	for i, v := range base {
		p[i] = color.RGBA{uint8(v >> 16), uint8(v >> 8), uint8(v), 0xff}
	}
	level := []uint8{0, 95, 135, 175, 215, 255}
	i := 16
	for r := range 6 {
		for g := range 6 {
			for b := range 6 {
				p[i] = color.RGBA{level[r], level[g], level[b], 0xff}
				i++
			}
		}
	}
	for j := range 24 {
		v := uint8(8 + 10*j)
		p[232+j] = color.RGBA{v, v, v, 0xff}
	}
	return p
}()

// The default colors, as the web viewer's.
var (
	DefaultFg = color.RGBA{0xdd, 0xdd, 0xdd, 0xff}
	DefaultBg = color.RGBA{0, 0, 0, 0xff}
)

// Colors returns the foreground and background colors of the style st,
// with reverse video applied, and whether the background is the default.
func Colors(st vt.Style) (fg, bg color.RGBA, defaultBg bool) {
	fg, bg = DefaultFg, DefaultBg
	defaultBg = true
	if c, ok := rgb(st.Fg); ok {
		fg = c
	}
	if c, ok := rgb(st.Bg); ok {
		bg, defaultBg = c, false
	}
	if st.Attrs&vt.AttrReverse != 0 {
		fg, bg, defaultBg = bg, fg, false
	}
	if st.Attrs&vt.AttrConceal != 0 {
		fg = bg
	}
	if st.Attrs&vt.AttrFaint != 0 {
		fg = mix(fg, bg)
	}
	return fg, bg, defaultBg
}

func rgb(c vt.Color) (color.RGBA, bool) {
	switch c.Kind {
	case vt.ColorIndexed:
		return Palette[c.Index], true
	case vt.ColorRGB:
		return color.RGBA{c.R, c.G, c.B, 0xff}, true
	}
	return color.RGBA{}, false
}

// mix returns a color between a and b, for faint text.
func mix(a, b color.RGBA) color.RGBA {
	m := func(x, y uint8) uint8 { return uint8((int(x)*3 + int(y)*2) / 5) }
	return color.RGBA{m(a.R, b.R), m(a.G, b.G), m(a.B, b.B), 0xff}
}
