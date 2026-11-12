//go:build linux

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/bradfitz/tsterm/pkg/proto"
	"github.com/bradfitz/tsterm/pkg/pty"
	"github.com/bradfitz/tsterm/pkg/session"
	"github.com/bradfitz/tsterm/pkg/session/sessiontest"
)

func TestValidHostname(t *testing.T) {
	for h, want := range map[string]bool{
		"braid-shell": true, "a": true, "x9": true,
		"": false, "-a": false, "a-": false, "Braid": false, "a.b": false, "a b": false,
	} {
		if got := validHostname(h); got != want {
			t.Errorf("validHostname(%q) = %v", h, got)
		}
	}
	if h := defaultShareHostname(); !validHostname(h) {
		t.Errorf("defaultShareHostname() = %q, invalid", h)
	}
}

// TestShareNodeOffAndDestroy checks what a sharing node does without
// starting tsnet: refusing bad configs, and destroying, which unshares
// every session and forgets its config and state.
func TestShareNodeOffAndDestroy(t *testing.T) {
	ctx := context.Background()
	mgr := sessiontest.NewManager(t)
	dir := t.TempDir()
	n := newShareNode(ctx, dir, mgr, nil)
	if err := n.startSaved(); err != nil || n.cur != nil {
		t.Fatalf("startSaved with nothing saved: %v, %v", err, n.cur)
	}
	if st := n.Status(ctx); st.State != "off" || st.Hostname == "" || n.URL() != "" {
		t.Errorf("Status = %+v, URL %q", st, n.URL())
	}
	for _, req := range []proto.StartShareNodeReq{
		{Hostname: "bad name"},
		{Hostname: "ok", Tags: []string{"notatag"}},
		{Hostname: "ok", Tags: []string{"tag:"}},
	} {
		if err := n.Start(ctx, req); err == nil {
			t.Errorf("Start(%+v) succeeded", req)
		}
	}

	s, err := mgr.Create(session.Config{PTY: pty.Config{Command: "/bin/sh", Args: []string{"-c", "sleep 5"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.SetShared(string(s.ID()), true); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, shareConfigFile), []byte(`{"hostname":"x"}`), 0o600)
	os.MkdirAll(filepath.Join(dir, shareTSNetDir), 0o700)
	os.WriteFile(filepath.Join(dir, shareTSNetDir, "tailscaled.state"), []byte("{}"), 0o600)
	if err := n.Destroy(ctx); err != nil {
		t.Fatal(err)
	}
	if s.Shared() {
		t.Error("session still shared after Destroy")
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("left in the state dir: %v", ents)
	}
}
