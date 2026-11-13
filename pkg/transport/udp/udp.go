// Package udp implements the tsterm hot-path datagram transport.
//
// One UDP listener handles all sessions. Each Hello frame attaches the
// source address to a session (after token + Tailscale-identity check); the
// server then exchanges Input/Resize/Ack frames and State/StateAck frames
// with the client.
//
// Lost packets are tolerated by design. Input is sequenced and acked, so the
// client retransmits unacked Input frames. Screen state is synchronized the
// way Mosh does it: the server numbers the states it sends, the client
// acknowledges the states it applies, and each update is a diff from the
// newest acknowledged state to the current screen. A lost update is never
// resent as such; the next update supersedes it. Once the client has
// acknowledged the current screen, the server sends nothing.
//
// Neither side sends keepalives, so an idle session sends no packets at all,
// which matters for battery life on mobile clients. A side with something to
// deliver retransmits with randomized exponential backoff for up to
// GiveUpAfter. If the client stays silent that long, the server goes quiet
// but remembers that the client is behind, and catches it up as soon as the
// client sends anything (such as the Hello it sends on waking from sleep).
//
// The server forgets clients that send nothing for IdleTimeout. Forgetting a
// client stops only its updates; the session and its PTY keep running, and
// the client reattaches by sending another Hello.
//
// A client's user may type Ctrl-^ " (menuKey after escapeByte) to open the
// session menu (see package switcher), which the server draws over the
// session's screen, and to which their keys go while it's open, and
// through which they may switch the client to another session. The client
// knows nothing of it, except to be told which session it's looking at
// (proto.Viewing).
package udp

import (
	"bytes"
	"context"
	"errors"
	"log"
	"maps"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bradfitz/tsterm/pkg/auth"
	"github.com/bradfitz/tsterm/pkg/proto"
	"github.com/bradfitz/tsterm/pkg/session"
	"github.com/bradfitz/tsterm/pkg/switcher"
	"github.com/bradfitz/tsterm/pkg/vt"
)

// Server runs the hot-path datagram listener.
type Server struct {
	Manager *session.Manager
	Auth    *auth.SelfUser

	// Logf, if non-nil, receives per-packet error logs. Defaults to no-op.
	Logf func(format string, args ...any)

	// IdleTimeout drops clients that have not sent a packet within this
	// window. An idle client costs no traffic, only memory, so this can be
	// long. Defaults to 24 hours.
	IdleTimeout time.Duration

	// GiveUpAfter is how long to retransmit screen state to a silent
	// client before going quiet. Defaults to the package's GiveUpAfter.
	GiveUpAfter time.Duration
}

// Screen-state send pacing, after Mosh. The server sends a new state at
// most once per send interval, which is half the smoothed RTT clamped to
// this range. Coalescing bursts of output this way keeps a busy screen from
// flooding the link with updates the client would immediately discard.
const (
	minSendInterval = 20 * time.Millisecond
	maxSendInterval = 250 * time.Millisecond
)

// Retransmission timeout bounds for unacknowledged screen states.
const (
	minRTO = 50 * time.Millisecond
	maxRTO = 1 * time.Second

	// maxBackoffRTO caps the exponentially backed-off retransmission
	// interval for a client that has stopped acknowledging.
	maxBackoffRTO = 10 * time.Second
)

// lastStateNum is the most recently assigned screen-state number. States
// are numbered from one process-wide counter so that numbers increase across
// every attachment. A client reattaching after the server forgot it thus
// sees new states numbered above anything it has already applied, and
// delayed datagrams from the old attachment are recognizably stale.
//
// The counter starts at the current time in nanoseconds, so that a
// restarted tstermd also numbers states above those of its predecessor,
// whose clients reattach with their old state numbers. States are numbered
// far less often than once per nanosecond, so a later start is always
// ahead.
var lastStateNum atomic.Uint64

func init() {
	lastStateNum.Store(uint64(time.Now().UnixNano()))
}

// maxSentStates bounds how many unacknowledged states the server remembers
// per client. An ack for a forgotten state is ignored; a later ack catches
// the server up.
const maxSentStates = 16

