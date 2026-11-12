package session

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/bradfitz/tsterm/pkg/pty"
)

func TestClaudeWorktree(t *testing.T) {
	for _, tt := range []struct {
		argv []string
		want string
	}{
		{[]string{"claude", "-w", "foo"}, "foo"},
		{[]string{"/home/me/.local/bin/claude", "--worktree", "foo"}, "foo"},
		{[]string{"claude", "--worktree=foo"}, "foo"},
		{[]string{"node", "/usr/lib/node_modules/bin/claude", "-w", "foo"}, "foo"}, // run by an interpreter
		{[]string{"claude", "-r", "abc", "-w", "foo"}, "foo"},
		{[]string{"claude", "-w", "--verbose"}, ""}, // a worktree it names itself
		{[]string{"claude"}, ""},
		{[]string{"vim", "-w", "foo"}, ""},
		{[]string{"sh", "-c", "claude -w foo"}, ""},
		{nil, ""},
	} {
		if got := claudeWorktree(tt.argv); got != tt.want {
			t.Errorf("claudeWorktree(%q) = %q, want %q", tt.argv, got, tt.want)
		}
	}
}

// fakeRepo makes a repository, with a linked worktree named name, in a
// temporary directory, and returns their paths.
func fakeRepo(t *testing.T, name string) (repo, worktree string) {
	t.Helper()
	dir := t.TempDir()
	repo = filepath.Join(dir, "repo")
	worktree = filepath.Join(repo, ".claude", "worktrees", name)
	for _, d := range []string{filepath.Join(repo, ".git", "worktrees", name), filepath.Join(worktree, "sub")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: "+filepath.Join(repo, ".git", "worktrees", name)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return repo, worktree
}

func TestWorktreeName(t *testing.T) {
	repo, wt := fakeRepo(t, "foo")
	sub := filepath.Join(t.TempDir(), "sub")
	os.MkdirAll(sub, 0o755)
	os.WriteFile(filepath.Join(sub, ".git"), []byte("gitdir: ../.git/modules/sub\n"), 0o644)
	for dir, want := range map[string]string{
		wt:                             "foo",
		filepath.Join(wt, "sub"):       "foo",
		repo:                           "", // the main checkout
		filepath.Join(repo, ".claude"): "",
		sub:                            "", // a submodule
		"/":                            "",
	} {
		if got := worktreeName(dir); got != want {
			t.Errorf("worktreeName(%s) = %q, want %q", dir, got, want)
		}
	}
}

func TestSanitizeName(t *testing.T) {
	for in, want := range map[string]string{
		"foo":        "foo",
		"my feature": "my-feature",
		"a/b":        "a-b",
		"-.x.-":      "x",
		"pty-abc":    "", // reserved
		"_tsterm_x":  "",
		"":           "",
		"日本":         "",
	} {
		if got := sanitizeName(in); got != want {
			t.Errorf("sanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestAutoName checks that sessions that have never had a name get one
// from what they run, and that sessions that have had one keep theirs.
func TestAutoName(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("auto-naming needs /proc")
	}
	m, _ := newTestManager(t)
	_, wt := fakeRepo(t, "foo")
	inWorktree := func() Config {
		c := makeCfg("sleep 30")
		c.PTY.Dir = filepath.Join(wt, "sub")
		return c
	}
	a, err := m.Create(inWorktree())
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.Create(inWorktree())
	if err != nil {
		t.Fatal(err)
	}

	// A fake Claude Code, run by sh.
	bin := t.TempDir()
	claude := filepath.Join(bin, "claude")
	if err := os.WriteFile(claude, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	c, err := m.Create(Config{PTY: pty.Config{Command: claude, Args: []string{"-w", "bar"}}})
	if err != nil {
		t.Fatal(err)
	}

	named := makeCfg("sleep 30")
	named.Name = "mine"
	named.PTY.Dir = wt
	d, err := m.Create(named)
	if err != nil {
		t.Fatal(err)
	}
	e, err := m.Create(inWorktree())
	if err != nil {
		t.Fatal(err)
	}
	// A name removed isn't replaced.
	if err := m.Rename(string(e.ID()), "temp"); err != nil {
		t.Fatal(err)
	}
	if err := m.Rename(string(e.ID()), ""); err != nil {
		t.Fatal(err)
	}

	time.Sleep(300 * time.Millisecond) // for the programs to start
	m.autoNameOnce()
	names := map[*Session]string{}
	for _, s := range []*Session{a, b, c, d, e} {
		names[s] = s.Name()
	}
	if got := []string{names[a], names[b]}; !(got[0] == "foo" && got[1] == "foo-2" || got[0] == "foo-2" && got[1] == "foo") {
		t.Errorf("sessions in worktree foo named %q", got)
	}
	if names[c] != "bar" {
		t.Errorf("claude -w bar's session named %q", names[c])
	}
	if names[d] != "mine" {
		t.Errorf("a named session was renamed %q", names[d])
	}
	if names[e] != "" {
		t.Errorf("a session whose name was removed was named %q", names[e])
	}
	// And names stick.
	m.autoNameOnce()
	if c.Name() != "bar" || m.ByName("bar") != c {
		t.Errorf("after again, %q", c.Name())
	}
}
