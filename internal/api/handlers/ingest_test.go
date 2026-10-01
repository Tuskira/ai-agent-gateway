package handlers

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	internalauth "github.com/Tuskira/tusk-ai-secured-gateway/internal/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/config"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink"
)

// ingestPrincipal builds a Principal with every identity field set to a
// distinct, recognizable value, so a test can assert the handler stamped
// TenantID/Principal/KeyID from THIS object and never from the request
// body (the "tenant and key stamping ignores payload values" case).
func ingestPrincipal(tenantID string) *pkgauth.Principal {
	return &pkgauth.Principal{
		Subject:    "principal-" + tenantID,
		TenantID:   tenantID,
		KeyID:      "key-" + tenantID,
		Roles:      []string{"interceptor"},
		AuthMethod: "test",
	}
}

func ingestRequestWithPrincipal(body string, tenantID string, gzipEncode bool) *http.Request {
	var buf bytes.Buffer
	if gzipEncode {
		gw := gzip.NewWriter(&buf)
		_, _ = gw.Write([]byte(body))
		_ = gw.Close()
	} else {
		buf.WriteString(body)
	}

	r := httptest.NewRequest(http.MethodPost, "/api/v1/ingest", &buf)
	if gzipEncode {
		r.Header.Set("Content-Encoding", "gzip")
	}
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(pkgauth.WithPrincipal(r.Context(), ingestPrincipal(tenantID)))
	return r
}

func newIngestDeps(is *ingestFakeSink, enabled bool) Deps {
	d := newTestDeps()
	if is != nil {
		d.IngestSink = is
	}
	d.IngestConfig = config.Ingest{Enabled: enabled, MaxBodyBytes: 32 << 20, MaxRecords: 1000, RatePerMinute: 0}
	d.Capture = config.Capture{StoreBodies: false, MaxRequestBytes: 1000, MaxResponseBytes: 1000}
	d.LLMCapture = config.LLMCapture{StoreBodies: false, MaxRequestBytes: 1000, MaxResponseBytes: 1000}
	return d
}

// ingestFakeSink is a hand-written sink.IngestSink test double: it records
// the tenantID/access/llm it was called with and returns whatever's
// configured, so tests can assert tenant stamping and the response's
// accepted/duplicate counts without a real ClickHouse.
type ingestFakeSink struct {
	// result, when non-nil, is returned verbatim (used by tests that
	// specifically exercise duplicate counts). Left nil, WriteIngestBatch
	// defaults to the real sink's no-duplicates-seen-yet behavior:
	// everything it was handed is "accepted".
	result *sink.IngestResult
	err    error

	gotTenantID string
	gotAccess   []*sink.AccessLog
	gotLLM      []*sink.LLMCall
	calls       int
}

func (f *ingestFakeSink) WriteIngestBatch(_ context.Context, tenantID string, access []*sink.AccessLog, llm []*sink.LLMCall) (sink.IngestResult, error) {
	f.calls++
	f.gotTenantID = tenantID
	f.gotAccess = access
	f.gotLLM = llm
	if f.err != nil {
		return sink.IngestResult{}, f.err
	}
	if f.result != nil {
		return *f.result, nil
	}
	return sink.IngestResult{AcceptedAccess: len(access), AcceptedLLM: len(llm)}, nil
}

func doIngest(t *testing.T, d Deps, r *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	h := Ingest{Deps: d}
	w := httptest.NewRecorder()
	h.Ingest(w, r)
	return w
}

func decodeIngestResponse(t *testing.T, w *httptest.ResponseRecorder) ingestResponse {
	t.Helper()
	var resp ingestResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, w.Body.String())
	}
	return resp
}

