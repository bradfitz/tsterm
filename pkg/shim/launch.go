package shim

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/bradfitz/tsterm/pkg/shimproto"
)

// A Launcher starts shims. Launch returns once the shim has been started;
// the caller then connects to cfg.Socket with DialWait.
type Launcher interface {
	Launch(ctx context.Context, cfg shimproto.SpawnConfig) error
}

// Exec launches shims by running a program that calls MainIfRequested,
// tstermd, so that they are separate processes that outlive tstermd.
type Exec struct {
	// Path is the program, as from SelfPath.
	Path string

	// Systemd, if true, starts each shim as its own transient systemd
	// unit, using systemd-run. That is required when tstermd itself runs
	// under systemd: stopping or restarting tstermd's unit kills every
	// process in its control group, which would include directly launched
	// shims and therefore every session. Without systemd, shims are
	// started in their own process session and reparented to init when
	// tstermd exits.
	Systemd bool
}

// UnitName returns the systemd unit name used for the shim with the given
// ID.
func UnitName(id string) string { return "tsterm-" + id }

// Launch implements Launcher.
func (e *Exec) Launch(ctx context.Context, cfg shimproto.SpawnConfig) error {
	spawn := filepath.Join(filepath.Dir(cfg.Socket), cfg.ID+".spawn.json")
	b, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	if err := os.WriteFile(spawn, b, 0o600); err != nil {
		return err
	}
	var cmd *exec.Cmd
	if e.Systemd {
		args := []string{
			"--unit=" + UnitName(cfg.ID),
			"--description=tsterm session " + cfg.ID,
			"--collect", // unload the unit even if the shim fails
			"--quiet",
		}
		if os.Getuid() != 0 {
			args = append(args, "--user")
		}
		args = append(args, "--setenv="+SpawnEnvVar+"="+spawn, "--", e.Path, "shim", cfg.ID)
		cmd = exec.CommandContext(ctx, "systemd-run", args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			os.Remove(spawn)
			return fmt.Errorf("systemd-run: %v: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	cmd = exec.Command(e.Path, "shim", cfg.ID) // the arguments are for ps
	cmd.Env = append(os.Environ(), SpawnEnvVar+"="+spawn)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		os.Remove(spawn)
		return err
	}
	go cmd.Wait() // reap it if it exits while we are still running
	return nil
}

// InProcess runs shims as goroutines in the calling process. Sessions then
// do not survive the process, so it is for tests.
type InProcess struct {
	// Options configures each shim.
	Options Options

	mu      sync.Mutex
	cancels []context.CancelFunc
	wg      sync.WaitGroup
}

// Launch implements Launcher.
func (p *InProcess) Launch(_ context.Context, cfg shimproto.SpawnConfig) error {
	ctx, cancel := context.WithCancel(context.Background())
	p.mu.Lock()
	p.cancels = append(p.cancels, cancel)
	p.mu.Unlock()
	p.wg.Go(func() {
		if err := Run(ctx, cfg, p.Options); err != nil && p.Options.Logf != nil {
			p.Options.Logf("shim %s: %v", cfg.ID, err)
		}
	})
	return nil
}

// Close kills every shim's child and waits for the shims to exit.
func (p *InProcess) Close() {
	p.mu.Lock()
	cancels := p.cancels
	p.cancels = nil
	p.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	p.wg.Wait()
}
