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
	// maxPending caps the records the run loop holds while every flush
	// slot is busy; past it the loop stops draining ch, so ch fills and
	// further writes are dropped (and counted) as before.
	maxPending int
	// async: background flushes use ClickHouse asynchronous inserts with
	// settings (asyncSettings) and up to maxInflight of them run at once.
	async       bool
	settings    clickhouse.Settings
	maxInflight int
	// insertTimeout bounds one background insert attempt; retryDelay is
	// the pause before its single retry (see retryable).
	insertTimeout time.Duration
	retryDelay    time.Duration

	ch        chan item
	done      chan struct{}
	closeOnce sync.Once
	dropped   atomic.Uint64
	logger    *slog.Logger
}

// New opens a ClickHouse connection per cfg, pings it, makes sure the
// sink's schema is in place (ensureSchema: a single ledger SELECT when the
// current schema is already recorded -- normally by `gateway migrate` --
// else the migrations themselves, with retry), and starts the sink's
// background batch-writer goroutine. Callers must call Close on shutdown
// to drain and flush whatever is still queued.
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

	c, err := open(cfg)
	if err != nil {
		return nil, err
	}

	migrateCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := ensureSchema(migrateCtx, c, cfg.Version, logger); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("clickhouse: apply baseline migration: %w", err)
	}

	if !cfg.SyncInsert {
		probeCtx, cancelProbe := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelProbe()
		cfg.adaptiveBusyTimeoutKnown = supportsAdaptiveBusyTimeout(probeCtx, c, logger)
		logger.Info("clickhouse sink: asynchronous inserts on", "busy_timeout", cfg.AsyncInsertBusyTimeout, "flush_interval", cfg.FlushInterval)
	}
	return newSink(c, &chWriter{c: c}, cfg, logger), nil
}

// maxInflightFlushes is how many background flushes may wait on
// ClickHouse at once in async mode. With wait_for_async_insert=1 an
// insert returns only once the server has written the buffer it joined,
// up to the busy timeout later, so flushes overlap rather than queue
// behind each other; records keep accumulating in the run loop meanwhile
// and the next free slot takes all of them as one batch.
const maxInflightFlushes = 4

const (
	// insertTimeoutSlack is added to the busy timeout to bound one async
	// insert attempt (a sync attempt gets the slack alone).
	insertTimeoutSlack = 10 * time.Second
	// closeFlushTimeout bounds Close's final, synchronous flush of what is
	// still buffered, retry included, so shutdown stays well inside a
	// Kubernetes termination grace period of 30s.
	closeFlushTimeout = 10 * time.Second
	defaultRetryDelay = time.Second
)

// asyncSettings are the per-insert settings of an async-mode flush:
// async_insert so ClickHouse merges small inserts from every replica into
// one part per busy timeout, wait_for_async_insert so an insert returns
// only once its rows are written (an error still means the rows were not,
// as with a synchronous insert), and the busy timeout itself. The server's
// adaptive busy timeout (ClickHouse 24.2+, on by default) shrinks the wait
// towards its 50ms minimum when inserts are sparse -- exactly the light
// traffic this is meant for -- so it is switched off when the server
// knows the setting. async_insert_busy_timeout_ms is the name every
// version accepts (an alias of async_insert_busy_timeout_max_ms on 24.2+).
func asyncSettings(busyTimeout time.Duration, adaptiveKnown bool) clickhouse.Settings {
	st := clickhouse.Settings{
		"async_insert":                 1,
		"wait_for_async_insert":        1,
		"async_insert_busy_timeout_ms": busyTimeout.Milliseconds(),
	}
	if adaptiveKnown {
		st["async_insert_use_adaptive_busy_timeout"] = 0
	}
	return st
}

