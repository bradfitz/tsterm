package main

import (
	"strings"
	"testing"
)

func TestUnitFile(t *testing.T) {
	unit := unitFile("/home/me/go/bin/tstermd", []string{"-port", "2300", "-sessions-dir", "/home/me/my sessions", "-hostname", "100%$x"})
	want := `ExecStart=/home/me/go/bin/tstermd run -port 2300 -sessions-dir "/home/me/my sessions" -hostname 100%%$$x` + "\n"
	if !strings.Contains(unit, want) {
		t.Errorf("unit:\n%s\nwant a line:\n%s", unit, want)
	}
	for _, s := range []string{"[Service]", "Restart=on-failure", "WantedBy=default.target"} {
		if !strings.Contains(unit, s) {
			t.Errorf("unit lacks %q", s)
		}
	}
}

func TestSystemdQuote(t *testing.T) {
	for in, want := range map[string]string{
		"plain":     "plain",
		"":          `""`,
		"a b":       `"a b"`,
		`say "hi"`:  `"say \"hi\""`,
		`back\path`: `"back\\path"`,
		"50%":       "50%%",
	} {
		if got := systemdQuote(in); got != want {
			t.Errorf("systemdQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestGoRunBuild(t *testing.T) {
	if !goRunBuild("/tmp/go-build123/b001/exe/tstermd") || goRunBuild("/home/me/go/bin/tstermd") {
		t.Error("goRunBuild is wrong")
	}
}
