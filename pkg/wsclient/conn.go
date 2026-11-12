package wsclient

import (
	"context"
	"encoding/json/v2"
	"net"
	"net/url"
	"strings"
	"sync"

	"github.com/coder/websocket"

	"github.com/bradfitz/tsterm/pkg/proto"
)

// A Conn is a connection to a session's WebSocket stream: a Snapshot, then
// Deltas and other frames, which Recv returns. A writable Conn may also
// send input and size the session. Conns acknowledge Deltas only when told
// to (Ack), once they're shown, so that the server sends no faster than
// the client can show them.
//
// Its methods that send queue what they send, in order, and return without
// waiting for the network, so that a UI may call them as the user types.
type Conn struct {
	ws       *websocket.Conn
	writable bool

	out       chan []byte // to send, in order
	done      chan struct{}
	closeOnce sync.Once
}

// sendQueue bounds the messages queued to send. A sender blocks while it's
// full, as when the network has stalled.
const sendQueue = 1024

// Dial connects to the stream of the session id, live or ended.
func (c *Client) Dial(ctx context.Context, id string, writable bool) (*Conn, error) {
	u := c.Base + sessionPath(id) + "/ws?acks=1"
	if writable {
		u += "&write=1"
	}
	if after, ok := strings.CutPrefix(u, "http"); ok {
		u = "ws" + after // http to ws, https to wss
	}
	if _, err := url.Parse(u); err != nil {
		return nil, err
	}
	ws, _, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPClient: c.HTTP})
	if err != nil {
		return nil, err
	}
	ws.SetReadLimit(64 << 20)
	c2 := &Conn{ws: ws, writable: writable, out: make(chan []byte, sendQueue), done: make(chan struct{})}
	go c2.writer()
	return c2, nil
}

// writer sends what's queued, in order, until the Conn is closed or a
// send fails, which closes it.
func (c *Conn) writer() {
	for {
		select {
		case <-c.done:
			return
		case b := <-c.out:
			if err := c.ws.Write(context.Background(), websocket.MessageText, b); err != nil {
				c.Close()
				return
			}
		}
	}
}

// Writable reports whether the Conn may drive the session.
func (c *Conn) Writable() bool { return c.writable }

// Recv returns the next frame from the server.
func (c *Conn) Recv(ctx context.Context) (proto.Envelope, error) {
	for {
		typ, b, err := c.ws.Read(ctx)
		if err != nil {
			return proto.Envelope{}, err
		}
		if typ != websocket.MessageBinary {
			continue
		}
		var env proto.Envelope
		if err := proto.Decode(b, &env); err != nil {
			return proto.Envelope{}, err
		}
		return env, nil
	}
}

// send queues a JSON message to send.
func (c *Conn) send(m any) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	select {
	case c.out <- b:
		return nil
	case <-c.done:
		return net.ErrClosed
	}
}

// Input types s into the session.
func (c *Conn) Input(s string) error {
	if s == "" {
		return nil
	}
	return c.send(map[string]string{"input": s})
}

// Resize says what size this client wants the session, and if lead, that
// the user is using it now, so that its size applies (see session.Sizer).
// Zero rows and cols want no size.
func (c *Conn) Resize(rows, cols int, lead bool) error {
	type size struct {
		Rows int `json:"rows"`
		Cols int `json:"cols"`
	}
	return c.send(struct {
		Resize size `json:"resize"`
		Lead   bool `json:"lead,omitzero"`
	}{size{rows, cols}, lead})
}

// Lead says that the user is using this client, so that its size applies.
func (c *Conn) Lead() error { return c.send(map[string]bool{"lead": true}) }

// History asks for up to count lines of history from line from, which
// come in a History frame.
func (c *Conn) History(from uint64, count int) error {
	type req struct {
		From  uint64 `json:"from"`
		Count int    `json:"count"`
	}
	return c.send(map[string]req{"history": {from, count}})
}

// Ack acknowledges the Delta seq, once it's shown.
func (c *Conn) Ack(seq uint64) error {
	if seq == 0 {
		return nil
	}
	return c.send(map[string]uint64{"ack": seq})
}

// Close closes the connection. Messages still queued aren't sent.
func (c *Conn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		close(c.done)
		err = c.ws.Close(websocket.StatusNormalClosure, "")
	})
	return err
}
