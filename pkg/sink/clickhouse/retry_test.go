package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink"
)

func TestRetryable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"too many parts", &clickhouse.Exception{Code: 252}, true},
		{"memory limit, wrapped", fmt.Errorf("send: %w", &clickhouse.Exception{Code: 241}), true},
		{"too many simultaneous queries", &clickhouse.Exception{Code: 202}, true},
		{"table read-only", &clickhouse.Exception{Code: 242}, true},
		{"unknown table", &clickhouse.Exception{Code: 60}, false},
		{"type mismatch", &clickhouse.Exception{Code: 53}, false},
		{"async wait timed out (rows still queued)", &clickhouse.Exception{Code: 159}, false},
		{"permanent exception before sending", fmt.Errorf("%w: %w", errNotSent, &clickhouse.Exception{Code: 60}), false},
		{"dial failure before sending", fmt.Errorf("%w: %w", errNotSent, errors.New("dial tcp: connection refused")), true},
		{"deadline before sending", fmt.Errorf("%w: %w", errNotSent, context.DeadlineExceeded), false},
		{"deadline while waiting", context.DeadlineExceeded, false},
		{"canceled", fmt.Errorf("x: %w", context.Canceled), false},
		{"connection broke while sending", io.ErrUnexpectedEOF, false},
		{"append conversion error", errors.New("converting string to UInt16 is unsupported"), false},
	}
	for _, c := range cases {
		if got := retryable(c.err); got != c.want {
			t.Errorf("%s: retryable(%v) = %v, want %v", c.name, c.err, got, c.want)
		}
	}
}

// scriptedWriter fails InsertAccess with errs[i] on call i (nil = success,
// past the end = success) and records the rows of each successful call.
type scriptedWriter struct {
	mu    sync.Mutex
	errs  []error
	calls int
	rows  int
}

func (w *scriptedWriter) InsertAccess(_ context.Context, rows []*sink.AccessLog) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	i := w.calls
	w.calls++
	if i < len(w.errs) && w.errs[i] != nil {
		return w.errs[i]
	}
	w.rows += len(rows)
	return nil
}

func (w *scriptedWriter) InsertLLM(context.Context, []*sink.LLMCall) error { return nil }

func TestSink_RetriesOnceOnlyWhenSafe(t *testing.T) {
	transient := &clickhouse.Exception{Code: 252, Message: "too many parts"}
	cases := []struct {
		name      string
		errs      []error
		wantCalls int
		wantRows  int
	}{
		{"success first time", nil, 1, 2},
		{"transient then success", []error{transient}, 2, 2},
		{"nothing sent then success", []error{fmt.Errorf("%w: %w", errNotSent, errors.New("connection refused"))}, 2, 2},
		{"transient twice: one retry only", []error{transient, transient}, 2, 0},
		{"permanent: no retry", []error{&clickhouse.Exception{Code: 60}}, 1, 0},
		{"ambiguous send failure: no retry", []error{io.ErrUnexpectedEOF}, 1, 0},
		{"deadline: no retry", []error{context.DeadlineExceeded}, 1, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := &scriptedWriter{errs: c.errs}
			s := newSink(nil, w, Config{BatchSize: 1000, FlushInterval: time.Hour, BufferSize: 10}, nil)
			s.retryDelay = time.Millisecond
			s.flush(batch{access: []*sink.AccessLog{{RequestID: "a"}, {RequestID: "b"}}})
			if err := s.Close(); err != nil {
				t.Fatalf("Close() error = %v", err)
			}
			if w.calls != c.wantCalls || w.rows != c.wantRows {
				t.Errorf("calls = %d, rows written = %d; want %d, %d", w.calls, w.rows, c.wantCalls, c.wantRows)
			}
		})
	}
}

// Close's final flush retries too, but never past closeFlushTimeout.
func TestSink_CloseFlushRetries(t *testing.T) {
	w := &scriptedWriter{errs: []error{&clickhouse.Exception{Code: 241}}}
	s := newSink(nil, w, Config{BatchSize: 1000, FlushInterval: time.Hour, BufferSize: 10}, nil)
	s.retryDelay = time.Millisecond
	s.WriteAccess(&sink.AccessLog{RequestID: "a"})
	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if w.calls != 2 || w.rows != 1 {
		t.Errorf("calls = %d, rows = %d; want 2, 1", w.calls, w.rows)
	}
}

func TestAsyncSettings(t *testing.T) {
	st := asyncSettings(7*time.Second, true)
	want := clickhouse.Settings{
		"async_insert": 1, "wait_for_async_insert": 1, "async_insert_busy_timeout_ms": int64(7000),
		"async_insert_use_adaptive_busy_timeout": 0,
	}
	if fmt.Sprint(st) != fmt.Sprint(want) {
		t.Errorf("asyncSettings(7s, adaptive known) = %v, want %v", st, want)
	}
	if _, ok := asyncSettings(time.Second, false)["async_insert_use_adaptive_busy_timeout"]; ok {
		t.Error("asyncSettings must not send async_insert_use_adaptive_busy_timeout to a server that lacks it")
	}
}

func TestNewSink_ModeDefaults(t *testing.T) {
	w := &scriptedWriter{}
	a := newSink(nil, w, Config{}, nil)
	defer a.Close()
	if !a.async || a.maxInflight != maxInflightFlushes || a.insertTimeout != defaultAsyncInsertBusyTimeout+insertTimeoutSlack {
		t.Errorf("async defaults: async=%v maxInflight=%d insertTimeout=%s", a.async, a.maxInflight, a.insertTimeout)
	}
	sy := newSink(nil, w, Config{SyncInsert: true}, nil)
	defer sy.Close()
	if sy.async || sy.maxInflight != 1 || sy.settings != nil || sy.insertTimeout != insertTimeoutSlack {
		t.Errorf("sync defaults: async=%v maxInflight=%d settings=%v insertTimeout=%s", sy.async, sy.maxInflight, sy.settings, sy.insertTimeout)
	}
}
