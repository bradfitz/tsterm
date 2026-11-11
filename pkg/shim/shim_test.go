//go:build unix

package shim

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bradfitz/tsterm/pkg/shimproto"
)

// sockDir returns a short temporary directory. t.TempDir paths can exceed
// the unix socket path limit.
func sockDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "shim")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

// startShim runs a shim for the given shell command and returns its socket
// path and a channel that receives Run's result.
func startShim(t *testing.T, opts Options, script string) (string, <-chan error) {
	t.Helper()
	sock := filepath.Join(sockDir(t), "pty-TEST.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		done <- Run(ctx, shimproto.SpawnConfig{
			ID:      "pty-TEST",
			Socket:  sock,
			Command: "/bin/sh",
			Args:    []string{"-c", script},
			Meta:    []byte("meta-v1"),
		}, opts)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(10 * time.Second):
			t.Error("shim did not exit")
		}
	})
	return sock, done
}

func dial(t *testing.T, sock string) *Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := DialWait(ctx, sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// readUntil reads output until it contains want, returning all of it.
func readUntil(t *testing.T, c *Conn, want string) string {
	t.Helper()
	var out bytes.Buffer
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(out.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %q; got %q", want, out.String())
		}
		b, exited, err := c.Read()
		if err != nil {
			t.Fatalf("Read: %v (output so far %q)", err, out.String())
		}
		if exited != nil {
			t.Fatalf("child exited (%s) before %q; output %q", exited.Status, want, out.String())
		}
		out.Write(b)
	}
	return out.String()
}

// readToExit reads output until the child's exit.
func readToExit(t *testing.T, c *Conn) (string, *shimproto.Exited) {
	t.Helper()
	var out bytes.Buffer
	for {
		b, exited, err := c.Read()
		if err != nil {
			t.Fatalf("Read: %v (output so far %q)", err, out.String())
		}
		if exited != nil {
			return out.String(), exited
		}
		out.Write(b)
	}
}

func TestOutputExitAndRelease(t *testing.T) {
	sock, done := startShim(t, Options{}, "printf hello; exit 3")
	c := dial(t, sock)
	if c.Info.ID != "pty-TEST" || c.Info.Version != shimproto.Version || string(c.Info.Meta) != "meta-v1" {
		t.Errorf("Info = %+v", c.Info)
	}
	ckpt, err := c.Attach()
	if err != nil {
		t.Fatal(err)
	}
	if ckpt.Offset != 0 || len(ckpt.Blob) != 0 {
		t.Errorf("initial checkpoint = %+v, want empty", ckpt)
	}
	out, exited := readToExit(t, c)
	if out != "hello" {
		t.Errorf("output = %q, want %q", out, "hello")
	}
	if exited.Status != "exit status 3" {
		t.Errorf("exit status = %q", exited.Status)
	}
	if err := c.Release(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shim did not exit after Release")
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Errorf("socket not removed: %v", err)
	}
}

func TestReattachReplaysFromCheckpoint(t *testing.T) {
	sock, _ := startShim(t, Options{}, "stty -echo; echo ready; cat")
	c := dial(t, sock)
	if _, err := c.Attach(); err != nil {
		t.Fatal(err)
	}
	first := readUntil(t, c, "ready")
	c.Input([]byte("one\n"))
	first += readUntil(t, c, "one")
	if err := c.SetCheckpoint(uint64(len(first)), []byte("BLOB")); err != nil {
		t.Fatal(err)
	}
	c.Input([]byte("two\n"))
	readUntil(t, c, "two")
	c.Close()

	// A new controller, as after a tstermd restart, gets the checkpoint and
	// only the output after it.
	c2 := dial(t, sock)
	ckpt, err := c2.Attach()
	if err != nil {
		t.Fatal(err)
	}
	if ckpt.Offset != uint64(len(first)) || string(ckpt.Blob) != "BLOB" {
		t.Errorf("checkpoint = {%d %q}, want {%d BLOB}", ckpt.Offset, ckpt.Blob, len(first))
	}
	rest := readUntil(t, c2, "two")
	if strings.Contains(rest, "one") || strings.Contains(rest, "ready") {
		t.Errorf("replay includes output from before the checkpoint: %q", rest)
	}
}

func TestBufferLimitPausesChild(t *testing.T) {
	const total = 200 << 10
	sock, _ := startShim(t, Options{BufferLimit: 1 << 10},
		"head -c 204800 /dev/zero | tr '\\0' x; echo; echo DONE")
	// With no controller checkpointing, the shim must stop reading.
	time.Sleep(500 * time.Millisecond)
	c := dial(t, sock)
	if _, err := c.Attach(); err != nil {
		t.Fatal(err)
	}
	var got int
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		_ = c.c.SetReadDeadline(deadline)
		b, _, err := c.Read()
		if err != nil {
			break
		}
		got += len(b)
	}
	_ = c.c.SetReadDeadline(time.Time{})
	if got >= total {
		t.Fatalf("read %d bytes without checkpointing; the child was not paused", got)
	}

	// Checkpointing everything received lets the child continue.
	c.SetCheckpoint(uint64(got), nil)
	offset := uint64(got)
	var tail string
	for !strings.Contains(tail, "DONE") {
		b, exited, err := c.Read()
		if err != nil || exited != nil {
			t.Fatalf("Read: %v %v after %d bytes", err, exited, offset)
		}
		offset += uint64(len(b))
		tail = string(b)
		c.SetCheckpoint(offset, nil)
	}
	if offset < total {
		t.Errorf("only %d bytes of output", offset)
	}
}

