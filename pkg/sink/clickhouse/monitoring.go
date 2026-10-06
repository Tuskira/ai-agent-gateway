package clickhouse

import (
	"context"
	"fmt"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/analytics"
)

// Token monitoring reads only llm_usage_canonical (migrate.go), so every
// figure -- totals, per-model, per-key, per-session and the series -- is
// computed from the same rows under the same definition and adds up.

var _ analytics.TokenMonitoringReader = (*Sink)(nil)

const (
	defaultMonitoringSessions = 50
	maxMonitoringSessions     = 500
)

// monBase selects the subject's rows in the current window or the previous
// one, flagging which with cur. Bind monArgs before any filter arguments.
func monBase(filter string) string {
	return `(SELECT *, timestamp >= ? AS cur FROM llm_usage_canonical
		WHERE tenant_id = ? AND ((timestamp >= ? AND timestamp < ?) OR (timestamp >= ? AND timestamp < ?))` + filter + `)`
}

func monArgs(tenantID string, b analytics.Period, filterArgs ...any) []any {
	return append([]any{b.Start, tenantID, b.Start, b.End, b.PrevStart, b.PrevEnd}, filterArgs...)
}

// usageSelect computes one analytics.TokenUsage; scan it with usageDest.
const usageSelect = `sumIf(total_tokens, cur) AS u_tokens,
	sumIf(prompt_tokens, cur) AS u_prompt,
	sumIf(completion_tokens, cur) AS u_completion,
	sumIf(cache_read_tokens, cur) AS u_cache_read,
	sumIf(cache_write_tokens, cur) AS u_cache_write,
	toFloat64(sumIf(ifNull(cost_usd, 0), cur AND priced)) AS u_cost,
	countIf(cur AND priced) AS u_priced,
	countIf(cur) AS u_calls,
	countIf(cur AND NOT priced) AS u_unpriced,
	sumIf(total_tokens, NOT cur) AS u_prev`

// usageScan holds a scanned usageSelect until it is converted.
type usageScan struct {
	u           analytics.TokenUsage
	cost        float64
	pricedCalls uint64
}

func (s *usageScan) dest() []any {
	return []any{&s.u.Tokens, &s.u.PromptTokens, &s.u.CompletionTokens, &s.u.CacheReadTokens, &s.u.CacheWriteTokens,
		&s.cost, &s.pricedCalls, &s.u.Calls, &s.u.UnpricedCalls, &s.u.PrevTokens}
}

// usage returns the scanned TokenUsage; CostUSD stays nil when nothing in
// the window was priced (unknown, not $0).
func (s *usageScan) usage() analytics.TokenUsage {
	u := s.u
	if s.pricedCalls > 0 {
		c := s.cost
		u.CostUSD = &c
	}
	return u
}

// TokenMonitoring implements analytics.TokenMonitoringReader.
func (s *Sink) TokenMonitoring(ctx context.Context, tenantID string, q analytics.TokenMonitoringQuery) (*analytics.TokenMonitoring, error) {
	b := q.Period
	var filter string
	var args []any
	switch {
	case q.Model != "":
		filter, args = " AND model = ?", []any{q.Model}
	case q.KeyID != "":
		filter, args = " AND key_id = ?", []any{q.KeyID}
	}
	fail := func(part string, err error) (*analytics.TokenMonitoring, error) {
		return nil, fmt.Errorf("clickhouse: token monitoring %s: %w", part, err)
	}
	out := &analytics.TokenMonitoring{Range: q.Range, Start: b.Start, End: b.End, Granularity: b.Granularity,
		ByModel: []analytics.ModelTokenUsage{}, ByKey: []analytics.KeyTokenUsage{}}
	var err error
	if out.Totals, err = s.monTotals(ctx, tenantID, b, filter, args...); err != nil {
		return fail("totals", err)
	}
	if q.Model == "" {
		if out.ByModel, err = s.monByModel(ctx, tenantID, b, filter, args...); err != nil {
			return fail("by model", err)
		}
	}
	if q.KeyID == "" {
		if out.ByKey, err = s.monByKey(ctx, tenantID, b, filter, args...); err != nil {
			return fail("by key", err)
		}
	}
	if out.Burn, err = s.monBurn(ctx, tenantID, b, filter, args...); err != nil {
		return fail("series", err)
	}
	if filter != "" {
		if out.Sessions, out.SessionsTotal, err = s.monSessions(ctx, tenantID, b, filter, args, q.Limit, q.Offset); err != nil {
			return fail("sessions", err)
		}
	}
	return out, nil
}

