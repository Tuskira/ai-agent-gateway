package metrics

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/config"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink"
)

// series is one sample line of the Prometheus text format.
type series struct {
	name   string
	labels map[string]string
	value  float64
}

var (
	sampleRE = regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)(?:\{(.*)\})? (\S+)$`)
	labelRE  = regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)="((?:[^"\\]|\\.)*)"`)
)

// scrape renders m's registry as Prometheus text and parses it.
func scrapeSeries(t *testing.T, m *Metrics) []series {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape status = %d", rec.Code)
	}
	var out []series
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		mm := sampleRE.FindStringSubmatch(line)
		if mm == nil {
			t.Fatalf("unparsable sample line %q", line)
		}
		v, err := strconv.ParseFloat(mm[3], 64)
		if err != nil {
			t.Fatalf("bad value in %q: %v", line, err)
		}
		s := series{name: mm[1], labels: map[string]string{}, value: v}
		for _, l := range labelRE.FindAllStringSubmatch(mm[2], -1) {
			s.labels[l[1]] = l[2]
		}
		out = append(out, s)
	}
	return out
}

// find returns the samples named name whose labels include want.
func find(all []series, name string, want map[string]string) []series {
	var out []series
	for _, s := range all {
		if s.name != name {
			continue
		}
		ok := true
		for k, v := range want {
			if s.labels[k] != v {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, s)
		}
	}
	return out
}

// sum adds up the samples find returns (0 when there are none).
func sum(all []series, name string, want map[string]string) float64 {
	var total float64
	for _, s := range find(all, name, want) {
		total += s.value
	}
	return total
}

// labelValues is the set of values label takes across the samples named name.
func labelValues(all []series, name, label string) map[string]bool {
	out := map[string]bool{}
	for _, s := range all {
		if s.name == name {
			out[s.labels[label]] = true
		}
	}
	return out
}

func newSinkMetrics(t *testing.T) (*Metrics, sink.LogSink) {
	t.Helper()
	m := newMetrics(t, promCfg("127.0.0.1:0"))
	return m, m.Sink()
}

// toolCall is a routed tools/call record, the shape the orchestrator writes.
func toolCall(tenant, tool string) *sink.AccessLog {
	return &sink.AccessLog{
		TenantID: tenant, Method: "tools/call", ToolName: tool,
		ConnectorID: "7d1b3c1e-0000-4000-8000-000000000001", StatusCode: 200, DurationMS: 120,
		// Fields that must never become labels.
		KeyID: "key-1", SessionID: "sess-1", RequestID: "req-1", Principal: "alice",
	}
}

