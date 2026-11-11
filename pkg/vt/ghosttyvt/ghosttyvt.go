// Package ghosttyvt is an experimental vt.Terminal backed by libghostty-vt,
// the terminal emulator from Ghostty, compiled to WebAssembly and run with
// wazero, a WebAssembly runtime in pure Go, so that tstermd needs no cgo.
//
// Each Terminal runs in its own instance of the module, so a fault in the
// emulator, which parses untrusted output, can only affect its own terminal.
//
// The module, its C glue, and the script that builds it live in
// github.com/bradfitz/tsterm-prebuilt/ghosttyvt, so that this repository
// doesn't carry the binary's history.
package ghosttyvt

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/bradfitz/tsterm-prebuilt/ghosttyvt"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"

	"github.com/bradfitz/tsterm/pkg/vt"
)

// abiVersion is the version of the glue's contract (its exports, its
// imports, and the snapshot encoding) that this package implements.
const abiVersion = 1

// DefaultScrollbackBytes bounds each terminal's scrollback memory.
const DefaultScrollbackBytes = 16 << 20

// runtime is the shared wazero runtime and the compiled module.
type runtime struct {
	r        wazero.Runtime
	compiled wazero.CompiledModule
}

var getRuntime = sync.OnceValues(func() (*runtime, error) {
	if ghosttyvt.ABIVersion != abiVersion {
		return nil, fmt.Errorf("ghosttyvt: prebuilt module has ABI version %d; want %d", ghosttyvt.ABIVersion, abiVersion)
	}
	ctx := context.Background()
	r := wazero.NewRuntime(ctx)
	_, err := r.NewHostModuleBuilder("env").
		NewFunctionBuilder().WithFunc(hostWritePTY).Export("write_pty").
		Instantiate(ctx)
	if err != nil {
		return nil, err
	}
	compiled, err := r.CompileModule(ctx, ghosttyvt.Wasm)
	if err != nil {
		return nil, err
	}
	// Check the module itself too, in case it and its version.go disagree.
	mod, err := r.InstantiateModule(ctx, compiled, wazero.NewModuleConfig().WithName("").WithStartFunctions())
	if err != nil {
		return nil, err
	}
	defer mod.Close(ctx)
	f := mod.ExportedFunction("tsterm_abi_version")
	if f == nil {
		return nil, errors.New("ghosttyvt: prebuilt module has no tsterm_abi_version")
	}
	if res, err := f.Call(ctx); err != nil || len(res) != 1 || res[0] != abiVersion {
		return nil, fmt.Errorf("ghosttyvt: prebuilt module reports ABI version %v (%v); want %d", res, err, abiVersion)
	}
	return &runtime{r: r, compiled: compiled}, nil
})

// terminals maps the IDs the module knows terminals by to the Terminals,
// for hostWritePTY.
var (
	terminals sync.Map // uint32 => *Terminal
	lastID    atomic.Uint32
)

// hostWritePTY is the module's write_pty import: the terminal's reply to a
// query, which belongs on the PTY's input. It runs during tsterm_write, with
// the Terminal's mu held.
func hostWritePTY(ctx context.Context, m api.Module, id, ptr, size uint32) {
	v, ok := terminals.Load(id)
	if !ok {
		return
	}
	if b, ok := m.Memory().Read(ptr, size); ok {
		t := v.(*Terminal)
		t.replies = append(t.replies, b...)
	}
}

// Terminal is a vt.Terminal backed by libghostty-vt. It is safe for
// concurrent use.
type Terminal struct {
	id uint32

	mu      sync.Mutex
	mod     api.Module
	ctx     uint32 // the glue's ctx pointer
	closed  bool
	replies []byte
	last    vt.Screen // baseline for Diff

	// in is a reusable buffer in the module's memory for writes.
	in, inCap uint32

	fWrite, fResize, fSnapshot, fOutLen, fCheckpoint, fFree, fAlloc, fFreeMem api.Function
}

var _ vt.Terminal = (*Terminal)(nil)