// supportsAdaptiveBusyTimeout reports whether the server has the
// async_insert_use_adaptive_busy_timeout setting: sending a setting the
// server doesn't know fails the insert (UNKNOWN_SETTING), so it is only
// sent where it exists.
func supportsAdaptiveBusyTimeout(ctx context.Context, c conn, logger *slog.Logger) bool {
	var n uint64
	if err := c.QueryRow(ctx, "SELECT count() FROM system.settings WHERE name = 'async_insert_use_adaptive_busy_timeout'").Scan(&n); err != nil {
		logger.Warn("clickhouse sink: could not check for the adaptive async-insert busy timeout; not overriding it", "error", err)
		return false
	}
	return n > 0
}

// Migrate applies the sink's whole schema to cfg's database, with retry,
// and records it in the schema ledger, so gateways started afterwards
// issue no DDL. Unlike New it always applies, even when the schema is
// already recorded: it is the one-shot `gateway migrate` step, run once
// per release ahead of the gateway replicas, and re-applying keeps the
// view matching the release being deployed (e.g. after a rollback).
func Migrate(ctx context.Context, cfg Config, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	cfg = cfg.withDefaults()

	c, err := open(cfg)
	if err != nil {
		return err
	}
	defer c.Close()

	stmts := schemaStatements()
	if err := applySchema(ctx, c, stmts, schemaFingerprint(stmts), cfg.Version, logger); err != nil {
		return fmt.Errorf("clickhouse: apply migrations: %w", err)
	}
	return nil
}

// open connects to cfg (already defaulted) and pings it.
func open(cfg Config) (conn, error) {
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
	return c, nil
}

// newSink wires up the queue engine over an already-connected conn and
// batchWriter. Split out from New so unit tests (sink_test.go) can drive
// it with a fake batchWriter and no real ClickHouse.
func newSink(c conn, w batchWriter, cfg Config, logger *slog.Logger) *Sink {
	cfg = cfg.withDefaults()
	if logger == nil {
		logger = slog.Default()
	}
	s := &Sink{
		c:             c,
		writer:        w,
		batchSize:     cfg.BatchSize,
		flushEvery:    cfg.FlushInterval,
		maxPending:    cfg.BufferSize,
		async:         !cfg.SyncInsert,
		maxInflight:   1,
		insertTimeout: insertTimeoutSlack,
		retryDelay:    defaultRetryDelay,
		ch:            make(chan item, cfg.BufferSize),
		done:          make(chan struct{}),
		logger:        logger,
	}
	if s.async {
		s.maxInflight = maxInflightFlushes
		s.insertTimeout = cfg.AsyncInsertBusyTimeout + insertTimeoutSlack
		s.settings = asyncSettings(cfg.AsyncInsertBusyTimeout, cfg.adaptiveBusyTimeoutKnown)
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

// Close stops accepting new writes, writes whatever is still queued or
// buffered in one final synchronous flush (bounded by closeFlushTimeout),
// waits for in-flight flushes (each bounded by the busy timeout plus
// insertTimeoutSlack), and closes the underlying connection.
// Safe to call multiple times.
func (s *Sink) Close() error {
	s.closeOnce.Do(func() { close(s.ch) })
	<-s.done
	if s.c != nil {
		return s.c.Close()
	}
	return nil
}

// batch is one flush's worth of records.
type batch struct {
	access []*sink.AccessLog
	llm    []*sink.LLMCall
}

func (b *batch) len() int { return len(b.access) + len(b.llm) }

func (b *batch) add(it item) {
	switch {
	case it.access != nil:
		b.access = append(b.access, it.access)
	case it.llm != nil:
		b.llm = append(b.llm, it.llm)
	}
}

// run is the sink's background loop: it accumulates queued records into
// a batch and hands the batch to a flush goroutine (one INSERT per table)
// whenever it reaches batchSize or flushEvery elapses, whichever comes
// first. At most maxInflight flushes run at once (one in sync mode, as
// before); while all are busy the loop keeps accumulating, up to
// maxPending records, and the next flush to finish takes the whole batch.
// Beyond maxPending the loop stops reading the queue, so it fills and
// further writes are dropped and counted -- WriteAccess/WriteLLMCall never
// block. When the queue channel is closed (Close), the loop drains it,
// does one final synchronous flush and waits for in-flight flushes.
func (s *Sink) run() {
	defer close(s.done)

	ticker := time.NewTicker(s.flushEvery)
	defer ticker.Stop()

	var (
		cur      batch
		inflight int
		due      bool // a flush came due while every slot was busy
		wg       sync.WaitGroup
		finished = make(chan struct{}, s.maxInflight)
	)

	dispatch := func() {
		if cur.len() == 0 {
			due = false
			return
		}
		if inflight >= s.maxInflight {
			due = true
			return
		}
		inflight++
		due = false
		b := cur
		cur = batch{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.flush(b)
			finished <- struct{}{}
		}()
	}

	for {
		in := s.ch
		if cur.len() >= s.maxPending {
			in = nil // full: stop draining until a flush slot frees up
		}
		select {
		case it, ok := <-in:
			if !ok {
				s.flushFinal(cur)
				wg.Wait()
				return
			}
			cur.add(it)
			if cur.len() >= s.batchSize {
				dispatch()
			}
		case <-ticker.C:
			dispatch()
		case <-finished:
			inflight--
			if due || cur.len() >= s.batchSize {
				dispatch()
			}
		}
	}
}

// flush writes b in the background path: async-insert settings when
// async, each table's insert in its own attempt-bounded context, the
// tables in parallel (each async insert waits out the busy timeout), and
// one retry of a transient failure.
func (s *Sink) flush(b batch) {
	var wg sync.WaitGroup
	if len(b.access) > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.insertWithRetry(context.Background(), "access logs", len(b.access), func(ctx context.Context) error {
				return s.writer.InsertAccess(s.insertCtx(ctx), b.access)
			})
		}()
	}
	if len(b.llm) > 0 {
		s.insertWithRetry(context.Background(), "llm calls", len(b.llm), func(ctx context.Context) error {
			return s.writer.InsertLLM(s.insertCtx(ctx), b.llm)
		})
	}
	wg.Wait()
}