func TestSinkMCPRequestsMethodAndOutcome(t *testing.T) {
	tests := []struct {
		name       string
		rec        sink.AccessLog
		wantMethod string
		wantResult string
	}{
		{"initialize ok", sink.AccessLog{Method: "initialize", StatusCode: 200}, "initialize", "ok"},
		{"tools/list ok", sink.AccessLog{Method: "tools/list", StatusCode: 200}, "tools/list", "ok"},
		{"prompts/get ok", sink.AccessLog{Method: "prompts/get", StatusCode: 200}, "prompts/get", "ok"},
		{"resources/templates/list", sink.AccessLog{Method: "resources/templates/list", StatusCode: 200}, "resources/templates/list", "ok"},
		{"skills/list", sink.AccessLog{Method: "skills/list", StatusCode: 200}, "skills/list", "ok"},
		{"notification", sink.AccessLog{Method: "notifications/initialized", StatusCode: 202}, "notifications/initialized", "ok"},
		{"agent answer", sink.AccessLog{Method: "response", StatusCode: 202}, "response", "ok"},
		{"connector sampling request", sink.AccessLog{Method: "sampling/createMessage", StatusCode: 200}, "sampling/createMessage", "ok"},
		{"stream is its own method", sink.AccessLog{Method: "GET /mcp/stream", StatusCode: 200}, "stream", "ok"},
		{"unknown method", sink.AccessLog{Method: "x/" + strings.Repeat("a", 40), StatusCode: 200}, "other", "ok"},
		{"unknown notification", sink.AccessLog{Method: "notifications/whatever", StatusCode: 200}, "other", "ok"},
		{"method with url-ish text", sink.AccessLog{Method: "GET /api/v1/connectors/123", StatusCode: 200}, "other", "ok"},
		{"rpc error rides http 200", sink.AccessLog{Method: "tools/call", StatusCode: 200, ErrorCode: "-32003"}, "tools/call", "rpc_error"},
		{"rpc error beats 4xx", sink.AccessLog{Method: "initialize", StatusCode: 403, ErrorCode: "-32006"}, "initialize", "rpc_error"},
		{"http 4xx", sink.AccessLog{Method: "tools/list", StatusCode: 404}, "tools/list", "http_4xx"},
		{"http 429", sink.AccessLog{Method: "tools/list", StatusCode: 429}, "tools/list", "http_4xx"},
		{"http 5xx", sink.AccessLog{Method: "tools/list", StatusCode: 502}, "tools/list", "http_5xx"},
		{"http 599", sink.AccessLog{Method: "ping", StatusCode: 599}, "ping", "http_5xx"},
		{"http 3xx counts as ok", sink.AccessLog{Method: "ping", StatusCode: 302}, "ping", "ok"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, s := newSinkMetrics(t)
			rec := tt.rec
			rec.TenantID = "tenant-a"
			s.WriteAccess(&rec)
			got := scrapeSeries(t, m)
			want := map[string]string{"tenant": "tenant-a", "method": tt.wantMethod, "outcome": tt.wantResult}
			if n := sum(got, "gateway_mcp_requests_total", want); n != 1 {
				t.Errorf("gateway_mcp_requests_total%v = %v, want 1; series: %v", want, n, find(got, "gateway_mcp_requests_total", nil))
			}
			if n := len(find(got, "gateway_mcp_requests_total", nil)); n != 1 {
				t.Errorf("%d request series, want exactly 1", n)
			}
		})
	}
}

func TestSinkMCPSkipsRecords(t *testing.T) {
	tests := []struct {
		name string
		rec  sink.AccessLog
	}{
		{"empty method (health, 401, DELETE)", sink.AccessLog{Method: "", StatusCode: 401, TenantID: "t"}},
		{"empty method with error code", sink.AccessLog{Method: "", StatusCode: 401, ErrorCode: "-32001"}},
		{"interceptor row", sink.AccessLog{Method: "tools/call", Source: "interceptor", StatusCode: 200, TenantID: "t"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, s := newSinkMetrics(t)
			rec := tt.rec
			s.WriteAccess(&rec)
			s.WriteAccess(nil)
			for _, sm := range scrapeSeries(t, m) {
				if strings.HasPrefix(sm.name, "gateway_mcp_") {
					t.Errorf("record produced %s%v, want no mcp series", sm.name, sm.labels)
				}
			}
		})
	}
}

func TestSinkMCPErrorCodesAreWhitelisted(t *testing.T) {
	m, s := newSinkMetrics(t)
	for _, code := range []string{"-32003", "-32003", "-32602", "-32002", "-1", "12345", "not-a-number", "-32003 "} {
		s.WriteAccess(&sink.AccessLog{TenantID: "t", Method: "tools/call", StatusCode: 200, ErrorCode: code})
	}
	got := scrapeSeries(t, m)
	base := map[string]string{"tenant": "t", "method": "tools/call"}
	for code, want := range map[string]float64{"-32003": 2, "-32602": 1, "-32002": 1, "other": 4} {
		w := map[string]string{"tenant": "t", "method": "tools/call", "error_code": code}
		if n := sum(got, "gateway_mcp_errors_total", w); n != want {
			t.Errorf("mcp_errors error_code=%s = %v, want %v", code, n, want)
		}
	}
	if vals := labelValues(got, "gateway_mcp_errors_total", "error_code"); len(vals) != 4 {
		t.Errorf("error_code values = %v, want 4 distinct", vals)
	}
	if n := sum(got, "gateway_mcp_requests_total", base); n != 8 {
		t.Errorf("mcp_requests = %v, want 8", n)
	}
	// A record with no error code adds no mcp_errors sample.
	s.WriteAccess(&sink.AccessLog{TenantID: "t2", Method: "ping", StatusCode: 200})
	if n := len(find(scrapeSeries(t, m), "gateway_mcp_errors_total", map[string]string{"tenant": "t2"})); n != 0 {
		t.Errorf("clean record produced %d mcp_errors series", n)
	}
}

