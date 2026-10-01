package capture

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink/bodystore/fs"
)

type failingBatch struct{ err error }

func (f failingBatch) WriteBatch(context.Context, []*sink.LLMCall) error { return f.err }
func (failingBatch) Close() error                                        { return nil }

type countingLog struct{ n int }

func (c *countingLog) WriteAccess(*sink.AccessLog) {}
func (c *countingLog) WriteLLMCall(*sink.LLMCall)  { c.n++ }
func (c *countingLog) Close() error                { return nil }

// The analytics tee still receives the call when the durable write fails, and
// the durable error is still returned to the caller.
func TestRecord_TeesEvenWhenDurableWriteFails(t *testing.T) {
	for _, dbErr := range []error{nil, errors.New("pg down")} {
		logs := &countingLog{}
		err := NewRecorder(failingBatch{dbErr}, logs, nil, 0).Record(context.Background(), &sink.LLMCall{RequestID: "r"})
		if (err != nil) != (dbErr != nil) || logs.n != 1 {
			t.Errorf("dbErr=%v: err=%v tee writes=%d, want 1", dbErr, err, logs.n)
		}
	}
}

// keptBatch is a BatchSink that keeps what it was given, standing in for
// Postgres so the tests can inspect the row as written.
type keptBatch struct{ calls []*sink.LLMCall }

func (k *keptBatch) WriteBatch(_ context.Context, c []*sink.LLMCall) error {
	k.calls = append(k.calls, c...)
	return nil
}
func (*keptBatch) Close() error { return nil }

func newFSStore(t *testing.T) (*fs.Store, string) {
	t.Helper()
	root := t.TempDir()
	s, err := fs.New(root)
	if err != nil {
		t.Fatal(err)
	}
	return s, root
}

func captureCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// Bodies over inline_max go to the store; the row keeps only the ref, and the
// ref resolves back to the original bytes. Derived summaries stay inline.
func TestRecord_OffloadsBodies(t *testing.T) {
	store, _ := newFSStore(t)
	batch := &keptBatch{}
	rec := NewRecorder(batch, nil, store, 0)
	req, resp := []byte(`{"messages":[]}`), []byte("\x00eventstream")

	c := &sink.LLMCall{RequestID: "req-offload", RequestBody: req, ResponseBody: resp, Messages: []byte("[]")}
	if err := rec.Record(captureCtx(t), c); err != nil {
		t.Fatal(err)
	}
	row := batch.calls[0]
	if row.BodyRef != "fs://req-offload" || row.RequestBody != nil || row.ResponseBody != nil || string(row.Messages) != "[]" {
		t.Errorf("row = ref %q req %q resp %q messages %q", row.BodyRef, row.RequestBody, row.ResponseBody, row.Messages)
	}
	gotReq, gotResp, err := store.Get(context.Background(), row.BodyRef)
	if err != nil || !bytes.Equal(gotReq, req) || !bytes.Equal(gotResp, resp) {
		t.Errorf("store.Get = %q / %q / %v", gotReq, gotResp, err)
	}
	if rec.Offloaded() != 1 || rec.Fallbacks() != 0 {
		t.Errorf("offloaded=%d fallbacks=%d, want 1/0", rec.Offloaded(), rec.Fallbacks())
	}
}

// Small calls (total <= inline_max) and body-less calls never touch the store.
func TestRecord_InlineBelowThresholdOrNoBody(t *testing.T) {
	store, root := newFSStore(t)
	batch := &keptBatch{}
	rec := NewRecorder(batch, nil, store, 10)

	for _, c := range []*sink.LLMCall{
		{RequestID: "req-small", RequestBody: []byte("12345"), ResponseBody: []byte("12345")}, // exactly 10
		{RequestID: "req-nobody"},
	} {
		if err := rec.Record(captureCtx(t), c); err != nil {
			t.Fatal(err)
		}
	}
	if batch.calls[0].BodyRef != "" || string(batch.calls[0].RequestBody) != "12345" || batch.calls[1].BodyRef != "" {
		t.Errorf("rows = %+v / %+v, want inline, no ref", batch.calls[0], batch.calls[1])
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 || rec.Offloaded() != 0 {
		t.Errorf("store touched: %d entries, offloaded=%d", len(entries), rec.Offloaded())
	}
}

// A failing store (its root replaced by a regular file, so every write fails)
// never loses the record: bodies stay inline and the fallback is counted.
func TestRecord_OffloadFailureFallsBackInline(t *testing.T) {
	store, root := newFSStore(t)
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	batch := &keptBatch{}
	logs := &countingLog{}
	rec := NewRecorder(batch, logs, store, 0)

	c := &sink.LLMCall{RequestID: "req-fallback", RequestBody: []byte("req"), ResponseBody: []byte("resp")}
	if err := rec.Record(captureCtx(t), c); err != nil {
		t.Fatalf("Record = %v, want nil (offload failure must not fail the record)", err)
	}
	row := batch.calls[0]
	if row.BodyRef != "" || string(row.RequestBody) != "req" || string(row.ResponseBody) != "resp" {
		t.Errorf("row = ref %q req %q resp %q, want inline bodies", row.BodyRef, row.RequestBody, row.ResponseBody)
	}
	if rec.Offloaded() != 0 || rec.Fallbacks() != 1 || logs.n != 1 {
		t.Errorf("offloaded=%d fallbacks=%d tee=%d, want 0/1/1", rec.Offloaded(), rec.Fallbacks(), logs.n)
	}
}
