package history

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/bradfitz/tsterm/pkg/vt"
)

// small makes frames and segments small, for the duration of the test.
func small(t *testing.T) {
	ft, st := frameTarget, segTarget
	frameTarget, segTarget = 200, 1000
	t.Cleanup(func() { frameTarget, segTarget = ft, st })
}

func line(i int) vt.Line { return vt.Line{Text: fmt.Sprintf("line %d", i)} }

func appendLines(t *testing.T, s *Store, from, to int) {
	t.Helper()
	for i := from; i < to; i++ {
		if err := s.Append(line(i)); err != nil {
			t.Fatal(err)
		}
	}
}

// checkLines checks that s has lines from through to-1, as appendLines
// made them.
func checkLines(t *testing.T, s *Store, from, to int) {
	t.Helper()
	if got := s.First(); got != uint64(from) {
		t.Errorf("First = %d, want %d", got, from)
	}
	if got := s.Next(); got != uint64(to) {
		t.Errorf("Next = %d, want %d", got, to)
	}
	first, lines, err := s.Read(0, to+10)
	if err != nil {
		t.Fatal(err)
	}
	if first != uint64(from) || len(lines) != to-from {
		t.Fatalf("Read = line %d and %d more, want %d and %d", first, len(lines), from, to-from)
	}
	for i, l := range lines {
		if want := line(from + i).Text; l.Text != want {
			t.Fatalf("line %d = %q, want %q", from+i, l.Text, want)
		}
	}
}

func open(t *testing.T, dir string, max int64) *Store {
	t.Helper()
	s, err := Open(dir, max)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestAppendRead(t *testing.T) {
	small(t)
	s := open(t, t.TempDir(), 0)
	appendLines(t, s, 0, 500)
	checkLines(t, s, 0, 500)
	// Reads in the middle, across frames.
	first, lines, err := s.Read(123, 50)
	if err != nil || first != 123 || len(lines) != 50 || lines[49].Text != "line 172" {
		t.Errorf("Read(123, 50) = %d, %d lines, %v", first, len(lines), err)
	}
	if n := len(s.segs); n < 3 {
		t.Errorf("%d segments, want several", n)
	}
}

func TestReopen(t *testing.T) {
	small(t)
	dir := t.TempDir()
	s, err := Open(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	appendLines(t, s, 0, 300)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = open(t, dir, 0)
	checkLines(t, s, 0, 300)
	appendLines(t, s, 300, 400)
	checkLines(t, s, 0, 400)
}

func TestTornFrame(t *testing.T) {
	small(t)
	dir := t.TempDir()
	s, err := Open(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	appendLines(t, s, 0, 100)
	s.Close()
	// A crash cut the newest segment's last frame short.
	segs, _ := filepath.Glob(filepath.Join(dir, "*.seg"))
	slices.Sort(segs)
	last := segs[len(segs)-1]
	fi, _ := os.Stat(last)
	if err := os.Truncate(last, fi.Size()-3); err != nil {
		t.Fatal(err)
	}
	s = open(t, dir, 0)
	n := int(s.Next())
	if n >= 100 || n < 50 {
		t.Fatalf("Next after a torn frame = %d", n)
	}
	checkLines(t, s, 0, n)
	appendLines(t, s, n, n+50)
	checkLines(t, s, 0, n+50)
}

func TestPrune(t *testing.T) {
	small(t)
	s := open(t, t.TempDir(), 3000)
	appendLines(t, s, 0, 2000)
	if size := s.Size(); size > 3000+segTarget+500 {
		t.Errorf("Size = %d, over the limit", size)
	}
	first := int(s.First())
	if first == 0 {
		t.Fatal("nothing pruned")
	}
	checkLines(t, s, first, 2000)
}

func TestTruncate(t *testing.T) {
	small(t)
	for _, n := range []int{450, 401, 250, 100, 1, 0} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			dir := t.TempDir()
			s := open(t, dir, 0)
			appendLines(t, s, 0, 460)
			if err := s.Truncate(uint64(n)); err != nil {
				t.Fatal(err)
			}
			checkLines(t, s, 0, n)
			// It continues from there, and survives reopening.
			appendLines(t, s, n, n+30)
			checkLines(t, s, 0, n+30)
			s.Close()
			checkLines(t, open(t, dir, 0), 0, n+30)
		})
	}
}

func TestStyledLinesRoundTrip(t *testing.T) {
	s := open(t, t.TempDir(), 0)
	l := vt.Line{Text: "red link", Runs: []vt.Run{
		{Len: 3, Style: vt.Style{Fg: vt.Color{Kind: vt.ColorIndexed, Index: 1}, Attrs: vt.AttrBold}},
		{Len: 1},
		{Len: 4, Link: "https://example.com/"},
	}, Continued: true}
	s.Append(l)
	s.Flush()
	s.hasCached = false
	_, lines, err := s.Read(0, 1)
	if err != nil || len(lines) != 1 {
		t.Fatal(lines, err)
	}
	if got := lines[0]; got.Text != l.Text || !slices.Equal(got.Runs, l.Runs) || !got.Continued {
		t.Errorf("read %+v, want %+v", got, l)
	}
}

func TestSearch(t *testing.T) {
	small(t)
	s := open(t, t.TempDir(), 0)
	appendLines(t, s, 0, 500)
	if n, ok, err := s.Search("line 42", s.Next(), 1000); err != nil || !ok || n != 429 {
		// "line 429" is the newest containing "line 42".
		t.Errorf("Search = %d, %v, %v; want 429", n, ok, err)
	}
	if n, ok, _ := s.Search("line 42", 429, 1000); !ok || n != 428 {
		t.Errorf("Search before 429 = %d, %v; want 428", n, ok)
	}
	if _, ok, _ := s.Search("nope", s.Next(), 1000); ok {
		t.Error("found a missing string")
	}
}
