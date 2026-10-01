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

func (w *fakeWriter) InsertAccess(_ context.Context, rows []*sink.AccessLog) error {
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
	// batchSize 1 so the very first write triggers a flush that blocks
	// inside InsertAccess until we release it.
	s := newSink(nil, w, Config{BatchSize: 1, FlushInterval: time.Hour, BufferSize: 1}, nil)

	s.WriteAccess(&sink.AccessLog{RequestID: "1"}) // triggers the blocking flush
	select {
	case <-w.started:
	case <-time.After(5 * time.Second):
		t.Fatal("writer never started its blocking insert")
	}

	// The run loop is now blocked inside InsertAccess for record 1, so
	// nothing is draining the channel (capacity 1): record 2 fills it,
	// and records 3 and 4 both find it full and are dropped.
	s.WriteAccess(&sink.AccessLog{RequestID: "2"})
	s.WriteAccess(&sink.AccessLog{RequestID: "3"})
	s.WriteAccess(&sink.AccessLog{RequestID: "4"})

	deadline := time.After(2 * time.Second)
	for s.Dropped() < 2 {
		select {
		case <-deadline:
			t.Fatalf("Dropped() = %d, want 2 (records 3 and 4)", s.Dropped())
		case <-time.After(5 * time.Millisecond):
		}
	}
	if got := s.Dropped(); got != 2 {
		t.Fatalf("Dropped() = %d, want exactly 2", got)
	}

	close(w.release)
	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if got := s.Dropped(); got != 2 {
		t.Fatalf("Dropped() after Close() = %d, want unchanged 2", got)
	}
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
	if c.Port != defaultPort || c.BatchSize != defaultBatchSize || c.FlushInterval != defaultFlushInterval || c.BufferSize != defaultBufferSize || c.Database != defaultDatabase {
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
