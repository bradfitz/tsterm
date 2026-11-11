package demoapp

import (
	"reflect"
	"testing"
)

func TestParser(t *testing.T) {
	tests := []struct {
		name   string
		chunks []string
		want   []event
	}{
		{"keys", []string{"ab"}, []event{{kind: evKey, text: "a"}, {kind: evKey, text: "b"}}},
		{"utf-8 split across reads", []string{"\xe4\xb8", "\x96"}, []event{{kind: evKey, text: "世"}}},
		{"arrow", []string{"\x1bOA"}, []event{{kind: evKey, text: "\x1bOA"}}},
		{"csi key", []string{"\x1b[3~"}, []event{{kind: evKey, text: "\x1b[3~"}}},
		{"alt key", []string{"\x1bx"}, []event{{kind: evKey, text: "\x1bx"}}},
		{"device attributes", []string{"\x1b[?62;22c"}, []event{{kind: evDA, text: "62;22"}}},
		{"cursor position", []string{"\x1b[1;3R"}, []event{{kind: evCPR, text: "1;3"}}},
		{"background color", []string{"\x1b]11;rgb:0000/0000/0000\x1b\\"}, []event{{kind: evOSC, text: "11;rgb:0000/0000/0000"}}},
		{"focus", []string{"\x1b[I\x1b[O"}, []event{{kind: evFocus, text: "in"}, {kind: evFocus, text: "out"}}},
		{"click", []string{"\x1b[<0;5;3M"}, []event{{kind: evMouse, x: 4, y: 2, press: true, text: "press button 0 at column 5, row 3"}}},
		{"release", []string{"\x1b[<0;5;3m"}, []event{{kind: evMouse, x: 4, y: 2, text: "release button 0 at column 5, row 3"}}},
		{"wheel", []string{"\x1b[<64;1;1M"}, []event{{kind: evMouse, text: "wheel button 0 at column 1, row 1"}}},
		{"paste", []string{"\x1b[200~hi\r\nthere\x1b[201~"}, []event{{kind: evPaste, text: "hi\r\nthere"}}},
		{"paste split across reads", []string{"\x1b[200~one ", "two\x1b[2", "01~q"}, []event{{kind: evPaste, text: "one two"}, {kind: evKey, text: "q"}}},
		{"paste with an escape inside", []string{"\x1b[200~a\x1bb\x1b[201~"}, []event{{kind: evPaste, text: "a\x1bb"}}},
	}
	for _, tt := range tests {
		var p parser
		var got []event
		for _, c := range tt.chunks {
			got = append(got, p.feed([]byte(c))...)
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: got %+v, want %+v", tt.name, got, tt.want)
		}
	}
}

func TestParserLoneEscape(t *testing.T) {
	var p parser
	if evs := p.feed([]byte("\x1b")); len(evs) != 0 {
		t.Fatalf("lone ESC parsed immediately: %+v", evs)
	}
	if p.escTimeout() == nil {
		t.Fatal("no timeout for a lone ESC")
	}
	evs := p.flush()
	if len(evs) != 1 || evs[0].text != "\x1b" {
		t.Errorf("flush = %+v, want the Escape key", evs)
	}
	// An unfinished paste must not be cut short by the timeout.
	p.feed([]byte("\x1b[200~ab"))
	if p.escTimeout() != nil {
		t.Error("a pending paste started the Escape timeout")
	}
}

func TestLogRegion(t *testing.T) {
	for _, rows := range []int{6, 10, 24, 60} {
		top, bottom := LogRegion(rows)
		if bottom != rows || top < 4 || top > bottom {
			t.Errorf("LogRegion(%d) = %d, %d", rows, top, bottom)
		}
	}
}