// monTotals also reads the previous period's tokens with cache, which only
// the headline total compares against.
func (s *Sink) monTotals(ctx context.Context, tenantID string, b analytics.Period, filter string, filterArgs ...any) (analytics.TokenUsage, error) {
	var sc usageScan
	var prevWithCache uint64
	err := s.c.QueryRow(ctx, `SELECT `+usageSelect+`, sumIf(total_tokens + cache_read_tokens + cache_write_tokens, NOT cur) FROM `+monBase(filter),
		monArgs(tenantID, b, filterArgs...)...).Scan(append(sc.dest(), &prevWithCache)...)
	u := sc.usage()
	u.PrevTokensWithCache = prevWithCache
	return u, err
}

func (s *Sink) monByModel(ctx context.Context, tenantID string, b analytics.Period, filter string, filterArgs ...any) ([]analytics.ModelTokenUsage, error) {
	rows, err := s.c.Query(ctx, `SELECT model, `+usageSelect+`
		FROM `+monBase(filter)+`
		GROUP BY model HAVING u_calls > 0
		ORDER BY u_tokens DESC, model`, monArgs(tenantID, b, filterArgs...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []analytics.ModelTokenUsage{}
	for rows.Next() {
		var m analytics.ModelTokenUsage
		var sc usageScan
		if err := rows.Scan(append([]any{&m.Model}, sc.dest()...)...); err != nil {
			return nil, err
		}
		m.TokenUsage = sc.usage()
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Sink) monByKey(ctx context.Context, tenantID string, b analytics.Period, filter string, filterArgs ...any) ([]analytics.KeyTokenUsage, error) {
	rows, err := s.c.Query(ctx, `SELECT key_id, anyHeavy(source), uniqExactIf(model, cur), `+usageSelect+`
		FROM `+monBase(filter)+`
		GROUP BY key_id HAVING u_calls > 0
		ORDER BY u_tokens DESC, key_id`, monArgs(tenantID, b, filterArgs...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []analytics.KeyTokenUsage{}
	for rows.Next() {
		var k analytics.KeyTokenUsage
		var sc usageScan
		if err := rows.Scan(append([]any{&k.KeyID, &k.Source, &k.Models}, sc.dest()...)...); err != nil {
			return nil, err
		}
		k.TokenUsage = sc.usage()
		out = append(out, k)
	}
	return out, rows.Err()
}

// monBurn is the current window's tokens per UTC hour or day, zero-filled.
func (s *Sink) monBurn(ctx context.Context, tenantID string, b analytics.Period, filter string, filterArgs ...any) ([]analytics.TokenBucket, error) {
	bucket := "toStartOfDay(timestamp, 'UTC')"
	if b.Granularity == analytics.GranularityHour {
		bucket = "toStartOfHour(timestamp, 'UTC')"
	}
	rows, err := s.c.Query(ctx, `SELECT `+bucket+` AS bucket, sum(total_tokens)
		FROM `+monBase(filter)+` WHERE cur
		GROUP BY bucket ORDER BY bucket`, monArgs(tenantID, b, filterArgs...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var points []analytics.TokenBucket
	for rows.Next() {
		var p analytics.TokenBucket
		if err := rows.Scan(&p.Bucket, &p.Tokens); err != nil {
			return nil, err
		}
		points = append(points, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return analytics.FillTokenBuckets(points, b), nil
}

// monSessions pages the current window's sessions (per session id and key,
// most tokens first) and counts them all.
func (s *Sink) monSessions(ctx context.Context, tenantID string, b analytics.Period, filter string, filterArgs []any, limit, offset int) ([]analytics.SessionTokenUsage, int, error) {
	if limit <= 0 {
		limit = defaultMonitoringSessions
	}
	limit, offset = min(limit, maxMonitoringSessions), max(offset, 0)
	args := monArgs(tenantID, b, filterArgs...)
	var total uint64
	if err := s.c.QueryRow(ctx, `SELECT count() FROM (SELECT 1 FROM `+monBase(filter)+`
		GROUP BY session_id, key_id HAVING countIf(cur) > 0)`, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.c.Query(ctx, `SELECT session_id, key_id, arraySort(groupUniqArrayIf(model, cur)),
			maxIf(timestamp, cur), `+usageSelect+`
		FROM `+monBase(filter)+`
		GROUP BY session_id, key_id HAVING u_calls > 0
		ORDER BY u_tokens DESC, session_id, key_id
		LIMIT ? OFFSET ?`, append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []analytics.SessionTokenUsage{}
	for rows.Next() {
		var r analytics.SessionTokenUsage
		var sc usageScan
		if err := rows.Scan(append([]any{&r.SessionID, &r.KeyID, &r.Models, &r.LastSeen}, sc.dest()...)...); err != nil {
			return nil, 0, err
		}
		r.LastSeen = r.LastSeen.UTC()
		r.TokenUsage = sc.usage()
		out = append(out, r)
	}
	return out, int(total), rows.Err()
}
