// tstermd is the tsterm daemon: it hosts PTY sessions and exposes them on
// the local tailnet via three transports — an HTTP/CBOR control plane, a
// UDP hot path for input and screen deltas, and a WebSocket viewer.
// Tailscale identity is required for every request and matched against the
// local node's tailnet user.
//
// Usage:
//
//	tstermd install [flags]   run as a systemd user service; again to upgrade
//	tstermd uninstall         stop and remove the service
//	tstermd run [flags]       run in the foreground
//
// Each session's PTY and child process live in their own shim process,
// tstermd's binary run again (see shim.MainIfRequested), so restarting or
// upgrading tstermd does not end any session: on startup, tstermd finds the
// running shims in its -run-dir and restores their sessions. Under systemd,
// tstermd starts each shim as its own transient unit, because stopping
// tstermd's unit kills every process in its control group. As a user
// service, that needs the user's systemd instance, so tstermd install
// enables lingering (loginctl enable-linger), for sessions to survive
// logout.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/tsnet"

	"github.com/bradfitz/tsterm/internal/demoapp"
	"github.com/bradfitz/tsterm/pkg/auth"
	"github.com/bradfitz/tsterm/pkg/pty"
	"github.com/bradfitz/tsterm/pkg/session"
	"github.com/bradfitz/tsterm/pkg/shim"
	"github.com/bradfitz/tsterm/pkg/transport/tcp"
	"github.com/bradfitz/tsterm/pkg/transport/udp"
	"github.com/bradfitz/tsterm/pkg/transport/ws"
)

func main() {
	// tstermd also serves as the demo program, which sessions named
	// session.DemoName run, and as their shims.
	demoapp.MainIfRequested()
	shim.MainIfRequested()

	args := os.Args[1:]
	cmd := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "run":
		run(args)
	case "install":
		install(args)
	case "uninstall":
		uninstall(args)
	case "", "help":
		usage(os.Stdout)
		if len(args) > 0 && cmd == "" {
			fmt.Printf("\n(Flags go after a command, as in: tstermd run %s)\n", strings.Join(args, " "))
			os.Exit(2)
		}
	default:
		fmt.Fprintf(os.Stderr, "tstermd: unknown command %q\n\n", cmd)
		usage(os.Stderr)
		os.Exit(2)
	}
}

// runFlags are the flags of tstermd run, which tstermd install passes on.
type runFlags struct {
	fs          *flag.FlagSet
	useTSNet    *bool
	stateDir    *string
	hostname    *string
	port        *int
	udpPort     *int
	bindAll     *bool
	runDir      *string
	sessionsDir *string
	shimLaunch  *string

	shareHostname *string
	shareTags     *string
	shareStateDir *string
}

func newRunFlags(name string) *runFlags {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	return &runFlags{
		fs:          fs,
		useTSNet:    fs.Bool("tsnet", false, "embed a tsnet.Server instead of using the host tailscaled"),
		stateDir:    fs.String("state-dir", defaultStateDir(), "tsnet state directory (only used with -tsnet)"),
		hostname:    fs.String("hostname", "", "tsnet hostname (only used with -tsnet; defaults to os.Hostname)"),
		port:        fs.Int("port", 2200, "control-plane HTTP port"),
		udpPort:     fs.Int("udp-port", 2201, "hot-path UDP port"),
		bindAll:     fs.Bool("bind-all", false, "bind on all interfaces (default: tailnet addresses only)"),
		runDir:      fs.String("run-dir", defaultRunDir(), "directory for the shims' unix sockets"),
		sessionsDir: fs.String("sessions-dir", defaultSessionsDir(), "directory for sessions' lasting state, such as their history"),
		shimLaunch:  fs.String("shim-launch", "auto", `how to start shims: "systemd" (each in its own transient unit), "direct", "inprocess" (sessions die with tstermd), or "auto" (systemd when running under systemd, else direct)`),

		shareHostname: fs.String("share-hostname", "", "if set, the hostname of a second, embedded (tsnet) Tailscale node, on which to share sessions, read-only, with other people (see -share-tags)"),
		shareTags:     fs.String("share-tags", "", `comma-separated tags for the sharing node to advertise, as "tag:foo", so that others can reach it`),
		shareStateDir: fs.String("share-state-dir", "", "the sharing node's state directory (default: the -sessions-dir, plus \"-share-node\")"),
	}
}