func TestIngest_Disabled404MatchesRouterNotFound(t *testing.T) {
	sinkFake := &ingestFakeSink{}
	d := newIngestDeps(sinkFake, false)
	r := ingestRequestWithPrincipal(`{"schema_version":1}`, "tenant-a", false)

	w := doIngest(t, d, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	// Must match internal/api/router.go's r.NotFound body byte for byte:
	// {"error":{"type":"not_found","message":"not found"}}\n
	want := `{"error":{"type":"not_found","message":"not found"}}` + "\n"
	if w.Body.String() != want {
		t.Errorf("body = %q, want %q (must be indistinguishable from an unknown route)", w.Body.String(), want)
	}
	if sinkFake.calls != 0 {
		t.Error("sink should not be called when ingest is disabled")
	}
}

func TestIngest_NoSinkConfigured503(t *testing.T) {
	d := newIngestDeps(nil, true)
	r := ingestRequestWithPrincipal(`{"schema_version":1}`, "tenant-a", false)

	w := doIngest(t, d, r)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body=%s", w.Code, w.Body.String())
	}
}

func TestIngest_StorageError503(t *testing.T) {
	sinkFake := &ingestFakeSink{err: errors.New("clickhouse down")}
	d := newIngestDeps(sinkFake, true)
	body := `{"schema_version":1,"llm_calls":[{"timestamp":"2026-01-15T12:00:00Z","request_id":"r1","status_code":200}]}`
	r := ingestRequestWithPrincipal(body, "tenant-a", false)

	w := doIngest(t, d, r)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body=%s", w.Code, w.Body.String())
	}
}

func TestIngest_UnsupportedSchemaVersion400(t *testing.T) {
	sinkFake := &ingestFakeSink{}
	d := newIngestDeps(sinkFake, true)
	r := ingestRequestWithPrincipal(`{"schema_version":2}`, "tenant-a", false)

	w := doIngest(t, d, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", w.Code, w.Body.String())
	}
}

func TestIngest_MalformedBody400(t *testing.T) {
	sinkFake := &ingestFakeSink{}
	d := newIngestDeps(sinkFake, true)
	r := ingestRequestWithPrincipal(`{not json`, "tenant-a", false)

	w := doIngest(t, d, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", w.Code, w.Body.String())
	}
}

func TestIngest_RecordCapExceeded400(t *testing.T) {
	sinkFake := &ingestFakeSink{}
	d := newIngestDeps(sinkFake, true)
	d.IngestConfig.MaxRecords = 2

	var calls []string
	for i := 0; i < 3; i++ {
		calls = append(calls, fmt.Sprintf(`{"timestamp":"2026-01-15T12:00:00Z","request_id":"r%d","status_code":200}`, i))
	}
	body := `{"schema_version":1,"llm_calls":[` + strings.Join(calls, ",") + `]}`
	r := ingestRequestWithPrincipal(body, "tenant-a", false)

	w := doIngest(t, d, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", w.Code, w.Body.String())
	}
	if sinkFake.calls != 0 {
		t.Error("sink should not be called when the record cap is exceeded")
	}
}

