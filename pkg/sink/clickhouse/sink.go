// Package clickhouse is a sink.LogSink that batches AccessLog and LLMCall
// records into ClickHouse (mcp_access_logs, llm_calls -- see migrate.go),
// off the request path, following the same async-bounded-queue,
// drop-on-overflow shape as pkg/sink/stdout. It also implements
// pkg/analytics.Reader over the same connection, which is what turns on
// the gateway's GET /api/v1/analytics/* routes (internal/api/handlers).
package clickhouse

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink"
)

// item is one queued record: exactly one of access/llm is set.
type item struct {
	access *sink.AccessLog
	llm    *sink.LLMCall
}

// Sink is a sink.LogSink (write side) and an analytics.Reader (read
// side, see reader.go) over one ClickHouse connection. The zero value is
// not usable; construct with New.
type Sink struct {
	c      conn        // used directly by the Reader methods (reader.go); nil in pure queue-engine unit tests
	writer batchWriter // real: chWriter{c}; fake in sink_test.go

	batchSize  int
	flushEvery time.Duration

	ch        chan item
	done      chan struct{}
	closeOnce sync.Once
	dropped   atomic.Uint64
	logger    *slog.Logger
}

// New opens a ClickHouse connection per cfg, pings it, applies the
// baseline migration (idempotent CREATE TABLE IF NOT EXISTS), and starts
// the sink's background batch-writer goroutine. Callers must call Close
// on shutdown to drain and flush whatever is still queued.
//
// The concrete *Sink returned also implements analytics.Reader and
// exposes Dropped() uint64; cmd/gateway/main.go recovers those via a type
// assertion on the sink.LogSink this returns, rather than this signature
// depending on either package -- the same seam pattern as pkg/ops.
func New(cfg Config, logger *slog.Logger) (sink.LogSink, error) {
	if logger == nil {
		logger = slog.Default()
	}
	cfg = cfg.withDefaults()

	c, err := clickhouse.Open(cfg.options())
	if err != nil {
		return nil, fmt.Errorf("clickhouse: open: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Ping(pingCtx); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("clickhouse: ping: %w", err)
	}

	migrateCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := applyMigrations(migrateCtx, c); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("clickhouse: apply baseline migration: %w", err)
	}

	return newSink(c, &chWriter{c: c}, cfg, logger), nil
}

// newSink wires up the queue engine over an already-connected conn and
// batchWriter. Split out from New so unit tests (sink_test.go) can drive
// it with a fake batchWriter and no real ClickHouse.
func newSink(c conn, w batchWriter, cfg Config, logger *slog.Logger) *Sink {
	cfg = cfg.withDefaults()
	s := &Sink{
		c:          c,
		writer:     w,
		batchSize:  cfg.BatchSize,
		flushEvery: cfg.FlushInterval,
		ch:         make(chan item, cfg.BufferSize),
		done:       make(chan struct{}),
		logger:     logger,
	}
	go s.run()
	return s
}

// WriteAccess enqueues a. Never blocks: if the queue is full, the record
// is dropped and Dropped's counter is incremented.
func (s *Sink) WriteAccess(a *sink.AccessLog) {
	s.enqueue(item{access: a})
}

// WriteLLMCall enqueues l, with the same drop-on-overflow behavior as
// WriteAccess.
func (s *Sink) WriteLLMCall(l *sink.LLMCall) {
	s.enqueue(item{llm: l})
}

func (s *Sink) enqueue(it item) {
	select {
	case s.ch <- it:
	default:
		s.dropped.Add(1)
	}
}

// Dropped returns the number of records dropped so far because the queue
// was full when they were written.
func (s *Sink) Dropped() uint64 {
	return s.dropped.Load()
}

// Close stops accepting new writes, flushes whatever is still queued or
// buffered, waits for the background goroutine to finish, and closes the
// underlying connection. Safe to call multiple times.
func (s *Sink) Close() error {
	s.closeOnce.Do(func() { close(s.ch) })
	<-s.done
	if s.c != nil {
		return s.c.Close()
	}
	return nil
}

// run is the sink's background writer goroutine: it accumulates queued
// records into per-type buffers, flushing them (one INSERT batch per
// type) whenever their combined size reaches batchSize or flushEvery
// elapses, whichever comes first. When the queue channel is closed
// (Close), it drains whatever is still buffered in the channel and does
// one final flush before returning.
func (s *Sink) run() {
	defer close(s.done)

	ticker := time.NewTicker(s.flushEvery)
	defer ticker.Stop()

	var accessBuf []*sink.AccessLog
	var llmBuf []*sink.LLMCall

	flush := func() {
		if len(accessBuf) == 0 && len(llmBuf) == 0 {
			return
		}
		ctx := context.Background()
		if len(accessBuf) > 0 {
			if err := s.writer.InsertAccess(ctx, accessBuf); err != nil {
				s.logger.Error("clickhouse sink: insert access logs failed", "error", err, "rows", len(accessBuf))
			}
			accessBuf = nil
		}
		if len(llmBuf) > 0 {
			if err := s.writer.InsertLLM(ctx, llmBuf); err != nil {
				s.logger.Error("clickhouse sink: insert llm calls failed", "error", err, "rows", len(llmBuf))
			}
			llmBuf = nil
		}
	}

loop:
	for {
		select {
		case it, ok := <-s.ch:
			if !ok {
				break loop
			}
			switch {
			case it.access != nil:
				accessBuf = append(accessBuf, it.access)
			case it.llm != nil:
				llmBuf = append(llmBuf, it.llm)
			}
			if len(accessBuf)+len(llmBuf) >= s.batchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
	flush()
}
