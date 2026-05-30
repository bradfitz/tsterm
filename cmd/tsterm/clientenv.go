package main

import "os"

// clientEnvForward is the set of environment variables tsterm sends to the
// daemon as overrides on top of the daemon-side base env. The list mirrors
// SSH's defaults for SendEnv plus TERM, which is essentially universal.
//
// TERM matters most (it controls escape-sequence handling in the remote
// shell); COLORTERM is what modern terminals advertise truecolor with; the
// LC_* / LANG variables make non-ASCII output render correctly in tools
// like ls and less. Anything else the user's shell sets via its profile.
var clientEnvForward = []string{
	"TERM",
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
