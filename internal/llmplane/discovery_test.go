package llmplane_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/llmplane"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	pkgsink "github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink"
)

// waitCall polls for the recorded call: a response with a Content-Length
// reaches the client before the router records the row.
func waitCall(c *captureRec) *pkgsink.LLMCall {
	for i := 0; i < 200 && c.get() == nil; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	return c.get()
}

const discoverySSE = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":3,"output_tokens":1}}}` + "\n\n" +
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t1","name":"Skill","input":{}}}` + "\n\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"skill\":"}}` + "\n\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"Review-PR\"}"}}` + "\n\n" +
	`data: {"type":"content_block_stop","index":0}` + "\n\n" +
	`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"t2","name":"mcp__gw__langfuse__get_trace","input":{}}}` + "\n\n" +
	`data: {"type":"content_block_stop","index":1}` + "\n\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":9}}` + "\n\n"

// The relay scans the response it sends the client for Skill / mcp__ tool
// calls and records names on the captured call, WITHOUT body storage, and
// the client still gets the stream byte for byte.
func TestDiscovery_StreamRecordedWithoutBodyStorage(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, discoverySSE)
	}))
	defer upstream.Close()

	cap := &captureRec{}
	core, err := llmplane.Handler(llmplane.Config{
		UpstreamBaseURL: upstream.URL, MaxRequestBytes: 1 << 20, MaxConcurrentPerTenant: 8,
		StoreBodies: false, Authorizer: pkgauth.NewRoleAuthorizer(),
	}, cap)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(principalMW(core, &pkgauth.Principal{Subject: "u", TenantID: "t", KeyID: "k", Roles: []string{"agent"}}))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/anthropic/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-haiku-4-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != discoverySSE {
		t.Errorf("client stream altered:\n%s", got)
	}
	call := waitCall(cap)
	if call == nil || call.ResponseBody != nil {
		t.Fatalf("call = %+v (want captured, no stored body)", call)
	}
	if !reflect.DeepEqual(call.SkillsUsed, []string{"review-pr"}) || !reflect.DeepEqual(call.MCPToolsUsed, []string{"gw__langfuse__get_trace"}) {
		t.Errorf("skills=%v mcp=%v", call.SkillsUsed, call.MCPToolsUsed)
	}
}

// A non-2xx answer and a plain text answer record nothing.
func TestDiscovery_ErrorResponseRecordsNothing(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"content":[{"type":"tool_use","name":"Skill","input":{"skill":"x"}}]}`)
	}))
	defer upstream.Close()
	cap := &captureRec{}
	core, _ := llmplane.Handler(llmplane.Config{
		UpstreamBaseURL: upstream.URL, MaxRequestBytes: 1 << 20, MaxConcurrentPerTenant: 8, Authorizer: pkgauth.NewRoleAuthorizer(),
	}, cap)
	srv := httptest.NewServer(principalMW(core, &pkgauth.Principal{Subject: "u", TenantID: "t", KeyID: "k", Roles: []string{"agent"}}))
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/anthropic/v1/messages", "application/json", strings.NewReader(`{"model":"claude-haiku-4-5","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if call := waitCall(cap); call == nil || len(call.SkillsUsed) != 0 || len(call.MCPToolsUsed) != 0 {
		t.Fatalf("call = %+v", call)
	}
}
