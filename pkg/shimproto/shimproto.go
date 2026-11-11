// Package shimproto defines the local protocol between tstermd and
// tsterm-shim.
//
// Each tsterm-shim process owns one PTY and its child process, so that
// tstermd can be restarted or upgraded without killing anyone's shell. A
// shim may outlive many tstermd versions, so this protocol must stay
// stable: change it only by adding message types and JSON fields, and keep
// the shim itself small and rarely changed. The shim never interprets
// terminal output; the terminal model lives in tstermd.
//
// A shim listens on a unix socket. Frames in both directions are a one-byte
// Type, a four-byte big-endian payload length, and the payload. Output and
// Input payloads are raw bytes; other payloads are JSON objects.
//
// On each new connection the shim first sends Info. The connecting side may
// then send Attach to become the shim's controller, replacing any previous
// controller. The shim answers Attach with a Checkpoint and then streams
// Output, starting at the checkpoint's offset, so that the controller can
// rebuild the terminal state by loading the checkpoint and replaying the
// output that follows it. Output offsets are implicit: each Output frame
// continues where the last one ended.
//
// The shim retains all output since the latest checkpoint, and stops
// reading from the PTY (pausing the child) when that exceeds its buffer
// limit. A controller should therefore send SetCheckpoint regularly.
package shimproto

import (
	"bufio"
	"encoding/binary"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"time"
)

// Version is the protocol version a shim reports in Info.
const Version = 1

// Type identifies a frame.
type Type uint8

// Frames sent by the shim.
const (
	TypeInfo       Type = 1 // JSON Info, sent first on every connection
	TypeCheckpoint Type = 2 // JSON Checkpoint, the reply to Attach
	TypeOutput     Type = 3 // raw PTY output
	TypeExited     Type = 4 // JSON Exited, after the child's final output
)

// Frames sent by the controller.
const (
	TypeAttach        Type = 16 // empty; become the controller
	TypeInput         Type = 17 // raw bytes to write to the PTY
	TypeResize        Type = 18 // JSON Resize
	TypeSignal        Type = 19 // JSON Signal, delivered to the child
	TypeSetCheckpoint Type = 20 // JSON Checkpoint
	TypeSetMeta       Type = 21 // JSON Meta
	TypeRelease       Type = 22 // empty; the shim may exit once the child has
)

// MaxPayload bounds the payload size of a frame.
const MaxPayload = 16 << 20

// Info describes a shim and its child.
type Info struct {
	Version    int       `json:"version"`
	ID         string    `json:"id"`
	Pid        int       `json:"pid"` // the child's
	Rows       int       `json:"rows"`
	Cols       int       `json:"cols"`
	Started    time.Time `json:"started"`
	Exited     bool      `json:"exited,omitzero"`
	ExitStatus string    `json:"exitStatus,omitzero"`

	// Meta is opaque to the shim. tstermd stores its own per-session
	// state here (name, capability token, and so on) so that a restarted
	// tstermd can recover it.
	Meta []byte `json:"meta,omitzero"`
}

// Checkpoint is a snapshot of terminal state at output offset Offset. The
// Blob is opaque to the shim; tstermd writes it as terminal output that
// reproduces the screen, so any terminal emulator can load it.
type Checkpoint struct {
	Offset uint64 `json:"offset"`
	Blob   []byte `json:"blob,omitzero"`
}

// Resize sets the PTY window size.
type Resize struct {
	Rows int `json:"rows"`
	Cols int `json:"cols"`
}

// Signal is a signal number to deliver to the child.
type Signal struct {
	Signal int `json:"signal"`
}

// Meta replaces the shim's opaque metadata.
type Meta struct {
	Meta []byte `json:"meta,omitzero"`
}

// Exited reports that the child has exited and all of its output has been
// sent. Status describes how it exited, such as "exit status 1".
type Exited struct {
	Status string `json:"status,omitzero"`
}

// SpawnConfig is how tstermd tells a new shim what to run. tstermd writes it
// as JSON to a file and passes the file's path to tsterm-shim, which reads
// and removes it.
type SpawnConfig struct {
	ID      string   `json:"id"`
	Socket  string   `json:"socket"` // unix socket path to listen on
	Command string   `json:"command,omitzero"`
	Args    []string `json:"args,omitzero"`
	Env     []string `json:"env,omitzero"`
	Dir     string   `json:"dir,omitzero"`
	Rows    int      `json:"rows,omitzero"`
	Cols    int      `json:"cols,omitzero"`
	NoLogin bool     `json:"noLogin,omitzero"`
	Meta    []byte   `json:"meta,omitzero"`
}

// WriteFrame writes one frame.
func WriteFrame(w io.Writer, t Type, payload []byte) error {
	if len(payload) > MaxPayload {
		return fmt.Errorf("shimproto: %d-byte payload exceeds limit", len(payload))
	}
	var hdr [5]byte
	hdr[0] = byte(t)
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

// WriteJSON writes a frame whose payload is v encoded as JSON.
func WriteJSON(w io.Writer, t Type, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return WriteFrame(w, t, b)
}

// ErrTooLarge is returned by ReadFrame for a frame over MaxPayload.
var ErrTooLarge = errors.New("shimproto: frame too large")

// ReadFrame reads one frame. The returned payload is newly allocated.
func ReadFrame(r *bufio.Reader) (Type, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > MaxPayload {
		return 0, nil, ErrTooLarge
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return Type(hdr[0]), payload, nil
}

// Decode decodes a JSON frame payload into v. Unknown fields are ignored,
// so newer peers can add fields.
func Decode(payload []byte, v any) error {
	return json.Unmarshal(payload, v)
}
