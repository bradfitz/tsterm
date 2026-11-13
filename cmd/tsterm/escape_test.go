package main

import "testing"

func TestEscaper(t *testing.T) {
	tests := []struct {
		name       string
		chunks     []string // separate reads
		want       string
		disconnect bool
	}{
		{"plain", []string{"hello"}, "hello", false},
		{"disconnect", []string{"ls\r\x1e."}, "ls\r", true},
		{"split across reads", []string{"ab\x1e", ".cd"}, "ab", true},
		{"literal escape, for the server", []string{"\x1e\x1e"}, "\x1e\x1e", false},
		{"session menu, for the server", []string{"\x1e\""}, "\x1e\"", false},
		{"other key passes through", []string{"\x1ex"}, "\x1ex", false},
		{"pending at end", []string{"a\x1e"}, "a", false},
	}
	for _, tt := range tests {
		var e escaper
		var got string
		var disconnect bool
		for _, c := range tt.chunks {
			out, d := e.filter([]byte(c))
			got += string(out)
			if d {
				disconnect = true
				break
			}
		}
		if got != tt.want || disconnect != tt.disconnect {
			t.Errorf("%s: got %q, disconnect=%v; want %q, %v", tt.name, got, disconnect, tt.want, tt.disconnect)
		}
	}
}
