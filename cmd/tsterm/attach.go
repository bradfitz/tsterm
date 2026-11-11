package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/bradfitz/tsterm/pkg/clientui"
	"github.com/bradfitz/tsterm/pkg/proto"
	"github.com/bradfitz/tsterm/pkg/transport/udp"
)

// The client sends nothing while idle. When it has input to deliver, or
// needs to know whether the server is there, it retransmits with randomized
// exponential backoff, and gives up after udp.GiveUpAfter.
const (
	// Retransmission backoff bounds.
	retryRTO    = 250 * time.Millisecond
	maxRetryRTO = 10 * time.Second

	// connectTimeout bounds the initial attach.
	connectTimeout = 15 * time.Second

	// staleAfter is how long the client may go unanswered while trying
	// before it shows a status line.
	staleAfter = 2 * time.Second

	// While retrying, timers run every activeTick, mostly to keep the
	// status line current. While idle they run every idleTick, only to
	// notice waking from sleep. Neither sends anything by itself.
	//
	// TODO(bradfitz): Get wake events from the OS instead of polling, so
	// an idle client never wakes the CPU: IOKit power notifications
	// (IORegisterForSystemPower) on macOS, and logind's PrepareForSleep
	// D-Bus signal on Linux. Keep polling as the fallback elsewhere.
	activeTick = 100 * time.Millisecond
	idleTick   = 5 * time.Second

	// minSleep is the least time suspended that counts as waking from
	// sleep, which makes the client check in with the server.
	minSleep = 3 * time.Second
)

// attach runs the interactive client against the given session. It blocks
// until the session ends, the user disconnects, or ctx is canceled.
func attach(ctx context.Context, ctrl *control, info proto.SessionInfo) error {
	rows, cols, ok := windowSize()
	if !ok {
		rows, cols = info.Rows, info.Cols
	}
	if rows == 0 {
		rows = 24
	}
	if cols == 0 {
		cols = 80
	}

	udpAddr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(ctrl.host(), fmt.Sprintf("%d", info.UDPPort)))
	if err != nil {
		return fmt.Errorf("resolve udp: %w", err)
	}
	conn, err := net.DialUDP("udp", nil, udpAddr)
	if err != nil {
		return fmt.Errorf("dial udp: %w", err)
	}
	defer conn.Close()

	stdinFd := int(os.Stdin.Fd())
	restore, err := makeRaw(stdinFd)
	if err != nil {
		return fmt.Errorf("raw mode: %w", err)
	}
	defer restore()

	renderer := clientui.New(os.Stdout)
	defer renderer.Close()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Closing the socket is what stops readLoop, which blocks in Read with
	// no deadline so that an idle client is not woken just to poll.
	context.AfterFunc(ctx, func() { conn.Close() })

	now := time.Now()
	state := &attachState{
		conn:     conn,
		renderer: renderer,
		info:     info,
		nonce:    rand.Text(),
		cancel:   cancel,
		rows:     rows,
		cols:     cols,
		nextSeq:  1,
		lastWall: now.Round(0),
		lastMono: now,
	}
	state.mu.Lock()
	state.probeLocked(now, now.Add(connectTimeout))
	state.mu.Unlock()

	var wg sync.WaitGroup
	wg.Add(3)

	// Reader: incoming UDP frames.
	go func() {
		defer wg.Done()
		defer cancel()
		state.readLoop(ctx)
	}()

	// Writer: stdin → input frames, plus timers.
	go func() {
		defer wg.Done()
		defer cancel()
		state.writeLoop(ctx)
	}()

	// SIGWINCH: window-size changes.
	go func() {
		defer wg.Done()
		state.winchLoop(ctx)
	}()

	wg.Wait()
	if errors.Is(state.exitErr, errDisconnected) {
		which := info.ID
		if info.Name != "" {
			which = info.Name
		}
		return fmt.Errorf("%w from %s; reattach with: tsterm %s %s", errDisconnected, info.ID, ctrl.host(), which)
	}
	return state.exitErr
}

// pendingInput is an Input or Resize awaiting the server's ack.
type pendingInput struct {
	seq       uint64
	keys      []byte // for Input
	resize    bool
	rows      int // for Resize
	cols      int // for Resize
	firstSent time.Time
}

