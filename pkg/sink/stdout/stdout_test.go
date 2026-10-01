package stdout

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink"
)

// blockingWriter blocks every Write on release, and signals started (once)
// the moment the first Write begins. It lets tests deterministically pin
// the sink's background goroutine mid-write so queue-full behavior can be
// observed without racing the consumer.
type blockingWriter struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once

	mu  sync.Mutex
	buf bytes.Buffer
}

func newBlockingWriter() *blockingWriter {
	return &blockingWriter{started: make(chan struct{}), release: make(chan struct{})}
}

func (w *blockingWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	<-w.release
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *blockingWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func TestSink_OverflowDropsAndCounts(t *testing.T) {
	w := newBlockingWriter()
	s := New(w, WithQueueSize(1))

	// The consumer goroutine dequeues this immediately and blocks inside
	// Write, so the queue (capacity 1) is free again by the time started
	// fires.
	s.WriteAccess(&sink.AccessLog{RequestID: "1"})
	select {
	case <-w.started:
	case <-time.After(5 * time.Second):
		t.Fatal("consumer never started writing the first record")
	}

	s.WriteAccess(&sink.AccessLog{RequestID: "2"}) // fills the now-empty queue
	s.WriteAccess(&sink.AccessLog{RequestID: "3"}) // queue full: dropped
	s.WriteAccess(&sink.AccessLog{RequestID: "4"}) // queue full: dropped

	if got, want := s.Dropped(), uint64(2); got != want {
		t.Fatalf("Dropped() = %d, want %d", got, want)
	}

	close(w.release) // let the consumer finish record 1, then drain record 2

	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	// Dropped count must not change after drain/close.
	if got, want := s.Dropped(), uint64(2); got != want {
		t.Fatalf("Dropped() after Close() = %d, want %d", got, want)
	}

	out := w.String()
	if strings.Count(out, "\"request_id\":\"1\"") != 1 {
		t.Errorf("expected record 1 written exactly once, got: %s", out)
	}
	if strings.Count(out, "\"request_id\":\"2\"") != 1 {
		t.Errorf("expected record 2 written exactly once, got: %s", out)
	}
	if strings.Contains(out, "\"request_id\":\"3\"") || strings.Contains(out, "\"request_id\":\"4\"") {
		t.Errorf("dropped records 3/4 should never reach the writer, got: %s", out)
	}
}

func TestSink_WritesJSONLines(t *testing.T) {
	var buf bytes.Buffer
	s := New(&buf)

	s.WriteAccess(&sink.AccessLog{TenantID: "t1", Method: "tools/call", StatusCode: 200})
	s.WriteLLMCall(&sink.LLMCall{TenantID: "t1", Provider: "anthropic", Model: "claude"})

	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %q", len(lines), buf.String())
	}

	var first struct {
		Type   string         `json:"type"`
		Record sink.AccessLog `json:"record"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("unmarshal line 0: %v", err)
	}
	if first.Type != "access" || first.Record.Method != "tools/call" {
		t.Errorf("line 0 = %+v", first)
	}

	var second struct {
		Type   string       `json:"type"`
		Record sink.LLMCall `json:"record"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatalf("unmarshal line 1: %v", err)
	}
	if second.Type != "llm_call" || second.Record.Model != "claude" {
		t.Errorf("line 1 = %+v", second)
	}
}

func TestSink_CloseIsIdempotent(t *testing.T) {
	var buf bytes.Buffer
	s := New(&buf)
	if err := s.Close(); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
}

func TestWithQueueSize_NonPositiveKeepsDefault(t *testing.T) {
	s := &Sink{queueSize: defaultQueueSize}
	WithQueueSize(0)(s)
	if s.queueSize != defaultQueueSize {
		t.Errorf("queueSize = %d, want default %d", s.queueSize, defaultQueueSize)
	}
	WithQueueSize(-5)(s)
	if s.queueSize != defaultQueueSize {
		t.Errorf("queueSize = %d, want default %d", s.queueSize, defaultQueueSize)
	}
}

func TestSink_BodiesOnlyWhenAsked(t *testing.T) {
	call := func() *sink.LLMCall {
		return &sink.LLMCall{RequestID: "r", RequestBody: []byte("SECRET-REQ"), ResponseBody: []byte("SECRET-RESP"),
			Messages: []byte(`["SECRET-MSG"]`), System: []byte(`"SECRET-SYS"`), Tools: []byte(`["SECRET-TOOL"]`)}
	}
	for _, on := range []bool{false, true} {
		var buf bytes.Buffer
		s := New(&buf, WithBodies(on))
		orig := call()
		s.WriteLLMCall(orig)
		s.WriteAccess(&sink.AccessLog{RequestBody: []byte("SECRET-REQ"), ResponseBody: []byte("SECRET-RESP")})
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(buf.String(), "request_body"); got != on {
			t.Errorf("bodies=%v: output has body fields = %v: %s", on, got, buf.String())
		}
		// The shared record other sinks persist must never be mutated.
		if string(orig.RequestBody) != "SECRET-REQ" || orig.Messages == nil {
			t.Errorf("bodies=%v: original record was mutated", on)
		}
	}
}
