// Package history stores a session's history: the lines that scroll off the
// top of its terminal's screen (see vt.Terminal.TakeHistory), numbered from
// 0 in that order. It keeps them on disk, so that history can be far longer
// than the terminal's own scrollback, and outlive tstermd and the machine.
//
// A Store is a directory of segment files, each a sequence of frames: a
// header, then the zstd-compressed CBOR of a run of lines. Lines collect in
// memory and are written as a frame once there are enough of them, or on
// Flush. Once a Store is over its size limit, its oldest segments are
// deleted, so its first line is wherever the oldest one kept starts (see
// First).
package history

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/fxamacker/cbor/v2"
	"github.com/klauspost/compress/zstd"

	"github.com/bradfitz/tsterm/pkg/vt"
)

// DefaultMaxBytes is the default limit on a Store's size on disk.
const DefaultMaxBytes = 256 << 20

const (
	// segMagic starts each segment file.
	segMagic = "tshist\x00\x01"

	// A frame's header is its compressed length (u32), its first line's
	// number (u64), its line count (u32), and the CRC-32 (IEEE) of its
	// compressed data (u32), all little endian.
	frameHeaderLen = 4 + 8 + 4 + 4

	// maxFrameLen bounds a frame's compressed length, as read.
	maxFrameLen = 64 << 20
)

// Sizes, which tests lower.
var (
	// frameTarget is about how many bytes of lines, uncompressed, make a
	// frame.
	frameTarget = 64 << 10

	// segTarget is about how big a segment file grows before the next
	// frame starts a new one.
	segTarget int64 = 4 << 20
)

var (
	zenc = sync.OnceValue(func() *zstd.Encoder {
		enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
		if err != nil {
			panic(err)
		}
		return enc
	})
	zdec = sync.OnceValue(func() *zstd.Decoder {
		dec, err := zstd.NewReader(nil, zstd.WithDecoderMaxMemory(256<<20))
		if err != nil {
			panic(err)
		}
		return dec
	})
)

// A Store is a session's history. It is safe for concurrent use.
type Store struct {
	dir      string
	maxBytes int64

	mu      sync.Mutex
	segs    []*segment // oldest first
	w       *os.File   // the last segment, open for appending, or nil
	pending []vt.Line  // lines not yet in a frame, the newest
	pendLen int        // their size, roughly
	next    uint64     // the next line's number
	closed  bool

	// cached is the last frame read, decoded.
	cachedAt  frameID
	cached    []vt.Line
	hasCached bool
}

type segment struct {
	path   string
	first  uint64 // the number of its first line
	size   int64
	frames []frame
}

type frame struct {
	off   int64 // of its header in its segment
	first uint64
	n     uint32
	clen  uint32
}

type frameID struct {
	path string
	off  int64
}

// Open opens the Store in dir, creating dir if needed, whose segments may
// take up to maxBytes on disk (DefaultMaxBytes if maxBytes is zero). The
// newest segment's end is cut off if a crash left it unfinished.
func Open(dir string, maxBytes int64) (*Store, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, maxBytes: maxBytes}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range ents {
		name, ok := strings.CutSuffix(e.Name(), ".seg")
		if !ok {
			continue
		}
		first, err := strconv.ParseUint(name, 10, 64)
		if err != nil {
			continue
		}
		s.segs = append(s.segs, &segment{path: filepath.Join(dir, e.Name()), first: first})
	}
	slices.SortFunc(s.segs, func(a, b *segment) int { return compareUint(a.first, b.first) })
	for i, seg := range s.segs {
		last := i == len(s.segs)-1
		if err := seg.scan(last); err != nil {
			return nil, fmt.Errorf("history: %s: %w", seg.path, err)
		}
	}
	if n := len(s.segs); n > 0 {
		s.next = s.segs[n-1].end()
	}
	return s, nil
}

