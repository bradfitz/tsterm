package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"tailscale.com/tsnet"

	"github.com/bradfitz/tsterm/pkg/auth"
	"github.com/bradfitz/tsterm/pkg/proto"
	"github.com/bradfitz/tsterm/pkg/session"
	"github.com/bradfitz/tsterm/pkg/transport/tcp"
	"github.com/bradfitz/tsterm/pkg/transport/ws"
)

// shareNode runs tstermd's sharing node, when the owner wants one: a tsnet
// node of its own, in this process, usually tagged so that people whom
// the owner's own machines are closed to may reach it. It serves the web
// UI on ports 443 and 80 (which redirects to 443), to the owner, as the
// owner, and to other people, read-only, for the sessions the owner shares
// (see auth.Sharing). Tagged devices get nothing.
//
// The owner starts and destroys it from the web UI (it implements
// tcp.ShareNodeControl), or with -share-hostname. A started node's config
// is kept in its state directory, so it starts again with tstermd, until
// it's destroyed.
//
// A node comes up in the background, as it may first need someone to log
// it in at a URL (see Status), unless it was given an auth key. Until it's
// up, URL returns "", and the owner's pages don't offer sharing.
type shareNode struct {
	dir   string // its state: shareConfigFile, and tsnet's in shareTSNetDir
	mgr   *session.Manager
	owner *auth.SelfUser
	ctx   context.Context // tstermd's

	mu  sync.Mutex
	cur *shareRun // or nil, if off
}

const (
	shareConfigFile = "config.json"
	shareTSNetDir   = "tsnet"
)

// shareConfig is a sharing node's config, as kept in shareConfigFile.
type shareConfig struct {
	Hostname string   `json:"hostname"`
	Tags     []string `json:"tags,omitzero"`
}

// shareRun is a started sharing node.
type shareRun struct {
	cfg    shareConfig
	srv    *tsnet.Server
	cancel context.CancelFunc
	done   chan struct{} // closed when serveSharing returns

	mu  sync.Mutex
	url string // once it's serving
	err error  // if it failed
}

func newShareNode(ctx context.Context, dir string, mgr *session.Manager, owner *auth.SelfUser) *shareNode {
	return &shareNode{dir: dir, mgr: mgr, owner: owner, ctx: ctx}
}

func shareLogf(format string, args ...any) {
	log.Printf("tstermd: sharing node: "+format, args...)
}

