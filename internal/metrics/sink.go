package metrics

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink"
)

// Bounds on label values taken from request data. A label value that a
// caller can choose freely would let one API key create an unbounded
// number of series, so every such value is whitelisted, capped or
// bucketed before it is used.
const (
	// maxDistinctTools caps the distinct "tool" label values per process;
	// the rest are recorded as otherValue.
	maxDistinctTools = 500
	// maxDistinctModels caps the distinct "model" label values per process.
	maxDistinctModels = 500
	// maxLabelLen is the longest tool or model name used as a label value.
	maxLabelLen = 128

	otherValue   = "other"
	unknownValue = "unknown"

	// sourceInterceptor marks a record ingested from a companion capture
	// component (POST /api/v1/ingest) rather than proxied by this
	// gateway; it is not this process's traffic.
	sourceInterceptor = "interceptor"

	// streamMethod is the Method of a GET /mcp/stream access-log record
	// (internal/dataplane/transport/sse.go); it is written when the stream
	// ends, so its duration is the stream's lifetime.
	streamMethod = "GET /mcp/stream"
)

// mcpMethods is the closed set of "method" label values. The method
// string on an access-log record is whatever JSON-RPC method the caller
// sent, so anything outside this set is recorded as "other". Notifications
// are listed one by one rather than matched by prefix for the same reason.
// The last group are the requests a connector sends to the agent through
// the gateway (internal/dataplane/orchestrator/relay.go) and the agent's
// answer to one, which are logged as access records too.
var mcpMethods = map[string]string{
	mcp.MethodInitialize:                 mcp.MethodInitialize,
	mcp.MethodInitialized:                mcp.MethodInitialized,
	mcp.MethodPing:                       mcp.MethodPing,
	mcp.MethodToolsList:                  mcp.MethodToolsList,
	mcp.MethodToolsCall:                  mcp.MethodToolsCall,
	mcp.MethodPromptsList:                mcp.MethodPromptsList,
	mcp.MethodPromptsGet:                 mcp.MethodPromptsGet,
	mcp.MethodResourcesList:              mcp.MethodResourcesList,
	mcp.MethodResourcesRead:              mcp.MethodResourcesRead,
	mcp.MethodResourcesTemplatesList:     mcp.MethodResourcesTemplatesList,
	mcp.MethodResourcesSubscribe:         mcp.MethodResourcesSubscribe,
	mcp.MethodResourcesUnsubscribe:       mcp.MethodResourcesUnsubscribe,
	mcp.MethodSkillsList:                 mcp.MethodSkillsList,
	mcp.MethodSkillsGet:                  mcp.MethodSkillsGet,
	mcp.MethodCompletionComplete:         mcp.MethodCompletionComplete,
	mcp.MethodLoggingSetLevel:            mcp.MethodLoggingSetLevel,
	mcp.NotificationCancelled:            mcp.NotificationCancelled,
	mcp.NotificationProgress:             mcp.NotificationProgress,
	mcp.NotificationToolsListChanged:     mcp.NotificationToolsListChanged,
	mcp.NotificationResourcesUpdated:     mcp.NotificationResourcesUpdated,
	mcp.NotificationResourcesListChanged: mcp.NotificationResourcesListChanged,
	mcp.NotificationPromptsListChanged:   mcp.NotificationPromptsListChanged,
	"sampling/createMessage":             "sampling/createMessage",
	"elicitation/create":                 "elicitation/create",
	"roots/list":                         "roots/list",
	"response":                           "response",
	streamMethod:                         "stream",
}

// rpcErrorCodes is the closed set of "error_code" label values: the codes
// the gateway itself produces (pkg/mcp). A code can also arrive from a
// connector or from the agent's answer to a relayed request, so any other
// value is recorded as "other".
var rpcErrorCodes = func() map[string]struct{} {
	m := make(map[string]struct{})
	for _, c := range []int{
		mcp.ErrorCodeParseError, mcp.ErrorCodeInvalidRequest, mcp.ErrorCodeMethodNotFound,
		mcp.ErrorCodeInvalidParams, mcp.ErrorCodeInternalError, mcp.ErrorCodeRequestCancelled,
		mcp.ErrorCodeSessionNotFound, mcp.ErrorCodeUnauthorized, mcp.ErrorCodeForbidden,
		mcp.ErrorCodeRateLimited, mcp.ErrorCodeToolExecution, mcp.ErrorCodeToolNotAllowed,
		mcp.ErrorCodeConnectorNotFound, mcp.ErrorCodeConnectorUnhealthy,
	} {
		m[strconv.Itoa(c)] = struct{}{}
	}
	return m
}()

// Histogram bucket boundaries, in seconds.
var (
	// toolCallBuckets suits a connector round trip.
	toolCallBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}
	// llmCallBuckets suits a model call, which can stream for minutes.
	llmCallBuckets = []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 20, 30, 60, 120, 300}
)

// cappedSet admits at most max distinct values. Once full, a value it has
// not seen before is refused (the caller records it as "other"); values
// already admitted keep being accepted.
type cappedSet struct {
	max  int64
	n    atomic.Int64
	seen sync.Map
}

