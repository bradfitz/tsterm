package vt

import (
	"strings"
	"testing"
)

func TestStyleDiffEmitsMinimalSGR(t *testing.T) {
	var sb strings.Builder
	prev := Style{}
	cur := Style{Attrs: AttrBold, Fg: Color{Kind: ColorIndexed, Index: 1}}
	WriteStyleChange(&sb, prev, cur)
	got := sb.String()
	if got != "\x1b[1;31m" {
		t.Errorf("bold+red SGR = %q, want %q", got, "\x1b[1;31m")
	}
}

func TestStyleDiffResetsWhenAttrTurnsOff(t *testing.T) {
	var sb strings.Builder
	prev := Style{Attrs: AttrBold, Fg: Color{Kind: ColorIndexed, Index: 1}}
	cur := Style{Fg: Color{Kind: ColorIndexed, Index: 2}}
	WriteStyleChange(&sb, prev, cur)
	got := sb.String()
	if !strings.HasPrefix(got, "\x1b[0;") || !strings.Contains(got, "32") {
		t.Errorf("expected reset+green, got %q", got)
	}
}

func TestStyleDiffNoOpWhenIdentical(t *testing.T) {
	var sb strings.Builder
	s := Style{Attrs: AttrBold}
	WriteStyleChange(&sb, s, s)
	if sb.Len() != 0 {
		t.Errorf("identical styles emitted %q", sb.String())
	}
}

func TestFgCodeTrueColor(t *testing.T) {
	got := fgCode(Color{Kind: ColorRGB, R: 10, G: 20, B: 30})
	if got != "38;2;10;20;30" {
		t.Errorf("fg truecolor = %q", got)
	}
}