// Serve reads packets from pc until ctx is canceled. Serve may be called
// concurrently with the same Server for several sockets, such as one per
// Tailscale address.
func Serve(ctx context.Context, pc net.PacketConn, srv *Server) error {
	s := *srv // apply defaults without modifying the shared Server
	if s.Logf == nil {
		s.Logf = func(string, ...any) {}
	}
	if s.IdleTimeout == 0 {
		s.IdleTimeout = 24 * time.Hour
	}
	if s.GiveUpAfter == 0 {
		s.GiveUpAfter = GiveUpAfter
	}
	mux := &mux{
		pc:      pc,
		srv:     &s,
		clients: map[netip.AddrPort]*client{},
		ctx:     ctx,
	}
	go mux.reaper()
	go mux.recheck()
	mux.resume()
	return mux.run()
}

type mux struct {
	pc  net.PacketConn
	srv *Server
	ctx context.Context

	mu      sync.Mutex
	clients map[netip.AddrPort]*client
}

func (m *mux) run() error {
	go func() {
		<-m.ctx.Done()
		_ = m.pc.SetReadDeadline(time.Now())
	}()
	buf := make([]byte, 64*1024)
	for {
		n, addr, err := m.pc.ReadFrom(buf)
		if err != nil {
			if m.ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		ap, ok := addrFromNet(addr)
		if !ok {
			continue
		}
		var env proto.Envelope
		if err := proto.Decode(buf[:n], &env); err != nil {
			m.srv.Logf("udp: decode from %v: %v", ap, err)
			continue
		}
		if env.Hello != nil {
			// Attach does a (cached) WhoIs, so keep it off the read loop.
			go m.attach(ap, env.Hello)
			continue
		}
		// Everything else is handled by the client's own goroutine, in
		// arrival order. Packets from unknown addresses are ignored; the
		// client notices the silence and sends a fresh Hello.
		if c, ok := m.clientFor(ap); ok {
			c.deliver(env)
		}
	}
}

func (m *mux) reaper() {
	t := time.NewTicker(m.srv.IdleTimeout / 2)
	defer t.Stop()
	for {
		select {
		case <-m.ctx.Done():
			m.mu.Lock()
			for _, c := range m.clients {
				c.stop()
			}
			m.clients = map[netip.AddrPort]*client{}
			m.mu.Unlock()
			return
		case <-t.C:
		}
		cutoff := time.Now().Add(-m.srv.IdleTimeout)
		var reaped []*client
		m.mu.Lock()
		for addr, c := range m.clients {
			if c.lastSeen().Before(cutoff) {
				c.stop()
				delete(m.clients, addr)
				reaped = append(reaped, c)
			}
		}
		m.mu.Unlock()
		for _, c := range reaped {
			m.forget(c.session(), c.addr)
		}
	}
}

func (m *mux) attach(addr netip.AddrPort, h *proto.Hello) {
	if err := m.srv.Auth.Allow(m.ctx, addr.String()); err != nil {
		m.srv.Logf("udp: attach from %v: %v", addr, err)
		return
	}
	m.mu.Lock()
	if prev, ok := m.clients[addr]; ok && len(h.Nonce) > 0 && bytes.Equal(prev.nonce, h.Nonce) {
		// A repeated Hello from a client we still know, such as one
		// waking from sleep, whatever session it names, since its
		// user may have switched it to another. Keep its state; it
		// updates its size, which applies if it still leads, but
		// waking up isn't using it, so it doesn't take the lead back.
		m.mu.Unlock()
		prev.deliver(proto.Envelope{Hello: h})
		return
	}
	m.mu.Unlock()

	// A client we've forgotten, but that switched sessions, returns to
	// the one it switched to. Its Nonce, a secret it shares only with
	// us, is enough to show it's the same client.
	sess := m.switchedTo(h.Nonce)
	if sess == nil {
		var err error
		sess, err = m.srv.Manager.Resolve(h.SessionID)
		if err != nil {
			// The peer is authorized, so it is safe to tell it the
			// session is gone. This is what a client sees after the
			// daemon restarts.
			m.srv.Logf("udp: attach from %v: %v", addr, err)
			m.sendTo(addr, proto.Envelope{Ended: &proto.Ended{Status: proto.EndedNoSession}})
			return
		}
		if !bytes.Equal(sess.Token(), h.Token) {
			m.srv.Logf("udp: attach from %v: bad token", addr)
			return
		}
	}
	m.mu.Lock()
	if prev, ok := m.clients[addr]; ok {
		prev.stop()
		delete(m.clients, addr)
	}
	// Attaching is using the client, so its size applies.
	c := m.addClientLocked(addr, sess, h.Nonce, h.InputAcked, h.Rows, h.Cols, true)
	m.mu.Unlock()
	if !c.names(h.SessionID) {
		m.sendTo(addr, viewing(sess))
	}
	m.remember(sess, addr, h.Nonce, !c.names(h.SessionID))
	// Attaching is wanting to see it: it's open again, if it was closed.
	if err := m.srv.Manager.SetClosed(string(sess.ID()), false); err != nil {
		m.srv.Logf("udp: reopening %s: %v", sess.ID(), err)
	}
	if err := sess.MarkAttached(); err != nil {
		m.srv.Logf("udp: recording attach: %v", err)
	}
}

// addClientLocked registers a client, whose screen is rows by cols (or
// unknown, if zero), and, if lead, which the user is using, so its size
// applies; and starts its goroutine, which begins by sending the client
// the whole screen.
func (m *mux) addClientLocked(addr netip.AddrPort, sess *session.Session, nonce []byte, inputAcked uint64, rows, cols int, lead bool) *client {
	ctx, cancel := context.WithCancel(m.ctx)
	c := &client{
		mux:      m,
		addr:     addr,
		nonce:    bytes.Clone(nonce),
		ctx:      ctx,
		cancel:   cancel,
		inbox:    make(chan proto.Envelope, 64),
		seen:     time.Now(),
		ackedSeq: inputAcked,
		dirty:    true,
		rows:     rows,
		cols:     cols,
	}
	c.watch(sess)
	if lead && rows > 0 && cols > 0 {
		if err := c.sizer.Lead(); err != nil {
			m.srv.Logf("udp: attach resize: %v", err)
		}
	}
	m.clients[addr] = c
	go c.run()
	return c
}

// switchedTo returns the session that the client with nonce switched to,
// if it did, and it's still running.
func (m *mux) switchedTo(nonce []byte) *session.Session {
	if len(nonce) == 0 {
		return nil
	}
	for _, sess := range m.srv.Manager.Sessions() {
		for _, r := range sess.Clients() {
			if r.Switched && bytes.Equal(r.Nonce, nonce) {
				return sess
			}
		}
	}
	return nil
}

// viewing returns the Viewing frame for sess.
func viewing(sess *session.Session) proto.Envelope {
	return proto.Envelope{Viewing: &proto.Viewing{
		SessionID: string(sess.ID()),
		Name:      sess.Name(),
		Token:     sess.Token(),
	}}
}

// maxRemembered bounds the clients recorded per session.
const maxRemembered = 8

// remember records the client at addr in sess, where it survives a tstermd
// restart; see resume. switched says it switched to sess, from the one it
// attached to; see switchedTo.
func (m *mux) remember(sess *session.Session, addr netip.AddrPort, nonce []byte, switched bool) {
	err := sess.UpdateClients(func(recs []session.Client) []session.Client {
		recs = slices.DeleteFunc(recs, func(r session.Client) bool { return r.Addr == addr.String() })
		recs = append(recs, session.Client{Addr: addr.String(), Nonce: bytes.Clone(nonce), Switched: switched})
		return recs[max(len(recs)-maxRemembered, 0):]
	})
	if err != nil {
		m.srv.Logf("udp: recording client %v: %v", addr, err)
	}
}

// forget removes the record of the client at addr from sess.
func (m *mux) forget(sess *session.Session, addr netip.AddrPort) {
	err := sess.UpdateClients(func(recs []session.Client) []session.Client {
		return slices.DeleteFunc(recs, func(r session.Client) bool { return r.Addr == addr.String() })
	})
	if err != nil {
		m.srv.Logf("udp: forgetting client %v: %v", addr, err)
	}
}

// drop forgets the client c, as when it detaches or loses authorization.
func (m *mux) drop(c *client) {
	m.mu.Lock()
	if m.clients[c.addr] == c {
		delete(m.clients, c.addr)
	}
	m.mu.Unlock()
	c.stop()
	m.forget(c.session(), c.addr)
}

// recheck drops attached clients that stop being authorized, as when their
// tailnet address is reassigned to another user's node.
func (m *mux) recheck() {
	for {
		changes := m.srv.Auth.Changes()
		m.mu.Lock()
		clients := slices.Collect(maps.Values(m.clients))
		m.mu.Unlock()
		for _, c := range clients {
			if err := m.srv.Auth.Allow(m.ctx, c.addr.String()); err != nil {
				m.srv.Logf("udp: dropping client %v: %v", c.addr, err)
				m.drop(c)
			}
		}
		select {
		case <-m.ctx.Done():
			return
		case <-changes:
		}
	}
}

// serves reports whether addr's address family is served by this mux's
// socket. With one socket per Tailscale address, IPv4 and IPv6 clients
// arrive on different sockets.
func (m *mux) serves(addr netip.AddrPort) bool {
	local, ok := addrFromNet(m.pc.LocalAddr())
	if !ok || local.Addr().IsUnspecified() {
		return true
	}
	return local.Addr().Is4() == addr.Addr().Unmap().Is4()
}

// resume reattaches the clients recorded in every session, as after a
// tstermd restart. An idle client sends nothing, so it would otherwise not
// learn that it is missing output until its user typed. Each resumed
// client is sent the whole screen and, if it is gone, given up on after
// GiveUpAfter like any other.
func (m *mux) resume() {
	for _, sess := range m.srv.Manager.Sessions() {
		for _, r := range sess.Clients() {
			addr, err := netip.ParseAddrPort(r.Addr)
			if err != nil || !m.serves(addr) {
				continue
			}
			go func() {
				// Tailnet addresses can be reassigned, so check the
				// peer's identity again.
				if err := m.srv.Auth.Allow(m.ctx, addr.String()); err != nil {
					m.srv.Logf("udp: resuming %v: %v", addr, err)
					m.forget(sess, addr)
					return
				}
				m.mu.Lock()
				defer m.mu.Unlock()
				if _, ok := m.clients[addr]; !ok {
					// The input stream's position is unknown, so start
					// at 0. The client's next Input carries a Floor,
					// which lets the server skip ahead to it.
					m.addClientLocked(addr, sess, r.Nonce, 0, 0, 0, false)
				}
			}()
		}
	}
}

func (m *mux) clientFor(addr netip.AddrPort) (*client, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.clients[addr]
	return c, ok
}

func (m *mux) sendTo(addr netip.AddrPort, env proto.Envelope) {
	b, err := proto.Encode(env)
	if err != nil {
		m.srv.Logf("udp: encode: %v", err)
		return
	}
	if _, err := m.pc.WriteTo(b, net.UDPAddrFromAddrPort(addr)); err != nil {
		m.srv.Logf("udp: send to %v: %v", addr, err)
	}
}

// client is the per-source-address attach state. Apart from seen, which the
// reaper reads, every field after cancel is owned by the run goroutine.
type client struct {
	mux    *mux
	addr   netip.AddrPort
	nonce  []byte
	ctx    context.Context
	cancel context.CancelFunc
	inbox  chan proto.Envelope

	// sess is the session the client is looking at, which the run
	// goroutine changes, under sessMu, when the user switches.
	sessMu sync.Mutex
	sess   *session.Session

	watcher     *session.Watcher
	watchCancel context.CancelFunc // stops watcher
	sizer       *session.Sizer
	sessEnded   bool // the session ended while the menu was open
	rows, cols  int  // the client's screen, if known
	escaped     bool // the last input byte was escapeByte
	menu        *switcher.Menu

	seenMu sync.Mutex
	seen   time.Time

	// ackedSeq is the highest contiguous input Seq applied.
	ackedSeq uint64

	// ended is set once the session has ended and Ended has been sent.
	ended bool

	// Screen-state sync. ackedNum and acked are the newest state the
	// client has acknowledged; state 0 is the zero Screen. sent holds the
	// states sent since then, oldest first.
	ackedNum uint64
	acked    vt.Screen
	sent     []sentState

	// dirty means the canonical screen may have changed since the newest
	// sent state was built.
	dirty    bool
	lastSend time.Time // when the newest state was first sent

	// Retransmission of the newest unacknowledged state. attempt counts
	// retransmissions since the client was last heard from and sets the
	// backoff. trySince is when the server began sending to a client that
	// has not been heard from since (zero if none), and gaveUp is set
	// once that has lasted GiveUpAfter: the server then sends nothing
	// until the client speaks, although the client is still behind.
	attempt  int
	nextTry  time.Time
	trySince time.Time
	gaveUp   bool

	srtt, rttvar time.Duration // zero until the first RTT sample
}

// sentState is a screen state sent to the client but not yet acknowledged.
type sentState struct {
	num    uint64
	from   uint64 // state the update was diffed from
	screen vt.Screen
	pkts   [][]byte
	sentAt time.Time // time of the first transmission
	tries  int       // transmissions, for Karn's algorithm
}

// session returns the session the client is looking at.
func (c *client) session() *session.Session {
	c.sessMu.Lock()
	defer c.sessMu.Unlock()
	return c.sess
}

// names reports whether idOrName names the client's session.
func (c *client) names(idOrName string) bool {
	s := c.session()
	return idOrName == string(s.ID()) || (idOrName != "" && idOrName == s.Name())
}

// watch makes sess the client's session, watching it, and sizing it to the
// client's screen, if known.
func (c *client) watch(sess *session.Session) {
	c.sessMu.Lock()
	c.sess = sess
	c.sessMu.Unlock()
	ctx, cancel := context.WithCancel(c.ctx)
	c.watcher, c.watchCancel = sess.Watch(ctx), cancel
	c.sizer = sess.NewSizer("tsterm")
	c.sessEnded = false
	if c.rows > 0 && c.cols > 0 {
		c.sizer.Want(c.rows, c.cols)
	}
}

func (c *client) touch() {
	c.seenMu.Lock()
	c.seen = time.Now()
	c.seenMu.Unlock()
}

func (c *client) lastSeen() time.Time {
	c.seenMu.Lock()
	defer c.seenMu.Unlock()
	return c.seen
}

func (c *client) stop() {
	c.cancel()
}

// deliver queues env for the run goroutine. If the client is hopelessly
// behind, the packet is dropped like any other lost datagram.
func (c *client) deliver(env proto.Envelope) {
	c.touch()
	select {
	case c.inbox <- env:
	default:
	}
}

func (c *client) send(env proto.Envelope) {
	c.mux.sendTo(c.addr, env)
}

func (c *client) sendPkts(pkts [][]byte) {
	ua := net.UDPAddrFromAddrPort(c.addr)
	for _, b := range pkts {
		if _, err := c.mux.pc.WriteTo(b, ua); err != nil {
			c.mux.srv.Logf("udp: send state to %v: %v", c.addr, err)
			return
		}
	}
}

func (c *client) sendAck() {
	c.send(proto.Envelope{Ack: &proto.Ack{Highest: c.ackedSeq}})
}

// run is the client's event loop. It owns all screen-sync and input state.
func (c *client) run() {
	defer func() {
		if c.menu != nil {
			c.menu.Close()
		}
		c.sizer.Close()
	}()
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		var ticks <-chan struct{}
		if !c.sessEnded && !c.ended {
			ticks = c.watcher.Ticks()
		}
		var menuChanged, menuDone <-chan struct{}
		if c.menu != nil {
			menuChanged, menuDone = c.menu.Changed(), c.menu.Done()
		}
		select {
		case <-c.ctx.Done():
			return
		case _, ok := <-ticks:
			if !ok {
				// The watcher closes when the session ends or when
				// this client is stopped.
				if c.ctx.Err() != nil {
					return
				}
				c.sessEnded = true
				if c.menu != nil {
					// The user may yet switch to another.
					c.dirty = true
					break
				}
				c.end()
				continue
			}
			c.dirty = true
		case <-menuChanged:
			c.dirty = true
		case <-menuDone:
			c.closeMenu()
		case env := <-c.inbox:
			c.handle(env)
		case <-timer.C:
		}
		if c.ended {
			continue
		}
		if wait, ok := c.step(time.Now()); ok {
			timer.Reset(wait)
		}
	}
}

