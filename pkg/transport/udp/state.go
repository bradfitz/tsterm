package udp

import (
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/klauspost/compress/zstd"

	"github.com/bradfitz/tsterm/pkg/proto"
	"github.com/bradfitz/tsterm/pkg/vt"
)

// MaxDatagram is the largest UDP payload tsterm sends. Tailscale's default
// MTU is 1280 bytes; subtracting a 40-byte IPv6 header and an 8-byte UDP
// header leaves 1232.
const MaxDatagram = 1232

// fragmentData is the number of compressed update bytes carried in each
// State fragment. The remainder of MaxDatagram is headroom for the Envelope
// and State framing, which is at most about 40 bytes.
const fragmentData = MaxDatagram - 64

const (
	// maxFragments bounds the fragment count of a single update, which
	// bounds reassembly memory at about 1.2 MB per partial update.
	maxFragments = 1024

	// maxUpdateSize bounds the decompressed size of a single update.
	maxUpdateSize = 16 << 20

	// maxPartials is how many incomplete updates a Reassembler tracks at
	// once. Updates supersede each other, so only the newest few matter.
	maxPartials = 4
)

var zstdEncoder = sync.OnceValue(func() *zstd.Encoder {
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		panic(err)
	}
	return enc
})

var zstdDecoder = sync.OnceValue(func() *zstd.Decoder {
	dec, err := zstd.NewReader(nil, zstd.WithDecoderMaxMemory(maxUpdateSize))
	if err != nil {
		panic(err)
	}
	return dec
})

// EncodeState returns the encoded datagrams that carry the screen-state
// update d, which takes a client from state from to state to.
func EncodeState(from, to uint64, d vt.ScreenDelta) ([][]byte, error) {
	raw, err := proto.Encode(d)
	if err != nil {
		return nil, err
	}
	z := zstdEncoder().EncodeAll(raw, nil)
	n := max((len(z)+fragmentData-1)/fragmentData, 1)
	if n > maxFragments {
		return nil, fmt.Errorf("udp: state update too large (%d compressed bytes)", len(z))
	}
	pkts := make([][]byte, 0, n)
	for i := range n {
		chunk := z[i*fragmentData : min((i+1)*fragmentData, len(z))]
		b, err := proto.Encode(proto.Envelope{State: &proto.State{
			From:  from,
			To:    to,
			Index: uint16(i),
			Count: uint16(n),
			Data:  chunk,
		}})
		if err != nil {
			return nil, err
		}
		pkts = append(pkts, b)
	}
	return pkts, nil
}

// Reassembler collects State fragments into complete screen-state updates.
// The zero value is ready to use. A Reassembler is not safe for concurrent
// use.
type Reassembler struct {
	partials map[updateKey]*partialUpdate
}

type updateKey struct{ from, to uint64 }

type partialUpdate struct {
	frags [][]byte // indexed by fragment Index; nil until received
	have  int      // number of non-nil frags
}

var errBadFragment = errors.New("udp: malformed state fragment")

// Add records the fragment f. If f completes an update, Add returns the
// decoded delta and true.
func (r *Reassembler) Add(f *proto.State) (vt.ScreenDelta, bool, error) {
	if f.Count == 0 || f.Count > maxFragments || f.Index >= f.Count {
		return vt.ScreenDelta{}, false, errBadFragment
	}
	if r.partials == nil {
		r.partials = map[updateKey]*partialUpdate{}
	}
	k := updateKey{f.From, f.To}
	p := r.partials[k]
	if p == nil || len(p.frags) != int(f.Count) {
		p = &partialUpdate{frags: make([][]byte, f.Count)}
		r.partials[k] = p
		r.evict()
	}
	if p.frags[f.Index] == nil {
		p.frags[f.Index] = f.Data
		p.have++
	}
	if p.have < len(p.frags) {
		return vt.ScreenDelta{}, false, nil
	}
	delete(r.partials, k)

	raw, err := zstdDecoder().DecodeAll(slices.Concat(p.frags...), nil)
	if err != nil {
		return vt.ScreenDelta{}, false, fmt.Errorf("udp: decompress state: %w", err)
	}
	var d vt.ScreenDelta
	if err := proto.Decode(raw, &d); err != nil {
		return vt.ScreenDelta{}, false, fmt.Errorf("udp: decode state: %w", err)
	}
	return d, true, nil
}

// Forget discards incomplete updates that lead to state n or earlier. A
// client calls it after applying state n, since those updates are stale.
func (r *Reassembler) Forget(n uint64) {
	for k := range r.partials {
		if k.to <= n {
			delete(r.partials, k)
		}
	}
}

// evict drops the oldest incomplete updates beyond maxPartials.
func (r *Reassembler) evict() {
	for len(r.partials) > maxPartials {
		var oldest updateKey
		first := true
		for k := range r.partials {
			if first || k.to < oldest.to || (k.to == oldest.to && k.from < oldest.from) {
				oldest, first = k, false
			}
		}
		delete(r.partials, oldest)
	}
}
