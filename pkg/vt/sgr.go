package vt

import (
	"fmt"
	"strings"
)

// WriteStyleChange appends to sb the SGR sequence that transitions a
// terminal from the prev style to cur. The strategy is:
//
//   - If any "set" attribute or color flips off, emit a full reset (\x1b[0m)
//     and re-set everything in cur. Fewer bytes than computing per-attribute
//     off-codes, and avoids subtle compatibility issues across terminals.
//   - Otherwise emit only the newly-set attributes and changed colors.
func WriteStyleChange(sb *strings.Builder, prev, cur Style) {
	if prev == cur {
		return
	}
	needsReset := needsReset(prev, cur)
	var parts []string
	if needsReset {
		parts = append(parts, "0")
		prev = Style{}
	}
	if cur.Attrs&AttrBold != 0 && prev.Attrs&AttrBold == 0 {
		parts = append(parts, "1")
	}
	if cur.Attrs&AttrFaint != 0 && prev.Attrs&AttrFaint == 0 {
		parts = append(parts, "2")
	}
	if cur.Attrs&AttrItalic != 0 && prev.Attrs&AttrItalic == 0 {
		parts = append(parts, "3")
	}
	if cur.Underline != UnderlineNone && prev.Underline == UnderlineNone {
		parts = append(parts, underlineCode(cur.Underline))
	}
	if cur.Attrs&AttrBlink != 0 && prev.Attrs&AttrBlink == 0 {
		parts = append(parts, "5")
	}
	if cur.Attrs&AttrReverse != 0 && prev.Attrs&AttrReverse == 0 {
		parts = append(parts, "7")
	}
	if cur.Attrs&AttrConceal != 0 && prev.Attrs&AttrConceal == 0 {
		parts = append(parts, "8")
	}
	if cur.Attrs&AttrStrikethrough != 0 && prev.Attrs&AttrStrikethrough == 0 {
		parts = append(parts, "9")
	}
	if cur.Fg != prev.Fg {
		parts = append(parts, fgCode(cur.Fg))
	}
	if cur.Bg != prev.Bg {
		parts = append(parts, bgCode(cur.Bg))
	}
	if len(parts) == 0 {
		return
	}
	sb.WriteString("\x1b[")
	for i, p := range parts {
		if i > 0 {
			sb.WriteByte(';')
		}
		sb.WriteString(p)
	}
	sb.WriteByte('m')
}

// needsReset returns true when transitioning from prev to cur requires
// turning some attribute off. It is cheaper and more compatible to issue a
// single SGR reset than to enumerate individual "off" codes.
func needsReset(prev, cur Style) bool {
	if (prev.Attrs &^ cur.Attrs) != 0 {
		return true
	}
	if prev.Underline != UnderlineNone && cur.Underline != prev.Underline {
		return true
	}
	if prev.Fg.Kind != ColorDefault && cur.Fg.Kind == ColorDefault {
		return true
	}
	if prev.Bg.Kind != ColorDefault && cur.Bg.Kind == ColorDefault {
		return true
	}
	return false
}

func underlineCode(u UnderlineStyle) string {
	switch u {
	case UnderlineNone:
		return ""
	case UnderlineDouble:
		return "21"
	case UnderlineCurly:
		return "4:3"
	case UnderlineDotted:
		return "4:4"
	case UnderlineDashed:
		return "4:5"
	default:
		return "4"
	}
}

func fgCode(c Color) string {
	switch c.Kind {
	case ColorDefault:
		return "39"
	case ColorIndexed:
		if c.Index < 8 {
			return fmt.Sprintf("%d", 30+c.Index)
		}
		if c.Index < 16 {
			return fmt.Sprintf("%d", 90+c.Index-8)
		}
		return fmt.Sprintf("38;5;%d", c.Index)
	case ColorRGB:
		return fmt.Sprintf("38;2;%d;%d;%d", c.R, c.G, c.B)
	}
	return ""
}

func bgCode(c Color) string {
	switch c.Kind {
	case ColorDefault:
		return "49"
	case ColorIndexed:
		if c.Index < 8 {
			return fmt.Sprintf("%d", 40+c.Index)
		}
		if c.Index < 16 {
			return fmt.Sprintf("%d", 100+c.Index-8)
		}
		return fmt.Sprintf("48;5;%d", c.Index)
	case ColorRGB:
		return fmt.Sprintf("48;2;%d;%d;%d", c.R, c.G, c.B)
	}
	return ""
}
