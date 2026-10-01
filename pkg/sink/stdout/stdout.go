// Package stdout is a sink.LogSink that writes AccessLog and LLMCall
// records as JSON lines to an io.Writer (typically os.Stdout). Writes are
// asynchronous: WriteAccess/WriteLLMCall enqueue onto a bounded channel and
// return immediately, so the request path is never blocked by log I/O. If
// the channel is full, the record is dropped and counted rather than
// blocking the caller or growing without bound.
package stdout

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"sync/atomic"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink"
)

// defaultQueueSize is used when New is called without WithQueueSize.
const defaultQueueSize = 1024

// line is the JSON shape written for every record.
type line struct {
	Type   string `json:"type"` // "access" | "llm_call"
	Record any    `json:"record"`
}

// Sink writes sink.AccessLog and sink.LLMCall records as JSON lines to an
// io.Writer, off the request path. The zero value is not usable; construct
// with New.
type Sink struct {
	w         io.Writer
	queueSize int
	bodies    bool

	ch        chan line
	done      chan struct{}
	closeOnce sync.Once
	dropped   atomic.Uint64
}

// Option configures a Sink at construction time.
type Option func(*Sink)

// WithQueueSize sets the bounded channel's capacity. Records written once
// the channel is full are dropped (see Dropped) rather than blocking the
// caller. n <= 0 falls back to the default.
func WithQueueSize(n int) Option {
	return func(s *Sink) {
		if n > 0 {
			s.queueSize = n
		}
	}
}

// WithBodies makes the sink print captured request/response bodies (and
// parsed messages/system/tools). Off by default: bodies are customer prompts
// and completions and must not land in container logs unless asked for.
func WithBodies(on bool) Option {
	return func(s *Sink) { s.bodies = on }
}

// New returns a Sink writing to w and starts its background writer
// goroutine. Callers must call Close when done to drain and stop it.
func New(w io.Writer, opts ...Option) *Sink {
	s := &Sink{w: w, queueSize: defaultQueueSize, done: make(chan struct{})}
	for _, opt := range opts {
		opt(s)
	}
	s.ch = make(chan line, s.queueSize)

	go s.run()
	return s
}

// WriteAccess enqueues a as a JSON line. It never blocks: if the queue is
// full, the record is dropped and Dropped's counter is incremented.
func (s *Sink) WriteAccess(a *sink.AccessLog) {
	if !s.bodies && a != nil && (a.RequestBody != nil || a.ResponseBody != nil) {
		c := *a // copy: other sinks share the original record
		c.RequestBody, c.ResponseBody = nil, nil
		a = &c
	}
	s.enqueue(line{Type: "access", Record: a})
}

// WriteLLMCall enqueues l as a JSON line, with the same drop-on-overflow
// behavior as WriteAccess.
func (s *Sink) WriteLLMCall(l *sink.LLMCall) {
	if !s.bodies && l != nil {
		c := *l // copy: other sinks share the original record
		c.RequestBody, c.ResponseBody = nil, nil
		c.Messages, c.System, c.Tools = nil, nil, nil
		l = &c
	}
	s.enqueue(line{Type: "llm_call", Record: l})
}

// WriteBatch lets the stdout sink serve as a best-effort sink.BatchSink — the
// zero-dependency dev fallback when neither Postgres nor ClickHouse is
// configured. It writes each call as a JSON line and always returns nil; it is
// not durable, so use the Postgres sink for lossless capture.
func (s *Sink) WriteBatch(_ context.Context, calls []*sink.LLMCall) error {
	for _, c := range calls {
		s.WriteLLMCall(c)
	}
	return nil
}

func (s *Sink) enqueue(rec line) {
	select {
	case s.ch <- rec:
	default:
		s.dropped.Add(1)
	}
}

// Dropped returns the number of records dropped so far because the queue
// was full when they were written.
func (s *Sink) Dropped() uint64 {
	return s.dropped.Load()
}

// Close stops accepting new writes' underlying goroutine after draining
// whatever is already queued, and waits for it to finish. It is safe to
// call multiple times.
func (s *Sink) Close() error {
	s.closeOnce.Do(func() { close(s.ch) })
	<-s.done
	return nil
}

func (s *Sink) run() {
	defer close(s.done)
	enc := json.NewEncoder(s.w)
	for rec := range s.ch {
		// Best-effort: a write/encode failure on the log sink must
		// never propagate back and disrupt the request path.
		_ = enc.Encode(rec)
	}
}
