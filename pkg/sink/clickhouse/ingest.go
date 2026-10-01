package clickhouse

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink"
)

// dupCheckWindow pads the batch's own [min timestamp, max timestamp] span
// by this much on either side before querying for already-written
// request_ids (see existingRequestIDs), matching the design's "[min ts -
// 1h, max ts + 1h]" window: an interceptor batch's records were captured
// live but may be forwarded late (spool replay after an outage), so the
// existing-row check must tolerate some clock/queueing skew without
// scanning the whole table.
const dupCheckWindow = time.Hour

// WriteIngestBatch implements sink.IngestSink. See that interface's doc
// comment for the contract; this is the synchronous insert path POST
// /api/v1/ingest uses instead of the async, drop-on-overflow LogSink
// queue (WriteAccess/WriteLLMCall) every other caller of this Sink goes
// through.
func (s *Sink) WriteIngestBatch(ctx context.Context, tenantID string, access []*sink.AccessLog, llm []*sink.LLMCall) (sink.IngestResult, error) {
	var result sink.IngestResult

	freshAccess, dupAccess, err := s.dedupAccess(ctx, tenantID, access)
	if err != nil {
		return result, fmt.Errorf("clickhouse: dedup access logs: %w", err)
	}
	result.DuplicateAccess = dupAccess
	if len(freshAccess) > 0 {
		if err := s.writer.InsertAccess(ctx, freshAccess); err != nil {
			return result, fmt.Errorf("clickhouse: insert access logs: %w", err)
		}
	}
	result.AcceptedAccess = len(freshAccess)

	freshLLM, dupLLM, err := s.dedupLLM(ctx, tenantID, llm)
	if err != nil {
		return result, fmt.Errorf("clickhouse: dedup llm calls: %w", err)
	}
	result.DuplicateLLM = dupLLM
	if len(freshLLM) > 0 {
		if err := s.writer.InsertLLM(ctx, freshLLM); err != nil {
			return result, fmt.Errorf("clickhouse: insert llm calls: %w", err)
		}
	}
	result.AcceptedLLM = len(freshLLM)

	return result, nil
}

// dedupAccess returns the subset of access that should actually be
// inserted (first occurrence of each request_id, and not already present
// in mcp_access_logs for tenantID within the batch's dup-check window),
// and how many records were dropped as duplicates -- whether duplicated
// within this same batch or already on record.
func (s *Sink) dedupAccess(ctx context.Context, tenantID string, access []*sink.AccessLog) ([]*sink.AccessLog, int, error) {
	if len(access) == 0 {
		return nil, 0, nil
	}

	inBatch, dup := dedupByRequestID(access, func(a *sink.AccessLog) string { return a.RequestID })

	ids := make([]string, len(inBatch))
	for i, a := range inBatch {
		ids[i] = a.RequestID
	}
	from, to := batchTimeRange(inBatch, func(a *sink.AccessLog) time.Time { return a.Timestamp })
	existing, err := s.existingRequestIDs(ctx, "mcp_access_logs", tenantID, from, to, ids)
	if err != nil {
		return nil, 0, err
	}

	fresh := make([]*sink.AccessLog, 0, len(inBatch))
	for _, a := range inBatch {
		if existing[a.RequestID] {
			dup++
			continue
		}
		fresh = append(fresh, a)
	}
	return fresh, dup, nil
}

// dedupLLM mirrors dedupAccess for llm_calls.
func (s *Sink) dedupLLM(ctx context.Context, tenantID string, llm []*sink.LLMCall) ([]*sink.LLMCall, int, error) {
	if len(llm) == 0 {
		return nil, 0, nil
	}

	inBatch, dup := dedupByRequestID(llm, func(l *sink.LLMCall) string { return l.RequestID })

	ids := make([]string, len(inBatch))
	for i, l := range inBatch {
		ids[i] = l.RequestID
	}
	from, to := batchTimeRange(inBatch, func(l *sink.LLMCall) time.Time { return l.Timestamp })
	existing, err := s.existingRequestIDs(ctx, "llm_calls", tenantID, from, to, ids)
	if err != nil {
		return nil, 0, err
	}

	fresh := make([]*sink.LLMCall, 0, len(inBatch))
	for _, l := range inBatch {
		if existing[l.RequestID] {
			dup++
			continue
		}
		fresh = append(fresh, l)
	}
	return fresh, dup, nil
}

// dedupByRequestID keeps the first record for each distinct request id
// (as returned by idOf) and reports how many later records sharing an
// already-seen id were dropped -- the in-batch half of WriteIngestBatch's
// duplicate check, ahead of the existing-rows query.
func dedupByRequestID[T any](records []T, idOf func(T) string) (kept []T, duplicates int) {
	seen := make(map[string]struct{}, len(records))
	kept = make([]T, 0, len(records))
	for _, r := range records {
		id := idOf(r)
		if _, ok := seen[id]; ok {
			duplicates++
			continue
		}
		seen[id] = struct{}{}
		kept = append(kept, r)
	}
	return kept, duplicates
}

// batchTimeRange returns [min-dupCheckWindow, max+dupCheckWindow] across
// records' timestamps (tsOf), the window existingRequestIDs queries.
// records is never empty when this is called (callers check len first).
func batchTimeRange[T any](records []T, tsOf func(T) time.Time) (from, to time.Time) {
	from, to = tsOf(records[0]), tsOf(records[0])
	for _, r := range records[1:] {
		if t := tsOf(r); t.Before(from) {
			from = t
		} else if t.After(to) {
			to = t
		}
	}
	return from.Add(-dupCheckWindow), to.Add(dupCheckWindow)
}

// existingRequestIDs returns the subset of ids already present in table
// for tenantID within [from, to], one SELECT per call (per the design's
// "one SELECT per table" -- WriteIngestBatch calls this at most once for
// mcp_access_logs and once for llm_calls). table is always one of the two
// package constants below, never caller input, so building the query
// string with it is safe.
func (s *Sink) existingRequestIDs(ctx context.Context, table, tenantID string, from, to time.Time, ids []string) (map[string]bool, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	query := `SELECT DISTINCT request_id FROM ` + table + ` WHERE tenant_id = ? AND timestamp >= ? AND timestamp <= ? AND request_id IN (` + placeholders + `)`

	args := make([]any, 0, 3+len(ids))
	args = append(args, tenantID, from, to)
	for _, id := range ids {
		args = append(args, id)
	}

	rows, err := s.c.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]bool, len(ids))
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}