type attachState struct {
	conn     *net.UDPConn
	info     proto.SessionInfo
	nonce    string
	cancel   context.CancelFunc
	renderer *clientui.Renderer // guarded by mu

	mu sync.Mutex

	// Window size, for Hello.
	rows, cols int

	// Input stream. Every Seq at or below floor is acked or abandoned.
	nextSeq    uint64
	pending    []*pendingInput // unacked, in Seq order
	highAcked  uint64
	floor      uint64
	dropped    int  // input bytes abandoned since last contact
	needResize bool // an abandoned Resize must be redone

	// Screen-state sync.
	cur   uint64 // newest applied state; 0 means none yet
	reasm udp.Reassembler

	// Retransmission. While trying, pending input is resent (along with
	// Hello, in case the server has forgotten us) on a backoff schedule.
	// Probing means Hello must be answered even if nothing is pending: on
	// first attach, and after waking from sleep.
	trying     bool
	tryStart   time.Time
	attempt    int
	nextTry    time.Time
	probing    bool
	probeUntil time.Time

	// Liveness.
	connected bool      // the server has answered at least once
	lastRecv  time.Time // last time the server was heard from
	lostSince time.Time // when we gave up on the server; zero if not

	// Sleep detection: wall and monotonic time at the last timer run.
	lastWall, lastMono time.Time

	esc escaper // used only by writeLoop

	// badUpdate, if non-empty, describes why the latest screen update
	// could not be used. It stays on the status line until one can.
	badUpdate string

	exitErr error
	closed  bool
}

func (s *attachState) readLoop(ctx context.Context) {
	buf := make([]byte, 64*1024)
	for {
		n, err := s.conn.Read(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
				return
			}
			// ECONNREFUSED, from ICMP while the daemon is unreachable,
			// is expected. Keep listening.
			continue
		}
		var env proto.Envelope
		if err := proto.Decode(buf[:n], &env); err != nil {
			continue
		}
		s.handle(env)
	}
}

func (s *attachState) handle(env proto.Envelope) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.onContactLocked(time.Now())
	switch {
	case env.State != nil:
		s.handleStateLocked(env.State)
	case env.Ack != nil:
		s.handleAckLocked(env.Ack.Highest)
	case env.Ended != nil:
		var err error
		if env.Ended.Status == proto.EndedNoSession {
			err = errors.New("session no longer exists on server")
		}
		s.exitLocked(err)
	}
}

// onContactLocked records that the server answered.
func (s *attachState) onContactLocked(now time.Time) {
	s.lastRecv = now
	s.connected = true
	s.lostSince = time.Time{}
	s.dropped = 0
	s.probing = false
	s.renderer.SetStatus(s.badUpdate)
	if s.trying {
		// Retry anything still pending promptly, with a fresh budget.
		s.tryStart = now
		s.attempt = 0
		s.nextTry = now.Add(udp.Backoff(retryRTO, maxRetryRTO, 0))
	}
	if s.needResize {
		s.needResize = false
		s.sendSeqLocked(now, &pendingInput{resize: true, rows: s.rows, cols: s.cols})
	}
	s.maybeStopTryingLocked()
}

func (s *attachState) handleStateLocked(f *proto.State) {
	if f.To <= s.cur {
		// A stale or duplicate update. Re-ack so the server learns
		// what we have, in case our earlier ack was lost.
		s.sendStateAckLocked()
		return
	}
	d, ok, err := s.reasm.Add(f)
	if err != nil {
		// Show it: otherwise the screen just stops updating.
		s.badUpdate = fmt.Sprintf("tsterm: can't use screen update from server: %v", err)
		s.renderer.SetStatus(s.badUpdate)
		return
	}
	if !ok {
		return
	}
	if f.From != 0 && f.From != s.cur {
		// Diffed from a state we have moved past. The ack tells the
		// server to rebuild the update from our current state.
		s.sendStateAckLocked()
		return
	}
	s.renderer.Apply(proto.Delta{D: d})
	s.badUpdate = ""
	s.renderer.SetStatus("")
	s.cur = f.To
	s.reasm.Forget(s.cur)
	s.sendStateAckLocked()
}

