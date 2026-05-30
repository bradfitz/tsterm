//go:build unix

package udp

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bradfitz/tsterm/pkg/auth"
	"github.com/bradfitz/tsterm/pkg/proto"
	"github.com/bradfitz/tsterm/pkg/pty"
	"github.com/bradfitz/tsterm/pkg/session"
	"github.com/bradfitz/tsterm/pkg/session/sessiontest"
	"github.com/bradfitz/tsterm/pkg/shim"
	"github.com/bradfitz/tsterm/pkg/vt"

	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
)

type allowAllLC struct{ user tailcfg.UserID }

func (a *allowAllLC) WhoIs(context.Context, string) (*apitype.WhoIsResponse, error) {
	return &apitype.WhoIsResponse{Node: &tailcfg.Node{User: a.user}}, nil
}
func (a *allowAllLC) Status(context.Context) (*ipnstate.Status, error) {
	return &ipnstate.Status{Self: &ipnstate.PeerStatus{UserID: a.user}}, nil
}

func newAuth(t *testing.T) *auth.SelfUser {
	a, err := auth.NewSelfUser(context.Background(), &allowAllLC{user: 1})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func startServer(t *testing.T) (*Server, *net.UDPConn) {
	t.Helper()
	return startServerWith(t, func(*Server) {})
}

func startServerIdle(t *testing.T, idle time.Duration) (*Server, *net.UDPConn) {
	t.Helper()
	return startServerWith(t, func(s *Server) { s.IdleTimeout = idle })
}

// startServerWith starts a Server after letting configure adjust it.
func startServerWith(t *testing.T, configure func(*Server)) (*Server, *net.UDPConn) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mgr := sessiontest.NewManager(t)
	t.Cleanup(func() { _ = pc.Close() })
	srv := &Server{
		Manager:     mgr,
		Auth:        newAuth(t),
		IdleTimeout: 2 * time.Second,
	}
	configure(srv)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = Serve(ctx, pc, srv) }()
	return srv, pc.(*net.UDPConn)
}

func newSession(t *testing.T, srv *Server, cmd string, args ...string) *session.Session {
	t.Helper()
	sess, err := srv.Manager.Create(session.Config{
		PTY: pty.Config{Command: cmd, Args: args},
	})
	if err != nil {
		t.Fatal(err)
	}
	return sess
}

// testClient is a minimal hot-path client. It tracks the screen state it
// has applied, like the real client in cmd/tsterm.
type testClient struct {
	t     *testing.T
	conn  *net.UDPConn
	sess  *session.Session
	nonce []byte
	reasm Reassembler
	cur   uint64
	from  uint64 // From of the last applied update
	delta vt.ScreenDelta
}

func dial(t *testing.T, srv *net.UDPConn, sess *session.Session) *testClient {
	t.Helper()
	c, err := net.DialUDP("udp", nil, srv.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return &testClient{t: t, conn: c, sess: sess, nonce: []byte("test-nonce")}
}

func (c *testClient) send(env proto.Envelope) {
	c.t.Helper()
	b, err := proto.Encode(env)
	if err != nil {
		c.t.Fatal(err)
	}
	if _, err := c.conn.Write(b); err != nil {
		c.t.Fatal(err)
	}
}

func (c *testClient) hello(inputAcked uint64) {
	c.t.Helper()
	c.send(proto.Envelope{Hello: &proto.Hello{
		SessionID:  string(c.sess.ID()),
		Token:      c.sess.Token(),
		Nonce:      c.nonce,
		InputAcked: inputAcked,
	}})
}

func (c *testClient) ack() {
	c.t.Helper()
	c.send(proto.Envelope{StateAck: &proto.StateAck{State: c.cur}})
}

// recv reads one envelope, or returns false if none arrives within d.
func (c *testClient) recv(d time.Duration) (proto.Envelope, []byte, bool) {
	c.t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(d))
	buf := make([]byte, 64*1024)
	n, err := c.conn.Read(buf)
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return proto.Envelope{}, nil, false
		}
		c.t.Fatalf("recv: %v", err)
	}
	var env proto.Envelope
	if err := proto.Decode(buf[:n], &env); err != nil {
		c.t.Fatalf("decode: %v", err)
	}
	return env, buf[:n], true
}

