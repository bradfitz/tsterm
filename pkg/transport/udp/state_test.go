package udp

import (
	"math/rand/v2"
	"testing"

	"github.com/bradfitz/tsterm/pkg/proto"
	"github.com/bradfitz/tsterm/pkg/vt"
)

// noisyScreen returns a rows×cols screen of random colored letters, which
// compresses poorly and so needs many fragments.
func noisyScreen(rows, cols int) vt.Screen {
	r := rand.New(rand.NewPCG(1, 2))
	cells := make([][]vt.Cell, rows)
	for y := range cells {
		cells[y] = make([]vt.Cell, cols)
		for x := range cells[y] {
			cells[y][x] = vt.Cell{
				Grapheme: string(rune('a' + r.IntN(26))),
				Width:    1,
				Style:    vt.Style{Fg: vt.Color{Kind: vt.ColorRGB, R: uint8(r.Uint32()), G: uint8(r.Uint32()), B: uint8(r.Uint32())}},
			}
		}
	}
	return vt.Screen{Rows: rows, Cols: cols, Cells: cells, Cursor: vt.CursorState{Row: 1, Col: 2, Visible: true}}
}

func decodeFrag(t *testing.T, b []byte) *proto.State {
	t.Helper()
	var env proto.Envelope
	if err := proto.Decode(b, &env); err != nil {
		t.Fatal(err)
	}
	if env.State == nil {
		t.Fatalf("datagram is not a State: %+v", env)
	}
	return env.State
}

func TestEncodeStateSmall(t *testing.T) {
	term, err := vt.New(24, 80)
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()
	scr := term.Snapshot()
	pkts, err := EncodeState(0, 1, vt.Diff(vt.Screen{}, scr))
	if err != nil {
		t.Fatal(err)
	}
	if len(pkts) != 1 {
		t.Fatalf("blank 24x80 screen took %d fragments, want 1", len(pkts))
	}
	var r Reassembler
	d, ok, err := r.Add(decodeFrag(t, pkts[0]))
	if err != nil || !ok {
		t.Fatalf("Add = %v, %v", ok, err)
	}
	if d.Rows != 24 || d.Cols != 80 || len(d.Regions) != 24 {
		t.Errorf("decoded delta %dx%d with %d regions", d.Rows, d.Cols, len(d.Regions))
	}
}

func TestEncodeStateFragmentsReassemble(t *testing.T) {
	want := noisyScreen(50, 200)
	pkts, err := EncodeState(7, 9, vt.Diff(vt.Screen{}, want))
	if err != nil {
		t.Fatal(err)
	}
	if len(pkts) < 3 {
		t.Fatalf("got %d fragments, want several", len(pkts))
	}
	for i, b := range pkts {
		if len(b) > MaxDatagram {
			t.Errorf("fragment %d is %d bytes, over MaxDatagram %d", i, len(b), MaxDatagram)
		}
	}

	// Deliver in reverse order with a duplicate. Only the final fragment
	// may complete the update.
	var r Reassembler
	order := []int{len(pkts) - 1, len(pkts) - 1}
	for i := len(pkts) - 2; i >= 0; i-- {
		order = append(order, i)
	}
	var got vt.ScreenDelta
	for n, i := range order {
		f := decodeFrag(t, pkts[i])
		if f.From != 7 || f.To != 9 || int(f.Count) != len(pkts) || int(f.Index) != i {
			t.Fatalf("fragment %d header = %+v", i, f)
		}
		d, ok, err := r.Add(f)
		if err != nil {
			t.Fatal(err)
		}
		if ok != (n == len(order)-1) {
			t.Fatalf("Add #%d (fragment %d) ok = %v", n, i, ok)
		}
		got = d
	}
	// Applying the bootstrap delta to an empty screen must yield want.
	var applied vt.Screen
	applied.Rows, applied.Cols, applied.Cursor = got.Rows, got.Cols, got.Cursor
	applied.Cells = make([][]vt.Cell, got.Rows)
	for _, run := range got.Regions {
		applied.Cells[run.Row] = run.Cells
	}
	applied.Modes = got.Modes
	if !applied.Equal(want) {
		t.Error("reassembled screen differs from original")
	}
}

func TestReassemblerCompletesFromRetransmit(t *testing.T) {
	pkts, err := EncodeState(0, 5, vt.Diff(vt.Screen{}, noisyScreen(30, 100)))
	if err != nil {
		t.Fatal(err)
	}
	if len(pkts) < 2 {
		t.Fatalf("need at least 2 fragments, got %d", len(pkts))
	}
	var r Reassembler
	// First transmission loses fragment 0.
	for _, b := range pkts[1:] {
		if _, ok, err := r.Add(decodeFrag(t, b)); ok || err != nil {
			t.Fatalf("completed early: ok=%v err=%v", ok, err)
		}
	}
	// The retransmission is identical, so its fragment 0 completes it.
	if _, ok, err := r.Add(decodeFrag(t, pkts[0])); !ok || err != nil {
		t.Fatalf("retransmitted fragment did not complete: ok=%v err=%v", ok, err)
	}
}

func TestReassemblerForgetAndEvict(t *testing.T) {
	var r Reassembler
	half := func(to uint64) *proto.State {
		return &proto.State{From: 0, To: to, Index: 0, Count: 2, Data: []byte("x")}
	}
	for to := uint64(1); to <= 10; to++ {
		if _, _, err := r.Add(half(to)); err != nil {
			t.Fatal(err)
		}
	}
	if len(r.partials) != maxPartials {
		t.Errorf("tracking %d partials, want %d", len(r.partials), maxPartials)
	}
	for k := range r.partials {
		if k.to <= 10-maxPartials {
			t.Errorf("kept old partial %+v", k)
		}
	}
	r.Forget(9)
	if len(r.partials) != 1 {
		t.Errorf("after Forget(9), tracking %d partials, want 1", len(r.partials))
	}
}

func TestReassemblerRejectsBadFragments(t *testing.T) {
	var r Reassembler
	for _, f := range []proto.State{
		{To: 1, Count: 0},
		{To: 1, Index: 2, Count: 2},
		{To: 1, Count: maxFragments + 1},
	} {
		if _, _, err := r.Add(&f); err == nil {
			t.Errorf("Add(%+v) succeeded, want error", f)
		}
	}
	// Garbage that is not zstd fails cleanly once complete.
	if _, ok, err := r.Add(&proto.State{To: 1, Count: 1, Data: []byte("not zstd")}); ok || err == nil {
		t.Errorf("garbage payload: ok=%v err=%v", ok, err)
	}
}