func (s *attachState) handleAckLocked(highest uint64) {
	if highest > s.highAcked {
		s.highAcked = highest
		s.floor = max(s.floor, highest)
	}
	i := 0
	for i < len(s.pending) && s.pending[i].seq <= highest {
		i++
	}
	s.pending = s.pending[i:]
	s.maybeStopTryingLocked()
}

// maybeStopTryingLocked ends the retry schedule once there is nothing left
// to deliver or confirm.
func (s *attachState) maybeStopTryingLocked() {
	if s.trying && len(s.pending) == 0 && !s.probing {
		s.trying = false
	}
}

func (s *attachState) writeLoop(ctx context.Context) {
	in := make(chan []byte, 64)
	go func() {
		defer close(in)
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				select {
				case in <- append([]byte(nil), buf[:n]...):
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	timer := time.NewTimer(activeTick)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case chunk, ok := <-in:
			if !ok {
				return
			}
			keys, disconnect := s.esc.filter(chunk)
			if len(keys) > 0 {
				s.sendInput(keys)
			}
			if disconnect {
				s.disconnect()
				return
			}
			timer.Reset(activeTick)
		case <-timer.C:
			timer.Reset(s.onTimer(time.Now()))
		}
	}
}

// onTimer runs the client's timers: retransmission, giving up, the status
// line, and sleep detection. It returns when it next needs to run.
func (s *attachState) onTimer(now time.Time) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return idleTick
	}

	// The monotonic clock stops while the machine is suspended and the
	// wall clock does not, so a gap between them means we slept. The
	// server may have output for us that it gave up delivering, so ask.
	// TODO(bradfitz): Replace this with OS wake events; see idleTick.
	slept := now.Round(0).Sub(s.lastWall) - now.Sub(s.lastMono)
	s.lastWall, s.lastMono = now.Round(0), now
	if slept >= minSleep && s.connected {
		s.probeLocked(now, now.Add(udp.GiveUpAfter))
	}

	// Drop input that could not be delivered in time. A user who typed
	// into a dead connection does not expect it to replay hours later.
	for len(s.pending) > 0 && now.Sub(s.pending[0].firstSent) >= udp.GiveUpAfter {
		p := s.pending[0]
		s.pending = s.pending[1:]
		s.floor = p.seq
		if p.resize {
			s.needResize = true
		} else {
			s.dropped += len(p.keys)
		}
	}
	if s.probing && !now.Before(s.probeUntil) {
		s.probing = false
		if !s.connected {
			s.exitLocked(fmt.Errorf("no response from %s", s.conn.RemoteAddr()))
			return idleTick
		}
	}
	if s.trying && len(s.pending) == 0 && !s.probing && s.lastRecv.Before(s.tryStart) {
		// Gave up without hearing back. Go quiet until there is
		// something new to send.
		s.lostSince = now
	}
	s.maybeStopTryingLocked()

	if s.trying && !now.Before(s.nextTry) {
		for _, p := range s.pending {
			s.sendLocked(s.frameLocked(p))
		}
		if s.probing || s.attempt > 0 {
			// The server may have forgotten us; Hello reattaches.
			s.sendHelloLocked()
		}
		s.attempt++
		s.nextTry = now.Add(udp.Backoff(retryRTO, maxRetryRTO, s.attempt))
	}

	switch {
	case s.trying && s.connected && now.Sub(s.lastRecv) > staleAfter && now.Sub(s.tryStart) > staleAfter:
		s.renderer.SetStatus(fmt.Sprintf("tsterm: no contact with server for %v; retrying...",
			now.Sub(s.lastRecv).Round(time.Second)))
	case !s.trying && !s.lostSince.IsZero():
		msg := "tsterm: can't reach server since " + s.lostSince.Format(time.TimeOnly)
		if s.dropped > 0 {
			msg += fmt.Sprintf("; dropped %d bytes of input", s.dropped)
		}
		s.renderer.SetStatus(msg + "; typing retries")
	}

	if s.trying {
		return activeTick
	}
	return idleTick
}

func (s *attachState) winchLoop(ctx context.Context) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	defer signal.Stop(ch)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ch:
			rows, cols, ok := windowSize()
			if !ok {
				continue
			}
			s.sendResize(rows, cols)
		}
	}
}