// recvUntil reads envelopes until pred returns true or d elapses. State
// fragments are reassembled and applied along the way, without acking.
func (c *testClient) recvUntil(d time.Duration, pred func(proto.Envelope) bool) proto.Envelope {
	c.t.Helper()
	deadline := time.Now().Add(d)
	for {
		env, _, ok := c.recv(time.Until(deadline))
		if !ok {
			c.t.Fatalf("no matching envelope within %v", d)
		}
		if env.State != nil {
			c.apply(env.State)
		}
		if pred(env) {
			return env
		}
	}
}

func (c *testClient) apply(f *proto.State) bool {
	c.t.Helper()
	d, ok, err := c.reasm.Add(f)
	if err != nil {
		c.t.Fatal(err)
	}
	if !ok || f.To <= c.cur || (f.From != 0 && f.From != c.cur) {
		return false
	}
	c.cur, c.from, c.delta = f.To, f.From, d
	return true
}

// syncState waits until the client has applied a state satisfying pred,
// acking each applied state like the real client does.
func (c *testClient) syncState(d time.Duration, pred func(vt.ScreenDelta) bool) {
	c.t.Helper()
	deadline := time.Now().Add(d)
	for {
		env, _, ok := c.recv(time.Until(deadline))
		if !ok {
			c.t.Fatalf("no matching state within %v", d)
		}
		if env.State == nil || !c.apply(env.State) {
			continue
		}
		c.ack()
		if pred(c.delta) {
			return
		}
	}
}

func rowText(d vt.ScreenDelta) string {
	var sb strings.Builder
	for _, run := range d.Regions {
		for _, cell := range run.Cells {
			sb.WriteString(cell.Grapheme)
		}
		sb.WriteByte('\n')
	}
	return sb.String()
}

func anyState(vt.ScreenDelta) bool { return true }

// expectSilence fails if the server sends anything within d.
func (c *testClient) expectSilence(d time.Duration) {
	c.t.Helper()
	if env, _, ok := c.recv(d); ok {
		c.t.Fatalf("server sent %+v to an idle, caught-up client", env)
	}
}

func TestAttachSendsFullState(t *testing.T) {
	srv, pc := startServer(t)
	sess := newSession(t, srv, "/bin/sh", "-c", "printf hi; sleep 5")
	c := dial(t, pc, sess)
	c.hello(0)
	c.syncState(2*time.Second, anyState)
	if c.from != 0 || !c.delta.Resized || len(c.delta.Regions) != c.delta.Rows {
		t.Errorf("first update is not a complete screen: from=%d resized=%v regions=%d rows=%d",
			c.from, c.delta.Resized, len(c.delta.Regions), c.delta.Rows)
	}
	if !strings.Contains(rowText(c.delta), "hi") {
		c.syncState(2*time.Second, func(d vt.ScreenDelta) bool { return strings.Contains(rowText(d), "hi") })
	}
}

func TestBadTokenIgnored(t *testing.T) {
	srv, pc := startServer(t)
	sess := newSession(t, srv, "/bin/sh", "-c", "sleep 5")
	c := dial(t, pc, sess)
	c.send(proto.Envelope{Hello: &proto.Hello{
		SessionID: string(sess.ID()),
		Token:     []byte("wrong-token"),
	}})
	if env, _, ok := c.recv(300 * time.Millisecond); ok {
		t.Errorf("got a response for bad-token attach: %+v", env)
	}
}

func TestHelloUnknownSessionEnded(t *testing.T) {
	srv, pc := startServer(t)
	sess := newSession(t, srv, "/bin/sh", "-c", "sleep 5")
	c := dial(t, pc, sess)
	c.send(proto.Envelope{Hello: &proto.Hello{SessionID: "nonexistent", Token: []byte("x")}})
	env := c.recvUntil(time.Second, func(e proto.Envelope) bool { return e.Ended != nil })
	if env.Ended.Status != proto.EndedNoSession {
		t.Errorf("Ended status = %q, want %q", env.Ended.Status, proto.EndedNoSession)
	}
}

func TestIdleClientGetsNoTraffic(t *testing.T) {
	srv, pc := startServer(t)
	sess := newSession(t, srv, "/bin/sh", "-c", "printf ready; sleep 10")
	c := dial(t, pc, sess)
	c.hello(0)
	c.syncState(2*time.Second, func(d vt.ScreenDelta) bool { return strings.Contains(rowText(d), "ready") })
	// Once the client has acked the current screen, the server must
	// send nothing: no keyframes, no deltas, no retransmissions.
	c.expectSilence(1500 * time.Millisecond)
}