func (s *cappedSet) admit(v string) bool {
	if _, ok := s.seen.Load(v); ok {
		return true
	}
	if s.n.Add(1) > s.max {
		s.n.Add(-1)
		return false
	}
	if _, loaded := s.seen.LoadOrStore(v, struct{}{}); loaded {
		// Another goroutine admitted the same value first.
		s.n.Add(-1)
	}
	return true
}

// recordSink turns the gateway's access-log and LLM-call records into
// metrics. It implements sink.LogSink so it rides sink.Multi next to the
// storage sinks; its writes are synchronous instrument Adds, which are
// cheap and never block on I/O, as the sink contract requires.
type recordSink struct {
	mcpRequests  metric.Int64Counter
	mcpToolCalls metric.Int64Counter
	mcpToolDur   metric.Float64Histogram
	mcpErrors    metric.Int64Counter

	llmCalls     metric.Int64Counter
	llmTokens    metric.Int64Counter
	llmCost      metric.Float64Counter
	llmDur       metric.Float64Histogram
	llmFallbacks metric.Int64Counter

	tools  cappedSet
	models cappedSet
}

// Sink returns the LogSink that records the MCP and LLM metrics from the
// access-log and LLM-call records. Add it to the sink.Multi when Enabled.
// On a nil or disabled *Metrics its instruments are no-ops. Close does
// nothing: the exporter is closed through (*Metrics).Close.
func (m *Metrics) Sink() sink.LogSink {
	meter := m.Meter()
	noopMeter := noop.NewMeterProvider().Meter(MeterName)
	reg := func(err error) {
		if err != nil && m != nil {
			// Only a malformed name or option reaches here, i.e. a
			// programming error: keep serving, with that instrument
			// replaced by a no-op below.
			m.logger.Error("metrics: create instrument", "error", err)
		}
	}
	// A failed construction leaves the zero value (nil) in the struct on
	// some SDK versions; swap in the no-op instrument so Add never panics.
	i64 := func(name, desc string) metric.Int64Counter {
		c, err := meter.Int64Counter(name, metric.WithDescription(desc))
		if err != nil {
			reg(err)
			c, _ = noopMeter.Int64Counter(name)
		}
		return c
	}
	f64c := func(name, desc string) metric.Float64Counter {
		c, err := meter.Float64Counter(name, metric.WithDescription(desc))
		if err != nil {
			reg(err)
			c, _ = noopMeter.Float64Counter(name)
		}
		return c
	}
	hist := func(name, desc string, buckets []float64) metric.Float64Histogram {
		h, err := meter.Float64Histogram(name,
			metric.WithDescription(desc),
			metric.WithUnit("s"),
			metric.WithExplicitBucketBoundaries(buckets...))
		if err != nil {
			reg(err)
			h, _ = noopMeter.Float64Histogram(name)
		}
		return h
	}

	return &recordSink{
		mcpRequests: i64("mcp_requests",
			"MCP requests that reached the orchestrator, by JSON-RPC method and outcome."),
		mcpToolCalls: i64("mcp_tool_calls",
			"tools/call requests routed to a connector, by connector and tool."),
		mcpToolDur: hist("mcp_tool_call_duration_seconds",
			"Duration of tools/call requests routed to a connector.", toolCallBuckets),
		mcpErrors: i64("mcp_errors",
			"MCP requests that ended in a JSON-RPC error, by error code."),

		llmCalls: i64("llm_calls",
			"LLM calls that were routed to a provider, by model, HTTP status and streaming."),
		llmTokens: i64("llm_tokens",
			"Tokens reported by the provider, by kind (input, output, cache_read, cache_creation)."),
		llmCost: f64c("llm_cost_usd",
			"Cost of LLM calls in US dollars, priced from the rate card at write time."),
		llmDur: hist("llm_call_duration_seconds",
			"Duration of LLM calls, including the whole stream for streaming calls.", llmCallBuckets),
		llmFallbacks: i64("llm_fallbacks",
			"LLM calls answered by a fallback target of a registered model."),

		tools:  cappedSet{max: maxDistinctTools},
		models: cappedSet{max: maxDistinctModels},
	}
}

// Close is a no-op: the sink owns no resources.
func (s *recordSink) Close() error { return nil }

