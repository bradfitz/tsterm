package wsclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bradfitz/tsterm/pkg/proto"
	"github.com/bradfitz/tsterm/pkg/pty"
	"github.com/bradfitz/tsterm/pkg/session"
	"github.com/bradfitz/tsterm/pkg/session/sessiontest"
	"github.com/bradfitz/tsterm/pkg/transport/ws"
	"github.com/bradfitz/tsterm/pkg/vt"
)

func TestKeyBytes(t *testing.T) {
	tests := []struct {
		k         Key
		appCursor bool
		want      string
	}{
		{Key{Rune: 'a'}, false, "a"},
		{Key{Rune: 'é'}, false, "é"},
		{Key{Rune: 'c', Ctrl: true}, false, "\x03"},
		{Key{Rune: 'C', Ctrl: true}, false, "\x03"},
		{Key{Rune: ' ', Ctrl: true}, false, "\x00"},
		{Key{Rune: 'x', Alt: true}, false, "\x1bx"},
		{Key{Name: "Enter"}, false, "\r"},
		{Key{Name: "Backspace"}, false, "\x7f"},
		{Key{Name: "Tab", Shift: true}, false, "\x1b[Z"},
		{Key{Name: "ArrowUp"}, false, "\x1b[A"},
		{Key{Name: "ArrowUp"}, true, "\x1bOA"},
		{Key{Name: "ArrowLeft", Ctrl: true}, true, "\x1b[1;5D"},
		{Key{Name: "Delete"}, false, "\x1b[3~"},
		{Key{Name: "F1"}, false, "\x1bOP"},
		{Key{Name: "F5", Shift: true}, false, "\x1b[15;2~"},
		{Key{Name: "Shift"}, false, ""},
	}
	for _, tt := range tests {
		if got := KeyBytes(tt.k, tt.appCursor); got != tt.want {
			t.Errorf("KeyBytes(%+v, %v) = %q, want %q", tt.k, tt.appCursor, got, tt.want)
		}
	}
}

func TestMouseReport(t *testing.T) {
	if got := MouseReport(MouseLeft, 4, 2, false, false, false, false, true); got != "\x1b[<0;5;3M" {
		t.Errorf("SGR press = %q", got)
	}
	if got := MouseReport(MouseLeft, 4, 2, true, false, false, true, true); got != "\x1b[<16;5;3m" {
		t.Errorf("SGR ctrl release = %q", got)
	}
	if got := MouseReport(MouseLeft, 0, 0, true, false, false, false, false); got != "\x1b[M#!!" {
		t.Errorf("X10 release = %q", got)
	}
}

func TestPaste(t *testing.T) {
	if got := Paste("a\nb\r\nc", false); got != "a\rb\rc" {
		t.Errorf("Paste = %q", got)
	}
	if got := Paste("x\x1b[201~y", true); got != "\x1b[200~xy\x1b[201~" {
		t.Errorf("bracketed Paste = %q", got)
	}
}

func TestWrapLine(t *testing.T) {
	text := func(rows [][]vt.Cell) []string {
		var out []string
		for _, r := range rows {
			var b strings.Builder
			for _, c := range r {
				b.WriteString(c.Grapheme)
			}
			out = append(out, b.String())
		}
		return out
	}
	if got := text(WrapLine(vt.Line{Text: "abcdefg"}, 3)); strings.Join(got, "|") != "abc|def|g" {
		t.Errorf("wrap = %q", got)
	}
	if got := text(WrapLine(vt.Line{}, 3)); len(got) != 1 || got[0] != "" {
		t.Errorf("empty line = %q", got)
	}
	// A wide character that doesn't fit goes to the next row.
	rows := WrapLine(vt.Line{Text: "ab世e"}, 3)
	if got := text(rows); strings.Join(got, "|") != "ab|世e" || rows[1][0].Width != 2 || rows[1][1].Width != 0 {
		t.Errorf("wide wrap = %q, %+v", got, rows)
	}
	// Combining marks stay with their base, and runs keep their styles.
	red := vt.Style{Fg: vt.Color{Kind: vt.ColorIndexed, Index: 1}}
	rows = WrapLine(vt.Line{Text: "éx", Runs: []vt.Run{{Len: 3, Style: red}, {Len: 1}}}, 10)
	if len(rows[0]) != 2 || rows[0][0].Grapheme != "é" || rows[0][0].Style != red || rows[0][1].Style != (vt.Style{}) {
		t.Errorf("styled = %+v", rows)
	}
}

