package metrics

import (
	"context"
	"database/sql"
	"fmt"
	"math"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// This file holds the instruments whose values already live in a counter
// or a field of some other object: they are read when Prometheus scrapes
// (OpenTelemetry observable instruments), so the code that owns the value
// does no metrics work on its hot path.
//
// Each Register function is called once from cmd/gateway/main.go, as soon
// as the object it reads exists. They depend on small interfaces, not on
// the concrete types, so this package imports none of the planes. Every
// one accepts a nil *Metrics (a no-op Meter) and a disabled one.
//
// Counters are cumulative since process start and exported as
// gateway_<name>_total; Prometheus applies rate() to them.

// AuthLimiter is what a plane's auth rate limiter exposes
// (internal/auth.RateLimiter).
type AuthLimiter interface {
	// LockedIPCount is how many client IPs are locked out right now.
	LockedIPCount() int
	// Failures is the auth failures reported since process start.
	Failures() uint64
}

// RegisterAuthLimiters exposes auth_locked_ips (gauge) and auth_failures
// (counter), labelled by plane, for each limiter in the map; keys are the
// plane names ("api", "mcp", "llm").
func RegisterAuthLimiters(m *Metrics, limiters map[string]AuthLimiter) error {
	type planeAttr struct {
		l    AuthLimiter
		opts metric.MeasurementOption
	}
	planes := make([]planeAttr, 0, len(limiters))
	for plane, l := range limiters {
		if l == nil {
			continue
		}
		planes = append(planes, planeAttr{l, metric.WithAttributes(attribute.String("plane", plane))})
	}
	meter := m.Meter()
	if _, err := meter.Int64ObservableGauge("auth_locked_ips",
		metric.WithDescription("Client IPs currently locked out after repeated auth failures, per plane."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			for _, p := range planes {
				o.Observe(int64(p.l.LockedIPCount()), p.opts)
			}
			return nil
		}),
	); err != nil {
		return fmt.Errorf("metrics: register auth_locked_ips: %w", err)
	}
	if _, err := meter.Int64ObservableCounter("auth_failures",
		metric.WithDescription("Failed authentications since start, per plane. Requests with no credential are not counted."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			for _, p := range planes {
				o.Observe(clamp(p.l.Failures()), p.opts)
			}
			return nil
		}),
	); err != nil {
		return fmt.Errorf("metrics: register auth_failures: %w", err)
	}
	return nil
}

// LLMLimiter is the per-key limiter of the LLM plane
// (internal/llmplane.Limiter). Denials are process-wide, not per tenant.
type LLMLimiter interface {
	BudgetDenials() uint64
	RPMDenials() uint64
}

// RegisterLLMLimits exposes llm_limit_denials (counter), labelled
// reason="budget" or reason="rpm".
func RegisterLLMLimits(m *Metrics, l LLMLimiter) error {
	budget := metric.WithAttributes(attribute.String("reason", "budget"))
	rpm := metric.WithAttributes(attribute.String("reason", "rpm"))
	if _, err := m.Meter().Int64ObservableCounter("llm_limit_denials",
		metric.WithDescription("LLM requests refused by a per-key limit since start, by reason (budget, rpm)."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(clamp(l.BudgetDenials()), budget)
			o.Observe(clamp(l.RPMDenials()), rpm)
			return nil
		}),
	); err != nil {
		return fmt.Errorf("metrics: register llm_limit_denials: %w", err)
	}
	return nil
}

// DetectionTee is the LLM plane's copy of finished turns to the detection
// agent (internal/llmplane.DetectionTee).
type DetectionTee interface {
	Sent() uint64
	Dropped() uint64
	Failed() uint64
	QueueDepth() int
}

// RegisterDetectionTee exposes llm_detection_turns (counter, labelled
// result="sent", "dropped" or "failed") and llm_detection_queue_depth
// (gauge).
func RegisterDetectionTee(m *Metrics, t DetectionTee) error {
	sent := metric.WithAttributes(attribute.String("result", "sent"))
	dropped := metric.WithAttributes(attribute.String("result", "dropped"))
	failed := metric.WithAttributes(attribute.String("result", "failed"))
	meter := m.Meter()
	if _, err := meter.Int64ObservableCounter("llm_detection_turns",
		metric.WithDescription("Turns offered to the detection agent since start, by result (sent, dropped, failed)."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(clamp(t.Sent()), sent)
			o.Observe(clamp(t.Dropped()), dropped)
			o.Observe(clamp(t.Failed()), failed)
			return nil
		}),
	); err != nil {
		return fmt.Errorf("metrics: register llm_detection_turns: %w", err)
	}
	if _, err := meter.Int64ObservableGauge("llm_detection_queue_depth",
		metric.WithDescription("Turns waiting to be posted to the detection agent."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(int64(t.QueueDepth()))
			return nil
		}),
	); err != nil {
		return fmt.Errorf("metrics: register llm_detection_queue_depth: %w", err)
	}
	return nil
}

// RegisterSinkDrops exposes sink_records_dropped (counter), labelled by
// sink, from each sink's Dropped counter. Keys are the sink names
// ("stdout", "clickhouse").
func RegisterSinkDrops(m *Metrics, dropped map[string]func() uint64) error {
	type sinkAttr struct {
		dropped func() uint64
		opts    metric.MeasurementOption
	}
	sinks := make([]sinkAttr, 0, len(dropped))
	for name, fn := range dropped {
		if fn == nil {
			continue
		}
		sinks = append(sinks, sinkAttr{fn, metric.WithAttributes(attribute.String("sink", name))})
	}
	if _, err := m.Meter().Int64ObservableCounter("sink_records_dropped",
		metric.WithDescription("Log records a sink dropped because its queue was full, since start, per sink."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			for _, s := range sinks {
				o.Observe(clamp(s.dropped()), s.opts)
			}
			return nil
		}),
	); err != nil {
		return fmt.Errorf("metrics: register sink_records_dropped: %w", err)
	}
	return nil
}

