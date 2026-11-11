// tstermd is the tsterm daemon: it hosts PTY sessions and exposes them on
// the local tailnet via three transports — an HTTP/CBOR control plane, a
// UDP hot path for input and screen deltas, and a read-only WebSocket
// viewer. Tailscale identity is required for every request and matched
// against the local node's tailnet user.
//
// Each session's PTY and child process live in their own tsterm-shim
// process, so restarting or upgrading tstermd does not end any session: on
// startup, tstermd finds the running shims in its -run-dir and restores
// their sessions. Under systemd, tstermd starts each shim as its own
// transient unit, because stopping tstermd's unit kills every process in
// its control group. As a user service, that needs the user's systemd
// instance, so enable lingering (loginctl enable-linger) for sessions to
// survive logout.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/tsnet"

	"github.com/bradfitz/tsterm/pkg/auth"
	"github.com/bradfitz/tsterm/pkg/session"
	"github.com/bradfitz/tsterm/pkg/shim"
	"github.com/bradfitz/tsterm/pkg/transport/tcp"
	"github.com/bradfitz/tsterm/pkg/transport/udp"
	"github.com/bradfitz/tsterm/pkg/transport/ws"
)

func main() {
	useTSNet := flag.Bool("tsnet", false, "embed a tsnet.Server instead of using the host tailscaled")
	stateDir := flag.String("state-dir", defaultStateDir(), "tsnet state directory (only used with -tsnet)")
	hostname := flag.String("hostname", "", "tsnet hostname (only used with -tsnet; defaults to os.Hostname)")
	port := flag.Int("port", 2200, "control-plane HTTP port")
	udpPort := flag.Int("udp-port", 2201, "hot-path UDP port")
	bindAll := flag.Bool("bind-all", false, "bind on all interfaces (default: tailnet addresses only)")
	runDir := flag.String("run-dir", defaultRunDir(), "directory for the shims' unix sockets")
	shimPath := flag.String("shim", defaultShimPath(), "path to the tsterm-shim binary")
	shimLaunch := flag.String("shim-launch", "auto", `how to start shims: "systemd" (each in its own transient unit), "direct", "inprocess" (sessions die with tstermd), or "auto" (systemd when running under systemd, else direct)`)
	flag.Parse()

	launcher, err := newLauncher(*shimLaunch, *shimPath)
	if err != nil {
		log.Fatalf("tstermd: %v", err)
	}
	if err := os.MkdirAll(*runDir, 0o700); err != nil {
		log.Fatalf("tstermd: %v", err)
	}
	if err := os.Chmod(*runDir, 0o700); err != nil {
		log.Fatalf("tstermd: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	lc, tcpLns, udpPCs, closeNet, err := setupNet(ctx, *useTSNet, *stateDir, *hostname, *port, *udpPort, *bindAll)
	if err != nil {
		log.Fatalf("tstermd: setup: %v", err)
	}
	defer closeNet()

	a, err := auth.NewSelfUser(ctx, lc)
	if err != nil {
		log.Fatalf("tstermd: auth: %v", err)
	}
	go a.Watch(ctx, lc, log.Printf)

	mgr := session.NewManager(*runDir, launcher)
	mgr.SetLogf(log.Printf)
	// On shutdown, leave the sessions running for the next tstermd.
	defer mgr.Detach()
	// Restore before serving, so that reattaching clients find their
	// sessions rather than being told they are gone.
	n, err := mgr.Restore(ctx)
	if err != nil {
		log.Fatalf("tstermd: restoring sessions: %v", err)
	}
	log.Printf("tstermd: restored %d sessions from %s", n, *runDir)

	wsSrv := &ws.Server{
		Manager: mgr,
		Auth:    a,
		Logf:    log.Printf,
	}
	tcpSrv := &tcp.Server{
		Manager:   mgr,
		Auth:      a,
		UDPPort:   *udpPort,
		WSHandler: wsSrv.Handler(),
	}
	udpSrv := &udp.Server{
		Manager: mgr,
		Auth:    a,
		Logf:    log.Printf,
	}

	errc := make(chan error, len(tcpLns)+len(udpPCs))
	for _, ln := range tcpLns {
		log.Printf("tstermd: HTTP on %s", ln.Addr())
		go func() { errc <- tcp.Serve(ctx, ln, tcpSrv) }()
	}
	for _, pc := range udpPCs {
		log.Printf("tstermd: UDP on %s", pc.LocalAddr())
		go func() { errc <- udp.Serve(ctx, pc, udpSrv) }()
	}
	log.Printf("tstermd: serving user %d", a.SelfUserID())

	select {
	case err := <-errc:
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("tstermd: %v", err)
		}
	case <-ctx.Done():
	}

	// Give in-flight handlers a moment to drain.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer shutdownCancel()
	for _, ln := range tcpLns {
		_ = ln.Close()
	}
	for _, pc := range udpPCs {
		_ = pc.Close()
	}
	<-shutdownCtx.Done()
}

// setupNet starts listening on the tailnet: one TCP listener and one UDP
// socket per Tailscale address (IPv4 and IPv6), or one of each on all
// interfaces with bindAll.
func setupNet(
	ctx context.Context,
	useTSNet bool,
	stateDir, hostname string,
	tcpPort, udpPort int,
	bindAll bool,
) (lc *local.Client, lns []net.Listener, pcs []net.PacketConn, closeNet func(), err error) {
	closeNet = func() {}
	defer func() {
		if err != nil {
			for _, ln := range lns {
				ln.Close()
			}
			for _, pc := range pcs {
				pc.Close()
			}
			closeNet()
		}
	}()
	if useTSNet {
		if err := os.MkdirAll(stateDir, 0o700); err != nil {
			return nil, nil, nil, nil, fmt.Errorf("mkdir state dir: %w", err)
		}
		host := hostname
		if host == "" {
			host, _ = os.Hostname()
		}
		srv := &tsnet.Server{
			Dir:      stateDir,
			Hostname: host,
		}
		if _, err := srv.Up(ctx); err != nil {
			srv.Close()
			return nil, nil, nil, nil, fmt.Errorf("tsnet up: %w", err)
		}
		closeNet = func() { _ = srv.Close() }
		if lc, err = srv.LocalClient(); err != nil {
			return nil, nil, nil, nil, fmt.Errorf("tsnet local client: %w", err)
		}
		// tsnet's TCP listener covers every Tailscale address; its UDP
		// sockets each take one address.
		ln, err := srv.Listen("tcp", ":"+strconv.Itoa(tcpPort))
		if err != nil {
			return nil, nil, nil, nil, err
		}
		lns = append(lns, ln)
		ip4, ip6 := srv.TailscaleIPs()
		for _, ip := range []netip.Addr{ip4, ip6} {
			if !ip.IsValid() {
				continue
			}
			pc, err := srv.ListenPacket("udp", netip.AddrPortFrom(ip, uint16(udpPort)).String())
			if err != nil {
				return nil, nil, nil, nil, err
			}
			pcs = append(pcs, pc)
		}
		return lc, lns, pcs, closeNet, nil
	}

	lc = &local.Client{}
	bindAddrs, err := pickBindAddrs(ctx, lc, bindAll)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	for _, ip := range bindAddrs {
		ln, err := net.Listen("tcp", net.JoinHostPort(ip, strconv.Itoa(tcpPort)))
		if err != nil {
			return nil, nil, nil, nil, err
		}
		lns = append(lns, ln)
		pc, err := net.ListenPacket("udp", net.JoinHostPort(ip, strconv.Itoa(udpPort)))
		if err != nil {
			return nil, nil, nil, nil, err
		}
		pcs = append(pcs, pc)
	}
	return lc, lns, pcs, closeNet, nil
}

// pickBindAddrs returns the addresses on which the daemon should listen.
// With -bind-all it returns "" (meaning all interfaces); otherwise it asks
// tailscaled for the local node's tailnet addresses.
func pickBindAddrs(ctx context.Context, lc *local.Client, bindAll bool) ([]string, error) {
	if bindAll {
		return []string{""}, nil
	}
	st, err := lc.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("tailscaled status: %w", err)
	}
	if st == nil || st.Self == nil || len(st.Self.TailscaleIPs) == 0 {
		return nil, errors.New("no Tailscale IPs on local node (use -bind-all if intended)")
	}
	out := make([]string, 0, len(st.Self.TailscaleIPs))
	for _, ip := range st.Self.TailscaleIPs {
		out = append(out, ip.String())
	}
	return out, nil
}

func defaultStateDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "tsterm-state"
	}
	return filepath.Join(home, ".tsterm", "state")
}

// defaultRunDir returns where shim sockets go by default: under
// $XDG_RUNTIME_DIR, which systemd provides for users, or /run for root.
func defaultRunDir() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "tsterm")
	}
	if os.Getuid() == 0 {
		return "/run/tsterm"
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".tsterm", "run")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("tsterm-%d", os.Getuid()))
}

// defaultShimPath returns the tsterm-shim next to this binary, if any,
// else the one in $PATH.
func defaultShimPath() string {
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), "tsterm-shim")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if p, err := exec.LookPath("tsterm-shim"); err == nil {
		return p
	}
	return "tsterm-shim"
}

func newLauncher(mode, shimPath string) (shim.Launcher, error) {
	if mode == "auto" {
		// systemd sets INVOCATION_ID for the processes of every unit.
		mode = "direct"
		if os.Getenv("INVOCATION_ID") != "" {
			mode = "systemd"
		}
	}
	switch mode {
	case "systemd", "direct":
		p, err := exec.LookPath(shimPath)
		if err != nil {
			return nil, fmt.Errorf("finding tsterm-shim (see -shim): %w", err)
		}
		if p, err = filepath.Abs(p); err != nil {
			return nil, err
		}
		log.Printf("tstermd: launching shims (%s) with %s", mode, p)
		return &shim.Exec{Path: p, Systemd: mode == "systemd"}, nil
	case "inprocess":
		log.Printf("tstermd: running shims in-process; sessions will not survive a restart")
		return &shim.InProcess{}, nil
	}
	return nil, fmt.Errorf("unknown -shim-launch mode %q", mode)
}
