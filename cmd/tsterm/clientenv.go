package main

import "os"

// clientEnvForward is the set of environment variables tsterm sends to the
// daemon as overrides on top of the daemon-side base env. The list mirrors
// SSH's defaults for SendEnv.
//
// COLORTERM is what modern terminals advertise truecolor with; the LC_* /
// LANG variables make non-ASCII output render correctly in tools like ls
// and less. Anything else the user's shell sets via its profile.
//
// Unlike SSH, tsterm does not send TERM: programs in the session write to
// tstermd's terminal emulator, not to this terminal, so the session's TERM
// describes that emulator (see session.Term). Sending, say,
// TERM=xterm-ghostty would name a terminal the server may not even have a
// terminfo entry for.
var clientEnvForward = []string{
	"COLORTERM",
	"LANG",
	"LC_ALL",
	"LC_CTYPE",
	"LC_MESSAGES",
}

// clientEnv returns the subset of os.Environ() to forward to the daemon
// when creating a session, formatted as "KEY=value" strings.
func clientEnv() []string {
	var out []string
	for _, k := range clientEnvForward {
		if v := os.Getenv(k); v != "" {
			out = append(out, k+"="+v)
		}
	}
	return out
}
