//go:build !linux

package shim

import "net"

// checkPeer is a no-op here; the socket directory's permissions restrict
// access to the owning user.
//
// TODO(bradfitz): Use getpeereid on macOS and the BSDs.
func checkPeer(c net.Conn) error { return nil }