// startSaved starts the node from its saved config, if it has one.
func (n *shareNode) startSaved() error {
	b, err := os.ReadFile(filepath.Join(n.dir, shareConfigFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var cfg shareConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		return fmt.Errorf("%s: %w", shareConfigFile, err)
	}
	return n.start(cfg, "")
}

// URL returns the node's web UI's URL, or "" if it isn't serving.
func (n *shareNode) URL() string {
	n.mu.Lock()
	r := n.cur
	n.mu.Unlock()
	if r == nil {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.url
}

// Status implements tcp.ShareNodeControl.
func (n *shareNode) Status(ctx context.Context) proto.ShareNode {
	n.mu.Lock()
	r := n.cur
	n.mu.Unlock()
	if r == nil {
		return proto.ShareNode{State: "off", Hostname: defaultShareHostname()}
	}
	st := proto.ShareNode{Hostname: r.cfg.Hostname, Tags: r.cfg.Tags}
	r.mu.Lock()
	st.URL = r.url
	err := r.err
	r.mu.Unlock()
	switch {
	case st.URL != "":
		st.State = "running"
	case err != nil:
		st.State, st.Error = "failed", err.Error()
	default:
		st.State = "starting"
		if lc, err := r.srv.LocalClient(); err == nil {
			ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			if s, err := lc.StatusWithoutPeers(ctx); err == nil && s.AuthURL != "" {
				st.State, st.LoginURL = "login", s.AuthURL
			}
		}
	}
	return st
}

// Start implements tcp.ShareNodeControl.
func (n *shareNode) Start(ctx context.Context, req proto.StartShareNodeReq) error {
	cfg := shareConfig{Hostname: strings.ToLower(strings.TrimSpace(req.Hostname))}
	if cfg.Hostname == "" {
		cfg.Hostname = defaultShareHostname()
	}
	if !validHostname(cfg.Hostname) {
		return fmt.Errorf("bad hostname %q: use letters, digits, and dashes", cfg.Hostname)
	}
	for _, t := range req.Tags {
		if t = strings.TrimSpace(t); t == "" {
			continue
		}
		if !strings.HasPrefix(t, "tag:") || len(t) == len("tag:") {
			return fmt.Errorf(`bad tag %q: tags look like "tag:foo"`, t)
		}
		cfg.Tags = append(cfg.Tags, t)
	}
	if err := n.start(cfg, strings.TrimSpace(req.AuthKey)); err != nil {
		return err
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(n.dir, shareConfigFile), b, 0o600)
}

// start starts the node with cfg, logging in with authKey, if any.
func (n *shareNode) start(cfg shareConfig, authKey string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.cur != nil {
		return fmt.Errorf("the sharing node %s is already started; destroy it first", n.cur.cfg.Hostname)
	}
	tsDir := filepath.Join(n.dir, shareTSNetDir)
	if err := os.MkdirAll(tsDir, 0o700); err != nil {
		return err
	}
	srv := &tsnet.Server{
		Dir:           tsDir,
		Hostname:      cfg.Hostname,
		AdvertiseTags: cfg.Tags,
		AuthKey:       authKey, // if empty, tsnet uses $TS_AUTHKEY
		UserLogf:      shareLogf,
	}
	if err := srv.Start(); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(n.ctx)
	r := &shareRun{cfg: cfg, srv: srv, cancel: cancel, done: make(chan struct{})}
	n.cur = r
	go func() {
		defer close(r.done)
		err := serveSharing(ctx, srv, n.mgr, n.owner, func(url string) {
			r.mu.Lock()
			r.url = url
			r.mu.Unlock()
		})
		if err != nil && ctx.Err() == nil {
			shareLogf("%v; not sharing", err)
			r.mu.Lock()
			r.err = err
			r.mu.Unlock()
		}
	}()
	shareLogf("starting %s", cfg.Hostname)
	return nil
}

// Destroy implements tcp.ShareNodeControl.
func (n *shareNode) Destroy(ctx context.Context) error {
	n.mu.Lock()
	r := n.cur
	n.cur = nil
	n.mu.Unlock()
	if r != nil {
		// Log out, so the node leaves the tailnet, as far as it may
		// without an error; then stop it.
		if lc, err := r.srv.LocalClient(); err == nil {
			lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			if err := lc.Logout(lctx); err != nil {
				shareLogf("logging out: %v", err)
			}
			cancel()
		}
		r.cancel()
		r.srv.Close()
		select {
		case <-r.done:
		case <-time.After(5 * time.Second):
		}
		shareLogf("destroyed %s", r.cfg.Hostname)
	}
	for _, s := range n.mgr.List() {
		if s.Shared {
			if err := n.mgr.SetShared(string(s.ID), false); err != nil {
				shareLogf("unsharing %s: %v", s.ID, err)
			}
		}
	}
	if err := os.Remove(filepath.Join(n.dir, shareConfigFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.RemoveAll(filepath.Join(n.dir, shareTSNetDir))
}

// defaultShareHostname is the hostname a sharing node gets if not given
// one: this machine's, plus "-shell".
func defaultShareHostname() string {
	h, _ := os.Hostname()
	h, _, _ = strings.Cut(strings.ToLower(h), ".")
	if !validHostname(h + "-shell") {
		return "tsterm-shell"
	}
	return h + "-shell"
}

// validHostname reports whether h is a valid DNS label.
func validHostname(h string) bool {
	if h == "" || len(h) > 63 || h[0] == '-' || h[len(h)-1] == '-' {
		return false
	}
	for _, c := range h {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// serveSharing waits for the sharing node srv to come up, and serves the
// web UI on it until ctx is done, calling up with its URL once it does.
func serveSharing(ctx context.Context, srv *tsnet.Server, mgr *session.Manager, owner *auth.SelfUser, up func(url string)) error {
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
		shareLogf("warning: %s isn't tagged, so it's owned by the person who logged it in, and others may not be able to reach it", fqdn)
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
		ViewAs:    sharing,
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
			shareLogf("getting a certificate for %s: %v", fqdn, err)
		}
	}()
	up(u)
	shareLogf("serving %s (and http://%s/), owner %s", u, strings.Split(fqdn, ".")[0], owner.SelfLogin())
	for {
		err := tcp.Serve(ctx, ln443, tcpSrv)
		if ctx.Err() != nil {
			return nil
		}
		shareLogf("serving: %v", err)
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
