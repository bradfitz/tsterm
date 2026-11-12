package vt

import (
	"bytes"
	"errors"
	"strconv"
)

// decodeHistory decodes tsterm_history's encoding (see the glue's
// tsterm.c): two flag bytes, then the lines as VT text, separated by CRLF.
func decodeHistory(b []byte) (lines []Line, lost bool, err error) {
	if len(b) < 2 {
		return nil, false, errors.New("vt: truncated history")
	}
	lost, continued, vt := b[0] != 0, b[1] != 0, b[2:]
	if len(vt) == 0 {
		return nil, lost, nil
	}
	for _, l := range bytes.Split(vt, []byte("\r\n")) {
		lines = append(lines, parseLine(l))
	}
	lines[len(lines)-1].Continued = continued
	return lines, lost, nil
}

// parseLine parses a line of the formatter's VT output: text, SGR
// sequences, OSC 8 hyperlinks, and erased cells, written as CSI n X then
// CSI n C to step over them. The blank cells at its end are left out.
func parseLine(b []byte) Line {
	var p lineParser
	for i := 0; i < len(b); {
		c := b[i]
		switch {
		case c == 0x1b && i+1 < len(b) && b[i+1] == '[':
			j := i + 2
			for j < len(b) && (b[j] < 0x40 || b[j] > 0x7e) {
				j++
			}
			if j < len(b) {
				p.csi(b[i+2:j], b[j])
			}
			i = j + 1
		case c == 0x1b && i+1 < len(b) && b[i+1] == ']':
			// OSC, ended by ST or BEL.
			j := i + 2
			for j < len(b) && b[j] != 0x07 && !(b[j] == 0x1b && j+1 < len(b) && b[j+1] == '\\') {
				j++
			}
			p.osc(b[i+2 : j])
			i = j + 1
			if j < len(b) && b[j] == 0x1b {
				i++
			}
		case c == 0x1b:
			i += 2 // some other escape: skip it
		case c < 0x20 || c == 0x7f:
			i++
		default:
			j := i + 1
			for j < len(b) && b[j] != 0x1b && b[j] >= 0x20 && b[j] != 0x7f {
				j++
			}
			p.add(b[i:j], p.style)
			i = j
		}
	}
	return p.line()
}

type lineParser struct {
	style  Style
	link   string
	erased int // cells CSI X erased at the cursor, which CSI C steps over

	text []byte
	runs []Run
	// keep is the length of text through its last cell that isn't blank,
	// and keepRuns how many runs that takes.
	keep, keepRuns int
}

func (p *lineParser) add(t []byte, st Style) {
	if n := len(p.runs); n == 0 || p.runs[n-1].Style != st || p.runs[n-1].Link != p.link {
		p.runs = append(p.runs, Run{Style: st, Link: p.link})
	}
	p.runs[len(p.runs)-1].Len += len(t)
	p.text = append(p.text, t...)
	if st != (Style{}) || p.link != "" {
		p.keep, p.keepRuns = len(p.text), len(p.runs)
		return
	}
	if i := bytes.LastIndexFunc(t, func(r rune) bool { return r != ' ' }); i >= 0 {
		p.keep, p.keepRuns = len(p.text)-len(t)+i+1, len(p.runs)
	}
}

func (p *lineParser) line() Line {
	text, runs := p.text[:p.keep], p.runs[:p.keepRuns]
	if len(runs) > 0 {
		// The last run kept may have lost some blanks.
		last := &runs[len(runs)-1]
		total := 0
		for _, r := range runs[:len(runs)-1] {
			total += r.Len
		}
		last.Len = len(text) - total
	}
	l := Line{Text: validUTF8(string(text))}
	if len(runs) > 1 || len(runs) == 1 && (runs[0].Style != (Style{}) || runs[0].Link != "") {
		l.Runs = runs
	}
	if len(l.Text) != len(text) {
		l.Runs = nil // not valid UTF-8, so the runs might not line up
	}
	return l
}

func (p *lineParser) osc(b []byte) {
	// OSC 8 ; params ; URI sets the hyperlink, or ends it if URI is empty.
	rest, ok := bytes.CutPrefix(b, []byte("8;"))
	if !ok {
		return
	}
	if _, uri, ok := bytes.Cut(rest, []byte(";")); ok {
		p.link = validUTF8(string(uri))
	}
}