// end tells the client the session is over. Afterwards the client goroutine
// stays registered until the client goes quiet, answering every packet with
// another Ended in case the first was lost.
func (c *client) end() {
	if c.ended {
		return
	}
	// Flush the final screen first so the client sees the last output.
	if c.dirty {
		c.dirty = false
		c.sendNewState(time.Now(), c.sess.Snapshot())
	}
	c.ended = true
	c.send(proto.Envelope{Ended: &proto.Ended{Status: "closed"}})
}

func (c *client) handle(env proto.Envelope) {
	if c.ended {
		c.send(proto.Envelope{Ended: &proto.Ended{Status: "closed"}})
		return
	}
	// Hearing from the client resets retransmission: if it is still
	// missing state, retry promptly and with a fresh GiveUpAfter budget.
	c.trySince = time.Time{}
	c.attempt = 0
	if c.gaveUp {
		c.gaveUp = false
		c.dirty = true
	}
	if len(c.sent) > 0 {
		c.nextTry = time.Now()
	}
	switch {
	case env.Hello != nil:
		h := env.Hello
		if h.Rows > 0 && h.Cols > 0 && (h.Rows != c.rows || h.Cols != c.cols) {
			c.resized(h.Rows, h.Cols)
		}
		// Reply so the client knows it got through even if there is no
		// screen to send, and, if it names another session, which one
		// it's looking at.
		c.sendAck()
		if !c.names(h.SessionID) {
			c.send(viewing(c.sess))
		}
	case env.Input != nil:
		c.handleInput(env.Input)
	case env.Resize != nil:
		c.handleResize(env.Resize)
	case env.StateAck != nil:
		c.handleStateAck(env.StateAck)
	case env.Detach != nil:
		c.mux.drop(c)
	}
}

