package gui

import (
	"bytes"
	_ "embed"
	"strings"
	"sync"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/theme"
	"github.com/go-text/typesetting/font"
)

// Fyne draws text in its theme's fonts, falling back to its bundled emoji
// and symbol fonts and then to the system's, but only to system fonts it
// finds for the character's script. Symbols in the Common script that
// TUIs use, as Claude Code's ⏵ and ⎿ and braille spinners, it finds none
// for, even where the system has one, and draws as boxes. So cells whose
// characters the theme's monospace font lacks are drawn in the Symbol text
// style, for which the theme returns Tsterm Symbols, a subset of
// JuliaMono, a monospace font made to cover most of Unicode, with just its
// symbols (see fonts/README), unless they're
// emoji, which Fyne's emoji font draws in color. Characters that can be
// either, as ⏺, are text unless marked as emoji (with U+FE0F), as in
// terminals and browsers.

//go:embed fonts/TstermSymbols-Regular.ttf
var symbolFontData []byte

var symbolFont = fyne.NewStaticResource("TstermSymbols-Regular.ttf", symbolFontData)

// symbolCell reports whether the cell holding grapheme g, whose first rune
// is r, is drawn in the symbol font.
func symbolCell(g string, r rune) bool {
	if r < 0x80 || covered(r) {
		return false
	}
	return !emoji(g, r)
}

// emoji reports whether the grapheme g, starting with r, is an emoji drawn
// as one by default (Unicode's Emoji_Presentation), or marked as one.
func emoji(g string, r rune) bool {
	if strings.ContainsRune(g, '\ufe0f') || r >= 0x1f000 && r < 0x1fb00 {
		return true // the emoji blocks, before Symbols for Legacy Computing
	}
	return emojiPresentation[r]
}

// emojiPresentation is the characters before U+1F000 that are emoji by
// default.
var emojiPresentation = func() map[rune]bool {
	m := map[rune]bool{}
	for _, r := range "⌚⌛⏩⏪⏫⏬⏰⏳◽◾☔☕♈♉♊♋♌♍♎♏♐♑♒♓♿⚓⚡⚪⚫⚽⚾⛄⛅⛎⛔⛪⛲⛳⛵⛺⛽✅✊✋✨❌❎❓❔❕❗➕➖➗➰➿⬛⬜⭐⭕" {
		m[r] = true
	}
	return m
}()

// covered reports whether Fyne's monospace font has a glyph for r.
func covered(r rune) bool {
	coverageOnce.Do(loadCoverage)
	coverageMu.Lock()
	defer coverageMu.Unlock()
	if v, ok := coverage[r]; ok {
		return v
	}
	v := false
	for _, f := range coverageFaces {
		if _, ok := f.NominalGlyph(r); ok {
			v = true
			break
		}
	}
	coverage[r] = v
	return v
}

var (
	coverageOnce  sync.Once
	coverageFaces []*font.Face
	coverageMu    sync.Mutex
	coverage      = map[rune]bool{}
)

func loadCoverage() {
	if f, err := font.ParseTTF(bytes.NewReader(theme.DefaultTextMonospaceFont().Content())); err == nil {
		coverageFaces = append(coverageFaces, f)
	}
}
