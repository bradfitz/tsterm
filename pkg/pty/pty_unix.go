//go:build unix

package pty

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strings"

	cpty "github.com/creack/pty"
)

func start(cfg Config) (*PTY, error) {
	cmd := buildCmd(cfg)
	ws := &cpty.Winsize{
		Rows: uint16(orDefault(cfg.Rows, 24)),
		Cols: uint16(orDefault(cfg.Cols, 80)),
	}
	f, err := cpty.StartWithSize(cmd, ws)
	if err != nil {
		return nil, fmt.Errorf("pty: start %s: %w", cmd.Path, err)
	}
	return &PTY{File: f, Cmd: cmd}, nil
}

// Resize sets the PTY's window size. This typically causes the child to
// receive SIGWINCH.
func (p *PTY) Resize(rows, cols int) error {
	if p == nil || p.File == nil {
		return os.ErrInvalid
	}
	return cpty.Setsize(p.File, &cpty.Winsize{
		Rows: uint16(rows),
		Cols: uint16(cols),
	})
}

func buildCmd(cfg Config) *exec.Cmd {
	command := cfg.Command
	args := cfg.Args
	defaultShell := command == ""
	if defaultShell {
		command = shellPath()
	}
	cmd := exec.Command(command, args...)
	if defaultShell {
		if cfg.NoLogin {
			if len(args) == 0 {
				cmd.Args = append(cmd.Args, "-i")
			}
		} else {
			// Login shell: the traditional Unix signal is argv[0] starting
			// with "-". The shell consults that and sources its login
			// startup files (/etc/profile, ~/.profile, ~/.bash_profile,
			// etc.) so the user sees the same PATH, prompt, and aliases
			// they would over SSH.
			cmd.Args[0] = "-" + filepath.Base(command)
		}
	}
	cmd.Env = buildEnv(cfg.Env)
	cmd.Dir = cfg.Dir
	if cmd.Dir == "" {
		cmd.Dir = homeDir()
	}
	return cmd
}

// defaultPath mirrors the secure PATH that SSH and login(1) install for
// fresh sessions when none is otherwise configured. The login shell's
// profile typically overwrites it.
const defaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// buildEnv constructs the child's environment as a base set of SSH-style
// variables plus any caller-supplied overrides.
//
// The base is intentionally small: TERM, PATH, HOME, USER, LOGNAME, SHELL.
// We do not inherit os.Environ() because the daemon's environment may
// carry variables the caller never intended to leak into a user shell
// (GOPATH, editor sockets, etc.); the login shell's profile will repopulate
// PATH and anything else it cares about.
func buildEnv(overrides []string) []string {
	base := map[string]string{
		"TERM": "xterm-256color",
		"PATH": defaultPath,
	}
	if u, err := user.Current(); err == nil {
		if u.HomeDir != "" {
			base["HOME"] = u.HomeDir
		}
		if u.Username != "" {
			base["USER"] = u.Username
			base["LOGNAME"] = u.Username
		}
	}
	base["SHELL"] = shellPath()
	for _, kv := range overrides {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		base[kv[:eq]] = kv[eq+1:]
	}
	out := make([]string, 0, len(base))
	for k, v := range base {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

func homeDir() string {
	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		return u.HomeDir
	}
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return "/"
}

func shellPath() string {
	if s := os.Getenv("SHELL"); s != "" {
		return s
	}
	return "/bin/sh"
}

func orDefault(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}