// skipAbandoned advances ackedSeq over input the client has abandoned, if
// floor says everything before seq is acked or abandoned.
func (c *client) skipAbandoned(seq, floor uint64) {
	if seq > c.ackedSeq+1 && floor >= seq-1 {
		c.ackedSeq = seq - 1
	}
}

func (c *client) handleInput(in *proto.Input) {
	c.skipAbandoned(in.Seq, in.Floor)
	if in.Seq <= c.ackedSeq {
		// Already applied; resend ack so the client can retire its buffer.
		c.sendAck()
		return
	}
	if in.Seq != c.ackedSeq+1 {
		// Out-of-order: drop without ack so the client retransmits.
		return
	}
	keys := in.Keys
	if c.menu == nil && len(keys) > 0 {
		var out []byte
		var open bool
		out, open, keys = c.escape(keys)
		if len(out) > 0 {
			// Whoever types is who the user is using.
			if err := c.sizer.Lead(); err != nil {
				c.mux.srv.Logf("udp: resize: %v", err)
			}
			if _, err := c.sess.WriteInput(out); err != nil {
				c.mux.srv.Logf("udp: write input: %v", err)
				return
			}
		}
		if open {
			c.openMenu()
		}
	}
	if c.menu != nil && len(keys) > 0 {
		c.menu.Input(keys)
	}
	c.ackedSeq = in.Seq
	c.sendAck()
}

