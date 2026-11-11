package shimproto

import (
	"bufio"
	"bytes"
	"testing"
	"time"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, TypeOutput, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	want := Info{Version: Version, ID: "pty-X", Pid: 42, Rows: 24, Cols: 80,
		Started: time.Unix(1700000000, 0).UTC(), Meta: []byte{0, 1, 2}}
	if err := WriteJSON(&buf, TypeInfo, want); err != nil {
		t.Fatal(err)
	}
	if err := WriteFrame(&buf, TypeAttach, nil); err != nil {
		t.Fatal(err)
	}

	r := bufio.NewReader(&buf)
	typ, p, err := ReadFrame(r)
	if err != nil || typ != TypeOutput || string(p) != "hello" {
		t.Fatalf("frame 1 = %v %q %v", typ, p, err)
	}
	typ, p, err = ReadFrame(r)
	if err != nil || typ != TypeInfo {
		t.Fatalf("frame 2 = %v %v", typ, err)
	}
	var got Info
	if err := Decode(p, &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != want.ID || got.Pid != want.Pid || !got.Started.Equal(want.Started) || !bytes.Equal(got.Meta, want.Meta) {
		t.Errorf("Info = %+v, want %+v", got, want)
	}
	typ, p, err = ReadFrame(r)
	if err != nil || typ != TypeAttach || len(p) != 0 {
		t.Fatalf("frame 3 = %v %q %v", typ, p, err)
	}
}

func TestDecodeIgnoresUnknownFields(t *testing.T) {
	var r Resize
	if err := Decode([]byte(`{"rows":3,"cols":4,"fromTheFuture":true}`), &r); err != nil {
		t.Fatal(err)
	}
	if r.Rows != 3 || r.Cols != 4 {
		t.Errorf("Resize = %+v", r)
	}
}

func TestReadFrameRejectsHuge(t *testing.T) {
	r := bufio.NewReader(bytes.NewReader([]byte{byte(TypeOutput), 0xff, 0xff, 0xff, 0xff}))
	if _, _, err := ReadFrame(r); err != ErrTooLarge {
		t.Errorf("err = %v, want ErrTooLarge", err)
	}
}
