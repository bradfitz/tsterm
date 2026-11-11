package shim

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"sync"
	"syscall"
	"time"

	"github.com/bradfitz/tsterm/pkg/shimproto"
)

// Conn is a controller's connection to a shim.
type Conn struct {
	c net.Conn
	r *bufio.Reader

	// Info is what the shim reported when the connection was made.
	Info shimproto.Info

	wmu sync.Mutex // serializes writes
}

// Dial connects to the shim listening on the unix socket at path and reads
// its Info. It does not attach.
func Dial(ctx context.Context, path string) (*Conn, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, err
	}
	sc := &Conn{c: c, r: bufio.NewReader(c)}
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetReadDeadline(dl)
	}
	typ, payload, err := shimproto.ReadFrame(sc.r)
	_ = c.SetReadDeadline(time.Time{})
	if err == nil && typ != shimproto.TypeInfo {
		err = fmt.Errorf("shim sent frame type %d before Info", typ)
	}
	if err == nil {
		err = shimproto.Decode(payload, &sc.Info)
	}
	if err == nil && sc.Info.Version < 1 {
		err = fmt.Errorf("shim protocol version %d unsupported", sc.Info.Version)
	}
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("shim %s: %w", path, err)
	}
	return sc, nil
}

// DialWait is like Dial but retries until the shim's socket accepts a
// connection or ctx is done. It is for connecting to a newly launched shim.
func DialWait(ctx context.Context, path string) (*Conn, error) {
	for {
		c, err := Dial(ctx, path)
		if err == nil {
			return c, nil
		}
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// Attach makes this connection the shim's controller. It returns the shim's
// latest checkpoint; Read then returns the output that follows it.
func (c *Conn) Attach() (shimproto.Checkpoint, error) {
	var ckpt shimproto.Checkpoint
	if err := c.write(shimproto.TypeAttach, nil); err != nil {
		return ckpt, err
	}
	typ, payload, err := shimproto.ReadFrame(c.r)
	if err != nil {
		return ckpt, err
	}
	if typ != shimproto.TypeCheckpoint {
		return ckpt, fmt.Errorf("shim answered Attach with frame type %d", typ)
	}
	err = shimproto.Decode(payload, &ckpt)
	return ckpt, err
}

// Read returns the next chunk of output, or the child's exit once all
// output has been read. Exactly one of output and exited is non-nil when
// err is nil.
func (c *Conn) Read() (output []byte, exited *shimproto.Exited, err error) {
	for {
		typ, payload, err := shimproto.ReadFrame(c.r)
		if err != nil {
			return nil, nil, err
		}
		switch typ {
		case shimproto.TypeOutput:
			return payload, nil, nil
		case shimproto.TypeExited:
			var e shimproto.Exited
			if err := shimproto.Decode(payload, &e); err != nil {
				return nil, nil, err
			}
			return nil, &e, nil
		}
		// Ignore unknown frame types from newer shims.
	}
}

// Input writes b to the PTY.
func (c *Conn) Input(b []byte) error { return c.write(shimproto.TypeInput, b) }

// Resize sets the PTY size.
func (c *Conn) Resize(rows, cols int) error {
	return c.writeJSON(shimproto.TypeResize, shimproto.Resize{Rows: rows, Cols: cols})
}

// Signal delivers sig to the child.
func (c *Conn) Signal(sig syscall.Signal) error {
	return c.writeJSON(shimproto.TypeSignal, shimproto.Signal{Signal: int(sig)})
}

// SetCheckpoint tells the shim that blob captures the terminal state at
// output offset, so it may discard earlier output.
func (c *Conn) SetCheckpoint(offset uint64, blob []byte) error {
	return c.writeJSON(shimproto.TypeSetCheckpoint, shimproto.Checkpoint{Offset: offset, Blob: blob})
}

// SetMeta replaces the shim's opaque metadata.
func (c *Conn) SetMeta(meta []byte) error {
	return c.writeJSON(shimproto.TypeSetMeta, shimproto.Meta{Meta: meta})
}

// Release lets the shim exit once its child has exited.
func (c *Conn) Release() error { return c.write(shimproto.TypeRelease, nil) }

// Close closes the connection. The shim and its child keep running.
func (c *Conn) Close() error { return c.c.Close() }

// CloseWrite closes the sending side of the connection. The shim handles
// everything sent before it, then closes the connection, so a subsequent
// Read returns an error once the shim is done.
func (c *Conn) CloseWrite() error {
	if uc, ok := c.c.(*net.UnixConn); ok {
		c.wmu.Lock()
		defer c.wmu.Unlock()
		return uc.CloseWrite()
	}
	return c.c.Close()
}

func (c *Conn) write(t shimproto.Type, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return shimproto.WriteFrame(c.c, t, payload)
}

func (c *Conn) writeJSON(t shimproto.Type, v any) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return shimproto.WriteJSON(c.c, t, v)
}
