package shim

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/bradfitz/tsterm/pkg/shimproto"
)

// SpawnEnvVar, when set, makes MainIfRequested run a shim. Its value is the
// path of a JSON shimproto.SpawnConfig, which the shim reads and removes.
const SpawnEnvVar = "TSTERM_SHIM_SPAWN"

// MainIfRequested runs a shim, and exits, if the environment asks for it
// (see SpawnEnvVar). tstermd calls it first thing in main, which makes
// tstermd's binary also the shim's: Exec runs it so. A shim is the same
// build as the tstermd that started it, and needs no other binary.
func MainIfRequested() {
	spawn := os.Getenv(SpawnEnvVar)
	if spawn == "" {
		return
	}
	os.Unsetenv(SpawnEnvVar) // the session's program needn't see it
	setProcName("tstermd-shim")
	b, err := os.ReadFile(spawn)
	if err != nil {
		log.Fatalf("tstermd shim: %v", err)
	}
	os.Remove(spawn)
	var cfg shimproto.SpawnConfig
	if err := shimproto.Decode(b, &cfg); err != nil {
		log.Fatalf("tstermd shim: %s: %v", spawn, err)
	}

	// The shim has no terminal of its own, so SIGHUP is meaningless to
	// it. SIGTERM, as from stopping its systemd unit, ends the session.
	signal.Ignore(syscall.SIGHUP)
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	if err := Run(ctx, cfg, Options{Logf: log.Printf}); err != nil {
		log.Fatalf("tstermd shim %s: %v", cfg.ID, err)
	}
	os.Exit(0)
}

// SelfPath returns the path to run, to run this program as a shim: on
// Linux, its own executable through /proc, which stays this build even if
// its file is replaced, as by an upgrade, while it runs.
func SelfPath() (string, error) {
	p := fmt.Sprintf("/proc/%d/exe", os.Getpid())
	if _, err := os.Stat(p); err == nil {
		return p, nil
	}
	return os.Executable()
}
