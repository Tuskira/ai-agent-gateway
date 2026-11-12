package clickhouse

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink"
)

// fakeWriter is a batchWriter that just records every InsertAccess/
// InsertLLM call (and the rows it was given), optionally blocking on a
// release channel so a test can deterministically pin the sink's
// background goroutine mid-flush -- the same technique
// pkg/sink/stdout's test uses to test overflow behavior without racing
// the consumer.
type fakeWriter struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once

	mu          sync.Mutex
	calls       int // InsertAccess calls entered, blocked or not
	accessCalls [][]*sink.AccessLog
	llmCalls    [][]*sink.LLMCall
}

func newFakeWriter() *fakeWriter {
	return &fakeWriter{started: make(chan struct{}), release: make(chan struct{})}
}

// newBlockingFakeWriter is a fakeWriter whose first call blocks until the
// test closes release, signaling started the moment that call begins.
func newBlockingFakeWriter() *fakeWriter {
	return newFakeWriter()
}

func (w *fakeWriter) block() {
	w.once.Do(func() { close(w.started) })
	<-w.release
}

func (w *fakeWriter) entered() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.calls
}

func (w *fakeWriter) InsertAccess(_ context.Context, rows []*sink.AccessLog) error {
	w.mu.Lock()
	w.calls++
	w.mu.Unlock()
	if w.release != nil {
		select {
		case <-w.release:
		default:
			w.block()
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.accessCalls = append(w.accessCalls, rows)
	return nil
}

func (w *fakeWriter) InsertLLM(_ context.Context, rows []*sink.LLMCall) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.llmCalls = append(w.llmCalls, rows)
	return nil
}

func (w *fakeWriter) snapshot() (access [][]*sink.AccessLog, llm [][]*sink.LLMCall) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([][]*sink.AccessLog{}, w.accessCalls...), append([][]*sink.LLMCall{}, w.llmCalls...)
}

func totalAccessRows(calls [][]*sink.AccessLog) int {
	n := 0
	for _, c := range calls {
		n += len(c)
	}
	return n
}

