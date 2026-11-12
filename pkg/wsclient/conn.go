package wsclient

import (
	"context"
	"encoding/json/v2"
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
type Conn struct {
	ws       *websocket.Conn
	writable bool

	mu sync.Mutex // serializes writes
}

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
	return &Conn{ws: ws, writable: writable}, nil
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

// send sends a JSON message.
func (c *Conn) send(m any) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ws.Write(context.Background(), websocket.MessageText, b)
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

// Close closes the connection.
func (c *Conn) Close() error { return c.ws.Close(websocket.StatusNormalClosure, "") }
