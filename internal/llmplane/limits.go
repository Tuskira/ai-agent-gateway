package llmplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	pkgsink "github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// KeyLookup resolves an API key's current row (for its Limits).
// store.APIKeyStore satisfies it.
type KeyLookup interface {
	GetByID(ctx context.Context, tenantID, id string) (*store.APIKey, error)
}

// LimiterConfig configures a Limiter.
type LimiterConfig struct {
	// Keys reads each API key's limits. Required.
	Keys KeyLookup
	// Spend reads a key's (or a model's) captured spend for the USD
	// budgets. Nil means no Postgres capture store is configured: a key or
	// model WITH a daily_usd or monthly_usd limit is then refused (503),
	// never waved through, since the budget cannot be checked.
	Spend pkgsink.SpendReader
	// TTL bounds how stale a key's cached limits and spend may be. Zero
	// uses defaultLimitsTTL (10 s).
	TTL time.Duration
	// Now is the clock (tests). Nil uses time.Now.
	Now func() time.Time
}

const (
	defaultLimitsTTL = 10 * time.Second
	// idleEntryTTL: a key entry unused this long is dropped by the sweep
	// (rotated/revoked keys, and the RPM window's memory with them).
	idleEntryTTL = 10 * time.Minute
	rpmWindow    = time.Minute
)

// Limiter enforces limits (store.Limits) in front of the upstream call:
// max_tokens (400), then the daily/monthly USD budgets (429), then requests
// per minute (429). Two subjects carry limits, checked in this order:
//
//   - the calling API key (check), before anything else. A request whose
//     Principal has no KeyID (OIDC, dev mode) has no key limits.
//   - the registered model the request names (checkModel), after the
//     registry lookup, per tenant: spend and the RPM window are keyed on
//     (tenant, requested model name), whichever key called and whichever
//     target answers. An unregistered name has no model limits.
//
// Caches, all in memory and per replica:
//   - a key's Limits and a subject's day/month spend are re-read at most
//     once per TTL (10 s); a call this replica records adds its cost to the
//     cached spend immediately (observe), so the local view does not lag
//     its own traffic. Spend from other replicas lands on the next refresh.
//     A model's Limits are the registry row's own (registry cache, 30 s,
//     invalidated by a write on the same replica).
//   - RPM is an exact sliding window per subject: a ring of the last rpm
//     admitted timestamps. Replicas do not share it.
//
// Budgets are therefore soft by up to one TTL of other replicas' spend
// plus the cost of calls already in flight when the budget is crossed.
type Limiter struct {
	keys  KeyLookup
	spend pkgsink.SpendReader
	ttl   time.Duration
	now   func() time.Time

	mu        sync.Mutex
	entries   map[string]*limitEntry // see keyEntry / modelEntry
	lastSweep time.Time

	budgetDenials atomic.Uint64
	rpmDenials    atomic.Uint64
	warnedNoSpend atomic.Bool
}

// limitEntry is one subject's cached limits + spend and its RPM window.
// mu serializes the refresh, so a cold subject costs one lookup, not one
// per concurrent request.
type limitEntry struct {
	mu        sync.Mutex
	limits    *store.Limits
	fetchedAt time.Time // zero = never fetched
	dayStart  time.Time // the UTC day the cached spend belongs to
	day       float64
	month     float64
	lastUsed  time.Time

	ring []time.Time // RPM window: last len(ring) admitted requests
	next int         // ring index of the oldest entry (overwritten next)
	n    int         // entries filled so far (< len(ring) until warm)
}

// NewLimiter builds a Limiter. cfg.Keys is required.
func NewLimiter(cfg LimiterConfig) (*Limiter, error) {
	if cfg.Keys == nil {
		return nil, errors.New("llmplane: LimiterConfig.Keys is required")
	}
	if cfg.TTL <= 0 {
		cfg.TTL = defaultLimitsTTL
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Limiter{keys: cfg.Keys, spend: cfg.Spend, ttl: cfg.TTL, now: cfg.Now, entries: map[string]*limitEntry{}}, nil
}

// Status is the /health view: denials since process start.
func (l *Limiter) Status() map[string]any {
	return map[string]any{
		"budget_denials": l.budgetDenials.Load(),
		"rpm_denials":    l.rpmDenials.Load(),
	}
}

// BudgetDenials is how many requests were refused for an exhausted USD or
// token budget since process start.
func (l *Limiter) BudgetDenials() uint64 { return l.budgetDenials.Load() }

// RPMDenials is how many requests were refused for exceeding a requests
// per minute limit since process start.
func (l *Limiter) RPMDenials() uint64 { return l.rpmDenials.Load() }

// denial is a refused request: the status, the client-facing message, the
// capture error text, and Retry-After seconds (0 = no header).
type denial struct {
	status     int
	message    string
	capture    string
	retryAfter int
}

func (d *denial) write(w http.ResponseWriter, requestID string) {
	if d.retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(d.retryAfter))
	}
	writeAnthropicError(w, d.status, requestID, d.message)
}

