package main

import (
	"bytes"
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/bradfitz/tsterm/pkg/clientui"
	"github.com/bradfitz/tsterm/pkg/proto"
	"github.com/bradfitz/tsterm/pkg/transport/udp"
)

// testAttach returns an attachState connected (as far as it knows) to a fake
// server socket that never answers, and a function that returns every
// envelope the fake server has received so far.
func testAttach(t *testing.T) (*attachState, *bytes.Buffer, func() []proto.Envelope) {
	t.Helper()
	srv, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	conn, err := net.DialUDP("udp", nil, srv.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	_, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	var out bytes.Buffer
	now := time.Now()
	s := &attachState{
		conn:      conn,
		renderer:  clientui.New(&out),
		info:      proto.SessionInfo{ID: "sess", Token: []byte("tok")},
		nonce:     "nonce",
		cancel:    cancel,
		rows:      24,
		cols:      80,
		nextSeq:   1,
		connected: true,
		lastRecv:  now,
		lastWall:  now.Round(0),
		lastMono:  now,
	}
	recvd := func() []proto.Envelope {
		var envs []proto.Envelope
		buf := make([]byte, 64*1024)
		for {
			_ = srv.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
			n, err := srv.Read(buf)
			if err != nil {
				return envs
			}
			var env proto.Envelope
			if err := proto.Decode(buf[:n], &env); err != nil {
				t.Fatal(err)
			}
			envs = append(envs, env)
		}
	}
	return s, &out, recvd
}

// runTimers calls onTimer at every activeTick over d, starting at start,
// without simulating any sleep.
func runTimers(s *attachState, start time.Time, d time.Duration) {
	for t := start; t.Before(start.Add(d)); t = t.Add(activeTick) {
		s.onTimer(t)
	}
}

func TestIdleClientSendsNothing(t *testing.T) {
	s, _, recvd := testAttach(t)
	runTimers(s, time.Now(), 10*time.Minute)
	if envs := recvd(); len(envs) != 0 {
		t.Errorf("idle client sent %d packets, want 0; first: %+v", len(envs), envs[0])
	}
}

func TestUndeliveredInputBacksOffAndIsDropped(t *testing.T) {
	s, out, recvd := testAttach(t)
	start := time.Now()
	s.sendInput([]byte("lost"))
	runTimers(s, start, udp.GiveUpAfter+10*time.Second)

	var inputs, hellos int
	for _, env := range recvd() {
		switch {
		case env.Input != nil:
			inputs++
		case env.Hello != nil:
			hellos++
		}
	}
	// 250ms doubling to a 10s cap covers 60s in about 10 tries.
	if inputs < 4 || inputs > 16 {
		t.Errorf("sent the input %d times in %v, want a handful with backoff", inputs, udp.GiveUpAfter)
	}
	if hellos == 0 {
		t.Error("never sent Hello to reattach while retrying")
	}
	if s.trying || len(s.pending) != 0 {
		t.Errorf("still trying after GiveUpAfter: trying=%v pending=%d", s.trying, len(s.pending))
	}
	if s.floor != 1 || s.dropped != 4 {
		t.Errorf("floor=%d dropped=%d, want 1 and 4", s.floor, s.dropped)
	}
	if !strings.Contains(out.String(), "can't reach server") {
		t.Errorf("no give-up status line in output %q", out.String())
	}

	// Once given up, the client is quiet again.
	runTimers(s, start.Add(udp.GiveUpAfter+10*time.Second), 5*time.Minute)
	if envs := recvd(); len(envs) != 0 {
		t.Errorf("sent %d packets after giving up", len(envs))
	}

	// New typing tries again, telling the server to skip the dropped Seq.
	s.sendInput([]byte("x"))
	envs := recvd()
	if len(envs) != 1 || envs[0].Input == nil {
		t.Fatalf("after new input, sent %+v", envs)
	}
	if in := envs[0].Input; in.Seq != 2 || in.Floor != 1 {
		t.Errorf("new input Seq=%d Floor=%d, want 2 and 1", in.Seq, in.Floor)
	}
}

func TestWakeFromSleepSendsHello(t *testing.T) {
	s, _, recvd := testAttach(t)
	now := time.Now()
	s.onTimer(now)
	// Simulate ten seconds suspended: wall time moved, monotonic did not.
	s.lastWall = s.lastWall.Add(-10 * time.Second)
	s.onTimer(now)
	envs := recvd()
	if len(envs) != 1 || envs[0].Hello == nil {
		t.Fatalf("after waking, sent %+v; want one Hello", envs)
	}
	if !s.probing {
		t.Error("not waiting for the server's answer after waking")
	}

	// The server's answer ends the probe, and the client goes quiet.
	s.handle(proto.Envelope{Ack: &proto.Ack{}})
	runTimers(s, now, time.Minute)
	if envs := recvd(); len(envs) != 0 {
		t.Errorf("sent %d packets after the server answered", len(envs))
	}
}

func TestAbandonedResizeRedoneOnContact(t *testing.T) {
	s, _, recvd := testAttach(t)
	start := time.Now()
	s.sendResize(30, 100)
	runTimers(s, start, udp.GiveUpAfter+time.Second)
	recvd()
	if !s.needResize {
		t.Fatal("abandoned resize not remembered")
	}
	s.handle(proto.Envelope{Ack: &proto.Ack{}})
	envs := recvd()
	if len(envs) != 1 || envs[0].Resize == nil {
		t.Fatalf("on contact, sent %+v; want one Resize", envs)
	}
	if rs := envs[0].Resize; rs.Rows != 30 || rs.Cols != 100 || rs.Floor != 1 {
		t.Errorf("resize = %+v, want 30x100 with Floor 1", rs)
	}
}