// escapeByte and menuKey, typed one after the other, open the session
// menu. escapeByte is Ctrl-^, which also begins the client's own commands,
// such as Ctrl-^ . to disconnect; it passes on the rest. Ctrl-^ Ctrl-^
// types one Ctrl-^.
const (
	escapeByte = 0x1e
	menuKey    = '"'
)

// escape scans keys, typed into the session, for Ctrl-^ ". It returns the
// keys to send the session, whether to open the menu, and, if so, the
// keys after it, which are the menu's.
func (c *client) escape(keys []byte) (out []byte, open bool, rest []byte) {
	for i, b := range keys {
		if !c.escaped {
			if b == escapeByte {
				c.escaped = true
			} else {
				out = append(out, b)
			}
			continue
		}
		c.escaped = false
		switch b {
		case menuKey:
			return out, true, keys[i+1:]
		case escapeByte:
			out = append(out, escapeByte)
		default:
			out = append(out, escapeByte, b)
		}
	}
	return out, false, nil
}

// openMenu opens the session menu over the client's screen.
func (c *client) openMenu() {
	rows, cols := c.rows, c.cols
	if rows <= 0 || cols <= 0 {
		rows, cols = c.sess.Size()
	}
	mgr := c.mux.srv.Manager
	items := func() []switcher.Item {
		var items []switcher.Item
		for _, s := range mgr.List() {
			if !s.Active {
				continue
			}
			items = append(items, switcher.Item{
				ID:       string(s.ID),
				Name:     s.Name,
				What:     strings.Join(s.Cmd, " "),
				Created:  s.Created,
				Activity: s.LastActivity(),
				Watchers: s.Watchers,
			})
		}
		return items
	}
	m, err := switcher.Open(items, string(c.sess.ID()), rows, cols)
	if err != nil {
		c.mux.srv.Logf("udp: opening the session menu: %v", err)
		return
	}
	c.menu = m
	c.dirty = true
}

