package wsclient

import (
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/width"
)

// graphemes splits s into what terminals put in one cell: a base character
// and the combining marks, variation selectors, and zero-width joiners
// after it, and the characters they join. It's an approximation of
// Unicode's grapheme clusters, good enough for terminal text.
func graphemes(s string) []string {
	var out []string
	start, joined := 0, false
	for i, r := range s {
		if i > start && !joined && !extends(r) {
			out = append(out, s[start:i])
			start = i
		}
		joined = r == '\u200d'
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

func extends(r rune) bool {
	return unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) ||
		r == '\u200d' || (r >= '\ufe00' && r <= '\ufe0f') ||
		(r >= 0x1f3fb && r <= 0x1f3ff) // skin tones
}

// RuneWidth returns how many columns the grapheme g takes in a terminal:
// 2 for East Asian wide and fullwidth characters and emoji, else 1.
func RuneWidth(g string) int {
	r, _ := utf8.DecodeRuneInString(g)
	switch width.LookupRune(r).Kind() {
	case width.EastAsianWide, width.EastAsianFullwidth:
		return 2
	}
	if r >= 0x1f300 && r <= 0x1faff || r >= 0x2600 && r <= 0x27bf && len(g) > 3 {
		return 2 // emoji, or a symbol in emoji presentation
	}
	return 1
}