func TestUnackedStateRetransmittedIdentically(t *testing.T) {
	srv, pc := startServer(t)
	sess := newSession(t, srv, "/bin/sh", "-c", "printf hi; sleep 5")
	c := dial(t, pc, sess)
	c.hello(0)
	c.recvUntil(2*time.Second, func(e proto.Envelope) bool {
		return e.State != nil && strings.Contains(rowText(c.delta), "hi")
	})
	// Never ack. The same update must come again, byte for byte.
	var first []byte
	for range 2 {
		env, b, ok := c.recv(2 * time.Second)
		if !ok {
			t.Fatal("no retransmission")
		}
		if env.State == nil || env.State.To != c.cur {
			continue
		}
		if first == nil {
			first = b
		} else if !bytes.Equal(first, b) {
			t.Errorf("retransmission differs from original")
		}
	}
}

func TestDeltaIsFromAckedState(t *testing.T) {
	srv, pc := startServer(t)
	sess := newSession(t, srv, "/bin/cat")
	c := dial(t, pc, sess)
	c.hello(0)
	c.syncState(2*time.Second, anyState)
	c.expectSilence(300 * time.Millisecond)
	base := c.cur

	c.send(proto.Envelope{Input: &proto.Input{Seq: 1, Keys: []byte("hello")}})
	c.syncState(2*time.Second, func(d vt.ScreenDelta) bool { return strings.Contains(rowText(d), "hello") })
	if c.from != base {
		t.Errorf("update diffed from state %d, want acked state %d", c.from, base)
	}
	if c.delta.Resized || len(c.delta.Regions) != 1 {
		t.Errorf("update is not a one-row diff: resized=%v regions=%d", c.delta.Resized, len(c.delta.Regions))
	}
}

func TestStaleBaselineRebuilt(t *testing.T) {
	srv, pc := startServer(t)
	sess := newSession(t, srv, "/bin/cat")
	c := dial(t, pc, sess)
	c.hello(0)
	c.syncState(2*time.Second, anyState)
	c.expectSilence(300 * time.Millisecond)

	// Apply a state but lose the ack, then cause another change. The
	// server, not knowing what we have, diffs from the older state.
	c.send(proto.Envelope{Input: &proto.Input{Seq: 1, Keys: []byte("a")}})
	c.recvUntil(2*time.Second, func(e proto.Envelope) bool { return e.State != nil && strings.Contains(rowText(c.delta), "a") })
	have := c.cur
	c.send(proto.Envelope{Input: &proto.Input{Seq: 2, Keys: []byte("b")}})
	// Our ack for have now arrives; the next update must build on it.
	c.ack()
	c.syncState(2*time.Second, func(d vt.ScreenDelta) bool { return strings.Contains(rowText(d), "ab") })
	if c.from != have {
		t.Errorf("update diffed from state %d, want %d", c.from, have)
	}
}

func TestInputAckAndApply(t *testing.T) {
	srv, pc := startServer(t)
	sess := newSession(t, srv, "/bin/cat")
	c := dial(t, pc, sess)
	c.hello(0)
	c.syncState(2*time.Second, anyState)

	c.send(proto.Envelope{Input: &proto.Input{Seq: 1, Keys: []byte("hi\n")}})
	ack := c.recvUntil(2*time.Second, func(e proto.Envelope) bool { return e.Ack != nil })
	if ack.Ack.Highest != 1 {
		t.Errorf("ack = %d, want 1", ack.Ack.Highest)
	}
}

func TestRepeatHelloKeepsState(t *testing.T) {
	srv, pc := startServer(t)
	sess := newSession(t, srv, "/bin/sh", "-c", "sleep 5")
	c := dial(t, pc, sess)
	c.hello(0)
	c.syncState(2*time.Second, anyState)

	// A repeated Hello with the same nonce, as sent on waking from sleep,
	// is answered with an Ack and does not resend the screen.
	c.hello(0)
	env := c.recvUntil(time.Second, func(e proto.Envelope) bool { return true })
	if env.Ack == nil {
		t.Fatalf("repeat Hello answered with %+v, want Ack", env)
	}
	c.expectSilence(300 * time.Millisecond)
}