// New returns a rows×cols terminal.
func New(rows, cols int) (*Terminal, error) {
	t, err := instantiate()
	if err != nil {
		return nil, err
	}
	res, err := t.call(t.mod.ExportedFunction("tsterm_new"), uint64(t.id), uint64(cols), uint64(rows), DefaultScrollbackBytes)
	if err != nil || res == 0 {
		t.Close()
		return nil, fmt.Errorf("ghosttyvt: creating terminal: %v", err)
	}
	t.ctx = uint32(res)
	t.SetColors(DefaultForeground, DefaultBackground)
	return t, nil
}

// The default colors, as 0xRRGGBB.
const (
	DefaultForeground = 0xffffff
	DefaultBackground = 0x000000
)

// SetColors sets the default foreground and background colors, as
// 0xRRGGBB, which the terminal reports to programs that ask. A client's
// theme could supply them.
func (t *Terminal) SetColors(fg, bg uint32) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.closed {
		t.call(t.mod.ExportedFunction("tsterm_set_colors"), uint64(t.ctx), uint64(fg), uint64(bg))
	}
}

// Restore returns a terminal restored from a Checkpoint.
func Restore(checkpoint []byte) (*Terminal, error) {
	t, err := instantiate()
	if err != nil {
		return nil, err
	}
	ptr, err := t.alloc(uint32(len(checkpoint)))
	if err != nil {
		t.Close()
		return nil, err
	}
	t.mod.Memory().Write(ptr, checkpoint)
	res, err := t.call(t.mod.ExportedFunction("tsterm_restore"), uint64(t.id), uint64(ptr), uint64(len(checkpoint)))
	t.call(t.fFreeMem, uint64(ptr), uint64(len(checkpoint)))
	if err != nil || res == 0 {
		t.Close()
		return nil, errors.New("ghosttyvt: checkpoint not restorable")
	}
	t.ctx = uint32(res)
	return t, nil
}

func instantiate() (*Terminal, error) {
	rt, err := getRuntime()
	if err != nil {
		return nil, err
	}
	mod, err := rt.r.InstantiateModule(context.Background(), rt.compiled,
		wazero.NewModuleConfig().WithName("").WithStartFunctions())
	if err != nil {
		return nil, err
	}
	t := &Terminal{
		id:          lastID.Add(1),
		mod:         mod,
		fWrite:      mod.ExportedFunction("tsterm_write"),
		fResize:     mod.ExportedFunction("tsterm_resize"),
		fSnapshot:   mod.ExportedFunction("tsterm_snapshot"),
		fOutLen:     mod.ExportedFunction("tsterm_out_len"),
		fCheckpoint: mod.ExportedFunction("tsterm_checkpoint"),
		fFree:       mod.ExportedFunction("tsterm_free"),
		fAlloc:      mod.ExportedFunction("ghostty_wasm_alloc"),
		fFreeMem:    mod.ExportedFunction("ghostty_wasm_free"),
	}
	terminals.Store(t.id, t)
	return t, nil
}

func (t *Terminal) call(f api.Function, args ...uint64) (uint64, error) {
	res, err := f.Call(context.Background(), args...)
	if err != nil {
		return 0, err
	}
	if len(res) == 0 {
		return 0, nil
	}
	return res[0], nil
}

func (t *Terminal) alloc(n uint32) (uint32, error) {
	ptr, err := t.call(t.fAlloc, uint64(n))
	if err != nil || ptr == 0 {
		return 0, fmt.Errorf("ghosttyvt: allocating %d bytes: %v", n, err)
	}
	return uint32(ptr), nil
}