// startTryingLocked starts the retry schedule if it is not running.
func (s *attachState) startTryingLocked(now time.Time) {
	if s.trying {
		return
	}
	s.trying = true
	s.tryStart = now
	s.attempt = 0
	s.nextTry = now.Add(udp.Backoff(retryRTO, maxRetryRTO, 0))
}

// probeLocked sends Hello and keeps resending it until the server answers
// or until deadline.
func (s *attachState) probeLocked(now, deadline time.Time) {
	s.probing = true
	s.probeUntil = deadline
	s.sendHelloLocked()
	s.startTryingLocked(now)
}

func (s *attachState) sendHelloLocked() {
	s.sendLocked(proto.Envelope{Hello: &proto.Hello{
		SessionID:  s.info.ID,
		Token:      s.info.Token,
		Rows:       s.rows,
		Cols:       s.cols,
		Nonce:      []byte(s.nonce),
		InputAcked: s.floor,
	}})
}

func (s *attachState) sendStateAckLocked() {
	s.sendLocked(proto.Envelope{StateAck: &proto.StateAck{State: s.cur}})
}

func (s *attachState) sendInput(keys []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if s.needResize {
		s.needResize = false
		s.sendSeqLocked(now, &pendingInput{resize: true, rows: s.rows, cols: s.cols})
	}
	s.sendSeqLocked(now, &pendingInput{keys: keys})
}

func (s *attachState) sendResize(rows, cols int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows, s.cols = rows, cols
	s.needResize = false
	s.sendSeqLocked(time.Now(), &pendingInput{resize: true, rows: rows, cols: cols})
}

// sendSeqLocked assigns p the next Seq, sends it, and holds it for
// retransmission until acked or abandoned.
func (s *attachState) sendSeqLocked(now time.Time, p *pendingInput) {
	p.seq = s.nextSeq
	s.nextSeq++
	p.firstSent = now
	s.pending = append(s.pending, p)
	s.sendLocked(s.frameLocked(p))
	s.startTryingLocked(now)
}

// frameLocked builds the wire frame for p. Floor is filled in at send time
// so that retransmissions after abandoning input carry the current value.
func (s *attachState) frameLocked(p *pendingInput) proto.Envelope {
	if p.resize {
		return proto.Envelope{Resize: &proto.Resize{Seq: p.seq, Rows: p.rows, Cols: p.cols, Floor: s.floor}}
	}
	return proto.Envelope{Input: &proto.Input{Seq: p.seq, Keys: p.keys, Floor: s.floor}}
}

// sendLocked sends env, ignoring errors: while the network is down, writes
// can fail (for example, with ECONNREFUSED), and the timers retry anyway.
func (s *attachState) sendLocked(env proto.Envelope) {
	b, err := proto.Encode(env)
	if err != nil {
		return
	}
	_, _ = s.conn.Write(b)
}

// errDisconnected is returned by attach when the user disconnects with the
// escape key.
var errDisconnected = errors.New("disconnected")

// disconnect ends the attach at the user's request, telling the server so
// that it forgets this client now. The session keeps running.
func (s *attachState) disconnect() {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Best effort, and twice in case one is lost; there is no retry.
	for range 2 {
		s.sendLocked(proto.Envelope{Detach: &proto.Detach{}})
	}
	s.exitLocked(errDisconnected)
}

// exitLocked ends the attach with err (nil for a clean exit). Only the
// first call has any effect.
func (s *attachState) exitLocked(err error) {
	if s.closed {
		return
	}
	s.closed = true
	s.exitErr = err
	s.cancel()
}

func windowSize() (rows, cols int, ok bool) {
	fd := int(os.Stdout.Fd())
	if !term.IsTerminal(fd) {
		fd = int(os.Stdin.Fd())
		if !term.IsTerminal(fd) {
			return 0, 0, false
		}
	}
	w, h, err := term.GetSize(fd)
	if err != nil {
		return 0, 0, false
	}
	return h, w, true
}

func makeRaw(fd int) (func(), error) {
	if !term.IsTerminal(fd) {
		return func() {}, nil
	}
	old, err := term.MakeRaw(fd)
	if err != nil {
		return nil, err
	}
	return func() {
		_ = term.Restore(fd, old)
		// Print a trailing newline so the shell prompt that follows
		// lands on its own line.
		_, _ = io.WriteString(os.Stdout, "\r\n")
	}, nil
}