func TestSinkMCPToolCalls(t *testing.T) {
	m, s := newSinkMetrics(t)

	s.WriteAccess(toolCall("t1", "github__create_issue"))
	s.WriteAccess(toolCall("t1", "github__create_issue"))
	failed := toolCall("t1", "github__create_issue")
	failed.ErrorCode = "-32002"
	s.WriteAccess(failed)
	s.WriteAccess(toolCall("t1", "jira__a__b")) // connector is the prefix before the FIRST "__"

	// Not tool calls we can attribute.
	noConnector := toolCall("t1", "ghost__tool") // connector never resolved: name is client text
	noConnector.ConnectorID = ""
	s.WriteAccess(noConnector)
	s.WriteAccess(toolCall("t1", "gateway-native-tool")) // no "__"
	s.WriteAccess(toolCall("t1", "__leading"))           // empty connector
	listCall := toolCall("t1", "github__create_issue")
	listCall.Method = "tools/list"
	s.WriteAccess(listCall)
	longTool := toolCall("t1", "github__"+strings.Repeat("x", 200))
	s.WriteAccess(longTool)

	got := scrapeSeries(t, m)
	cases := []struct {
		labels map[string]string
		want   float64
	}{
		{map[string]string{"connector": "github", "tool": "github__create_issue", "outcome": "ok"}, 2},
		{map[string]string{"connector": "github", "tool": "github__create_issue", "outcome": "rpc_error"}, 1},
		{map[string]string{"connector": "jira", "tool": "jira__a__b", "outcome": "ok"}, 1},
		{map[string]string{"connector": "github", "tool": "other", "outcome": "ok"}, 1},
	}
	for _, c := range cases {
		c.labels["tenant"] = "t1"
		if n := sum(got, "gateway_mcp_tool_calls_total", c.labels); n != c.want {
			t.Errorf("mcp_tool_calls%v = %v, want %v", c.labels, n, c.want)
		}
	}
	if n := len(find(got, "gateway_mcp_tool_calls_total", nil)); n != 4 {
		t.Errorf("%d mcp_tool_calls series, want 4: %v", n, find(got, "gateway_mcp_tool_calls_total", nil))
	}

	// The duration histogram has the connector label only: four
	// attributable github calls (three create_issue and the long-named
	// one) at 120 ms each.
	if n := sum(got, "gateway_mcp_tool_call_duration_seconds_count", map[string]string{"connector": "github"}); n != 4 {
		t.Errorf("github duration count = %v, want 4", n)
	}
	if n := sum(got, "gateway_mcp_tool_call_duration_seconds_sum", map[string]string{"connector": "jira"}); n < 0.119 || n > 0.121 {
		t.Errorf("jira duration sum = %v, want 0.12", n)
	}
	for _, sm := range find(got, "gateway_mcp_tool_call_duration_seconds_bucket", nil) {
		if len(sm.labels) != 2 { // connector, le
			t.Errorf("histogram series labels = %v, want connector and le only", sm.labels)
		}
	}
	// Identifiers never appear as label values, anywhere.
	for _, sm := range got {
		for k, v := range sm.labels {
			switch v {
			case "key-1", "sess-1", "req-1", "alice", "7d1b3c1e-0000-4000-8000-000000000001":
				t.Errorf("%s has identifier %s=%q as a label", sm.name, k, v)
			}
		}
	}
}

