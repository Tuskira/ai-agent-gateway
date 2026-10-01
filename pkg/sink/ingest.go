package sink

import "context"

// IngestResult is the outcome of one IngestSink.WriteIngestBatch call: how
// many of the caller's records were actually written versus skipped as
// duplicates. It says nothing about validation -- a record the caller
// (internal/api/handlers.Ingest) rejected before ever reaching the sink
// never appears here at all.
type IngestResult struct {
	AcceptedAccess, DuplicateAccess int
	AcceptedLLM, DuplicateLLM       int
}

// IngestSink is implemented by a LogSink that can also perform a
// synchronous, tenant-scoped, duplicate-checked batch write of externally
// produced records -- what backs POST /api/v1/ingest
// (internal/api/handlers.Ingest, fed by a companion capture component;
// see docs/api.md). It deliberately does NOT go through
// LogSink's WriteAccess/WriteLLMCall: those enqueue onto a bounded queue
// and drop silently on overflow (see the async Sink's doc comment), which
// is fine for a best-effort analytics tee of gateway-proxied traffic but
// not for an endpoint whose entire job is durably recording someone else's
// traffic -- a caller that got a 200 must actually be able to trust it.
//
// WriteIngestBatch returns an error only for an infrastructure failure
// (the insert itself failed); a record skipped as a duplicate is not an
// error; a record failing validation should never reach this method at
// all (the handler validates first).
//
// Implemented by pkg/sink/clickhouse.Sink. A deployment with no
// ClickHouse sink configured has no IngestSink at all -- the ingest
// handler answers 503 rather than calling through a nil (see
// internal/api/handlers.Ingest).
type IngestSink interface {
	// WriteIngestBatch inserts access/llm, scoped to tenantID (every
	// record's TenantID must already be stamped to it by the caller).
	// Idempotency (see the sink.AccessLog/LLMCall.RequestID doc comments)
	// is best-effort: a record whose (tenant_id, request_id) already
	// exists, in the store or earlier in this same batch, is counted as a
	// duplicate and not written again, but nothing here enforces that as
	// a hard constraint -- a sink backed by a store with no unique index
	// (ClickHouse's MergeTree, notably) cannot guarantee it under
	// concurrent overlapping batches.
	WriteIngestBatch(ctx context.Context, tenantID string, access []*AccessLog, llm []*LLMCall) (IngestResult, error)
}