func TestNewControllerReplacesOld(t *testing.T) {
	sock, _ := startShim(t, Options{}, "echo hi; sleep 30")
	c1 := dial(t, sock)
	if _, err := c1.Attach(); err != nil {
		t.Fatal(err)
	}
	readUntil(t, c1, "hi")
	c2 := dial(t, sock)
	if _, err := c2.Attach(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c1.Read(); err == nil {
		t.Error("old controller still connected")
	}
	readUntil(t, c2, "hi")
}

func TestOnlyControllerMayDrive(t *testing.T) {
	sock, _ := startShim(t, Options{}, "stty -echo; echo ready; cat")
	ctrl := dial(t, sock)
	if _, err := ctrl.Attach(); err != nil {
		t.Fatal(err)
	}
	readUntil(t, ctrl, "ready")
	other := dial(t, sock)
	other.Input([]byte("ignored\n"))
	ctrl.Input([]byte("accepted\n"))
	out := readUntil(t, ctrl, "accepted")
	if strings.Contains(out, "ignored") {
		t.Errorf("input from a non-controller was applied: %q", out)
	}
}

func TestMetaAndSizeInInfo(t *testing.T) {
	sock, _ := startShim(t, Options{}, "sleep 30")
	c := dial(t, sock)
	if _, err := c.Attach(); err != nil {
		t.Fatal(err)
	}
	c.SetMeta([]byte("meta-v2"))
	c.Resize(30, 100)
	time.Sleep(100 * time.Millisecond)
	info := dial(t, sock).Info
	if string(info.Meta) != "meta-v2" || info.Rows != 30 || info.Cols != 100 {
		t.Errorf("Info = meta %q, %dx%d; want meta-v2, 30x100", info.Meta, info.Rows, info.Cols)
	}
}

func TestExitReportedDespiteGrandchild(t *testing.T) {
	// The backgrounded sleep keeps the PTY open after the shell exits.
	sock, _ := startShim(t, Options{}, "sleep 30 & echo bye")
	c := dial(t, sock)
	if _, err := c.Attach(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	out, exited := readToExit(t, c)
	if !strings.Contains(out, "bye") || exited.Status != "exit status 0" {
		t.Errorf("output %q, exit %q", out, exited.Status)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("exit took %v", d)
	}
}

func TestExitedInfoForLateController(t *testing.T) {
	sock, _ := startShim(t, Options{}, "echo gone")
	time.Sleep(time.Second)
	c := dial(t, sock)
	if !c.Info.Exited || c.Info.ExitStatus != "exit status 0" {
		t.Fatalf("Info = %+v, want exited", c.Info)
	}
	if _, err := c.Attach(); err != nil {
		t.Fatal(err)
	}
	out, _ := readToExit(t, c)
	if !strings.Contains(out, "gone") {
		t.Errorf("final output %q lost", out)
	}
}

func TestCancelKillsChild(t *testing.T) {
	sock := filepath.Join(sockDir(t), "pty-K.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, shimproto.SpawnConfig{ID: "pty-K", Socket: sock, Command: "/bin/sleep", Args: []string{"30"}}, Options{})
	}()
	c := dial(t, sock)
	pid := c.Info.Pid
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
	if err := processAlive(pid); err == nil {
		t.Errorf("child %d still running", pid)
	}
}

// processAlive returns nil if pid exists.
func processAlive(pid int) error { return syscall.Kill(pid, 0) }

func TestActivityTimesInInfo(t *testing.T) {
	sock, _ := startShim(t, Options{}, "stty -echo; echo ready; cat >/dev/null")
	c := dial(t, sock)
	if !c.Info.LastInput.IsZero() {
		t.Errorf("LastInput = %v before any input", c.Info.LastInput)
	}
	if _, err := c.Attach(); err != nil {
		t.Fatal(err)
	}
	readUntil(t, c, "ready")
	before := time.Now()
	c.Input([]byte("x\n"))
	time.Sleep(100 * time.Millisecond)

	info := dial(t, sock).Info
	if info.LastInput.Before(before) {
		t.Errorf("LastInput = %v, want after %v", info.LastInput, before)
	}
	if info.LastOutput.IsZero() || info.LastOutput.After(before) {
		// "ready" was the only output, and it came before the input.
		t.Errorf("LastOutput = %v, want before %v", info.LastOutput, before)
	}
	if info.OutputEnd != uint64(len("ready\r\n")) {
		t.Errorf("OutputEnd = %d, want %d", info.OutputEnd, len("ready\r\n"))
	}
}