// WriteAccess records one MCP access-log record.
func (s *recordSink) WriteAccess(a *sink.AccessLog) {
	// Method "" is a request that never reached the orchestrator: health
	// probes, authentication failures, session DELETE. HTTP-level counts
	// for those come from the plane middleware.
	if a == nil || a.Method == "" || a.Source == sourceInterceptor {
		return
	}
	ctx := context.Background()
	tenant := orUnknown(a.TenantID)
	method, ok := mcpMethods[a.Method]
	if !ok {
		method = otherValue
	}
	outcome := mcpOutcome(a.StatusCode, a.ErrorCode)

	s.mcpRequests.Add(ctx, 1, metric.WithAttributes(
		attribute.String("tenant", tenant),
		attribute.String("method", method),
		attribute.String("outcome", outcome),
	))

	if a.ErrorCode != "" {
		code := a.ErrorCode
		if _, ok := rpcErrorCodes[code]; !ok {
			code = otherValue
		}
		s.mcpErrors.Add(ctx, 1, metric.WithAttributes(
			attribute.String("tenant", tenant),
			attribute.String("method", method),
			attribute.String("error_code", code),
		))
	}

	// ToolName is set from the request before routing, so it is only a
	// real tool when a connector was resolved for the call. The prefix
	// before "__" is the connector slug; ConnectorID is a UUID and is
	// never used as a label. GET /mcp/stream and every other method have
	// Method != tools/call and never reach the duration histogram.
	if a.Method != mcp.MethodToolsCall || a.ConnectorID == "" {
		return
	}
	connector, _, found := strings.Cut(a.ToolName, "__")
	if !found || connector == "" {
		return
	}
	connector = boundLabel(connector)
	tool := a.ToolName
	if len(tool) > maxLabelLen || !s.tools.admit(tool) {
		tool = otherValue
	}
	s.mcpToolCalls.Add(ctx, 1, metric.WithAttributes(
		attribute.String("tenant", tenant),
		attribute.String("connector", connector),
		attribute.String("tool", tool),
		attribute.String("outcome", outcome),
	))
	if a.DurationMS >= 0 {
		s.mcpToolDur.Record(ctx, float64(a.DurationMS)/1000, metric.WithAttributes(
			attribute.String("connector", connector),
		))
	}
}

// WriteLLMCall records one LLM-plane record.
func (s *recordSink) WriteLLMCall(c *sink.LLMCall) {
	if c == nil || c.Source == sourceInterceptor {
		return
	}
	ctx := context.Background()
	tenant := orUnknown(c.TenantID)
	provider := orUnknown(c.Provider)
	model := s.modelLabel(c)

	s.llmCalls.Add(ctx, 1, metric.WithAttributes(
		attribute.String("tenant", tenant),
		attribute.String("provider", provider),
		attribute.String("model", model),
		attribute.String("status", strconv.Itoa(c.StatusCode)),
		attribute.String("stream", strconv.FormatBool(c.Stream)),
	))

	for _, t := range [...]struct {
		kind string
		n    int64
	}{
		{"input", c.InputTokens},
		{"output", c.OutputTokens},
		{"cache_read", c.CacheReadTokens},
		{"cache_creation", c.CacheCreationTokens},
	} {
		if t.n <= 0 {
			continue
		}
		s.llmTokens.Add(ctx, t.n, metric.WithAttributes(
			attribute.String("tenant", tenant),
			attribute.String("provider", provider),
			attribute.String("model", model),
			attribute.String("kind", t.kind),
		))
	}

	if c.CostUSD != nil && *c.CostUSD > 0 {
		s.llmCost.Add(ctx, *c.CostUSD, metric.WithAttributes(
			attribute.String("tenant", tenant),
			attribute.String("provider", provider),
			attribute.String("model", model),
		))
	}

	if c.DurationMS >= 0 {
		s.llmDur.Record(ctx, float64(c.DurationMS)/1000, metric.WithAttributes(
			attribute.String("provider", provider),
			attribute.String("model", model),
		))
	}

	if c.FallbackIndex > 0 {
		s.llmFallbacks.Add(ctx, 1, metric.WithAttributes(
			attribute.String("tenant", tenant),
			attribute.String("model", model),
		))
	}
}

// modelLabel returns the bounded "model" label for c: the name the client
// asked for, as long as it is a name the gateway or the vendor knows.
// RequestedModel is read from the request body (or, for Bedrock, the URL
// path) verbatim, so an arbitrary string can end up in it. It is used when
// the model registry resolved it (ResolvedVendor set: an operator-created
// name) or when the upstream accepted it (2xx: a real vendor model id);
// any other call is "unknown". The result also passes through a
// per-process cap on distinct values.
func (s *recordSink) modelLabel(c *sink.LLMCall) string {
	model := c.RequestedModel
	if model == "" {
		model = c.Model
	}
	if model == "" {
		return unknownValue
	}
	registered := c.ResolvedVendor != ""
	accepted := c.StatusCode >= 200 && c.StatusCode < 300
	if !registered && !accepted {
		return unknownValue
	}
	if len(model) > maxLabelLen || !s.models.admit(model) {
		return otherValue
	}
	return model
}

// mcpOutcome classifies an access-log record: "rpc_error" when the
// response carried a JSON-RPC error (these return HTTP 200), else by HTTP
// status. A tool that ran and reported isError is still "ok" here: the
// record does not carry that.
func mcpOutcome(status int, errorCode string) string {
	switch {
	case errorCode != "":
		return "rpc_error"
	case status >= 500:
		return "http_5xx"
	case status >= 400:
		return "http_4xx"
	default:
		return "ok"
	}
}

func orUnknown(v string) string {
	if v == "" {
		return unknownValue
	}
	return v
}

// boundLabel keeps an over-long value out of a label.
func boundLabel(v string) string {
	if len(v) > maxLabelLen {
		return otherValue
	}
	return v
}