// utcPeriods returns the start of now's UTC day and calendar month and the
// next boundary of each.
func utcPeriods(now time.Time) (dayStart, nextDay, monthStart, nextMonth time.Time) {
	u := now.UTC()
	dayStart = time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
	monthStart = time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC)
	return dayStart, dayStart.AddDate(0, 0, 1), monthStart, monthStart.AddDate(0, 1, 0)
}

// secondsUntil is Retry-After for a boundary: whole seconds, rounded up,
// at least 1.
func secondsUntil(now, t time.Time) int {
	return max(1, int(math.Ceil(t.Sub(now).Seconds())))
}

// subject is what a set of limits is attached to: an API key or a
// registered model.
type subject struct {
	entry string // cache key (keyEntry / modelEntry)
	what  string // in messages: "this key", "model <name>"
	whose string // possessive: "this key's", "model <name>'s"
	attrs []any  // log attributes
	// lookup reads the subject's current limits (a key's row). Nil when
	// they are already in hand: pinned.
	lookup func(ctx context.Context) (*store.Limits, error)
	// pinned is the limits of a subject without lookup (a registry row's,
	// which the registry already caches and invalidates).
	pinned *store.Limits
	// spend reads the subject's captured spend.
	spend func(ctx context.Context, r pkgsink.SpendReader, dayStart, monthStart time.Time) (day, month float64, err error)
}

func keyEntry(tenantID, keyID string) string  { return "k\x00" + tenantID + "\x00" + keyID }
func modelEntry(tenantID, name string) string { return "m\x00" + tenantID + "\x00" + name }

// check decides one request against the calling key's limits. body is the
// client's request body (for max_tokens). nil = admitted, and the request
// has taken an RPM slot.
func (l *Limiter) check(ctx context.Context, tenantID, keyID string, body []byte) *denial {
	if keyID == "" {
		return nil
	}
	return l.decide(ctx, subject{
		entry: keyEntry(tenantID, keyID), what: "this key", whose: "this key's",
		attrs: []any{"tenant_id", tenantID, "key_id", keyID},
		lookup: func(ctx context.Context) (*store.Limits, error) {
			k, err := l.keys.GetByID(ctx, tenantID, keyID)
			switch {
			case errors.Is(err, store.ErrNotFound):
				// The authenticator accepted the key moments ago; a row gone
				// since is being revoked/deleted and has no limits left to apply.
				return nil, nil
			case err != nil:
				return nil, fmt.Errorf("read key limits: %w", err)
			}
			return k.Limits, nil
		},
		spend: func(ctx context.Context, r pkgsink.SpendReader, dayStart, monthStart time.Time) (float64, float64, error) {
			return r.KeySpend(ctx, tenantID, keyID, dayStart, monthStart)
		},
	}, body)
}

// checkModel decides one request against the limits of the registry row it
// resolved to (m.Limits), for the caller's tenant. Spend and the RPM window
// are keyed on (tenant, m.Name) -- the name the client asked for, which is
// what capture stores as requested_model. nil = admitted.
func (l *Limiter) checkModel(ctx context.Context, tenantID string, m *store.Model, body []byte) *denial {
	if m == nil || m.Limits.Empty() {
		return nil
	}
	return l.decide(ctx, subject{
		entry: modelEntry(tenantID, m.Name), what: "model " + m.Name, whose: "model " + m.Name + "'s",
		attrs:  []any{"tenant_id", tenantID, "model", m.Name},
		pinned: m.Limits,
		spend: func(ctx context.Context, r pkgsink.SpendReader, dayStart, monthStart time.Time) (float64, float64, error) {
			return r.ModelSpend(ctx, tenantID, m.Name, dayStart, monthStart)
		},
	}, body)
}

