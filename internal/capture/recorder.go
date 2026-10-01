// Package capture is the LLM-plane capture path: a Recorder writes each call
// synchronously to a sink.BatchSink (Postgres), committed before the request
// completes so nothing is lost on a crash, and tees a best-effort copy into the
// shared analytics LogSink (ClickHouse/OTel). It bridges the LLM plane to
// pkg/sink so the plane doesn't depend on a concrete store.
package capture

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink"
)

// Recorder captures one LLMCall per request. The primary write is a single
// synchronous WriteBatch to the durable sink (Postgres), the same pattern
// Tuskira's runner uses: the row is committed before Record returns, so nothing
// is lost on a later crash, and it is idempotent (request_id PK + ON CONFLICT DO
// NOTHING). A failure there is returned to the caller to log — never silently
// dropped. It then tees the call into the shared analytics sink (best-effort,
// async, may drop) so LLM calls also surface in the ClickHouse/OTel-backed
// analytics and web LLM-logs views; an analytics failure never fails the record.
//
// With a BodyStore, bodies over inlineMax are offloaded first and the row
// carries only BodyRef; a failed offload falls back to inline bodies, so the
// audit record is never lost to an object-store outage.
type Recorder struct {
	sink      sink.BatchSink
	analytics sink.LogSink   // best-effort tee (ClickHouse/OTel/stdout Multi); may be nil
	bodies    sink.BodyStore // body offload; nil keeps bodies inline
	inlineMax int            // bodies totalling <= this stay inline even with a store

	offloaded atomic.Uint64
	fallbacks atomic.Uint64
}

// NewRecorder returns a Recorder that writes durably through batch and tees a
// best-effort copy into analytics (nil disables the tee). bodies, when non-nil,
// receives every call's bodies whose request+response total exceeds inlineMax
// (0 = offload any call with a body).
func NewRecorder(batch sink.BatchSink, analytics sink.LogSink, bodies sink.BodyStore, inlineMax int) *Recorder {
	return &Recorder{sink: batch, analytics: analytics, bodies: bodies, inlineMax: inlineMax}
}

// Record offloads the bodies when configured, writes the call synchronously to
// the durable sink, then tees a best-effort copy to the analytics sink. The tee
// runs even when the durable write fails, so the call still surfaces in
// analytics.
func (r *Recorder) Record(ctx context.Context, c *sink.LLMCall) error {
	r.offload(ctx, c)
	err := r.sink.WriteBatch(ctx, []*sink.LLMCall{c})
	if r.analytics != nil {
		r.analytics.WriteLLMCall(c) // best-effort; never fails the durable record
	}
	if err != nil {
		return fmt.Errorf("capture: write: %w", err)
	}
	return nil
}

// offload moves c's bodies to the BodyStore and sets c.BodyRef. It never
// fails the record: on error the bodies stay inline and fallbacks counts it.
// Put gets at most half of ctx's remaining budget, so a hung object store
// cannot starve the durable write that shares the capture deadline.
func (r *Recorder) offload(ctx context.Context, c *sink.LLMCall) {
	n := len(c.RequestBody) + len(c.ResponseBody)
	if r.bodies == nil || n == 0 || n <= r.inlineMax {
		return
	}
	putCtx := ctx
	if dl, ok := ctx.Deadline(); ok {
		var cancel context.CancelFunc
		putCtx, cancel = context.WithTimeout(ctx, time.Until(dl)/2)
		defer cancel()
	}
	ref, err := r.bodies.Put(putCtx, c.RequestID, c.RequestBody, c.ResponseBody)
	if err != nil {
		r.fallbacks.Add(1)
		slog.Warn("capture: body offload failed; storing bodies inline", "request_id", c.RequestID, "bytes", n, "error", err)
		return
	}
	r.offloaded.Add(1)
	c.BodyRef, c.RequestBody, c.ResponseBody = ref, nil, nil
}

// Offloaded is the number of calls whose bodies went to the BodyStore since
// process start.
func (r *Recorder) Offloaded() uint64 { return r.offloaded.Load() }

// Fallbacks is the number of calls whose offload failed and were stored inline
// instead, since process start. Non-zero means the body store is unhealthy.
func (r *Recorder) Fallbacks() uint64 { return r.fallbacks.Load() }