// closeMenu handles the menu's closing: switching to the session the user
// chose, if any.
func (c *client) closeMenu() {
	choice := c.menu.Choice()
	c.menu = nil
	c.dirty = true
	if choice != "" && choice != string(c.sess.ID()) {
		if sess, err := c.mux.srv.Manager.Resolve(choice); err == nil {
			c.switchTo(sess)
			return
		} else {
			c.mux.srv.Logf("udp: switching to %s: %v", choice, err)
		}
	}
	if c.sessEnded {
		c.end()
	}
}

// switchTo switches the client to sess.
func (c *client) switchTo(sess *session.Session) {
	old := c.sess
	c.watchCancel()
	c.sizer.Close()
	c.watch(sess)
	// Switching to it is using it.
	if err := c.sizer.Lead(); err != nil {
		c.mux.srv.Logf("udp: resize: %v", err)
	}
	c.mux.forget(old, c.addr)
	c.mux.remember(sess, c.addr, c.nonce, true)
	if err := c.mux.srv.Manager.SetClosed(string(sess.ID()), false); err != nil {
		c.mux.srv.Logf("udp: reopening %s: %v", sess.ID(), err)
	}
	if err := sess.MarkAttached(); err != nil {
		c.mux.srv.Logf("udp: recording attach: %v", err)
	}
	c.send(viewing(sess))
}

