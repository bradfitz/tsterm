//go:build unix

package pty

import (
	"io"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cpty "github.com/creack/pty"
)

func TestEchoOutput(t *testing.T) {
	p, err := Start(Config{
		Command: "/bin/sh",
		Args:    []string{"-c", "echo hi"},
		Rows:    24,
		Cols:    80,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()

	out := readAllWithTimeout(t, p, 3*time.Second)
	if !strings.Contains(out, "hi") {
		t.Errorf("output = %q, want to contain %q", out, "hi")
	}
	if err := p.Wait(); err != nil {
		t.Errorf("Wait: %v", err)
	}
}

func TestExitCodeNonZero(t *testing.T) {
	p, err := Start(Config{
		Command: "/bin/sh",
		Args:    []string{"-c", "exit 7"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()
	_ = readAllWithTimeout(t, p, 3*time.Second)
	err = p.Wait()
	if err == nil {
		t.Fatal("Wait returned nil, want exit-error")
	}
	if !strings.Contains(err.Error(), "exit status 7") {
		t.Errorf("Wait error = %v, want exit status 7", err)
	}
}

func TestResizeRoundtrip(t *testing.T) {
	// Run cat so the PTY stays open while we resize.
	p, err := Start(Config{
		Command: "/bin/cat",
		Rows:    10,
		Cols:    20,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		_ = p.Close()
		_ = p.Wait()
	}()

	rows, cols, err := cpty.Getsize(p.File)
	if err != nil {
		t.Fatalf("Getsize: %v", err)
	}
	if rows != 10 || cols != 20 {
		t.Errorf("initial size = %dx%d, want 10x20", rows, cols)
	}

	if err := p.Resize(40, 132); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	rows, cols, err = cpty.Getsize(p.File)
	if err != nil {
		t.Fatalf("Getsize: %v", err)
	}
	if rows != 40 || cols != 132 {
		t.Errorf("size after resize = %dx%d, want 40x132", rows, cols)
	}
}

func TestWriteToChild(t *testing.T) {
	// cat echoes its input. Read it back and verify.
	p, err := Start(Config{
		Command: "/bin/cat",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		_ = p.Close()
		_ = p.Wait()
	}()

	go func() {
		_, _ = p.Write([]byte("ping\n"))
	}()

	out := readUntil(t, p, "ping", 3*time.Second)
	if !strings.Contains(out, "ping") {
		t.Errorf("cat output = %q, want to contain %q", out, "ping")
	}
}

func TestLoginShellArgv0(t *testing.T) {
	cmd := buildCmd(Config{}) // default = login
	want := "-" + filepath.Base(cmd.Path)
	if cmd.Args[0] != want {
		t.Errorf("argv[0] = %q, want %q", cmd.Args[0], want)
	}
}

func TestNoLoginShellArgv0(t *testing.T) {
	cmd := buildCmd(Config{NoLogin: true})
	if strings.HasPrefix(cmd.Args[0], "-") {
		t.Errorf("argv[0] = %q, expected no leading dash", cmd.Args[0])
	}
	// Non-login default shell should at least be interactive.
	joined := strings.Join(cmd.Args, " ")
	if !strings.Contains(joined, "-i") {
		t.Errorf("expected -i in args, got %q", joined)
	}
}

func TestExplicitCommandSkipsLogin(t *testing.T) {
	cmd := buildCmd(Config{Command: "/bin/sh", Args: []string{"-c", "echo hi"}})
	if strings.HasPrefix(cmd.Args[0], "-") {
		t.Errorf("argv[0] = %q, expected no dash when Command is set", cmd.Args[0])
	}
}

func TestBaseEnvContainsExpectedVars(t *testing.T) {
	env := buildEnv(nil)
	required := []string{"TERM=", "PATH=", "HOME=", "USER=", "LOGNAME=", "SHELL="}
	got := strings.Join(env, "\n")
	for _, k := range required {
		if !strings.Contains(got, k) {
			t.Errorf("env missing %q\n%s", k, got)
		}
	}
}

func TestEnvOverridesApply(t *testing.T) {
	env := buildEnv([]string{"TERM=tmux-256color", "MYVAR=hi"})
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "TERM=tmux-256color") {
		t.Errorf("TERM override not applied: %s", joined)
	}
	if !strings.Contains(joined, "MYVAR=hi") {
		t.Errorf("MYVAR override not applied: %s", joined)
	}
}

func TestDefaultDirIsHome(t *testing.T) {
	cmd := buildCmd(Config{})
	u, err := user.Current()
	if err != nil || u.HomeDir == "" {
		t.Skip("no current user with HomeDir")
	}
	if cmd.Dir != u.HomeDir {
		t.Errorf("Dir = %q, want %q", cmd.Dir, u.HomeDir)
	}
}

func TestLoginShellRunsProfile(t *testing.T) {
	// A login shell prints a different $0 than a non-login one. We can ask
	// the shell itself: in bash, $0 starts with "-" for a login shell. Use
	// `printf %s "$0"` to print it without a newline.
	p, err := Start(Config{
		Args: []string{"-c", `printf %s "$0"; exit`},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()
	out := readAllWithTimeout(t, p, 3*time.Second)
	_ = p.Wait()
	if !strings.Contains(out, "-") {
		t.Errorf("login-shell $0 should contain '-': got %q", out)
	}
}

// readAllWithTimeout drains p until EOF or timeout. PTY masters often return
// EIO rather than EOF when the slave closes; both terminate the read.
func readAllWithTimeout(t *testing.T, r io.Reader, d time.Duration) string {
	t.Helper()
	type result struct {
		s   string
		err error
	}
	ch := make(chan result, 1)
	go func() {
		b, err := io.ReadAll(r)
		ch <- result{string(b), err}
	}()
	select {
	case res := <-ch:
		return res.s
	case <-time.After(d):
		t.Fatalf("read timed out after %v", d)
		return ""
	}
}

// readUntilNeedle reads from p until needle appears in its output or the
// deadline elapses. It relies on (*os.File).SetReadDeadline, which works for
// PTY masters on Unix.
func readUntil(t *testing.T, p *PTY, needle string, d time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(d)
	if err := p.File.SetReadDeadline(deadline); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	defer p.File.SetReadDeadline(time.Time{})

	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := p.File.Read(buf)
		if n > 0 {
			b.Write(buf[:n])
			if strings.Contains(b.String(), needle) {
				return b.String()
			}
		}
		if err != nil {
			t.Fatalf("readUntil(%q): %v; got %q", needle, err, b.String())
			return b.String()
		}
	}
}
