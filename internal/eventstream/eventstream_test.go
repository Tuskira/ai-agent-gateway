package eventstream

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	h := map[string]string{":message-type": "event", ":event-type": "chunk"}
	a, b := Encode(h, []byte(`{"a":1}`)), Encode(h, []byte(`{"b":2}`))
	if !bytes.Equal(a, Encode(h, []byte(`{"a":1}`))) {
		t.Fatal("Encode is not deterministic")
	}
	// Delivered in small pieces: frames still decode.
	r := NewReader(io.MultiReader(bytes.NewReader(a[:5]), bytes.NewReader(a[5:]), bytes.NewReader(b)))
	for _, want := range []string{`{"a":1}`, `{"b":2}`} {
		m, err := r.Next()
		if err != nil || string(m.Payload) != want || m.Headers[":event-type"] != "chunk" {
			t.Fatalf("Next = %+v, %v; want payload %s", m, err, want)
		}
	}
	if _, err := r.Next(); err != io.EOF {
		t.Fatalf("after the last frame: %v, want io.EOF", err)
	}
}

func TestErrors(t *testing.T) {
	frame := Encode(map[string]string{":message-type": "event"}, []byte(`{}`))
	for name, c := range map[string]struct {
		in      []byte
		want    string
		cutting bool
	}{
		"cut prelude": {frame[:7], "truncated frame prelude", true},
		"cut body":    {frame[:len(frame)-1], "truncated frame body", true},
		"bad crc":     {append(append([]byte{}, frame[:len(frame)-1]...), frame[len(frame)-1]^0xff), "message CRC mismatch", false},
	} {
		_, err := NewReader(bytes.NewReader(c.in)).Next()
		if err == nil || !strings.Contains(err.Error(), c.want) || errors.Is(err, io.ErrUnexpectedEOF) != c.cutting {
			t.Errorf("%s: err = %v, want %q (unexpected EOF: %v)", name, err, c.want, c.cutting)
		}
	}
}