// decide applies s's limits to one request.
func (l *Limiter) decide(ctx context.Context, s subject, body []byte) *denial {
	now := l.now()
	e := l.entry(s.entry, now)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.lastUsed = now

	dayStart, nextDay, monthStart, nextMonth := utcPeriods(now)
	if err := l.refresh(ctx, e, s, now, dayStart, monthStart); err != nil {
		slog.Error("llmplane: limits check failed", append(s.attrs, "error", err)...)
		return &denial{status: http.StatusServiceUnavailable, message: "limits check unavailable, retry shortly",
			capture: "limits_unavailable: " + err.Error(), retryAfter: 1}
	}
	lim := e.limits
	if lim.Empty() {
		return nil
	}

	// 1. max_tokens: a request-shape error, so it never touches the budget
	// or takes an RPM slot.
	if lim.MaxTokens != nil {
		if asked, ok := requestedMaxTokens(body); ok && asked > float64(*lim.MaxTokens) {
			msg := fmt.Sprintf("max_tokens %v exceeds %s limit of %d", asked, s.whose, *lim.MaxTokens)
			return &denial{status: http.StatusBadRequest, message: msg, capture: "max_tokens_exceeded: " + msg}
		}
	}

	// 2. USD budgets. The monthly one wins when both are spent: its reset
	// is the later of the two, so that is when a retry can succeed.
	if lim.DailyUSD != nil || lim.MonthlyUSD != nil {
		if l.spend == nil {
			return &denial{status: http.StatusServiceUnavailable,
				message: "budget enforcement unavailable: no Postgres capture store configured",
				capture: "limits_unavailable: no spend reader"}
		}
		switch {
		case lim.MonthlyUSD != nil && e.month >= *lim.MonthlyUSD:
			l.budgetDenials.Add(1)
			msg := "monthly budget exceeded for " + s.what
			return &denial{status: http.StatusTooManyRequests, message: msg, capture: "budget_exceeded: " + msg,
				retryAfter: secondsUntil(now, nextMonth)}
		case lim.DailyUSD != nil && e.day >= *lim.DailyUSD:
			l.budgetDenials.Add(1)
			msg := "daily budget exceeded for " + s.what
			return &denial{status: http.StatusTooManyRequests, message: msg, capture: "budget_exceeded: " + msg,
				retryAfter: secondsUntil(now, nextDay)}
		}
	}

	// 3. RPM, last: only an otherwise-admitted request takes a slot.
	if lim.RPM != nil && !e.admitRPM(*lim.RPM, now) {
		l.rpmDenials.Add(1)
		msg := "rate limit exceeded for " + s.what + " (requests per minute)"
		return &denial{status: http.StatusTooManyRequests, message: msg, capture: "rpm_exceeded: " + msg, retryAfter: 1}
	}
	return nil
}

// entry returns the subject's entry, creating it; it also sweeps idle
// entries at most once per idleEntryTTL.
func (l *Limiter) entry(k string, now time.Time) *limitEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.lastSweep) > idleEntryTTL {
		for id, e := range l.entries {
			if e.mu.TryLock() {
				idle := now.Sub(e.lastUsed) > idleEntryTTL
				e.mu.Unlock()
				if idle {
					delete(l.entries, id)
				}
			}
		}
		l.lastSweep = now
	}
	e := l.entries[k]
	if e == nil {
		e = &limitEntry{lastUsed: now}
		l.entries[k] = e
	}
	return e
}

// refresh re-reads the subject's limits (and, when it has a USD budget,
// its spend) once the cache is older than the TTL, the UTC day has rolled
// over, or -- for a subject whose limits are pinned -- the limits in hand
// are not the ones the cache was built for. On a failed read it keeps
// serving the previous values when there are any (a transient database
// error must not lock every budgeted key out); with nothing cached it
// returns the error and the caller refuses. Called with e.mu held.
func (l *Limiter) refresh(ctx context.Context, e *limitEntry, s subject, now, dayStart, monthStart time.Time) error {
	current := s.lookup != nil || e.limits == s.pinned
	if !e.fetchedAt.IsZero() && now.Sub(e.fetchedAt) < l.ttl && e.dayStart.Equal(dayStart) && current {
		return nil
	}
	limits := s.pinned
	if s.lookup != nil {
		var err error
		if limits, err = s.lookup(ctx); err != nil {
			return l.stale(e, current, dayStart, err)
		}
	}
	var day, month float64
	if limits != nil && (limits.DailyUSD != nil || limits.MonthlyUSD != nil) && l.spend != nil {
		var err error
		day, month, err = s.spend(ctx, l.spend, dayStart, monthStart)
		if err != nil {
			return l.stale(e, current, dayStart, fmt.Errorf("read spend: %w", err))
		}
	} else if limits != nil && (limits.DailyUSD != nil || limits.MonthlyUSD != nil) && l.warnedNoSpend.CompareAndSwap(false, true) {
		slog.Warn("llmplane: a USD budget is set but no Postgres capture store is configured; the requests it covers are refused (503)", s.attrs...)
	}
	e.limits, e.day, e.month, e.dayStart, e.fetchedAt = limits, day, month, dayStart, now
	return nil
}