// flushFinal is Close's last flush: synchronous (no async-insert
// settings, so it does not wait out a busy timeout -- its rows become one
// part of their own), with every attempt, retry included, inside
// closeFlushTimeout.
func (s *Sink) flushFinal(b batch) {
	if b.len() == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), closeFlushTimeout)
	defer cancel()
	if len(b.access) > 0 {
		s.insertWithRetry(ctx, "access logs", len(b.access), func(ctx context.Context) error {
			return s.writer.InsertAccess(ctx, b.access)
		})
	}
	if len(b.llm) > 0 {
		s.insertWithRetry(ctx, "llm calls", len(b.llm), func(ctx context.Context) error {
			return s.writer.InsertLLM(ctx, b.llm)
		})
	}
}

// insertCtx attaches the async-insert settings to ctx in async mode.
func (s *Sink) insertCtx(ctx context.Context) context.Context {
	if !s.async {
		return ctx
	}
	return clickhouse.Context(ctx, clickhouse.WithSettings(s.settings))
}

// insertWithRetry runs insert (each attempt bounded by insertTimeout and
// by parent) and, if it fails with an error retryable reports as safe to
// repeat, runs it once more after retryDelay. Every failed attempt is
// logged with its row count; an unretried or second failure loses the
// rows, as a failed insert always has.
func (s *Sink) insertWithRetry(parent context.Context, what string, rows int, insert func(ctx context.Context) error) {
	for attempt := 1; ; attempt++ {
		ctx, cancel := context.WithTimeout(parent, s.insertTimeout)
		err := insert(ctx)
		cancel()
		if err == nil {
			return
		}
		retry := attempt == 1 && retryable(err)
		s.logger.Error("clickhouse sink: insert "+what+" failed", "error", err, "rows", rows, "attempt", attempt, "will_retry", retry)
		if !retry {
			return
		}
		select {
		case <-time.After(s.retryDelay):
		case <-parent.Done():
			s.logger.Error("clickhouse sink: insert "+what+" not retried: out of time", "rows", rows)
			return
		}
	}
}
