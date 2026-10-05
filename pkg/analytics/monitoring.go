package analytics

import (
	"context"
	"math"
	"time"
)

// MonitoringWindow is one of the Token Monitoring page's time windows.
type MonitoringWindow string

const (
	// WindowToday is the current UTC calendar day so far.
	WindowToday MonitoringWindow = "today"
	// Window7d and Window30d are rolling windows ending now.
	Window7d  MonitoringWindow = "7d"
	Window30d MonitoringWindow = "30d"
)

// ParseMonitoringWindow validates a "window" query parameter.
func ParseMonitoringWindow(s string) (MonitoringWindow, bool) {
	switch w := MonitoringWindow(s); w {
	case WindowToday, Window7d, Window30d:
		return w, true
	}
	return "", false
}

// Granularity is the bucket size of a usage-over-time series.
type Granularity string

const (
	GranularityHour Granularity = "hour"
	GranularityDay  Granularity = "day"
)

func (g Granularity) step() time.Duration {
	if g == GranularityHour {
		return time.Hour
	}
	return 24 * time.Hour
}

// MonitoringBounds is a window resolved against a clock: usage is counted
// in [Start, End) and compared with [PrevStart, PrevEnd), the same span one
// period earlier. All times are UTC.
type MonitoringBounds struct {
	Start, End, PrevStart, PrevEnd time.Time
	Granularity                    Granularity
}

// Bounds resolves w at now. Today starts at UTC midnight and is compared
// with the same hours of yesterday; 7d/30d are compared with the window of
// the same length immediately before.
func (w MonitoringWindow) Bounds(now time.Time) MonitoringBounds {
	now = now.UTC()
	b := MonitoringBounds{End: now, Granularity: GranularityDay}
	period := 24 * time.Hour
	switch w {
	case Window7d:
		period = 7 * 24 * time.Hour
		b.Start = now.Add(-period)
	case Window30d:
		period = 30 * 24 * time.Hour
		b.Start = now.Add(-period)
	default: // WindowToday
		b.Start = now.Truncate(24 * time.Hour)
		b.Granularity = GranularityHour
	}
	b.PrevStart, b.PrevEnd = b.Start.Add(-period), b.End.Add(-period)
	return b
}

// DeltaPct is the change from prev to cur in percent, rounded to two
// decimals. nil when prev is 0: there is no meaningful percentage, and
// "0%" would wrongly read as "no change".
func DeltaPct(cur, prev uint64) *float64 {
	if prev == 0 {
		return nil
	}
	d := math.Round((float64(cur)-float64(prev))/float64(prev)*100*100) / 100
	return &d
}

// TokenBucket is one point of a usage-over-time series.
type TokenBucket struct {
	Bucket time.Time `json:"bucket"`
	Tokens uint64    `json:"tokens"`
}

// FillTokenBuckets returns one bucket per granularity step from the step
// containing b.Start through the one containing b.End, taking tokens from
// points (keyed by bucket start) and 0 elsewhere, so a chart has no gaps.
func FillTokenBuckets(points []TokenBucket, b MonitoringBounds) []TokenBucket {
	step := b.Granularity.step()
	by := make(map[int64]uint64, len(points))
	for _, p := range points {
		by[p.Bucket.UTC().Truncate(step).Unix()] += p.Tokens
	}
	out := []TokenBucket{}
	for t := b.Start.UTC().Truncate(step); t.Before(b.End) || t.Equal(b.End.Truncate(step)); t = t.Add(step) {
		out = append(out, TokenBucket{Bucket: t, Tokens: by[t.Unix()]})
	}
	return out
}

// TokenUsage is usage over a window, from the llm_usage_canonical view:
// Tokens = prompt + completion; cache reads/writes are reported alongside
// but are not part of Tokens. CostUSD is nil when no call in the window was
// priced; UnpricedCalls counts calls with no known cost. PrevTokens is the
// same subject's Tokens over the previous period.
type TokenUsage struct {
	Tokens           uint64   `json:"tokens"`
	PromptTokens     uint64   `json:"prompt_tokens"`
	CompletionTokens uint64   `json:"completion_tokens"`
	CacheReadTokens  uint64   `json:"cache_read_tokens"`
	CacheWriteTokens uint64   `json:"cache_write_tokens"`
	CostUSD          *float64 `json:"cost_usd"`
	Calls            uint64   `json:"calls"`
	UnpricedCalls    uint64   `json:"unpriced_calls"`
	PrevTokens       uint64   `json:"prev_tokens"`
}

// ModelTokenUsage is one model's usage (the name callers asked for).
type ModelTokenUsage struct {
	Model string `json:"model"`
	TokenUsage
}

// KeyTokenUsage is one caller's (API key's) usage. Source is "gateway" for
// proxied calls or "interceptor" for ingested ones.
type KeyTokenUsage struct {
	KeyID  string `json:"key_id"`
	Source string `json:"source"`
	Models uint64 `json:"models"`
	TokenUsage
}

// SessionTokenUsage is one session's usage under one key.
type SessionTokenUsage struct {
	SessionID string    `json:"session_id"`
	KeyID     string    `json:"key_id"`
	Models    []string  `json:"models"`
	LastSeen  time.Time `json:"last_seen"`
	TokenUsage
}

// TokenMonitoringQuery selects the Token Monitoring page (neither Model nor
// KeyID set) or one drill-down (exactly one set). Limit/Offset page a
// drill-down's sessions.
type TokenMonitoringQuery struct {
	Window        MonitoringWindow
	Model, KeyID  string
	Limit, Offset int
}

// TokenMonitoring is usage for one window. The page breaks it down by model
// and by key; a model drill-down by key, a key drill-down by model, and both
// add their sessions (paged; SessionsTotal counts them all). Every figure
// comes from the same rows, so breakdowns and the series add up to Totals.
type TokenMonitoring struct {
	Window        MonitoringWindow    `json:"window"`
	Start         time.Time           `json:"start"`
	End           time.Time           `json:"end"`
	Granularity   Granularity         `json:"granularity"`
	Totals        TokenUsage          `json:"totals"`
	ByModel       []ModelTokenUsage   `json:"by_model"`
	ByKey         []KeyTokenUsage     `json:"by_key"`
	Burn          []TokenBucket       `json:"burn"`
	Sessions      []SessionTokenUsage `json:"sessions,omitempty"`
	SessionsTotal int                 `json:"sessions_total,omitempty"`
}

// TokenMonitoringReader is implemented by readers that can serve the Token
// Monitoring page (pkg/sink/clickhouse). It is separate from Reader so
// other Reader implementations need not provide it; the handler answers
// 404 when the configured Reader does not.
type TokenMonitoringReader interface {
	TokenMonitoring(ctx context.Context, tenantID string, q TokenMonitoringQuery) (*TokenMonitoring, error)
}