// BodyStore is the capture recorder's body-offload tally
// (internal/capture.Recorder).
type BodyStore interface {
	Offloaded() uint64
	Fallbacks() uint64
}

// RegisterBodyStore exposes body_store_offloads (calls whose bodies went
// to the body store) and body_store_fallbacks (offloads that failed and
// were stored inline instead; non-zero means an unhealthy store), both
// counters.
func RegisterBodyStore(m *Metrics, b BodyStore) error {
	meter := m.Meter()
	if _, err := meter.Int64ObservableCounter("body_store_offloads",
		metric.WithDescription("LLM calls whose request and response bodies were offloaded to the body store, since start."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(clamp(b.Offloaded()))
			return nil
		}),
	); err != nil {
		return fmt.Errorf("metrics: register body_store_offloads: %w", err)
	}
	if _, err := meter.Int64ObservableCounter("body_store_fallbacks",
		metric.WithDescription("Body offloads that failed and stored the bodies inline instead, since start."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(clamp(b.Fallbacks()))
			return nil
		}),
	); err != nil {
		return fmt.Errorf("metrics: register body_store_fallbacks: %w", err)
	}
	return nil
}

// DBStatser is a store that can report its connection pool
// (internal/store/postgres.Store).
type DBStatser interface {
	Stats() sql.DBStats
}

// RegisterDBStats exposes db_connections (gauge, labelled state="open",
// "idle" or "in_use") and db_wait_duration_seconds (counter: total time
// callers waited for a connection).
func RegisterDBStats(m *Metrics, s DBStatser) error {
	open := metric.WithAttributes(attribute.String("state", "open"))
	idle := metric.WithAttributes(attribute.String("state", "idle"))
	inUse := metric.WithAttributes(attribute.String("state", "in_use"))
	meter := m.Meter()
	if _, err := meter.Int64ObservableGauge("db_connections",
		metric.WithDescription("Database pool connections by state (open, idle, in_use)."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			st := s.Stats()
			o.Observe(int64(st.OpenConnections), open)
			o.Observe(int64(st.Idle), idle)
			o.Observe(int64(st.InUse), inUse)
			return nil
		}),
	); err != nil {
		return fmt.Errorf("metrics: register db_connections: %w", err)
	}
	if _, err := meter.Float64ObservableCounter("db_wait_duration_seconds",
		metric.WithDescription("Total time callers have waited for a database connection, since start."),
		metric.WithUnit("s"),
		metric.WithFloat64Callback(func(_ context.Context, o metric.Float64Observer) error {
			o.Observe(s.Stats().WaitDuration.Seconds())
			return nil
		}),
	); err != nil {
		return fmt.Errorf("metrics: register db_wait_duration_seconds: %w", err)
	}
	return nil
}

// RegisterSessions exposes mcp_sessions_active (gauge) and
// mcp_sessions_created (counter). active returns the live session count,
// or a negative number when the session driver cannot say cheaply
// (redis); the gauge then reports no value at all rather than a wrong
// one. created is the sessions created since process start.
func RegisterSessions(m *Metrics, active func() int, created func() uint64) error {
	meter := m.Meter()
	if _, err := meter.Int64ObservableGauge("mcp_sessions_active",
		metric.WithDescription("MCP sessions held by this process's session store. Absent when the driver cannot count them."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			if n := active(); n >= 0 {
				o.Observe(int64(n))
			}
			return nil
		}),
	); err != nil {
		return fmt.Errorf("metrics: register mcp_sessions_active: %w", err)
	}
	if _, err := meter.Int64ObservableCounter("mcp_sessions_created",
		metric.WithDescription("MCP sessions created by this process since start."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(clamp(created()))
			return nil
		}),
	); err != nil {
		return fmt.Errorf("metrics: register mcp_sessions_created: %w", err)
	}
	return nil
}

// ToolCacheLookupHook returns the function to hand the MCP plane as
// dataplane.Deps.OnToolCacheLookup. It adds one to mcp_tool_cache_lookups
// (counter), labelled result="hit", "stale" or "miss", for every tools/list
// that consults the tool cache. result must be one of those three; the
// plane passes the cache.Lookup* constants.
func ToolCacheLookupHook(m *Metrics) (func(result string), error) {
	c, err := m.Meter().Int64Counter("mcp_tool_cache_lookups",
		metric.WithDescription("tools/list reads of the tool cache, by result (hit, stale, miss)."))
	if err != nil {
		return nil, fmt.Errorf("metrics: register mcp_tool_cache_lookups: %w", err)
	}
	// Prebuilt, so the request path allocates nothing for the attribute.
	opts := map[string]metric.AddOption{
		"hit":   metric.WithAttributes(attribute.String("result", "hit")),
		"stale": metric.WithAttributes(attribute.String("result", "stale")),
		"miss":  metric.WithAttributes(attribute.String("result", "miss")),
	}
	return func(result string) {
		if o, ok := opts[result]; ok {
			c.Add(context.Background(), 1, o)
		}
	}, nil
}

// clamp converts a cumulative uint64 counter to the int64 an observer
// takes, saturating instead of wrapping negative.
func clamp(v uint64) int64 {
	if v > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(v)
}