func TestIdleTimeoutDropsClient(t *testing.T) {
	srv, pc := startServerIdle(t, 200*time.Millisecond)
	sess := newSession(t, srv, "/bin/sh", "-c", "sleep 5")
	c := dial(t, pc, sess)
	c.hello(0)
	c.syncState(time.Second, anyState)

	// Wait past the idle window, then send an Input. Without an attach the
	// server should ignore it; no Ack returns.
	time.Sleep(srv.IdleTimeout + srv.IdleTimeout/2 + 200*time.Millisecond)
	c.send(proto.Envelope{Input: &proto.Input{Seq: 1, Keys: []byte("x")}})
	if env, _, ok := c.recv(300 * time.Millisecond); ok {
		t.Errorf("got %+v after idle expiry", env)
	}
}

func TestReattachAfterIdleTimeout(t *testing.T) {
	srv, pc := startServerIdle(t, 200*time.Millisecond)
	sess := newSession(t, srv, "/bin/cat")
	c := dial(t, pc, sess)
	c.hello(0)
	c.syncState(time.Second, anyState)
	c.send(proto.Envelope{Input: &proto.Input{Seq: 1, Keys: []byte("x")}})
	c.recvUntil(time.Second, func(e proto.Envelope) bool { return e.Ack != nil && e.Ack.Highest == 1 })
	before := c.cur
	time.Sleep(srv.IdleTimeout + srv.IdleTimeout/2 + 200*time.Millisecond)

	// Reattach. The server must send a complete screen numbered above
	// what we already have (which can include old-attachment updates still
	// in flight), and resume the input stream after Seq 1.
	c.hello(1)
	c.syncState(time.Second, func(vt.ScreenDelta) bool { return c.from == 0 })
	if c.cur <= before {
		t.Errorf("reattach state %d not above prior state %d", c.cur, before)
	}
	c.send(proto.Envelope{Input: &proto.Input{Seq: 2, Keys: []byte("y")}})
	c.recvUntil(time.Second, func(e proto.Envelope) bool { return e.Ack != nil && e.Ack.Highest == 2 })
}

func TestSessionEndSendsEnded(t *testing.T) {
	srv, pc := startServer(t)
	sess := newSession(t, srv, "/bin/sh", "-c", "sleep 0.3; printf bye")
	c := dial(t, pc, sess)
	c.hello(0)
	c.recvUntil(3*time.Second, func(e proto.Envelope) bool { return e.Ended != nil })
	if !strings.Contains(rowText(c.delta), "bye") {
		t.Errorf("final screen not delivered before Ended")
	}
	// Ended may be lost, so the server repeats it for later packets.
	c.ack()
	c.recvUntil(time.Second, func(e proto.Envelope) bool { return e.Ended != nil })
}

func TestRetransmitBacksOffThenGivesUp(t *testing.T) {
	const giveUp = 2 * time.Second
	srv, pc := startServerWith(t, func(s *Server) { s.GiveUpAfter = giveUp })
	sess := newSession(t, srv, "/bin/sh", "-c", "printf hi; sleep 10")
	c := dial(t, pc, sess)
	c.hello(0)

	// Never ack. Record when each State datagram arrives.
	start := time.Now()
	var times []time.Duration
	for {
		env, _, ok := c.recv(giveUp + 2*time.Second)
		if !ok {
			break
		}
		if env.State != nil {
			times = append(times, time.Since(start))
		}
	}
	if len(times) < 4 {
		t.Fatalf("only %d transmissions before giving up", len(times))
	}
	last := times[len(times)-1]
	if last > giveUp+500*time.Millisecond {
		t.Errorf("still sending %v after first send; GiveUpAfter is %v", last, giveUp)
	}
	// With exponential backoff, transmissions are far fewer than with a
	// fixed short timer. 200ms doubling over 2s allows about 6 tries; a
	// generous bound still catches a timer that never backs off.
	if len(times) > 12 {
		t.Errorf("%d transmissions in %v; want exponential backoff", len(times), last)
	}
}

func TestResumesAfterGiveUp(t *testing.T) {
	const giveUp = 500 * time.Millisecond
	srv, pc := startServerWith(t, func(s *Server) { s.GiveUpAfter = giveUp })
	sess := newSession(t, srv, "/bin/cat")
	c := dial(t, pc, sess)
	c.hello(0)
	c.syncState(time.Second, anyState)

	// The screen changes while the client is away (its packets are lost,
	// so drain them unread), and the server gives up.
	sess.WriteInput([]byte("missed"))
	for {
		if _, _, ok := c.recv(giveUp + time.Second); !ok {
			break
		}
	}

	// The client wakes and says Hello. The server must catch it up with
	// the output it missed.
	c.hello(0)
	c.syncState(time.Second, func(d vt.ScreenDelta) bool { return strings.Contains(rowText(d), "missed") })
	c.expectSilence(500 * time.Millisecond)
}

