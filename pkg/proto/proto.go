// Package proto defines the on-the-wire types exchanged between tsterm
// clients and tstermd. CBOR is used for every transport (TCP control plane,
// UDP hot path, WebSocket viewer) so that one set of types serializes the
// same way everywhere.
//
// Hot-path messages share a single Envelope: a CBOR map keyed by small
// integers, each value optional, with exactly one populated field per frame.
// That shape keeps the wire compact (small int keys + omitempty) without
// needing a separate union discriminator.
//
// Control-plane requests and responses are not enveloped; the route already
// implies the type, so the body is the bare CBOR encoding of the struct.
// The control plane also speaks JSON, for browsers and other clients that
// lack CBOR, using the json field names below.
package proto

import (
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/bradfitz/tsterm/pkg/vt"
)

// ContentType is the Content-Type advertised on control-plane requests and
// responses.
const ContentType = "application/cbor"

// Envelope is the single hot-path frame. Exactly one pointer field is set
// per frame; the rest serialize to omitted CBOR keys.
type Envelope struct {
	Hello    *Hello    `cbor:"1,keyasint,omitempty"`
	Input    *Input    `cbor:"2,keyasint,omitempty"`
	Resize   *Resize   `cbor:"3,keyasint,omitempty"`
	Ack      *Ack      `cbor:"4,keyasint,omitempty"`
	Delta    *Delta    `cbor:"5,keyasint,omitempty"`
	Snapshot *Snapshot `cbor:"6,keyasint,omitempty"`
	Ended    *Ended    `cbor:"7,keyasint,omitempty"`
	State    *State    `cbor:"8,keyasint,omitempty"`
	StateAck *StateAck `cbor:"9,keyasint,omitempty"`
	Detach   *Detach   `cbor:"10,keyasint,omitempty"`
}

// Hello is the first packet a client sends on UDP attach. The server uses
// it to bind the source address to a session, after verifying both the
// opaque Token and the Tailscale identity of the source.
//
// Clients resend Hello whenever they stop hearing from the server, so it
// must be idempotent. Nonce identifies one client process: a Hello whose
// source address and Nonce match an existing attachment refreshes that
// attachment instead of replacing it. When the server has forgotten the
// client (for example, after an idle timeout), InputAcked tells it which
// input Seq the client last saw acknowledged, so the client's input stream
// resumes where it left off instead of restarting at 1.
type Hello struct {
	SessionID  string `cbor:"id"`
	Token      []byte `cbor:"tok"`
	Rows       int    `cbor:"r,omitempty"`
	Cols       int    `cbor:"c,omitempty"`
	Nonce      []byte `cbor:"n,omitempty"`
	InputAcked uint64 `cbor:"ia,omitempty"`
}

// Input is a batched client-to-server keystroke event. Seq is monotonically
// increasing per client; the server applies inputs exactly once and replies
// with an Ack carrying the highest contiguous Seq it has applied.
//
// A client drops keystrokes it could not deliver within about a minute
// rather than replaying them much later. Floor, if nonzero, says that every Seq up
// to and including Floor is either acked or abandoned, which lets the server
// skip over the abandoned gap.
type Input struct {
	Seq   uint64 `cbor:"s"`
	Keys  []byte `cbor:"k"`
	Floor uint64 `cbor:"f,omitempty"`
}

// Resize is a client-initiated window-size change. Resize is sequenced in
// the same stream as Input so retransmissions are stable.
type Resize struct {
	Seq   uint64 `cbor:"s"`
	Rows  int    `cbor:"r"`
	Cols  int    `cbor:"c"`
	Floor uint64 `cbor:"f,omitempty"` // as in Input
}

// Ack carries the highest contiguous input Seq the server has applied.
// Clients use it to retire retransmissions. The server also sends an Ack in
// reply to every Hello, which tells a client probing after sleep that the
// server is reachable even when there is no screen state to send.
type Ack struct {
	Highest uint64 `cbor:"h"`
}

// Delta wraps a vt.ScreenDelta for transport. Wrapping (instead of using
// vt.ScreenDelta directly) lets us add transport-level metadata later
// without disturbing the canonical model.
type Delta struct {
	Seq uint64         `cbor:"s,omitempty"`
	D   vt.ScreenDelta `cbor:"d"`
}

// Snapshot wraps a vt.Screen for transport. The WebSocket viewer sends one
// on attach and Deltas thereafter. The UDP hot path uses State instead.
type Snapshot struct {
	Seq uint64    `cbor:"s,omitempty"`
	S   vt.Screen `cbor:"s_"`
}

