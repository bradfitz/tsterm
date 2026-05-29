// Package pty owns the PTY lifecycle for tsterm: it forks and execs a
// command (typically a login shell) attached to a fresh pseudo-terminal,
// exposes the master file for I/O, and provides resize, wait, and close.
//
// pkg/pty deliberately does no terminal emulation and no transport. The
// caller is expected to copy bytes from PTY.File into a vt.Terminal, and
// to copy client input bytes into PTY.File. Higher layers (pkg/session)
// orchestrate that pumping plus watcher fanout.
package pty

import (
	"os"
	"os/exec"
)

// Config describes a command to run under a fresh PTY. The zero value
// produces a login shell for the user the daemon is running as, with a
// clean SSH-like environment.
type Config struct {
	// Command is the program to exec. If empty, $SHELL is consulted and
	// falls back to /bin/sh, and the shell is launched as a login shell
	// (see NoLogin below).
	Command string

	// Args is the argument list passed to the command, not including
	// argv[0]. If Command is empty and Args is nil, the shell is invoked
	// with no arguments: login mode (see NoLogin) plus the kernel/terminal
	// combination naturally produce an interactive session.
	Args []string

	// Env is a list of "KEY=value" pairs applied on top of the base
	// environment built per session (TERM, PATH, HOME, USER, LOGNAME,
	// SHELL). Variables in Env override the base. Callers that want a
	// fully-controlled environment can supply every variable they care
	// about; everything else still comes from the base, which is much
	// smaller than os.Environ() and matches what an SSH login receives.
	Env []string

	// Dir is the child's working directory. If empty, $HOME is used (the
	// SSH-style default), with "/" as a final fallback.
	Dir string

	// Rows and Cols set the initial PTY size. If either is zero a
	// reasonable default (24x80) is used.
	Rows, Cols int

	// NoLogin disables login-shell semantics when Command is empty. With
	// NoLogin = true the default shell is exec'd as a plain interactive
	// shell ("-i") instead of a login shell (argv[0] prefixed with "-").
	// Has no effect when Command is set.
	NoLogin bool
}

// PTY is a running command attached to a pseudo-terminal master file.
//
// File is the PTY master: read it to consume the child's output, write to
// it to feed the child's stdin. Cmd is the underlying *exec.Cmd; callers
// may inspect Cmd.Process for the PID or call Cmd.Process.Signal to deliver
// signals.
//
// PTY is not safe for concurrent calls to Close from multiple goroutines.
// Read, Write, and Resize on the master file are safe to call concurrently
// with each other; that is the documented behavior of *os.File on Unix.
type PTY struct {
	File *os.File
	Cmd  *exec.Cmd
}

// Start spawns cfg.Command under a new PTY and returns a PTY whose File is
// the master end of that PTY. The returned PTY's Cmd has already had Start
// called on it; callers must call Wait or Close to reap the child.
func Start(cfg Config) (*PTY, error) {
	return start(cfg)
}

// Pid returns the child PID, or 0 if the process has not started or has
// already been reaped.
func (p *PTY) Pid() int {
	if p == nil || p.Cmd == nil || p.Cmd.Process == nil {
		return 0
	}
	return p.Cmd.Process.Pid
}

// Wait blocks until the child exits and returns its exit error, if any.
// The behavior matches *exec.Cmd.Wait.
func (p *PTY) Wait() error {
	if p == nil || p.Cmd == nil {
		return os.ErrInvalid
	}
	return p.Cmd.Wait()
}

// Signal delivers sig to the child process.
func (p *PTY) Signal(sig os.Signal) error {
	if p == nil || p.Cmd == nil || p.Cmd.Process == nil {
		return os.ErrInvalid
	}
	return p.Cmd.Process.Signal(sig)
}

// Read reads bytes produced by the child from the PTY master.
func (p *PTY) Read(b []byte) (int, error) { return p.File.Read(b) }

// Write sends bytes to the child as if they had been typed.
func (p *PTY) Write(b []byte) (int, error) { return p.File.Write(b) }

// Close closes the PTY master. The child sees its slave hang up and will
// typically exit; callers should still call Wait to reap it. Close does
// not kill the child.
func (p *PTY) Close() error {
	if p == nil || p.File == nil {
		return nil
	}
	return p.File.Close()
}