// screen returns what the client should see: its session's screen, and
// the menu over it, if it's open.
func (c *client) screen() vt.Screen {
	s := c.sess.Snapshot()
	if c.menu == nil {
		return s
	}
	rows, cols := c.rows, c.cols
	if rows <= 0 || cols <= 0 {
		rows, cols = s.Rows, s.Cols
	}
	return c.menu.Compose(s, rows, cols)
}

// resized records that the client's screen is now rows by cols.
func (c *client) resized(rows, cols int) {
	c.rows, c.cols = rows, cols
	if err := c.sizer.Want(rows, cols); err != nil {
		c.mux.srv.Logf("udp: resize: %v", err)
	}
	if c.menu != nil {
		c.menu.Resize(rows, cols)
		c.dirty = true
	}
}

func (c *client) handleResize(rs *proto.Resize) {
	c.skipAbandoned(rs.Seq, rs.Floor)
	if rs.Seq <= c.ackedSeq {
		c.sendAck()
		return
	}
	if rs.Seq != c.ackedSeq+1 {
		return
	}
	// The terminal's window changing size is the user using it.
	c.resized(rs.Rows, rs.Cols)
	if err := c.sizer.Lead(); err != nil {
		c.mux.srv.Logf("udp: resize: %v", err)
		return
	}
	c.ackedSeq = rs.Seq
	c.sendAck()
}

func (c *client) handleStateAck(a *proto.StateAck) {
	if a.State <= c.ackedNum {
		return
	}
	i := -1
	for j, s := range c.sent {
		if s.num == a.State {
			i = j
			break
		}
	}
	if i < 0 {
		return // forgotten or bogus; a later ack will catch us up
	}
	s := c.sent[i]
	if s.tries == 1 {
		// Karn's algorithm: only unambiguous samples update the RTT.
		c.sampleRTT(time.Since(s.sentAt))
	}
	c.ackedNum, c.acked = s.num, s.screen
	c.sent = c.sent[i+1:]
	if len(c.sent) > 0 && c.sent[len(c.sent)-1].from != c.ackedNum {
		// The outstanding update is diffed from an older state than the
		// client now has. Rebuild it against the new baseline, which is
		// both smaller and something the client can apply.
		c.dirty = true
	}
}