// Write feeds terminal output to the emulator.
func (t *Terminal) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return 0, errors.New("ghosttyvt: terminal closed")
	}
	n := len(p)
	for len(p) > 0 {
		chunk := p[:min(len(p), 64<<10)]
		if uint32(len(chunk)) > t.inCap {
			if t.in != 0 {
				t.call(t.fFreeMem, uint64(t.in), uint64(t.inCap))
			}
			ptr, err := t.alloc(64 << 10)
			if err != nil {
				return 0, err
			}
			t.in, t.inCap = ptr, 64<<10
		}
		t.mod.Memory().Write(t.in, chunk)
		if _, err := t.call(t.fWrite, uint64(t.ctx), uint64(t.in), uint64(len(chunk))); err != nil {
			return 0, fmt.Errorf("ghosttyvt: %w", err)
		}
		p = p[len(chunk):]
	}
	return n, nil
}

// Resize changes the grid size.
func (t *Terminal) Resize(rows, cols int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.closed {
		t.call(t.fResize, uint64(t.ctx), uint64(cols), uint64(rows))
	}
}

// Replies returns and clears the terminal's replies to queries.
func (t *Terminal) Replies() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	r := t.replies
	t.replies = nil
	return r
}

// Checkpoint returns the terminal's complete state, for Restore: both
// screens, scrollback, modes, the scroll region, the saved cursor, and any
// unfinished escape sequence.
func (t *Terminal) Checkpoint() ([]byte, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.outLocked(t.fCheckpoint)
}

// outLocked calls f, a glue function returning its output buffer, and
// copies the buffer out.
func (t *Terminal) outLocked(f api.Function) ([]byte, error) {
	if t.closed {
		return nil, errors.New("ghosttyvt: terminal closed")
	}
	ptr, err := t.call(f, uint64(t.ctx))
	if err != nil {
		return nil, err
	}
	if ptr == 0 {
		return nil, errors.New("ghosttyvt: failed")
	}
	n, _ := t.call(t.fOutLen, uint64(t.ctx))
	b, ok := t.mod.Memory().Read(uint32(ptr), uint32(n))
	if !ok {
		return nil, errors.New("ghosttyvt: bad output buffer")
	}
	return append([]byte(nil), b...), nil
}

// Close frees the terminal's module instance.
func (t *Terminal) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}
	t.closed = true
	terminals.Delete(t.id)
	return t.mod.Close(context.Background())
}

// Snapshot returns the visible screen.
func (t *Terminal) Snapshot() vt.Screen {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, err := t.snapshotLocked()
	if err != nil {
		return vt.Screen{}
	}
	return s
}

func (t *Terminal) snapshotLocked() (vt.Screen, error) {
	b, err := t.outLocked(t.fSnapshot)
	if err != nil {
		return vt.Screen{}, err
	}
	return decodeSnapshot(b)
}

// Diff returns the changes since the last Diff.
func (t *Terminal) Diff() vt.ScreenDelta {
	t.mu.Lock()
	defer t.mu.Unlock()
	cur, _ := t.snapshotLocked()
	d := vt.Diff(t.last, cur)
	t.last = cur
	return d
}

// Modes returns the terminal's modes.
func (t *Terminal) Modes() vt.Modes { return t.Snapshot().Modes }

// Size returns the grid size.
func (t *Terminal) Size() (rows, cols int) {
	s := t.Snapshot()
	return s.Rows, s.Cols
}

// Mode bits in the glue's snapshot encoding.
const (
	modeAltScreen      = 1 << 0
	modeAppCursor      = 1 << 1
	modeBracketedPaste = 1 << 2
	modeFocusEvents    = 1 << 3
	modeMouseSGR       = 1 << 4
	modeCursorVisible  = 1 << 5
	modeMouseShift     = 8
)

// Style flag bits, as in libghostty's GhosttyStyleFlags.
const (
	flagBold = 1 << iota
	flagItalic
	flagFaint
	flagBlink
	flagInverse
	flagInvisible
	flagStrikethrough
	flagOverline
	underlineShift = 8
)

// flagAttrs maps libghostty style flags to vt attributes.
var flagAttrs = []struct {
	flag uint16
	attr vt.Attr
}{
	{flagBold, vt.AttrBold},
	{flagItalic, vt.AttrItalic},
	{flagFaint, vt.AttrFaint},
	{flagBlink, vt.AttrBlink},
	{flagInverse, vt.AttrReverse},
	{flagInvisible, vt.AttrConceal},
	{flagStrikethrough, vt.AttrStrikethrough},
}