func compareUint(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// end returns the number of the line after seg's last.
func (seg *segment) end() uint64 {
	if n := len(seg.frames); n > 0 {
		f := seg.frames[n-1]
		return f.first + uint64(f.n)
	}
	return seg.first
}

// scan reads seg's frame headers. If seg is the newest segment, a frame
// that is cut off or corrupt, as by a crash while writing it, is cut off
// with everything after it; in an older one, it ends the segment.
func (seg *segment) scan(newest bool) error {
	f, err := os.OpenFile(seg.path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	size := fi.Size()
	magic := make([]byte, len(segMagic))
	if _, err := io.ReadFull(f, magic); err != nil || string(magic) != segMagic {
		if size <= int64(len(segMagic)) && newest {
			// Created, but the crash came before its magic was
			// written.
			if err := f.Truncate(0); err != nil {
				return err
			}
			if _, err := f.WriteAt([]byte(segMagic), 0); err != nil {
				return err
			}
			seg.size = int64(len(segMagic))
			return nil
		}
		return errors.New("not a history segment")
	}
	off := int64(len(segMagic))
	hdr := make([]byte, frameHeaderLen)
	var data []byte
	for off < size {
		fr, ok := readFrameHeader(f, off, hdr)
		ok = ok && off+frameHeaderLen+int64(fr.clen) <= size
		if ok {
			data = slices.Grow(data[:0], int(fr.clen))[:fr.clen]
			if _, err := f.ReadAt(data, off+frameHeaderLen); err != nil {
				return err
			}
			ok = crc32.ChecksumIEEE(data) == binary.LittleEndian.Uint32(hdr[16:]) && fr.first == seg.end()
		}
		if !ok {
			if newest {
				if err := f.Truncate(off); err != nil {
					return err
				}
			}
			break
		}
		seg.frames = append(seg.frames, fr)
		off += frameHeaderLen + int64(fr.clen)
	}
	seg.size = off
	return nil
}

func readFrameHeader(f *os.File, off int64, hdr []byte) (frame, bool) {
	if _, err := f.ReadAt(hdr, off); err != nil {
		return frame{}, false
	}
	fr := frame{
		off:   off,
		clen:  binary.LittleEndian.Uint32(hdr[0:]),
		first: binary.LittleEndian.Uint64(hdr[4:]),
		n:     binary.LittleEndian.Uint32(hdr[12:]),
	}
	return fr, fr.clen <= maxFrameLen && fr.n > 0
}

// First returns the number of the oldest line the Store has.
func (s *Store) First() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.firstLocked()
}

func (s *Store) firstLocked() uint64 {
	for _, seg := range s.segs {
		if len(seg.frames) > 0 {
			return seg.first
		}
	}
	return s.next - uint64(len(s.pending))
}

// Next returns the number the next line appended will have: the Store has
// the lines from First up to it.
func (s *Store) Next() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.next
}

// Size returns the bytes the Store's segments take on disk.
func (s *Store) Size() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for _, seg := range s.segs {
		n += seg.size
	}
	return n
}

// lineSize estimates a line's size, encoded.
func lineSize(l vt.Line) int { return len(l.Text) + 16*len(l.Runs) + 4 }

// Append adds lines to the end of the history.
func (s *Store) Append(lines ...vt.Line) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("history: closed")
	}
	for _, l := range lines {
		s.pending = append(s.pending, l)
		s.pendLen += lineSize(l)
		s.next++
	}
	if s.pendLen >= frameTarget {
		return s.flushLocked()
	}
	return nil
}

// Flush writes the lines appended since the last frame as a frame.
func (s *Store) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.flushLocked()
}

