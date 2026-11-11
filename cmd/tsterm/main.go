// tsterm is the tsterm CLI client.
//
// Usage:
//
//	tsterm HOST [SESSION]   attach (auto-pick: 0 → create, 1 → attach, ≥2 → list)
//	tsterm -list HOST       list sessions on HOST
//	tsterm -new  HOST [NAME] create a new session and attach
//	tsterm -kill HOST IDORNAME  kill a session
//
// While attached, Ctrl-^ (which many terminals also send for Ctrl-~)
// followed by "." disconnects, leaving the session running. Ctrl-^ twice
// sends a literal Ctrl-^.
//
// HOST may include :port; default control port is 2200. The client uses the
// host's normal DNS / MagicDNS to reach HOST; no tsnet is embedded.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bradfitz/tsterm/pkg/proto"
)

var (
	flagList = flag.Bool("list", false, "list sessions on HOST and exit")
	flagNew  = flag.Bool("new", false, "create a new session (and attach unless -no-attach)")
	flagKill = flag.Bool("kill", false, "kill the named session and exit")
	flagNoAttach = flag.Bool("no-attach", false, "do not attach after -new")
)

func main() {
	flag.Usage = usage
	flag.Parse()
	if flag.NArg() < 1 {
		usage()
		os.Exit(2)
	}
	host := flag.Arg(0)
	var name string
	if flag.NArg() >= 2 {
		name = flag.Arg(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	ctrl := newControl(host)

	switch {
	case *flagList:
		if err := runList(ctx, ctrl); err != nil {
			fail(err)
		}
	case *flagKill:
		if name == "" {
			fail(errors.New("-kill requires a session name or ID"))
		}
		if err := ctrl.kill(ctx, name); err != nil {
			fail(err)
		}
	case *flagNew:
		if err := runNew(ctx, ctrl, name); err != nil {
			fail(err)
		}
	default:
		if err := runAttach(ctx, ctrl, name); err != nil {
			fail(err)
		}
	}
}

func runList(ctx context.Context, ctrl *control) error {
	sessions, err := ctrl.list(ctx)
	if err != nil {
		return err
	}
	fmt.Print(formatList(sessions))
	return nil
}

func runNew(ctx context.Context, ctrl *control, name string) error {
	rows, cols, _ := windowSize()
	info, err := ctrl.create(ctx, proto.CreateSessionReq{
		Name: name,
		Rows: rows,
		Cols: cols,
		Env:  clientEnv(),
	})
	if err != nil {
		return err
	}
	if *flagNoAttach {
		fmt.Printf("created %s (%s) on udp port %d\n", info.Name, info.ID, info.UDPPort)
		return nil
	}
	return attach(ctx, ctrl, info)
}

func runAttach(ctx context.Context, ctrl *control, name string) error {
	listCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	sessions, err := ctrl.list(listCtx)
	cancel()
	if err != nil {
		return err
	}
	pick, err := pickSession(sessions, name)
	if errors.Is(err, ErrAmbiguous) {
		fmt.Fprintln(os.Stderr, "multiple sessions present; pick one by name:")
		fmt.Fprint(os.Stderr, formatList(sessions))
		os.Exit(2)
	}
	if err != nil {
		return err
	}
	var info proto.SessionInfo
	switch {
	case pick.AttachID != "":
		info, err = ctrl.info(ctx, pick.AttachID)
		if err != nil {
			return err
		}
	default:
		rows, cols, _ := windowSize()
		info, err = ctrl.create(ctx, proto.CreateSessionReq{
			Name: pick.CreateName,
			Rows: rows,
			Cols: cols,
			Env:  clientEnv(),
		})
		if err != nil {
			return err
		}
	}
	return attach(ctx, ctrl, info)
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  tsterm HOST [SESSION]   attach (auto-pick: 0 → create, 1 → attach, ≥2 → list)
  tsterm -list HOST       list sessions on HOST
  tsterm -new  HOST [NAME] create a new session and attach
  tsterm -kill HOST IDORNAME  kill a session

While attached, type Ctrl-^ (or Ctrl-~) then "." to disconnect, leaving
the session running.

flags:`)
	flag.PrintDefaults()
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "tsterm: %v\n", err)
	if errors.Is(err, errDisconnected) {
		os.Exit(0) // the user asked to leave
	}
	os.Exit(1)
}
