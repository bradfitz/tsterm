package tcp

import (
	"bufio"
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"sync"
	"time"
)

// sniffListener serves TLS and cleartext connections on one port. A TLS
// connection starts with a handshake record, whose first byte is 0x16; a
// cleartext HTTP request starts with its method, in ASCII. Accept returns
// TLS connections as *tls.Conn, which net/http recognizes as HTTPS.
type sniffListener struct {
	net.Listener
	tls *tls.Config

	conns     chan net.Conn
	done      chan struct{}
	closeOnce sync.Once
	err       error // from the underlying Accept, if it closed done
}

// sniffTimeout bounds how long a new connection may take to send its first
// byte.
const sniffTimeout = 10 * time.Second

func newSniffListener(ln net.Listener, config *tls.Config) *sniffListener {
	l := &sniffListener{
		Listener: ln,
		tls:      config,
		conns:    make(chan net.Conn),
		done:     make(chan struct{}),
	}
	go l.acceptLoop()
	return l
}

func (l *sniffListener) acceptLoop() {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			// Record err only if Close hasn't already ended things.
			l.closeOnce.Do(func() {
				l.err = err
				close(l.done)
			})
			return
		}
		// Sniff concurrently, so that a slow client doesn't hold up others.
		go l.sniff(c)
	}
}

func (l *sniffListener) sniff(c net.Conn) {
	c.SetReadDeadline(time.Now().Add(sniffTimeout))
	br := bufio.NewReader(c)
	first, err := br.Peek(1)
	c.SetReadDeadline(time.Time{})
	if err != nil {
		c.Close()
		return
	}
	var out net.Conn = &peekedConn{Conn: c, r: br}
	if first[0] == 0x16 {
		out = tls.Server(out, l.tls)
	}
	select {
	case l.conns <- out:
	case <-l.done:
		out.Close()
	}
}

func (l *sniffListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		if l.err != nil {
			return nil, l.err
		}
		return nil, net.ErrClosed
	}
}

func (l *sniffListener) Close() error {
	l.closeOnce.Do(func() { close(l.done) })
	return l.Listener.Close()
}

// peekedConn is a net.Conn whose first bytes were read into r.
type peekedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *peekedConn) Read(b []byte) (int, error) { return c.r.Read(b) }

// localPort returns the port a request arrived on.
func localPort(ctx context.Context) string {
	if a, ok := ctx.Value(http.LocalAddrContextKey).(net.Addr); ok {
		if _, port, err := net.SplitHostPort(a.String()); err == nil {
			return port
		}
	}
	return ""
}