func TestHistory(t *testing.T) {
	var h History
	h.SetTotal(1200)
	from, count, ok := h.Next(false, true)
	if !ok || from != 700 || count != 500 {
		t.Fatalf("first request = %d, %d, %v", from, count, ok)
	}
	if _, _, ok := h.Next(false, true); ok {
		t.Error("two requests in flight")
	}
	lines := make([]vt.Line, 500)
	h.Apply(&proto.History{From: 700, Lines: lines}, true)
	if h.From != 700 || h.To() != 1200 {
		t.Errorf("loaded %d-%d", h.From, h.To())
	}
	from, count, ok = h.Next(true, false)
	if !ok || from != 200 || count != 500 {
		t.Fatalf("older request = %d, %d, %v", from, count, ok)
	}
	if n := h.Apply(&proto.History{From: 200, Lines: lines}, false); n != 500 || h.From != 200 {
		t.Errorf("prepended %d, from %d", n, h.From)
	}
	// Far behind while following, it skips to the newest.
	h.SetTotal(100000)
	from, _, _ = h.Next(false, true)
	if from != 100000-HistoryChunk || len(h.Lines) != 0 {
		t.Errorf("after skipping, asked from %d with %d loaded", from, len(h.Lines))
	}
}

// TestLive checks the client against a real tstermd's handlers: listing,
// creating, streaming a screen, typing, and history.
func TestLive(t *testing.T) {
	mgr := sessiontest.NewManager(t)
	mux := http.NewServeMux()
	mux.Handle("/v1/sessions/{id}/ws", (&ws.Server{Manager: mgr}).Handler())
	hts := httptest.NewServer(mux)
	t.Cleanup(hts.Close)
	c := NewClient(strings.TrimPrefix(hts.URL, "http://"))
	if c.Base != hts.URL {
		t.Fatalf("Base = %q, want %q", c.Base, hts.URL)
	}

	sess, err := mgr.Create(session.Config{
		PTY:  pty.Config{Command: "/bin/sh", Args: []string{"-c", "i=0; while [ $i -lt 40 ]; do echo old-$i; i=$((i+1)); done; stty -echo; cat"}},
		Rows: 10, Cols: 40,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := c.Dial(ctx, string(sess.ID()), true)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var st State
	var hist History
	screenHas := func(s string) bool {
		for _, row := range st.Screen.Cells {
			var b strings.Builder
			for _, c := range row {
				b.WriteString(c.Grapheme)
			}
			if strings.Contains(b.String(), s) {
				return true
			}
		}
		return false
	}
	until := func(what string, done func(Change) bool) {
		t.Helper()
		for {
			env, err := conn.Recv(ctx)
			if err != nil {
				t.Fatalf("waiting for %s: %v", what, err)
			}
			ch := st.Apply(env)
			conn.Ack(ch.Seq)
			if ch.Lines != nil {
				hist.Apply(ch.Lines, true)
			}
			if done(ch) {
				return
			}
		}
	}
	until("old-39", func(Change) bool { return screenHas("old-39") })
	conn.Input("typed\n")
	until("typed", func(Change) bool { return screenHas("typed") })

	hist.SetTotal(st.Screen.History)
	from, count, ok := hist.Next(false, true)
	if !ok {
		t.Fatal("no history to ask for")
	}
	conn.History(from, count)
	until("history", func(ch Change) bool { return ch.Lines != nil })
	if len(hist.Lines) == 0 || hist.Lines[0].Text != "old-0" {
		t.Errorf("history = %+v", hist.Lines)
	}

	if err := conn.Resize(12, 50, true); err != nil {
		t.Fatal(err)
	}
	until("resize", func(Change) bool { return st.Screen.Rows == 12 && st.Screen.Cols == 50 })
}
