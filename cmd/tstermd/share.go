package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"tailscale.com/tsnet"

	"github.com/bradfitz/tsterm/pkg/auth"
	"github.com/bradfitz/tsterm/pkg/session"
	"github.com/bradfitz/tsterm/pkg/transport/tcp"
	"github.com/bradfitz/tsterm/pkg/transport/ws"
)

// startSharing starts tstermd's sharing node, if -share-hostname asks for
// one: a tsnet node of its own, in this process, usually tagged (with
// -share-tags) so that people whom the owner's own machines are closed to
// may reach it. It serves the web UI on ports 443 and 80 (which redirects
// to 443), to the owner, as the owner, and to other people, read-only, for
// the sessions the owner shares (see auth.Sharing). Tagged devices get
// nothing.
//
// The node comes up in the background, as it may first need someone to
// log it in (at a URL it logs) unless $TS_AUTHKEY is set. Until it's up,
// shareURL returns "", and the owner's pages don't offer sharing.
func startSharing(ctx context.Context, f *runFlags, mgr *session.Manager, owner *auth.SelfUser) (shareURL func() string, closeFn func(), err error) {
	if *f.shareHostname == "" {
		return func() string { return "" }, func() {}, nil
	}
	dir := *f.shareStateDir
	if dir == "" {
		dir = defaultShareStateDir(*f.sessionsDir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("sharing node: %w", err)
	}
	var tags []string
	for t := range strings.SplitSeq(*f.shareTags, ",") {
		if t = strings.TrimSpace(t); t != "" {
			tags = append(tags, t)
		}
	}
	logf := func(format string, args ...any) { log.Printf("tstermd: sharing node: "+format, args...) }
	srv := &tsnet.Server{
		Dir:           dir,
		Hostname:      *f.shareHostname,
		AdvertiseTags: tags,
		UserLogf:      logf,
	}
	if err := srv.Start(); err != nil {
		return nil, nil, fmt.Errorf("sharing node: %w", err)
	}
	var url atomic.Pointer[string]
	go func() {
		if err := serveSharing(ctx, srv, mgr, owner, func(u string) { url.Store(&u) }, logf); err != nil && ctx.Err() == nil {
			logf("%v; not sharing", err)
		}
	}()
	shareURL = func() string {
		if u := url.Load(); u != nil {
			return *u
		}
		return ""
	}
	return shareURL, func() { srv.Close() }, nil
}

// serveSharing waits for the sharing node srv to come up, and serves the
// web UI on it, calling up with its URL once it does.
func serveSharing(ctx context.Context, srv *tsnet.Server, mgr *session.Manager, owner *auth.SelfUser, up func(url string), logf func(string, ...any)) error {
	st, err := srv.Up(ctx)
	if err != nil {
		return err
	}
	lc, err := srv.LocalClient()
	if err != nil {
		return err
	}
	fqdn := strings.TrimSuffix(st.Self.DNSName, ".")
	if fqdn == "" || !slices.Contains(srv.CertDomains(), fqdn) {
		return fmt.Errorf("%s can't get an HTTPS certificate (are HTTPS certificates enabled for the tailnet?)", fqdn)
	}
	if st.Self.Tags == nil || st.Self.Tags.Len() == 0 {
		logf("warning: %s isn't tagged, so it's owned by the person who logged it in, and others may not be able to reach it", fqdn)
	}
	sharing := &auth.Sharing{Owner: owner, LC: lc}
	go sharing.Watch(ctx, lc, log.Printf)
	u := "https://" + fqdn + "/"
	tcpSrv := &tcp.Server{
		Manager:   mgr,
		Auth:      sharing,
		WSHandler: (&ws.Server{Manager: mgr, Auth: sharing, Logf: log.Printf}).Handler(),
		TLS:       tlsConfig(lc, fqdn),
		HTTPSHost: fqdn,
		ShareURL:  func() string { return u },
	}
	ln443, err := srv.Listen("tcp", ":443")
	if err != nil {
		return err
	}
	ln80, err := srv.Listen("tcp", ":80")
	if err != nil {
		ln443.Close()
		return err
	}
	// Port 80 is only for sending browsers to 443, before any auth: it
	// tells nobody anything.
	redirect := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://"+fqdn+r.URL.RequestURI(), http.StatusFound)
	})}
	go redirect.Serve(ln80)
	go func() {
		<-ctx.Done()
		redirect.Close()
	}()
	// Get the certificate now: the first one can take a while.
	go func() {
		if _, _, err := lc.CertPair(ctx, fqdn); err != nil {
			logf("getting a certificate for %s: %v", fqdn, err)
		}
	}()
	up(u)
	logf("serving %s (and http://%s/), owner %s", u, strings.Split(fqdn, ".")[0], owner.SelfLogin())
	for {
		err := tcp.Serve(ctx, ln443, tcpSrv)
		if ctx.Err() != nil {
			return nil
		}
		logf("serving: %v", err)
		time.Sleep(time.Second)
		if ln443, err = srv.Listen("tcp", ":443"); err != nil {
			return err
		}
	}
}

// defaultShareStateDir returns where the sharing node keeps its state by
// default: beside the sessions directory.
func defaultShareStateDir(sessionsDir string) string {
	return sessionsDir + "-share-node"
}
