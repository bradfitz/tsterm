package session

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Auto-naming gives a session that has never had a name one, from what
// it's doing, as the program in front's git worktree: "claude -w foo"
// names its session "foo". Once a session has had a name, from the user or
// from auto-naming, it's left be, even if its name is removed.

// autoNameInterval is how often AutoName looks at sessions. Tests lower it.
var autoNameInterval = 5 * time.Second

// AutoName names unnamed sessions, from time to time, until ctx is done.
func (m *Manager) AutoName(ctx context.Context) {
	t := time.NewTicker(autoNameInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.autoNameOnce()
		}
	}
}

func (m *Manager) autoNameOnce() {
	for _, s := range m.Sessions() {
		if !s.Active() || s.everNamed() {
			continue
		}
		name := sanitizeName(suggestName(s.Pid()))
		if name == "" {
			continue
		}
		if got, err := m.autoRename(s, name); err != nil {
			m.logf("session %s: auto-naming %q: %v", s.ID(), name, err)
		} else if got != "" {
			m.logf("session %s: named %q, for what it's running", s.ID(), got)
		}
	}
}

// everNamed reports whether the session has ever had a name.
func (s *Session) everNamed() bool {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	return s.meta.Named || s.meta.Name != ""
}

// autoRename names s name, or name-2, name-3, and so on, the first free,
// unless it has been named meanwhile, and returns the name it gave.
func (m *Manager) autoRename(s *Session, name string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.everNamed() {
		return "", nil // named meanwhile, as by the user
	}
	for i := 1; ; i++ {
		n := name
		if i > 1 {
			n += "-" + strconv.Itoa(i)
		}
		if _, taken := m.byName[n]; taken {
			continue
		}
		if !ValidName(n) {
			return "", nil
		}
		if err := s.setName(n); err != nil {
			return "", err
		}
		m.byName[n] = s.ID()
		return n, nil
	}
}

// sanitizeName makes s a valid session name, replacing other characters
// with "-", or returns "" if it can't.
func sanitizeName(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		}
		return '-'
	}, s)
	s = strings.Trim(s, "-.")
	if len(s) > 56 { // room for a -N suffix
		s = s[:56]
	}
	if !ValidName(s) {
		return ""
	}
	return s
}

// suggestName suggests a name for the session whose child (its shell) is
// pid, from the program in front, its foreground process group's leader:
// a worktree named on Claude Code's command line, as "claude -w foo", or
// else the git worktree it's working in.
func suggestName(pid int) string {
	fg := foreground(pid)
	if fg <= 0 {
		return ""
	}
	if n := claudeWorktree(cmdline(fg)); n != "" {
		return n
	}
	if cwd, err := os.Readlink(procPath(fg, "cwd")); err == nil {
		return worktreeName(cwd)
	}
	return ""
}

// claudeWorktree returns the worktree named on a Claude Code command line,
// with -w or --worktree.
func claudeWorktree(argv []string) string {
	i := 0
	for i < len(argv) && filepath.Base(argv[i]) != "claude" {
		i++
		if i > 1 {
			return "" // claude is the program, or what an interpreter runs
		}
	}
	if i >= len(argv) {
		return ""
	}
	args := argv[i+1:]
	for j, a := range args {
		if v, ok := strings.CutPrefix(a, "--worktree="); ok {
			return v
		}
		if (a == "-w" || a == "--worktree") && j+1 < len(args) && !strings.HasPrefix(args[j+1], "-") {
			return args[j+1]
		}
		if a == "--" {
			break
		}
	}
	return ""
}

// worktreeName returns the name of the linked git worktree dir is in, if
// it's in one: its .git is a file pointing into the repository's
// .git/worktrees. A main checkout has no such name.
func worktreeName(dir string) string {
	for d := dir; ; d = filepath.Dir(d) {
		fi, err := os.Lstat(filepath.Join(d, ".git"))
		if err == nil {
			if fi.IsDir() {
				return "" // a main checkout
			}
			b, err := os.ReadFile(filepath.Join(d, ".git"))
			if err != nil {
				return ""
			}
			gitdir, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir: ")
			if !ok || filepath.Base(filepath.Dir(gitdir)) != "worktrees" {
				return "" // a submodule, say
			}
			return filepath.Base(gitdir)
		}
		if filepath.Dir(d) == d {
			return ""
		}
	}
}
