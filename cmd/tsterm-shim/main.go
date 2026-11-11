// tsterm-shim owns one terminal session's PTY and child process on behalf
// of tstermd, so that tstermd can be restarted or upgraded without killing
// the session. tstermd starts it; it is not meant to be run by hand.
//
// Usage:
//
//	tsterm-shim -spawn FILE
//
// FILE is a JSON shimproto.SpawnConfig, which tsterm-shim reads and
// removes.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/bradfitz/tsterm/pkg/shim"
	"github.com/bradfitz/tsterm/pkg/shimproto"
)

func main() {
	spawn := flag.String("spawn", "", "path of the JSON spawn config to run")
	flag.Parse()
	if *spawn == "" || flag.NArg() != 0 {
		flag.Usage()
		os.Exit(2)
	}
	b, err := os.ReadFile(*spawn)
	if err != nil {
		log.Fatalf("tsterm-shim: %v", err)
	}
	os.Remove(*spawn)
	var cfg shimproto.SpawnConfig
	if err := shimproto.Decode(b, &cfg); err != nil {
		log.Fatalf("tsterm-shim: %s: %v", *spawn, err)
	}

	// The shim has no terminal of its own, so SIGHUP is meaningless to
	// it. SIGTERM, as from stopping its systemd unit, ends the session.
	signal.Ignore(syscall.SIGHUP)
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	if err := shim.Run(ctx, cfg, shim.Options{Logf: log.Printf}); err != nil {
		log.Fatalf("tsterm-shim %s: %v", cfg.ID, err)
	}
}