func TestSinkStreamRecordNotInHistogram(t *testing.T) {
	m, s := newSinkMetrics(t)
	// A stream record carries the connector-less, minutes-long lifetime.
	s.WriteAccess(&sink.AccessLog{TenantID: "t", Method: "GET /mcp/stream", StatusCode: 200, DurationMS: 600000,
		ToolName: "github__x", ConnectorID: "c"})
	got := scrapeSeries(t, m)
	if n := len(find(got, "gateway_mcp_tool_call_duration_seconds_count", nil)); n != 0 {
		t.Errorf("stream record reached the tool-call histogram")
	}
	if n := sum(got, "gateway_mcp_requests_total", map[string]string{"method": "stream"}); n != 1 {
		t.Errorf("stream requests = %v, want 1", n)
	}
	if n := len(find(got, "gateway_mcp_tool_calls_total", nil)); n != 0 {
		t.Errorf("stream record counted as a tool call")
	}
}

func TestSinkToolCardinalityCap(t *testing.T) {
	m, s := newSinkMetrics(t)
	const distinct = 501
	for i := 0; i < distinct; i++ {
		s.WriteAccess(toolCall("t", fmt.Sprintf("echo__tool%03d", i)))
	}
	// Tools already admitted keep their own series after the cap is hit;
	// new ones go to "other".
	s.WriteAccess(toolCall("t", "echo__tool000"))
	s.WriteAccess(toolCall("t", "echo__brand_new"))

	got := scrapeSeries(t, m)
	tools := labelValues(got, "gateway_mcp_tool_calls_total", "tool")
	if len(tools) != maxDistinctTools+1 {
		t.Fatalf("distinct tool values = %d, want %d (cap + other)", len(tools), maxDistinctTools+1)
	}
	if !tools["other"] {
		t.Error(`no "other" tool series after exceeding the cap`)
	}
	if n := sum(got, "gateway_mcp_tool_calls_total", map[string]string{"tool": "other"}); n != 2 {
		t.Errorf(`"other" = %v, want 2 (tool500 and brand_new)`, n)
	}
	if n := sum(got, "gateway_mcp_tool_calls_total", map[string]string{"tool": "echo__tool000"}); n != 2 {
		t.Errorf("admitted tool000 = %v, want 2", n)
	}
	if tools["echo__tool500"] || tools["echo__brand_new"] {
		t.Error("a tool past the cap kept its own series")
	}
	if n := sum(got, "gateway_mcp_tool_calls_total", nil); n != distinct+2 {
		t.Errorf("total tool calls = %v, want %d: the cap must not drop counts", n, distinct+2)
	}
}

