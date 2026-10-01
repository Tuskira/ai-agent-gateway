package fs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink"
)

func TestPutGet_RoundTrip(t *testing.T) {
	root := t.TempDir()
	s, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	req, resp := []byte(`{"model":"m"}`), []byte{0x00, 0x01, 0xff} // binary response (eventstream)

	ref, err := s.Put(ctx, "abcdef-1", req, resp)
	if err != nil {
		t.Fatal(err)
	}
	if ref != "fs://abcdef-1" {
		t.Errorf("ref = %q, want fs://abcdef-1", ref)
	}
	// Fan-out layout: <root>/<key[0:2]>/<key>.{request,response}, no temp files left.
	entries, _ := os.ReadDir(filepath.Join(root, "ab"))
	if len(entries) != 2 || entries[0].Name() != "abcdef-1.request" || entries[1].Name() != "abcdef-1.response" {
		t.Errorf("dir entries = %v, want exactly the .request and .response files", entries)
	}

	gotReq, gotResp, err := s.Get(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotReq, req) || !bytes.Equal(gotResp, resp) {
		t.Errorf("Get = %q / %q, want %q / %q", gotReq, gotResp, req, resp)
	}
}

// An empty body round-trips as empty, not as missing.
func TestPutGet_EmptyBody(t *testing.T) {
	s, _ := New(t.TempDir())
	ref, err := s.Put(context.Background(), "req-empty", nil, []byte("resp"))
	if err != nil {
		t.Fatal(err)
	}
	req, resp, err := s.Get(context.Background(), ref)
	if err != nil || len(req) != 0 || string(resp) != "resp" {
		t.Errorf("Get = %q / %q / %v", req, resp, err)
	}
}

// A second Put for the same key overwrites (atomic rename replaces).
func TestPut_Overwrite(t *testing.T) {
	s, _ := New(t.TempDir())
	ctx := context.Background()
	_, _ = s.Put(ctx, "same-key", []byte("a"), []byte("b"))
	ref, err := s.Put(ctx, "same-key", []byte("c"), []byte("d"))
	if err != nil {
		t.Fatal(err)
	}
	req, resp, _ := s.Get(ctx, ref)
	if string(req) != "c" || string(resp) != "d" {
		t.Errorf("Get = %q / %q, want c / d", req, resp)
	}
}

func TestGet_Missing(t *testing.T) {
	s, _ := New(t.TempDir())
	if _, _, err := s.Get(context.Background(), "fs://nope-123"); !errors.Is(err, sink.ErrBodyNotFound) {
		t.Errorf("err = %v, want ErrBodyNotFound", err)
	}
}

func TestInvalidKeysAndRefs(t *testing.T) {
	s, _ := New(t.TempDir())
	ctx := context.Background()
	for _, key := range []string{"", "a", "../etc", "ab/cd", `ab\cd`, ".hidden", "ab\x00c"} {
		if _, err := s.Put(ctx, key, nil, nil); err == nil {
			t.Errorf("Put(%q) succeeded, want error", key)
		}
	}
	if _, _, err := s.Get(ctx, "s3://bucket/key"); err == nil || errors.Is(err, sink.ErrBodyNotFound) {
		t.Errorf("Get(foreign ref) err = %v, want a non-NotFound error", err)
	}
	if _, err := New(" "); err == nil {
		t.Error("New(blank root) succeeded, want error")
	}
}
