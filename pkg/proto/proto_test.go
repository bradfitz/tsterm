package proto

import (
	"bytes"
	"reflect"
	"testing"
	"time"

	"github.com/bradfitz/tsterm/pkg/vt"
)

func TestEnvelopeRoundtrip(t *testing.T) {
	cases := []struct {
		name string
		in   Envelope
	}{
		{"hello", Envelope{Hello: &Hello{SessionID: "abc", Token: []byte{1, 2, 3}, Rows: 24, Cols: 80}}},
		{"input", Envelope{Input: &Input{Seq: 7, Keys: []byte("hello")}}},
		{"resize", Envelope{Resize: &Resize{Seq: 8, Rows: 30, Cols: 100}}},
		{"ack", Envelope{Ack: &Ack{Highest: 42}}},
		{"ended", Envelope{Ended: &Ended{Status: "exit 0"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b, err := Encode(c.in)
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			var out Envelope
			if err := Decode(b, &out); err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if !reflect.DeepEqual(c.in, out) {
				t.Fatalf("roundtrip mismatch:\n in=%+v\n out=%+v", c.in, out)
			}
		})
	}
}

func TestEnvelopeOnlyOneFieldEncoded(t *testing.T) {
	// CBOR should omit nil pointer fields, so a single-variant envelope is
	// tiny; sanity-check that.
	e := Envelope{Ack: &Ack{Highest: 1}}
	b, err := Encode(e)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > 16 {
		t.Errorf("single-variant envelope unexpectedly large: %d bytes (%v)", len(b), b)
	}
}

func TestSnapshotAndDeltaRoundtrip(t *testing.T) {
	term, err := vt.New(2, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()
	if _, err := term.Write([]byte("中A\r\nhi")); err != nil {
		t.Fatalf("term.Write: %v", err)
	}
	snap := term.Snapshot()
	delta := term.Diff()

	for _, payload := range []Envelope{
		{Snapshot: &Snapshot{Seq: 1, S: snap}},
		{Delta: &Delta{Seq: 2, D: delta}},
	} {
		b, err := Encode(payload)
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		var out Envelope
		if err := Decode(b, &out); err != nil {
			t.Fatalf("Decode: %v", err)
		}
		switch {
		case payload.Snapshot != nil:
			if out.Snapshot == nil {
				t.Fatal("snapshot missing after roundtrip")
			}
			if out.Snapshot.S.Cells[0][0].Grapheme != "中" {
				t.Errorf("grapheme survived: got %q", out.Snapshot.S.Cells[0][0].Grapheme)
			}
			if out.Snapshot.S.Cells[0][0].Width != 2 {
				t.Errorf("wide-cell width survived: got %d", out.Snapshot.S.Cells[0][0].Width)
			}
		case payload.Delta != nil:
			if out.Delta == nil {
				t.Fatal("delta missing after roundtrip")
			}
			if out.Delta.D.Rows != delta.Rows || out.Delta.D.Cols != delta.Cols {
				t.Errorf("size mismatch: %+v vs %+v", out.Delta.D, delta)
			}
		}
	}
}

func TestControlPlaneRoundtrip(t *testing.T) {
	created := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	summaries := []SessionSummary{
		{ID: "s1", Name: "build", Rows: 24, Cols: 80, Created: created, Active: true, Watchers: 2},
		{ID: "s2", Name: "logs", Rows: 40, Cols: 132, Created: created, Active: false, Watchers: 0},
	}
	b, err := Encode(summaries)
	if err != nil {
		t.Fatal(err)
	}
	var out []SessionSummary
	if err := Decode(b, &out); err != nil {
		t.Fatal(err)
	}
	if len(out) != len(summaries) {
		t.Fatalf("len: got %d want %d", len(out), len(summaries))
	}
	for i := range summaries {
		if out[i].Name != summaries[i].Name || !out[i].Created.Equal(summaries[i].Created) {
			t.Errorf("summary %d roundtrip mismatch: got %+v want %+v", i, out[i], summaries[i])
		}
	}

	info := SessionInfo{ID: "s1", Name: "build", Token: []byte{0xde, 0xad}, UDPPort: 2201, Rows: 24, Cols: 80}
	b, err = Encode(info)
	if err != nil {
		t.Fatal(err)
	}
	var info2 SessionInfo
	if err := Decode(b, &info2); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(info.Token, info2.Token) || info.UDPPort != info2.UDPPort {
		t.Errorf("SessionInfo roundtrip: got %+v want %+v", info2, info)
	}
}