// decodeSnapshot decodes tsterm_snapshot's encoding; see glue/tsterm.c.
func decodeSnapshot(b []byte) (vt.Screen, error) {
	d := decoder{b: b}
	var s vt.Screen
	s.Rows = int(d.u16())
	s.Cols = int(d.u16())
	s.Cursor.Col = int(d.u16())
	s.Cursor.Row = int(d.u16())
	s.Cursor.Visible = d.u8() != 0
	s.Cursor.Blink = d.u8() != 0
	switch d.u8() {
	case 0:
		s.Cursor.Style = vt.CursorBar
	case 2:
		s.Cursor.Style = vt.CursorUnderline
	default:
		s.Cursor.Style = vt.CursorBlock
	}
	modes := d.u32()
	s.Modes = vt.Modes{
		AltScreen:         modes&modeAltScreen != 0,
		ApplicationCursor: modes&modeAppCursor != 0,
		BracketedPaste:    modes&modeBracketedPaste != 0,
		FocusEvents:       modes&modeFocusEvents != 0,
		MouseSGR:          modes&modeMouseSGR != 0,
		CursorVisible:     modes&modeCursorVisible != 0,
		Mouse:             vt.MouseMode(modes >> modeMouseShift & 0xf),
	}
	s.BellSeq = d.u64()
	s.Title = string(d.bytes(int(d.u32())))
	s.Cells = make([][]vt.Cell, s.Rows)
	for y := range s.Rows {
		row := make([]vt.Cell, s.Cols)
		for x := range s.Cols {
			wide := d.u8()
			flags := d.u16()
			fg, bg := d.color(), d.color()
			g := string(d.bytes(int(d.u8())))
			link := string(d.bytes(int(d.u16())))
			c := vt.Cell{Grapheme: g, Width: 1, Link: link}
			switch wide {
			case 1:
				c.Width = 2
			case 2: // the second column of a wide character
				c.Width, c.Grapheme = 0, ""
			}
			if c.Width != 0 && c.Grapheme == "" {
				c.Grapheme = " "
			}
			c.Style = vt.Style{Fg: fg, Bg: bg, Underline: vt.UnderlineStyle(flags >> underlineShift & 0x7)}
			for _, fa := range flagAttrs {
				if flags&fa.flag != 0 {
					c.Style.Attrs |= fa.attr
				}
			}
			row[x] = c
		}
		s.Cells[y] = row
	}
	if d.err != nil {
		return vt.Screen{}, d.err
	}
	return s, nil
}

type decoder struct {
	b   []byte
	err error
}

func (d *decoder) bytes(n int) []byte {
	if d.err != nil || n > len(d.b) {
		d.err = errors.New("ghosttyvt: truncated snapshot")
		return nil
	}
	v := d.b[:n]
	d.b = d.b[n:]
	return v
}

func (d *decoder) u8() uint8 {
	b := d.bytes(1)
	if b == nil {
		return 0
	}
	return b[0]
}

func (d *decoder) u16() uint16 {
	if b := d.bytes(2); b != nil {
		return binary.LittleEndian.Uint16(b)
	}
	return 0
}

func (d *decoder) u32() uint32 {
	if b := d.bytes(4); b != nil {
		return binary.LittleEndian.Uint32(b)
	}
	return 0
}

func (d *decoder) u64() uint64 {
	if b := d.bytes(8); b != nil {
		return binary.LittleEndian.Uint64(b)
	}
	return 0
}

func (d *decoder) color() vt.Color {
	tag, a, g, b := d.u8(), d.u8(), d.u8(), d.u8()
	switch tag {
	case 1:
		return vt.Color{Kind: vt.ColorIndexed, Index: a}
	case 2:
		return vt.Color{Kind: vt.ColorRGB, R: a, G: g, B: b}
	}
	return vt.Color{}
}