// stale keeps the cached values after a failed refresh when they are for
// the current day and the current limits; otherwise it returns err.
func (l *Limiter) stale(e *limitEntry, current bool, dayStart time.Time, err error) error {
	if !e.fetchedAt.IsZero() && e.dayStart.Equal(dayStart) && current {
		slog.Warn("llmplane: limits refresh failed; using cached values", "error", err)
		return nil
	}
	return err
}

// observe adds a just-recorded call's cost to the cached spend of its key
// and of the model name it asked for, so this replica's own traffic counts
// before the next refresh. Unknown cost (nil) counts as 0.
func (l *Limiter) observe(tenantID, keyID, requestedModel string, at time.Time, cost *float64) {
	if cost == nil || *cost <= 0 {
		return
	}
	dayStart, _, _, _ := utcPeriods(at)
	for _, k := range []string{keyEntry(tenantID, keyID), modelEntry(tenantID, requestedModel)} {
		l.mu.Lock()
		e := l.entries[k]
		l.mu.Unlock()
		if e == nil {
			continue
		}
		e.mu.Lock()
		if !e.fetchedAt.IsZero() && e.dayStart.Equal(dayStart) {
			e.day += *cost
			e.month += *cost
		}
		e.mu.Unlock()
	}
}

// admitRPM is the exact sliding window: the ring holds the timestamps of
// the last rpm admitted requests; a new one is admitted iff fewer than rpm
// were admitted in the last minute, i.e. the ring is not yet full or its
// oldest entry is at least a minute old. O(1) per request, rpm*8 bytes per
// key. A changed rpm restarts the window. Called with e.mu held.
func (e *limitEntry) admitRPM(rpm int, now time.Time) bool {
	if rpm <= 0 {
		return false
	}
	if len(e.ring) != rpm {
		e.ring, e.next, e.n = make([]time.Time, rpm), 0, 0
	}
	if e.n == rpm && now.Sub(e.ring[e.next]) < rpmWindow {
		return false
	}
	e.ring[e.next] = now
	e.next = (e.next + 1) % rpm
	if e.n < rpm {
		e.n++
	}
	return true
}

// requestedMaxTokens reads the output-token cap a request asks for, across
// the dialects the plane serves: Anthropic / Bedrock-invoke / OpenAI Chat
// `max_tokens`, OpenAI `max_completion_tokens`, OpenAI Responses
// `max_output_tokens`, Gemini `generationConfig.maxOutputTokens`, Bedrock
// Converse `inferenceConfig.maxTokens`. The largest one present wins.
// ok=false when none is present (or the body is not JSON): a request that
// asks for no cap is not rejected — Anthropic requires the field, and for
// the others the provider default applies.
func requestedMaxTokens(body []byte) (float64, bool) {
	var m struct {
		MaxTokens           *float64 `json:"max_tokens"`
		MaxCompletionTokens *float64 `json:"max_completion_tokens"`
		MaxOutputTokens     *float64 `json:"max_output_tokens"`
		GenerationConfig    struct {
			MaxOutputTokens *float64 `json:"maxOutputTokens"`
		} `json:"generationConfig"`
		InferenceConfig struct {
			MaxTokens *float64 `json:"maxTokens"`
		} `json:"inferenceConfig"`
	}
	if json.Unmarshal(body, &m) != nil {
		return 0, false
	}
	var best float64
	var ok bool
	for _, v := range []*float64{m.MaxTokens, m.MaxCompletionTokens, m.MaxOutputTokens, m.GenerationConfig.MaxOutputTokens, m.InferenceConfig.MaxTokens} {
		if v != nil && (!ok || *v > best) {
			best, ok = *v, true
		}
	}
	return best, ok
}