// sampleRTT folds one round-trip measurement into srtt and rttvar, per
// RFC 6298.
func (c *client) sampleRTT(r time.Duration) {
	if c.srtt == 0 {
		c.srtt, c.rttvar = r, r/2
		return
	}
	d := c.srtt - r
	if d < 0 {
		d = -d
	}
	c.rttvar = (3*c.rttvar + d) / 4
	c.srtt = (7*c.srtt + r) / 8
}

func (c *client) sendInterval() time.Duration {
	return min(max(c.srtt/2, minSendInterval), maxSendInterval)
}

// rto is the base retransmission timeout, before backoff.
func (c *client) rto() time.Duration {
	if c.srtt == 0 {
		return 200 * time.Millisecond // before the first RTT sample
	}
	return min(max(c.srtt+4*c.rttvar, minRTO), maxRTO)
}

// step sends whatever screen state is due at time now. It reports how long
// until it next needs to run, or false if nothing is pending.
func (c *client) step(now time.Time) (time.Duration, bool) {
	if c.gaveUp {
		return 0, false // quiet until the client speaks
	}
	if !c.trySince.IsZero() && now.Sub(c.trySince) >= c.mux.srv.GiveUpAfter {
		c.gaveUp = true
		return 0, false
	}
	if c.dirty && c.attempt > 0 && now.Before(c.nextTry) {
		// The client has stopped acknowledging. Hold new states to the
		// backoff schedule too, rather than streaming output into silence.
		return c.nextTry.Sub(now), true
	}
	if c.dirty {
		if wait := c.lastSend.Add(c.sendInterval()).Sub(now); wait > 0 {
			return wait, true
		}
		c.dirty = false
		cur := c.screen()
		if n := len(c.sent); n > 0 && c.sent[n-1].from == c.ackedNum && c.sent[n-1].screen.Equal(cur) {
			// The newest outstanding update still describes the screen;
			// leave it to the retransmission timer.
		} else if len(c.sent) > 0 || !c.acked.Equal(cur) {
			c.sendNewState(now, cur)
			return c.nextTry.Sub(now), true
		}
	}
	if len(c.sent) == 0 {
		return 0, false // client is caught up: nothing to send
	}
	if wait := c.nextTry.Sub(now); wait > 0 {
		return wait, true
	}
	// Resend the newest state's identical datagrams so the client can fill
	// in any fragments it is missing.
	latest := &c.sent[len(c.sent)-1]
	latest.tries++
	c.sendPkts(latest.pkts)
	if c.trySince.IsZero() {
		c.trySince = now
	}
	c.attempt++
	wait := Backoff(c.rto(), maxBackoffRTO, c.attempt)
	c.nextTry = now.Add(wait)
	return wait, true
}

// sendNewState sends cur to the client as a new state, diffed from the
// newest acknowledged state.
func (c *client) sendNewState(now time.Time, cur vt.Screen) {
	num := lastStateNum.Add(1)
	pkts, err := EncodeState(c.ackedNum, num, vt.Diff(c.acked, cur))
	if err != nil {
		c.mux.srv.Logf("udp: encode state: %v", err)
		return
	}
	c.sendPkts(pkts)
	c.sent = append(c.sent, sentState{
		num:    num,
		from:   c.ackedNum,
		screen: cur,
		pkts:   pkts,
		sentAt: now,
		tries:  1,
	})
	if len(c.sent) > maxSentStates {
		c.sent = c.sent[len(c.sent)-maxSentStates:]
	}
	c.lastSend = now
	if c.attempt > 0 {
		c.attempt++ // sent in place of a retransmission
	}
	c.nextTry = now.Add(Backoff(c.rto(), maxBackoffRTO, c.attempt))
	if c.trySince.IsZero() {
		c.trySince = now
	}
}

// addrFromNet converts a net.Addr returned by ReadFrom to a netip.AddrPort.
func addrFromNet(a net.Addr) (netip.AddrPort, bool) {
	switch v := a.(type) {
	case *net.UDPAddr:
		ap, ok := netip.AddrFromSlice(v.IP)
		if !ok {
			return netip.AddrPort{}, false
		}
		return netip.AddrPortFrom(ap.Unmap(), uint16(v.Port)), true
	}
	return netip.AddrPort{}, false
}

// Discard is a Logf compatible with Server.Logf that silences all messages.
// Useful for tests.
func Discard(string, ...any) {}

// LogPrintf adapts the standard log.Printf to Server.Logf.
var LogPrintf = log.Printf
