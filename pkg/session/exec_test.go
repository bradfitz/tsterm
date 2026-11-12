//go:build linux

package session

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/bradfitz/tsterm/pkg/shim"
)

// TestExecShim checks sessions whose shims are separate processes, this
// test binary run as one (as tstermd runs itself), and that they outlive
// the Manager, as they outlive tstermd.
func TestExecShim(t *testing.T) {
	self, err := shim.SelfPath()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("", "sess")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	l := &shim.Exec{Path: self}
	m := NewManager(dir, l)
	m.SetStateDir(filepath.Join(dir, "state"))
	s, err := m.Create(makeCfg("echo from-a-real-shim; stty -echo; cat"))
	if err != nil {
		t.Fatal(err)
	}
	waitScreen(t, s, "from-a-real-shim")
	m.Detach()

	m2 := NewManager(dir, l)
	m2.SetStateDir(filepath.Join(dir, "state"))
	t.Cleanup(func() { m2.Close() })
	if n, err := m2.Restore(context.Background()); err != nil || n != 1 {
		t.Fatalf("Restore = %d, %v", n, err)
	}
	s2 := m2.Get(s.ID())
	s2.WriteInput([]byte("still-here\n"))
	waitScreen(t, s2, "still-here")
}