func TestInputFloorSkipsAbandoned(t *testing.T) {
	srv, pc := startServer(t)
	sess := newSession(t, srv, "/bin/cat")
	c := dial(t, pc, sess)
	c.hello(0)
	c.syncState(time.Second, anyState)

	// Seqs 1 and 2 never arrived and the client abandoned them.
	c.send(proto.Envelope{Input: &proto.Input{Seq: 3, Keys: []byte("z"), Floor: 2}})
	c.recvUntil(time.Second, func(e proto.Envelope) bool { return e.Ack != nil && e.Ack.Highest == 3 })
	// A late copy of abandoned input must not be applied.
	c.send(proto.Envelope{Input: &proto.Input{Seq: 1, Keys: []byte("LATE")}})
	c.send(proto.Envelope{Input: &proto.Input{Seq: 4, Keys: []byte("q")}})
	c.syncState(time.Second, func(d vt.ScreenDelta) bool { return strings.Contains(rowText(d), "zq") })
	if strings.Contains(rowText(c.delta), "LATE") {
		t.Errorf("abandoned input was applied: %q", rowText(c.delta))
	}
}

func TestInputGapWithoutFloorWaits(t *testing.T) {
	srv, pc := startServer(t)
	sess := newSession(t, srv, "/bin/cat")
	c := dial(t, pc, sess)
	c.hello(0)
	c.syncState(time.Second, anyState)
	// Seq 2 arriving before 1, with no floor, is held for retransmission.
	c.send(proto.Envelope{Input: &proto.Input{Seq: 2, Keys: []byte("b")}})
	if env, _, ok := c.recv(300 * time.Millisecond); ok {
		t.Fatalf("out-of-order input answered with %+v", env)
	}
}

func TestRestartResumesIdleClient(t *testing.T) {
	dir, err := os.MkdirTemp("", "sess")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	launcher := &shim.InProcess{}
	defer launcher.Close()

	// serve runs a tstermd-like server: a Manager restored from dir and a
	// UDP server on addr. It returns the server's socket and a function
	// that shuts it down the way tstermd does.
	serve := func(addr string) (*session.Manager, *net.UDPConn, func()) {
		mgr := session.NewManager(dir, launcher)
		if _, err := mgr.Restore(context.Background()); err != nil {
			t.Fatal(err)
		}
		pc, err := net.ListenPacket("udp", addr)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			Serve(ctx, pc, &Server{Manager: mgr, Auth: newAuth(t)})
		}()
		return mgr, pc.(*net.UDPConn), func() {
			cancel()
			pc.Close()
			<-done
			mgr.Detach()
		}
	}

	mgr1, pc1, stop1 := serve("127.0.0.1:0")
	sess, err := mgr1.Create(session.Config{PTY: pty.Config{Command: "/bin/cat"}})
	if err != nil {
		t.Fatal(err)
	}
	c := dial(t, pc1, sess)
	c.hello(0)
	c.syncState(2*time.Second, anyState)
	c.send(proto.Envelope{Input: &proto.Input{Seq: 1, Keys: []byte("before")}})
	c.syncState(2*time.Second, func(d vt.ScreenDelta) bool { return strings.Contains(rowText(d), "before") })
	c.expectSilence(300 * time.Millisecond)
	before := c.cur

	// Restart the server on the same port while the client is idle. The
	// shim, and so the child, keeps running.
	addr := pc1.LocalAddr().String()
	stop1()
	_, _, stop2 := serve(addr)
	defer stop2()

	// Without sending anything, the client must get the whole screen,
	// numbered above what it has.
	c.syncState(3*time.Second, func(vt.ScreenDelta) bool { return c.from == 0 })
	if c.cur <= before || !strings.Contains(rowText(c.delta), "before") {
		t.Errorf("after restart got state %d (had %d), screen %q", c.cur, before, rowText(c.delta))
	}

	// Input continues: the new server does not know the input position,
	// and the Floor lets it skip ahead.
	c.send(proto.Envelope{Input: &proto.Input{Seq: 2, Keys: []byte("after"), Floor: 1}})
	c.recvUntil(2*time.Second, func(e proto.Envelope) bool { return e.Ack != nil && e.Ack.Highest == 2 })
	c.syncState(2*time.Second, func(d vt.ScreenDelta) bool { return strings.Contains(rowText(d), "beforeafter") })
}