func (p *lineParser) csi(params []byte, final byte) {
	erased := p.erased
	p.erased = 0
	switch final {
	case 'm':
		p.sgr(params)
	case 'X':
		p.erased = atoi(params, 1)
	case 'C':
		// Stepping over cells: those just erased have the background
		// color; others are empty.
		n := min(atoi(params, 1), 1000)
		st := Style{}
		if erased >= n {
			st.Bg = p.style.Bg
		}
		p.add(bytes.Repeat([]byte(" "), n), st)
	}
}

func atoi(b []byte, def int) int {
	n, err := strconv.Atoi(string(b))
	if err != nil || n < 1 {
		return def
	}
	return n
}

// sgr applies an SGR sequence's parameters to p.style.
func (p *lineParser) sgr(params []byte) {
	fields := bytes.Split(params, []byte(";"))
	s := &p.style
	for i := 0; i < len(fields); i++ {
		sub := bytes.Split(fields[i], []byte(":"))
		n, _ := strconv.Atoi(string(sub[0]))
		switch {
		case n == 0:
			*s = Style{}
		case n == 1:
			s.Attrs |= AttrBold
		case n == 2:
			s.Attrs |= AttrFaint
		case n == 3:
			s.Attrs |= AttrItalic
		case n == 4:
			s.Underline = UnderlineSingle
			if len(sub) > 1 {
				u, _ := strconv.Atoi(string(sub[1]))
				s.Underline = UnderlineStyle(min(u, int(UnderlineDashed)))
			}
		case n == 5:
			s.Attrs |= AttrBlink
		case n == 6:
			s.Attrs |= AttrRapidBlink
		case n == 7:
			s.Attrs |= AttrReverse
		case n == 8:
			s.Attrs |= AttrConceal
		case n == 9:
			s.Attrs |= AttrStrikethrough
		case n == 21:
			s.Underline = UnderlineDouble
		case n == 22:
			s.Attrs &^= AttrBold | AttrFaint
		case n == 23:
			s.Attrs &^= AttrItalic
		case n == 24:
			s.Underline = UnderlineNone
		case n == 25:
			s.Attrs &^= AttrBlink | AttrRapidBlink
		case n == 27:
			s.Attrs &^= AttrReverse
		case n == 28:
			s.Attrs &^= AttrConceal
		case n == 29:
			s.Attrs &^= AttrStrikethrough
		case n >= 30 && n <= 37:
			s.Fg = Color{Kind: ColorIndexed, Index: uint8(n - 30)}
		case n == 39:
			s.Fg = Color{}
		case n >= 40 && n <= 47:
			s.Bg = Color{Kind: ColorIndexed, Index: uint8(n - 40)}
		case n == 49:
			s.Bg = Color{}
		case n >= 90 && n <= 97:
			s.Fg = Color{Kind: ColorIndexed, Index: uint8(n - 90 + 8)}
		case n >= 100 && n <= 107:
			s.Bg = Color{Kind: ColorIndexed, Index: uint8(n - 100 + 8)}
		case n == 38 || n == 48 || n == 58:
			var c Color
			if len(sub) > 1 {
				c = extColor(sub[1:])
			} else {
				var used int
				c, used = extColorFields(fields[i+1:])
				i += used
			}
			switch n {
			case 38:
				s.Fg = c
			case 48:
				s.Bg = c
			}
			// 58, the underline color, isn't in a Style.
		}
	}
}

// extColor parses an extended color's colon-separated parameters: 5:n, or
// 2:r:g:b, possibly with an empty color space ID (2::r:g:b).
func extColor(sub [][]byte) Color {
	num := func(i int) uint8 {
		if i >= len(sub) {
			return 0
		}
		n, _ := strconv.Atoi(string(sub[i]))
		return uint8(n)
	}
	switch string(sub[0]) {
	case "5":
		return Color{Kind: ColorIndexed, Index: num(1)}
	case "2":
		if len(sub) >= 5 {
			return Color{Kind: ColorRGB, R: num(2), G: num(3), B: num(4)}
		}
		return Color{Kind: ColorRGB, R: num(1), G: num(2), B: num(3)}
	}
	return Color{}
}

// extColorFields parses an extended color's semicolon-separated
// parameters, 5;n or 2;r;g;b, returning how many fields it used.
func extColorFields(f [][]byte) (Color, int) {
	if len(f) == 0 {
		return Color{}, 0
	}
	switch string(f[0]) {
	case "5":
		if len(f) >= 2 {
			return extColor(f[:2]), 2
		}
	case "2":
		if len(f) >= 4 {
			return extColor(f[:4]), 4
		}
	}
	return Color{}, len(f)
}