func TestSink_FlushesOnBatchSize(t *testing.T) {
	w := newFakeWriter()
	close(w.release) // never actually block for this test
	s := newSink(nil, w, Config{BatchSize: 3, FlushInterval: time.Hour, BufferSize: 100}, nil)
	defer s.Close()

	for i := 0; i < 3; i++ {
		s.WriteAccess(&sink.AccessLog{RequestID: "r"})
	}

	deadline := time.After(2 * time.Second)
	for {
		access, _ := w.snapshot()
		if totalAccessRows(access) == 3 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("batch of 3 was never flushed; got %d rows across %d calls", totalAccessRows(access), len(access))
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestSink_FlushesOnInterval(t *testing.T) {
	w := newFakeWriter()
	close(w.release)
	s := newSink(nil, w, Config{BatchSize: 1000, FlushInterval: 20 * time.Millisecond, BufferSize: 100}, nil)
	defer s.Close()

	s.WriteAccess(&sink.AccessLog{RequestID: "only-one"})

	deadline := time.After(2 * time.Second)
	for {
		access, _ := w.snapshot()
		if totalAccessRows(access) == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("record was never flushed by the interval timer")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestSink_OverflowDropsAndCounts(t *testing.T) {
	w := newBlockingFakeWriter()
	// batchSize 1: every record is its own flush, and every flush blocks
	// inside InsertAccess until we release it. In async mode up to
	// maxInflightFlushes flushes wait at once; after that the run loop
	// holds at most BufferSize (1) record, the queue holds BufferSize (1)
	// more, and the rest are dropped.
	s := newSink(nil, w, Config{BatchSize: 1, FlushInterval: time.Hour, BufferSize: 1}, nil)

	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(2 * time.Millisecond)
		}
	}

	n := 0
	for ; n < maxInflightFlushes; n++ {
		s.WriteAccess(&sink.AccessLog{RequestID: "inflight"})
		want := n + 1
		waitFor("flush to start", func() bool { return w.entered() == want })
	}
	// One more is taken off the queue into the run loop's batch, which
	// is then full (every flush slot busy)...
	s.WriteAccess(&sink.AccessLog{RequestID: "held"})
	waitFor("queue to drain", func() bool { return len(s.ch) == 0 })
	// ...so this one stays in the queue, and the next two are dropped.
	s.WriteAccess(&sink.AccessLog{RequestID: "queued"})
	s.WriteAccess(&sink.AccessLog{RequestID: "dropped-1"})
	s.WriteAccess(&sink.AccessLog{RequestID: "dropped-2"})
	if got := s.Dropped(); got != 2 {
		t.Fatalf("Dropped() = %d, want exactly 2", got)
	}
	if w.entered() != maxInflightFlushes {
		t.Fatalf("flushes started = %d, want %d (held records must wait for a free slot)", w.entered(), maxInflightFlushes)
	}

	close(w.release)
	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if got := s.Dropped(); got != 2 {
		t.Fatalf("Dropped() after Close() = %d, want unchanged 2", got)
	}
	access, _ := w.snapshot()
	if got := totalAccessRows(access); got != maxInflightFlushes+2 {
		t.Fatalf("rows written = %d, want %d (every record not dropped)", got, maxInflightFlushes+2)
	}
}

// In sync mode only one flush runs at a time, as before async inserts;
// records arriving meanwhile are batched for the next one.
func TestSink_SyncModeOneFlushAtATime(t *testing.T) {
	w := newBlockingFakeWriter()
	s := newSink(nil, w, Config{BatchSize: 1, FlushInterval: time.Hour, BufferSize: 10, SyncInsert: true}, nil)

	s.WriteAccess(&sink.AccessLog{RequestID: "1"})
	select {
	case <-w.started:
	case <-time.After(5 * time.Second):
		t.Fatal("writer never started")
	}
	s.WriteAccess(&sink.AccessLog{RequestID: "2"})
	s.WriteAccess(&sink.AccessLog{RequestID: "3"})
	time.Sleep(50 * time.Millisecond)
	if got := w.entered(); got != 1 {
		t.Fatalf("flushes started while one is in flight = %d, want 1", got)
	}
	close(w.release)
	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	access, _ := w.snapshot()
	if len(access) != 2 || len(access[0]) != 1 || len(access[1]) != 2 {
		t.Fatalf("flush batches = %v, want [1 record] then [2 records]", batchSizes(access))
	}
}

func batchSizes(calls [][]*sink.AccessLog) []int {
	var out []int
	for _, c := range calls {
		out = append(out, len(c))
	}
	return out
}

func TestSink_CloseFlushesBufferedRecords(t *testing.T) {
	w := newFakeWriter()
	close(w.release)
	// Large batch size and flush interval so nothing flushes on its own;
	// only Close's final flush should deliver these rows.
	s := newSink(nil, w, Config{BatchSize: 1000, FlushInterval: time.Hour, BufferSize: 100}, nil)

	s.WriteAccess(&sink.AccessLog{RequestID: "a"})
	s.WriteAccess(&sink.AccessLog{RequestID: "b"})
	s.WriteLLMCall(&sink.LLMCall{RequestID: "c"})

	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	access, llm := w.snapshot()
	if totalAccessRows(access) != 2 {
		t.Errorf("access rows flushed on close = %d, want 2", totalAccessRows(access))
	}
	llmRows := 0
	for _, c := range llm {
		llmRows += len(c)
	}
	if llmRows != 1 {
		t.Errorf("llm rows flushed on close = %d, want 1", llmRows)
	}
}

func TestSink_CloseIsIdempotent(t *testing.T) {
	w := newFakeWriter()
	close(w.release)
	s := newSink(nil, w, Config{BatchSize: 10, FlushInterval: time.Hour, BufferSize: 10}, nil)
	if err := s.Close(); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
}

func TestConfig_WithDefaults(t *testing.T) {
	c := Config{}.withDefaults()
	if c.Port != defaultPort || c.BatchSize != defaultBatchSize || c.FlushInterval != defaultFlushInterval || c.BufferSize != defaultBufferSize || c.Database != defaultDatabase ||
		c.AsyncInsertBusyTimeout != defaultAsyncInsertBusyTimeout || c.SyncInsert {
		t.Errorf("withDefaults() = %+v", c)
	}

	custom := Config{Port: 1, BatchSize: 2, FlushInterval: time.Second, BufferSize: 3, Database: "d"}.withDefaults()
	if custom.Port != 1 || custom.BatchSize != 2 || custom.FlushInterval != time.Second || custom.BufferSize != 3 || custom.Database != "d" {
		t.Errorf("withDefaults() overrode explicit values: %+v", custom)
	}
}

// decCost keeps an unknown cost NULL and rounds (not truncates) to the
// column's 12 places.
func TestDecCost(t *testing.T) {
	if decCost(nil) != nil {
		t.Error("decCost(nil) must be nil (NULL), not 0")
	}
	inexact := (10*0.06 + 3*0.24) / 1e6 // 1.3199999999999998e-06
	if got := decCost(&inexact).String(); got != "0.00000132" {
		t.Errorf("decCost(%v) = %s, want 0.00000132", inexact, got)
	}
}