func TestIngest_TenantAndKeyStampingIgnoresPayload(t *testing.T) {
	sinkFake := &ingestFakeSink{}
	d := newIngestDeps(sinkFake, true)

	// The payload tries to smuggle tenant_id/key_id/principal/cost_usd/source
	// fields; the handler's llmCallIn/accessLogIn structs have no such
	// fields to decode them into (decoded WITHOUT DisallowUnknownFields, so
	// they're silently ignored, not rejected).
	body := `{
		"schema_version": 1,
		"user": "person@example.com",
		"llm_calls": [{"timestamp":"2026-01-15T12:00:00Z","request_id":"llm1","status_code":200,
			"tenant_id":"evil-tenant","key_id":"evil-key","principal":"evil-principal","cost_usd":999,"source":"gateway"}],
		"access_logs": [{"timestamp":"2026-01-15T12:00:00Z","request_id":"acc1","status_code":200,
			"tenant_id":"evil-tenant"}]
	}`
	r := ingestRequestWithPrincipal(body, "tenant-a", false)

	w := doIngest(t, d, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	if sinkFake.gotTenantID != "tenant-a" {
		t.Errorf("WriteIngestBatch tenantID = %q, want %q", sinkFake.gotTenantID, "tenant-a")
	}
	if len(sinkFake.gotLLM) != 1 {
		t.Fatalf("got %d llm calls, want 1", len(sinkFake.gotLLM))
	}
	llm := sinkFake.gotLLM[0]
	if llm.TenantID != "tenant-a" || llm.Principal != "principal-tenant-a" || llm.KeyID != "key-tenant-a" {
		t.Errorf("llm identity = %+v, want stamped from the authenticated principal, not the payload", llm)
	}
	if llm.CostUSD != nil {
		t.Errorf("CostUSD = %v, want nil (cost_usd is never accepted from the payload)", llm.CostUSD)
	}
	if llm.Source != "interceptor" {
		t.Errorf("Source = %q, want %q regardless of what the payload sent", llm.Source, "interceptor")
	}
	if llm.User != "person@example.com" {
		t.Errorf("User = %q, want the batch-level user field", llm.User)
	}

	acc := sinkFake.gotAccess[0]
	if acc.TenantID != "tenant-a" || acc.Principal != "principal-tenant-a" || acc.KeyID != "key-tenant-a" {
		t.Errorf("access identity = %+v, want stamped from the authenticated principal, not the payload", acc)
	}
	if acc.Source != "interceptor" {
		t.Errorf("Source = %q, want %q", acc.Source, "interceptor")
	}

	resp := decodeIngestResponse(t, w)
	if resp.Accepted.LLMCalls != 1 || resp.Accepted.AccessLogs != 1 {
		t.Errorf("Accepted = %+v, want {1 1}", resp.Accepted)
	}
}

func TestIngest_ValidationRejections(t *testing.T) {
	sinkFake := &ingestFakeSink{}
	d := newIngestDeps(sinkFake, true)

	body := `{
		"schema_version": 1,
		"llm_calls": [
			{"timestamp":"not-a-time","request_id":"r1","status_code":200},
			{"timestamp":"2026-01-15T12:00:00Z","request_id":"","status_code":200},
			{"timestamp":"2026-01-15T12:00:00Z","request_id":"r3","status_code":0},
			{"timestamp":"2026-01-15T12:00:00Z","request_id":"r4","status_code":200}
		],
		"access_logs": [
			{"timestamp":"2026-01-15T12:00:00Z","request_id":"a1","status_code":9999}
		]
	}`
	r := ingestRequestWithPrincipal(body, "tenant-a", false)

	w := doIngest(t, d, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (rejections are per-record, not a batch failure), body=%s", w.Code, w.Body.String())
	}

	resp := decodeIngestResponse(t, w)
	if len(resp.Rejected) != 4 {
		t.Fatalf("Rejected = %+v, want 4 entries", resp.Rejected)
	}
	if resp.Accepted.LLMCalls != 1 {
		t.Errorf("Accepted.LLMCalls = %d, want 1 (only r4 is valid)", resp.Accepted.LLMCalls)
	}
	if sinkFake.calls != 1 {
		t.Fatalf("sink calls = %d, want 1", sinkFake.calls)
	}
	if len(sinkFake.gotLLM) != 1 || sinkFake.gotLLM[0].RequestID != "r4" {
		t.Errorf("sink got llm calls %+v, want only r4", sinkFake.gotLLM)
	}
	if len(sinkFake.gotAccess) != 0 {
		t.Errorf("sink got %d access logs, want 0 (the only one was invalid)", len(sinkFake.gotAccess))
	}
}

func TestIngest_DuplicateCountsPassThrough(t *testing.T) {
	sinkFake := &ingestFakeSink{result: &sink.IngestResult{
		AcceptedLLM: 1, DuplicateLLM: 2,
		AcceptedAccess: 0, DuplicateAccess: 1,
	}}
	d := newIngestDeps(sinkFake, true)
	body := `{"schema_version":1,"llm_calls":[{"timestamp":"2026-01-15T12:00:00Z","request_id":"r1","status_code":200}]}`
	r := ingestRequestWithPrincipal(body, "tenant-a", false)

	w := doIngest(t, d, r)
	resp := decodeIngestResponse(t, w)
	if resp.Duplicates.LLMCalls != 2 || resp.Duplicates.AccessLogs != 1 {
		t.Errorf("Duplicates = %+v, want {LLMCalls:2 AccessLogs:1}", resp.Duplicates)
	}
	if resp.Accepted.LLMCalls != 1 {
		t.Errorf("Accepted.LLMCalls = %d, want 1", resp.Accepted.LLMCalls)
	}
}

func TestIngest_StoreBodiesOffDropsBodies(t *testing.T) {
	sinkFake := &ingestFakeSink{}
	d := newIngestDeps(sinkFake, true)
	d.Capture.StoreBodies = false
	d.LLMCapture.StoreBodies = false

	body := `{
		"schema_version": 1,
		"llm_calls": [{"timestamp":"2026-01-15T12:00:00Z","request_id":"r1","status_code":200,
			"request_body":"req","response_body":"resp","messages":"msgs"}],
		"access_logs": [{"timestamp":"2026-01-15T12:00:00Z","request_id":"a1","status_code":200,
			"request_body":"req","response_body":"resp"}]
	}`
	r := ingestRequestWithPrincipal(body, "tenant-a", false)
	doIngest(t, d, r)

	llm := sinkFake.gotLLM[0]
	if llm.RequestBody != nil || llm.ResponseBody != nil || llm.Messages != nil {
		t.Errorf("llm bodies = %+v, want all nil when store_bodies is off", llm)
	}
	acc := sinkFake.gotAccess[0]
	if acc.RequestBody != nil || acc.ResponseBody != nil {
		t.Errorf("access bodies = %+v, want nil when store_bodies is off", acc)
	}
}

// Interceptor-supplied response bodies are scanned for Skill / mcp__ tool
// calls even when body storage is off; request bodies are never scanned and a
// non-2xx record contributes nothing.
func TestIngest_DiscoveryFromResponseBody(t *testing.T) {
	sinkFake := &ingestFakeSink{}
	d := newIngestDeps(sinkFake, true)
	d.LLMCapture.StoreBodies = false

	resp := `{"content":[{"type":"tool_use","name":"Skill","input":{"skill":"Deploy"}},{"type":"tool_use","name":"mcp__langfuse__get_trace","input":{}}]}`
	reqOnly := `{"messages":[{"role":"assistant","content":[{"type":"tool_use","name":"Skill","input":{"skill":"old"}}]}]}`
	payload, _ := json.Marshal(map[string]any{
		"schema_version": 1,
		"llm_calls": []map[string]any{
			{"timestamp": "2026-01-15T12:00:00Z", "request_id": "r1", "status_code": 200, "response_body": resp, "request_body": reqOnly},
			{"timestamp": "2026-01-15T12:00:00Z", "request_id": "r2", "status_code": 500, "response_body": resp},
			{"timestamp": "2026-01-15T12:00:00Z", "request_id": "r3", "status_code": 200, "request_body": reqOnly},
		},
	})
	doIngest(t, d, ingestRequestWithPrincipal(string(payload), "tenant-a", false))

	if len(sinkFake.gotLLM) != 3 {
		t.Fatalf("got %d llm calls", len(sinkFake.gotLLM))
	}
	a := sinkFake.gotLLM[0]
	if len(a.SkillsUsed) != 1 || a.SkillsUsed[0] != "deploy" || len(a.MCPToolsUsed) != 1 || a.MCPToolsUsed[0] != "langfuse__get_trace" {
		t.Errorf("r1 skills=%v mcp=%v", a.SkillsUsed, a.MCPToolsUsed)
	}
	if a.ResponseBody != nil {
		t.Errorf("bodies must stay dropped with store_bodies off")
	}
	for _, l := range sinkFake.gotLLM[1:] {
		if len(l.SkillsUsed) != 0 || len(l.MCPToolsUsed) != 0 {
			t.Errorf("%s must not record discovery: %v %v", l.RequestID, l.SkillsUsed, l.MCPToolsUsed)
		}
	}
}

func TestIngest_StoreBodiesOnTruncates(t *testing.T) {
	sinkFake := &ingestFakeSink{}
	d := newIngestDeps(sinkFake, true)
	d.LLMCapture = config.LLMCapture{StoreBodies: true, MaxRequestBytes: 4, MaxResponseBytes: 4}

	body := `{"schema_version":1,"llm_calls":[{"timestamp":"2026-01-15T12:00:00Z","request_id":"r1","status_code":200,
		"request_body":"0123456789","response_body":"abcdefghij"}]}`
	r := ingestRequestWithPrincipal(body, "tenant-a", false)
	doIngest(t, d, r)

	llm := sinkFake.gotLLM[0]
	if string(llm.RequestBody) != "0123" {
		t.Errorf("RequestBody = %q, want capped to %q", llm.RequestBody, "0123")
	}
	if string(llm.ResponseBody) != "abcd" {
		t.Errorf("ResponseBody = %q, want capped to %q", llm.ResponseBody, "abcd")
	}
	if !llm.Truncated {
		t.Error("Truncated should be true")
	}
}

func TestIngest_GzipBody(t *testing.T) {
	sinkFake := &ingestFakeSink{}
	d := newIngestDeps(sinkFake, true)
	body := `{"schema_version":1,"llm_calls":[{"timestamp":"2026-01-15T12:00:00Z","request_id":"r1","status_code":200}]}`
	r := ingestRequestWithPrincipal(body, "tenant-a", true)

	w := doIngest(t, d, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	if sinkFake.calls != 1 {
		t.Fatalf("sink calls = %d, want 1", sinkFake.calls)
	}
}

func TestIngest_CompressedBodyTooLarge413(t *testing.T) {
	sinkFake := &ingestFakeSink{}
	d := newIngestDeps(sinkFake, true)

	r := ingestRequestWithPrincipal("", "tenant-a", false)
	// Simulate an oversized body via Content-Length without actually
	// allocating 9MB: the handler checks r.ContentLength first.
	r.ContentLength = (8 << 20) + 1

	w := doIngest(t, d, r)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413, body=%s", w.Code, w.Body.String())
	}
}

func TestIngest_DecompressedBodyTooLarge413(t *testing.T) {
	sinkFake := &ingestFakeSink{}
	d := newIngestDeps(sinkFake, true)
	d.IngestConfig.MaxBodyBytes = 16 // absurdly small, to trigger the cap cheaply

	body := `{"schema_version":1,"llm_calls":[{"timestamp":"2026-01-15T12:00:00Z","request_id":"r1","status_code":200}]}`
	r := ingestRequestWithPrincipal(body, "tenant-a", true)

	w := doIngest(t, d, r)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413, body=%s", w.Code, w.Body.String())
	}
}

func TestIngest_RateLimited429(t *testing.T) {
	sinkFake := &ingestFakeSink{}
	d := newIngestDeps(sinkFake, true)
	// rate=60/min -> burst = clamp(60/20, 5, 50) = 5. Drain it with the SAME
	// key id ingestPrincipal mints ("key-tenant-a") before the request under
	// test, so that one is guaranteed to be refused.
	limiter := internalauth.NewKeyRateLimiter(60, nil)
	for i := 0; i < 5; i++ {
		limiter.Allow("key-tenant-a")
	}
	d.IngestRateLimiter = limiter

	body := `{"schema_version":1}`
	r := ingestRequestWithPrincipal(body, "tenant-a", false)

	w := doIngest(t, d, r)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429, body=%s", w.Code, w.Body.String())
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("429 response missing Retry-After header")
	}
	if sinkFake.calls != 0 {
		t.Error("sink should not be called when rate limited")
	}
}
