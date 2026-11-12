package gui

import "testing"

func TestSymbolCell(t *testing.T) {
	for g, want := range map[string]bool{
		"a":  false, // ASCII, in the monospace font
		"─":  false, // box drawing, in it too
		"⏵":  true,  // Claude Code's, which it lacks
		"⎿":  true,
		"⣿":  true,  // braille
		"⏺":  true,  // text by default
		"⏺️": false, // marked as emoji
		"🙂":  false, // emoji
		"⌛":  false, // emoji by default
		"🬀":  true,  // legacy computing, not emoji
	} {
		r := []rune(g)[0]
		if got := symbolCell(g, r); got != want {
			t.Errorf("symbolCell(%q) = %v, want %v", g, got, want)
		}
	}
}