func (s *Store) flushLocked() error {
	if len(s.pending) == 0 {
		return nil
	}
	enc, err := cbor.Marshal(s.pending)
	if err != nil {
		return err
	}
	data := zenc().EncodeAll(enc, nil)
	first := s.next - uint64(len(s.pending))
	seg, err := s.writableSegLocked(first)
	if err != nil {
		return err
	}
	buf := make([]byte, frameHeaderLen, frameHeaderLen+len(data))
	binary.LittleEndian.PutUint32(buf[0:], uint32(len(data)))
	binary.LittleEndian.PutUint64(buf[4:], first)
	binary.LittleEndian.PutUint32(buf[12:], uint32(len(s.pending)))
	binary.LittleEndian.PutUint32(buf[16:], crc32.ChecksumIEEE(data))
	buf = append(buf, data...)
	if _, err := s.w.WriteAt(buf, seg.size); err != nil {
		return err
	}
	seg.frames = append(seg.frames, frame{off: seg.size, first: first, n: uint32(len(s.pending)), clen: uint32(len(data))})
	seg.size += int64(len(buf))
	s.pending, s.pendLen = nil, 0
	return s.pruneLocked()
}

// writableSegLocked returns the segment for a frame starting at line
// first, starting a new one if the newest is full, and opens it in s.w.
func (s *Store) writableSegLocked(first uint64) (*segment, error) {
	if n := len(s.segs); n > 0 {
		seg := s.segs[n-1]
		if seg.size < segTarget || len(seg.frames) == 0 {
			if s.w == nil {
				w, err := os.OpenFile(seg.path, os.O_RDWR, 0)
				if err != nil {
					return nil, err
				}
				s.w = w
			}
			return seg, nil
		}
	}
	if s.w != nil {
		s.w.Close()
		s.w = nil
	}
	seg := &segment{path: filepath.Join(s.dir, fmt.Sprintf("%020d.seg", first)), first: first, size: int64(len(segMagic))}
	w, err := os.OpenFile(seg.path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write([]byte(segMagic)); err != nil {
		w.Close()
		return nil, err
	}
	s.w = w
	s.segs = append(s.segs, seg)
	return seg, nil
}

// pruneLocked deletes the oldest segments while the Store is over its
// size limit, always keeping the newest.
func (s *Store) pruneLocked() error {
	var total int64
	for _, seg := range s.segs {
		total += seg.size
	}
	for total > s.maxBytes && len(s.segs) > 1 {
		seg := s.segs[0]
		if err := os.Remove(seg.path); err != nil && !os.IsNotExist(err) {
			return err
		}
		total -= seg.size
		s.segs = s.segs[1:]
	}
	return nil
}

// Read returns up to max lines starting at line from, and the number of
// the first one, which is later than from if the Store no longer has the
// lines from there.
func (s *Store) Read(from uint64, max int) (first uint64, lines []vt.Line, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	from = maxUint(from, s.firstLocked())
	first = from
	pendFirst := s.next - uint64(len(s.pending))
	for len(lines) < max && from < s.next {
		if from >= pendFirst {
			i := int(from - pendFirst)
			got := s.pending[i:min(len(s.pending), i+max-len(lines))]
			lines = append(lines, got...)
			from += uint64(len(got))
			continue
		}
		seg, fr, ok := s.findLocked(from)
		if !ok {
			return first, lines, fmt.Errorf("history: line %d missing", from)
		}
		frLines, err := s.frameLocked(seg, fr)
		if err != nil {
			return first, lines, err
		}
		i := int(from - fr.first)
		got := frLines[i:min(len(frLines), i+max-len(lines))]
		lines = append(lines, got...)
		from += uint64(len(got))
	}
	return first, lines, nil
}

func maxUint(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}

// findLocked returns the segment and frame holding line n.
func (s *Store) findLocked(n uint64) (*segment, frame, bool) {
	i, found := slices.BinarySearchFunc(s.segs, n, func(seg *segment, n uint64) int { return compareUint(seg.first, n) })
	if !found {
		i--
	}
	if i < 0 {
		return nil, frame{}, false
	}
	seg := s.segs[i]
	j, found := slices.BinarySearchFunc(seg.frames, n, func(f frame, n uint64) int { return compareUint(f.first, n) })
	if !found {
		j--
	}
	if j < 0 || n >= seg.frames[j].first+uint64(seg.frames[j].n) {
		return nil, frame{}, false
	}
	return seg, seg.frames[j], true
}

// frameLocked returns a frame's lines.
func (s *Store) frameLocked(seg *segment, fr frame) ([]vt.Line, error) {
	id := frameID{seg.path, fr.off}
	if s.hasCached && s.cachedAt == id {
		return s.cached, nil
	}
	f, err := os.Open(seg.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data := make([]byte, fr.clen)
	if _, err := f.ReadAt(data, fr.off+frameHeaderLen); err != nil {
		return nil, err
	}
	dec, err := zdec().DecodeAll(data, nil)
	if err != nil {
		return nil, err
	}
	var lines []vt.Line
	if err := cbor.Unmarshal(dec, &lines); err != nil {
		return nil, err
	}
	if len(lines) != int(fr.n) {
		return nil, fmt.Errorf("history: frame at %s:%d has %d lines, not %d", seg.path, fr.off, len(lines), fr.n)
	}
	s.cachedAt, s.cached, s.hasCached = id, lines, true
	return lines, nil
}

// Truncate drops the lines numbered n and up, so that the next line
// appended is line n.
func (s *Store) Truncate(n uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n >= s.next {
		return nil
	}
	s.hasCached = false
	pendFirst := s.next - uint64(len(s.pending))
	if n >= pendFirst {
		s.pending = s.pending[:n-pendFirst]
		s.next = n
		return nil
	}
	s.pending, s.pendLen = nil, 0
	if s.w != nil {
		s.w.Close()
		s.w = nil
	}
	var keep []vt.Line // of a frame that n cuts, the lines before n
	for len(s.segs) > 0 {
		seg := s.segs[len(s.segs)-1]
		if seg.first >= n {
			if err := os.Remove(seg.path); err != nil && !os.IsNotExist(err) {
				return err
			}
			s.segs = s.segs[:len(s.segs)-1]
			continue
		}
		i := len(seg.frames)
		for i > 0 && seg.frames[i-1].first >= n {
			i--
		}
		if i > 0 && n < seg.frames[i-1].first+uint64(seg.frames[i-1].n) {
			fr := seg.frames[i-1]
			lines, err := s.frameLocked(seg, fr)
			if err != nil {
				return err
			}
			keep = slices.Clone(lines[:n-fr.first])
			i--
		}
		cut := int64(len(segMagic))
		if i < len(seg.frames) {
			cut = seg.frames[i].off
		}
		if err := os.Truncate(seg.path, cut); err != nil {
			return err
		}
		seg.frames, seg.size = seg.frames[:i], cut
		break
	}
	s.hasCached = false
	s.next = n - uint64(len(keep))
	for _, l := range keep {
		s.pending = append(s.pending, l)
		s.pendLen += lineSize(l)
		s.next++
	}
	return nil
}

// Close writes any lines not yet written, syncs, and closes the Store.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	err := s.flushLocked()
	s.closed = true
	if s.w != nil {
		if serr := s.w.Sync(); err == nil {
			err = serr
		}
		if cerr := s.w.Close(); err == nil {
			err = cerr
		}
		s.w = nil
	}
	return err
}

// Search returns the number of the newest line before line before that
// contains query, looking at no more than max lines, and whether it found
// one.
func (s *Store) Search(query string, before uint64, max int) (uint64, bool, error) {
	q := []byte(query)
	for before > s.First() && max > 0 {
		n := min(uint64(max), 1024, before-s.First())
		first, lines, err := s.Read(before-n, int(n))
		if err != nil {
			return 0, false, err
		}
		for i := len(lines) - 1; i >= 0; i-- {
			if bytes.Contains([]byte(lines[i].Text), q) {
				return first + uint64(i), true, nil
			}
		}
		if first >= before {
			break
		}
		max -= int(before - first)
		before = first
	}
	return 0, false, nil
}
