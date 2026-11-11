// Package sessiontest provides session.Manager instances for tests.
package sessiontest

import (
	"os"
	"testing"

	"github.com/bradfitz/tsterm/pkg/session"
	"github.com/bradfitz/tsterm/pkg/shim"
)

// NewManager returns a session.Manager whose shims run in-process, in a
// fresh temporary directory. Every session is killed when the test ends.
func NewManager(tb testing.TB) *session.Manager {
	tb.Helper()
	// Not tb.TempDir: its long paths can exceed the unix socket path limit.
	dir, err := os.MkdirTemp("", "sess")
	if err != nil {
		tb.Fatal(err)
	}
	l := &shim.InProcess{}
	m := session.NewManager(dir, l)
	tb.Cleanup(func() {
		m.Close()
		l.Close()
		os.RemoveAll(dir)
	})
	return m
}