func TestCappedSetConcurrent(t *testing.T) {
	s := cappedSet{max: 50}
	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted := map[string]bool{}
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 400; i++ {
				v := fmt.Sprintf("v%03d", i)
				if s.admit(v) {
					mu.Lock()
					admitted[v] = true
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	if len(admitted) > 50 {
		t.Errorf("admitted %d distinct values, cap is 50", len(admitted))
	}
	if n := s.n.Load(); n < 0 || n > 50 {
		t.Errorf("counter = %d, want within [0,50]", n)
	}
	// Values admitted once stay admitted.
	for v := range admitted {
		if !s.admit(v) {
			t.Errorf("admitted value %q was refused later", v)
		}
	}
}

func f64(v float64) *float64 { return &v }

func noneCfg() config.Metrics {
	c := config.Default().Metrics
	c.Driver = config.MetricsDriverNone
	return c
}

func TestSinkLLMCall(t *testing.T) {
	m, s := newSinkMetrics(t)

	s.WriteLLMCall(&sink.LLMCall{
		TenantID: "t1", Provider: "anthropic", Model: "claude-sonnet-4-5", RequestedModel: "claude-sonnet-4-5",
		StatusCode: 200, DurationMS: 2500, Stream: true,
		InputTokens: 100, OutputTokens: 40, CacheReadTokens: 7, CacheCreationTokens: 0,
		CostUSD: f64(0.0125),
		KeyID:   "key-9", SessionID: "sess-9", RequestID: "req-9",
	})
	s.WriteLLMCall(&sink.LLMCall{
		TenantID: "t1", Provider: "anthropic", Model: "claude-sonnet-4-5", RequestedModel: "claude-sonnet-4-5",
		StatusCode: 200, InputTokens: 10, OutputTokens: 5, CostUSD: f64(0.0025), DurationMS: 500,
	})
	// A count_tokens style call: no cost, no output.
	s.WriteLLMCall(&sink.LLMCall{
		TenantID: "t1", Provider: "anthropic", Model: "claude-sonnet-4-5", RequestedModel: "claude-sonnet-4-5",
		StatusCode: 200, InputTokens: 3, CostUSD: nil, DurationMS: 20,
	})
	// Answered by the second target of a registered model.
	s.WriteLLMCall(&sink.LLMCall{
		TenantID: "t2", Provider: "openai", Model: "gpt-4o", RequestedModel: "smart",
		ResolvedVendor: "openai", ResolvedModel: "gpt-4o", FallbackIndex: 1,
		StatusCode: 200, InputTokens: 1, DurationMS: 100,
	})
	s.WriteLLMCall(&sink.LLMCall{
		TenantID: "t2", Provider: "openai", Model: "gpt-4o", RequestedModel: "smart",
		ResolvedVendor: "openai", ResolvedModel: "gpt-4o", FallbackIndex: 2,
		StatusCode: 200, DurationMS: 100,
	})
	// Never tried a target: FallbackIndex -1 is not a fallback.
	s.WriteLLMCall(&sink.LLMCall{
		TenantID: "t2", Provider: "openai", Model: "smart", RequestedModel: "smart",
		ResolvedVendor: "openai", FallbackIndex: -1, StatusCode: 429, DurationMS: 1,
	})

	got := scrapeSeries(t, m)
	check := func(name string, labels map[string]string, want float64) {
		t.Helper()
		if n := sum(got, name, labels); n != want {
			t.Errorf("%s%v = %v, want %v", name, labels, n, want)
		}
	}

	check("gateway_llm_calls_total", map[string]string{"tenant": "t1", "provider": "anthropic", "model": "claude-sonnet-4-5", "status": "200", "stream": "true"}, 1)
	check("gateway_llm_calls_total", map[string]string{"tenant": "t1", "provider": "anthropic", "model": "claude-sonnet-4-5", "status": "200", "stream": "false"}, 2)
	check("gateway_llm_calls_total", map[string]string{"tenant": "t2", "model": "smart", "status": "200"}, 2)
	check("gateway_llm_calls_total", map[string]string{"tenant": "t2", "model": "smart", "status": "429"}, 1)

	tok := func(kind string) map[string]string {
		return map[string]string{"tenant": "t1", "provider": "anthropic", "model": "claude-sonnet-4-5", "kind": kind}
	}
	check("gateway_llm_tokens_total", tok("input"), 113)
	check("gateway_llm_tokens_total", tok("output"), 45)
	check("gateway_llm_tokens_total", tok("cache_read"), 7)
	// Zero kinds are not recorded at all.
	if n := len(find(got, "gateway_llm_tokens_total", tok("cache_creation"))); n != 0 {
		t.Errorf("zero cache_creation produced %d series", n)
	}

	cost := sum(got, "gateway_llm_cost_usd_total", map[string]string{"tenant": "t1", "provider": "anthropic", "model": "claude-sonnet-4-5"})
	if cost < 0.01499 || cost > 0.01501 {
		t.Errorf("llm_cost_usd = %v, want 0.015 (nil cost skipped)", cost)
	}
	if n := len(find(got, "gateway_llm_cost_usd_total", map[string]string{"tenant": "t2"})); n != 0 {
		t.Errorf("calls with nil cost produced %d cost series", n)
	}

	check("gateway_llm_fallbacks_total", map[string]string{"tenant": "t2", "model": "smart"}, 2)
	if n := len(find(got, "gateway_llm_fallbacks_total", map[string]string{"tenant": "t1"})); n != 0 {
		t.Errorf("FallbackIndex 0 produced %d fallback series", n)
	}

	// Duration: one histogram per provider/model, in seconds.
	check("gateway_llm_call_duration_seconds_count", map[string]string{"provider": "anthropic", "model": "claude-sonnet-4-5"}, 3)
	d := sum(got, "gateway_llm_call_duration_seconds_sum", map[string]string{"provider": "anthropic", "model": "claude-sonnet-4-5"})
	if d < 3.019 || d > 3.021 {
		t.Errorf("duration sum = %v, want 3.02 s", d)
	}
	for _, sm := range find(got, "gateway_llm_call_duration_seconds_bucket", nil) {
		if len(sm.labels) != 3 { // provider, model, le
			t.Errorf("duration series labels = %v, want provider, model, le", sm.labels)
		}
	}

	for _, sm := range got {
		for k, v := range sm.labels {
			if v == "key-9" || v == "sess-9" || v == "req-9" {
				t.Errorf("%s has identifier %s=%q as a label", sm.name, k, v)
			}
		}
	}
}

func TestSinkLLMSkipsInterceptor(t *testing.T) {
	m, s := newSinkMetrics(t)
	s.WriteLLMCall(&sink.LLMCall{
		TenantID: "t", Provider: "anthropic", Model: "m", RequestedModel: "m", Source: "interceptor",
		StatusCode: 200, InputTokens: 5, CostUSD: f64(1), FallbackIndex: 1,
	})
	s.WriteLLMCall(nil)
	for _, sm := range scrapeSeries(t, m) {
		if strings.HasPrefix(sm.name, "gateway_llm_") {
			t.Errorf("interceptor record produced %s%v", sm.name, sm.labels)
		}
	}
	// An explicit "gateway" source and an empty one both count.
	s.WriteLLMCall(&sink.LLMCall{TenantID: "t", Provider: "anthropic", Model: "m", RequestedModel: "m", Source: "gateway", StatusCode: 200})
	s.WriteLLMCall(&sink.LLMCall{TenantID: "t", Provider: "anthropic", Model: "m", RequestedModel: "m", StatusCode: 200})
	if n := sum(scrapeSeries(t, m), "gateway_llm_calls_total", nil); n != 2 {
		t.Errorf("llm_calls = %v, want 2", n)
	}
}

func TestSinkLLMModelLabelIsBounded(t *testing.T) {
	tests := []struct {
		name string
		call sink.LLMCall
		want string
	}{
		{"requested model preferred over resolved", sink.LLMCall{RequestedModel: "alias", Model: "vendor-id", ResolvedVendor: "openai", StatusCode: 200}, "alias"},
		{"falls back to Model", sink.LLMCall{Model: "vendor-id", StatusCode: 200}, "vendor-id"},
		{"registered name kept on upstream error", sink.LLMCall{RequestedModel: "alias", ResolvedVendor: "openai", StatusCode: 503}, "alias"},
		{"unregistered name accepted upstream", sink.LLMCall{RequestedModel: "claude-x", Model: "claude-x", StatusCode: 200}, "claude-x"},
		{"unregistered name rejected upstream", sink.LLMCall{RequestedModel: "attacker-1234", Model: "attacker-1234", StatusCode: 404}, "unknown"},
		{"unregistered name never forwarded", sink.LLMCall{RequestedModel: "attacker-1234", Model: "attacker-1234", StatusCode: -1}, "unknown"},
		{"no model at all", sink.LLMCall{StatusCode: 200}, "unknown"},
		{"overlong model", sink.LLMCall{RequestedModel: strings.Repeat("m", 300), StatusCode: 200}, "other"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, s := newSinkMetrics(t)
			c := tt.call
			c.TenantID, c.Provider = "t", "openai"
			s.WriteLLMCall(&c)
			got := scrapeSeries(t, m)
			if n := sum(got, "gateway_llm_calls_total", map[string]string{"model": tt.want}); n != 1 {
				t.Errorf("llm_calls model=%q = %v, want 1; series: %v", tt.want, n, find(got, "gateway_llm_calls_total", nil))
			}
		})
	}
}

func TestSinkModelCardinalityCap(t *testing.T) {
	m, s := newSinkMetrics(t)
	for i := 0; i < maxDistinctModels+1; i++ {
		s.WriteLLMCall(&sink.LLMCall{TenantID: "t", Provider: "openai", RequestedModel: fmt.Sprintf("model-%03d", i), StatusCode: 200})
	}
	s.WriteLLMCall(&sink.LLMCall{TenantID: "t", Provider: "openai", RequestedModel: "model-000", StatusCode: 200})
	got := scrapeSeries(t, m)
	models := labelValues(got, "gateway_llm_calls_total", "model")
	if len(models) != maxDistinctModels+1 || !models["other"] {
		t.Errorf("distinct model values = %d (other=%v), want %d including other", len(models), models["other"], maxDistinctModels+1)
	}
	if n := sum(got, "gateway_llm_calls_total", map[string]string{"model": "model-000"}); n != 2 {
		t.Errorf("admitted model keeps counting: %v, want 2", n)
	}
}

func TestSinkUnknownTenantAndProvider(t *testing.T) {
	m, s := newSinkMetrics(t)
	s.WriteLLMCall(&sink.LLMCall{RequestedModel: "m", StatusCode: 200})
	s.WriteAccess(&sink.AccessLog{Method: "ping", StatusCode: 200})
	got := scrapeSeries(t, m)
	if n := sum(got, "gateway_llm_calls_total", map[string]string{"tenant": "unknown", "provider": "unknown"}); n != 1 {
		t.Errorf("empty tenant/provider not mapped to unknown: %v", find(got, "gateway_llm_calls_total", nil))
	}
	if n := sum(got, "gateway_mcp_requests_total", map[string]string{"tenant": "unknown"}); n != 1 {
		t.Errorf("empty tenant not mapped to unknown on mcp_requests")
	}
}

func TestSinkCloseAndDisabled(t *testing.T) {
	m, s := newSinkMetrics(t)
	if err := s.Close(); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}
	// The sink still works after Close (the exporter's lifetime is
	// Metrics.Close), and closing twice is harmless.
	s.WriteAccess(&sink.AccessLog{Method: "ping", StatusCode: 200})
	if err := s.Close(); err != nil {
		t.Errorf("second Close = %v", err)
	}
	if n := sum(scrapeSeries(t, m), "gateway_mcp_requests_total", nil); n != 1 {
		t.Errorf("write after Close lost: %v", n)
	}

	// A nil *Metrics and a "none" driver give a sink whose writes are no-ops.
	var nilM *Metrics
	none := newMetrics(t, noneCfg())
	for name, sk := range map[string]sink.LogSink{"nil": nilM.Sink(), "none": none.Sink()} {
		sk.WriteAccess(&sink.AccessLog{Method: "tools/call", ToolName: "a__b", ConnectorID: "c", StatusCode: 200})
		sk.WriteLLMCall(&sink.LLMCall{RequestedModel: "m", StatusCode: 200, InputTokens: 1, CostUSD: f64(1)})
		if err := sk.Close(); err != nil {
			t.Errorf("%s: Close = %v", name, err)
		}
	}
	if none.Enabled() {
		t.Error(`driver "none" reports Enabled`)
	}
}