// run runs tstermd in the foreground.
func run(args []string) {
	f := newRunFlags("tstermd run")
	f.fs.Parse(args)
	if f.fs.NArg() > 0 {
		log.Fatalf("tstermd run: unexpected arguments %q", f.fs.Args())
	}
	useTSNet, stateDir, hostname := f.useTSNet, f.stateDir, f.hostname
	port, udpPort, bindAll := f.port, f.udpPort, f.bindAll
	runDir, sessionsDir, shimLaunch := f.runDir, f.sessionsDir, f.shimLaunch
	log.Printf("tstermd: version %s", version())

	launcher, err := newLauncher(*shimLaunch)
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
	mgr.SetStateDir(*sessionsDir)
	mgr.SetDemo(func() pty.Config {
		cmd, env := demoapp.Command()
		return pty.Config{Command: cmd, Env: env}
	})
	// On shutdown, leave the sessions running for the next tstermd.
	defer mgr.Detach()
	// Restore before serving, so that reattaching clients find their
	// sessions rather than being told they are gone.
	n, err := mgr.Restore(ctx)
	if err != nil {
		log.Fatalf("tstermd: restoring sessions: %v", err)
	}
	log.Printf("tstermd: restored %d sessions from %s", n, *runDir)
	// Name unnamed sessions for what they run, as their git worktrees.
	go mgr.AutoName(ctx)

	shareURL, closeShare, err := startSharing(ctx, f, mgr, a)
	if err != nil {
		log.Fatalf("tstermd: %v", err)
	}
	defer closeShare()

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
		ShareURL:  shareURL,
	}
	if fqdn, ok := httpsName(ctx, lc); ok {
		tcpSrv.TLS = tlsConfig(lc, fqdn)
		tcpSrv.HTTPSHost = fqdn
		log.Printf("tstermd: web UI at https://%s:%d/", fqdn, *port)
		// Get the certificate now: the first one can take a while.
		go func() {
			if _, _, err := lc.CertPair(ctx, fqdn); err != nil {
				log.Printf("tstermd: getting a certificate for %s: %v", fqdn, err)
			}
		}()
	} else {
		log.Printf("tstermd: HTTPS certificates aren't enabled for this tailnet; serving the web UI over HTTP only")
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

// httpsName returns the node's MagicDNS name, if the tailnet lets it have
// a certificate (HTTPS certificates are enabled in the tailnet's DNS
// settings).
func httpsName(ctx context.Context, lc *local.Client) (string, bool) {
	st, err := lc.Status(ctx)
	if err != nil || st.Self == nil {
		return "", false
	}
	fqdn := strings.TrimSuffix(st.Self.DNSName, ".")
	return fqdn, fqdn != "" && slices.Contains(st.CertDomains, fqdn)
}

// tlsConfig returns a TLS config serving fqdn's certificate from
// tailscaled.
func tlsConfig(lc *local.Client, fqdn string) *tls.Config {
	return &tls.Config{
		// HTTP/1.1 only: browsers' WebSockets need it, and the web UI
		// gains nothing from HTTP/2.
		NextProtos: []string{"http/1.1"},
		GetCertificate: func(hi *tls.ClientHelloInfo) (*tls.Certificate, error) {
			// fqdn's is the only certificate there is; clients that
			// connect by IP address or by short name get it too.
			h := *hi
			h.ServerName = fqdn
			return lc.GetCertificate(&h)
		},
	}
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

// defaultSessionsDir returns where sessions' lasting state goes by
// default: under $XDG_STATE_HOME (~/.local/state), or /var/lib for root.
// Unlike $XDG_RUNTIME_DIR, it survives a reboot.
func defaultSessionsDir() string {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "tsterm", "sessions")
	}
	if os.Getuid() == 0 {
		return "/var/lib/tsterm/sessions"
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".local", "state", "tsterm", "sessions")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("tsterm-%d-sessions", os.Getuid()))
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

func newLauncher(mode string) (shim.Launcher, error) {
	if mode == "auto" {
		// systemd sets INVOCATION_ID for the processes of every unit.
		mode = "direct"
		if os.Getenv("INVOCATION_ID") != "" {
			mode = "systemd"
		}
	}
	switch mode {
	case "systemd", "direct":
		// Shims are this binary run again.
		p, err := shim.SelfPath()
		if err != nil {
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