// State is one fragment of a UDP screen-state update.
//
// The server numbers the screen states it sends to each client. An update
// takes the client from state From, which the client has acknowledged, to
// state To. From == 0 denotes the empty screen, so such an update is a
// complete screen that any client can apply.
//
// The update payload is a zstd-compressed CBOR vt.ScreenDelta, split into
// Count fragments so that each datagram fits in one tailnet packet. Index is
// the fragment's position in [0, Count). Fragments of a retransmitted update
// are byte-for-byte identical to the originals, so a client can complete a
// partially received update from a retransmission.
type State struct {
	From  uint64 `cbor:"f,omitempty"`
	To    uint64 `cbor:"t"`
	Index uint16 `cbor:"i,omitempty"`
	Count uint16 `cbor:"n"`
	Data  []byte `cbor:"d"`
}

// StateAck tells the server that the client has applied screen state State.
// The server then sends future updates as diffs from that state, and sends
// nothing at all once the acknowledged state matches the current screen.
type StateAck struct {
	State uint64 `cbor:"s,omitempty"`
}

// Detach tells the server that the client is leaving on purpose (the user
// disconnected), so the server forgets it now rather than retrying screen
// updates and holding its state until IdleTimeout. The session keeps
// running.
type Detach struct{}

// Ended signals that the session has ended and includes a short status
// string (typically the child's exit error or "closed").
type Ended struct {
	Status string `cbor:"s"`
}

// EndedNoSession is the Ended Status the UDP server sends in reply to a
// Hello for a session that does not exist, such as after a daemon restart.
const EndedNoSession = "no such session"

// SessionSummary is the control-plane row returned by GET /v1/sessions and
// by name lookups.
//
// The activity times are zero if the event has never happened. Sorting by
// LastActivity puts the sessions where something last happened first;
// sorting by LastAttach puts the ones last looked at first.
type SessionSummary struct {
	ID       string    `cbor:"id" json:"id"`
	Name     string    `cbor:"name" json:"name,omitzero"`
	Rows     int       `cbor:"r" json:"rows"`
	Cols     int       `cbor:"c" json:"cols"`
	Created  time.Time `cbor:"t" json:"created"`
	Active   bool      `cbor:"a" json:"active"`
	Watchers int       `cbor:"w" json:"watchers"`

	LastInput    time.Time `cbor:"li" json:"lastInput,omitzero"`    // keyboard input sent to the session
	LastOutput   time.Time `cbor:"lo" json:"lastOutput,omitzero"`   // output from the session, changing its screen
	LastActivity time.Time `cbor:"lx" json:"lastActivity,omitzero"` // the later of LastInput and LastOutput
	LastAttach   time.Time `cbor:"la" json:"lastAttach,omitzero"`   // a client attached to look at the session
}

// CreateSessionReq is the body of POST /v1/sessions.
type CreateSessionReq struct {
	Name string   `cbor:"name,omitempty" json:"name,omitzero"`
	Cmd  []string `cbor:"cmd,omitempty" json:"cmd,omitzero"`
	Env  []string `cbor:"env,omitempty" json:"env,omitzero"`
	Rows int      `cbor:"r,omitempty" json:"rows,omitzero"`
	Cols int      `cbor:"c,omitempty" json:"cols,omitzero"`
}

// UpdateSessionReq is the body of PATCH /v1/sessions/{id}. Fields left
// nil are unchanged.
type UpdateSessionReq struct {
	// Name, if non-nil, renames the session; the empty string removes
	// its name.
	Name *string `cbor:"name,omitempty" json:"name,omitzero"`
}

// SessionInfo is returned by POST /v1/sessions and by GET /v1/sessions/{id}.
// Token is the per-session capability that UDP clients must echo back in
// every Hello.
type SessionInfo struct {
	ID      string `cbor:"id" json:"id"`
	Name    string `cbor:"name" json:"name,omitzero"`
	Token   []byte `cbor:"tok" json:"token"`
	UDPPort int    `cbor:"udp" json:"udpPort"`
	Rows    int    `cbor:"r" json:"rows"`
	Cols    int    `cbor:"c" json:"cols"`
}

// Error is a structured error body returned with non-2xx control-plane
// responses.
type Error struct {
	Code    string `cbor:"code" json:"code"`
	Message string `cbor:"msg" json:"message"`
}

// Encode serializes v to CBOR.
func Encode(v any) ([]byte, error) { return cbor.Marshal(v) }

// Decode deserializes b into v. v must be a pointer.
func Decode(b []byte, v any) error { return cbor.Unmarshal(b, v) }
