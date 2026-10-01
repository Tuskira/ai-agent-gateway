package llmplane_test

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/llmplane"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
)

// esFrame builds one AWS event-stream message (string headers, CRCs).
func esFrame(evType string, payload []byte) []byte {
	var h []byte
	for _, kv := range [][2]string{{":message-type", "event"}, {":event-type", evType}} {
		h = append(h, byte(len(kv[0])))
		h = append(h, kv[0]...)
		h = append(h, 7, 0, byte(len(kv[1])))
		h = append(h, kv[1]...)
	}
	total := 12 + len(h) + len(payload) + 4
	f := make([]byte, 12, total)
	binary.BigEndian.PutUint32(f[0:], uint32(total))
	binary.BigEndian.PutUint32(f[4:], uint32(len(h)))
	binary.BigEndian.PutUint32(f[8:], crc32.ChecksumIEEE(f[:8]))
	f = append(append(f, h...), payload...)
	return binary.BigEndian.AppendUint32(f, crc32.ChecksumIEEE(f))
}

func chunk(ev string) []byte {
	return esFrame("chunk", []byte(fmt.Sprintf(`{"bytes":"%s"}`, base64.StdEncoding.EncodeToString([]byte(ev)))))
}

func cat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

type fixedTransport struct{ target *url.URL }

func (f fixedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.URL.Scheme, r.URL.Host = f.target.Scheme, f.target.Host
	return (&http.Transport{}).RoundTrip(r)
}

// Every provider path records SkillsUsed / MCPToolsUsed on the LLMCall, and
// the client receives the upstream bytes unchanged. Bedrock's host is fixed
// (bedrock-runtime.<region>.amazonaws.com), so the Bedrock cases redirect the
// plane's HTTP client (http.DefaultTransport) to the local upstream.
func TestDiscovery_ProviderPaths(t *testing.T) {
	gem := `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"Skill","args":{"skill":"Deploy"}}},{"functionCall":{"name":"mcp__jira__create_issue","args":{}}}]}}]}`
	bedrockStream := cat(
		chunk(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t","name":"Skill","input":{}}}`),
		chunk(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"skill\":"}}`),
		chunk(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"Deploy\"}"}}`),
		chunk(`{"type":"content_block_stop","index":0}`),
		chunk(`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"t2","name":"mcp__jira__create_issue","input":{}}}`),
		chunk(`{"type":"content_block_stop","index":1}`),
	)
	converseStream := cat(
		esFrame("contentBlockStart", []byte(`{"contentBlockIndex":0,"start":{"toolUse":{"toolUseId":"a","name":"Skill"}}}`)),
		esFrame("contentBlockDelta", []byte(`{"contentBlockIndex":0,"delta":{"toolUse":{"input":"{\"skill\":\"Deploy\"}"}}}`)),
		esFrame("contentBlockStop", []byte(`{"contentBlockIndex":0}`)),
		esFrame("contentBlockStart", []byte(`{"contentBlockIndex":1,"start":{"toolUse":{"toolUseId":"b","name":"mcp__jira__create_issue"}}}`)),
		esFrame("contentBlockStop", []byte(`{"contentBlockIndex":1}`)),
	)
	sigv4 := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20260101/us-east-1/bedrock/aws4_request, SignedHeaders=host, Signature=00"
	bedrockHdr := map[string]string{"Authorization": sigv4}
	openaiHdr := map[string]string{"Authorization": "Bearer sk-x"}
	const anthropicReq = `{"anthropic_version":"bedrock-2023-05-31","max_tokens":9,"messages":[{"role":"user","content":"hi"}]}`
	const converseReq = `{"messages":[{"role":"user","content":[{"text":"hi"}]}]}`
	const geminiReq = `{"contents":[{"parts":[{"text":"hi"}]}]}`

	cases := []struct {
		name, path, ctype string
		hdr               map[string]string
		reqBody           string
		resp              []byte
	}{
		{"bedrock invoke", "/model/anthropic.claude-sonnet-4/invoke", "application/json", bedrockHdr, anthropicReq,
			[]byte(`{"type":"message","content":[{"type":"tool_use","id":"t","name":"Skill","input":{"skill":"Deploy"}},{"type":"tool_use","id":"t2","name":"mcp__jira__create_issue","input":{}}],"usage":{"input_tokens":1,"output_tokens":1}}`)},
		{"bedrock invoke-with-response-stream", "/model/anthropic.claude-sonnet-4/invoke-with-response-stream", "application/vnd.amazon.eventstream", bedrockHdr, anthropicReq, bedrockStream},
		{"bedrock converse", "/model/anthropic.claude-sonnet-4/converse", "application/json", bedrockHdr, converseReq,
			[]byte(`{"output":{"message":{"role":"assistant","content":[{"toolUse":{"toolUseId":"a","name":"Skill","input":{"skill":"Deploy"}}},{"toolUse":{"toolUseId":"b","name":"mcp__jira__create_issue","input":{}}}]}},"usage":{"inputTokens":1,"outputTokens":1}}`)},
		{"bedrock converse-stream", "/model/anthropic.claude-sonnet-4/converse-stream", "application/vnd.amazon.eventstream", bedrockHdr, converseReq, converseStream},
		{"gemini json", "/gemini/v1beta/models/gemini-3-flash:generateContent", "application/json", nil, geminiReq, []byte(gem)},
		{"gemini sse", "/gemini/v1beta/models/gemini-3-flash:streamGenerateContent?alt=sse", "text/event-stream", nil, geminiReq,
			[]byte("data: " + gem + "\r\n\r\n")},
		{"gemini json array stream", "/gemini/v1beta/models/gemini-3-flash:streamGenerateContent", "application/json", nil, geminiReq,
			[]byte("[" + gem + "\n]")},
		{"openai responses json", "/openai/v1/responses", "application/json", openaiHdr, `{"model":"gpt-6","input":"hi"}`,
			[]byte(`{"id":"resp_1","output":[{"id":"fc_1","type":"function_call","call_id":"c","name":"Skill","arguments":"{\"skill\":\"Deploy\"}"},{"id":"mcp_1","type":"mcp_call","server_label":"Jira","name":"create_issue","arguments":"{}"}],"usage":{"input_tokens":1,"output_tokens":1}}`)},
		{"openai responses stream", "/openai/v1/responses", "text/event-stream", openaiHdr, `{"model":"gpt-6","input":"hi","stream":true}`,
			[]byte(`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"c","name":"Skill","arguments":""}}` + "\n\n" +
				`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"delta":"{\"skill\":"}` + "\n\n" +
				`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"delta":"\"Deploy\"}"}` + "\n\n" +
				`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_1","name":"Skill","arguments":"{\"skill\":\"Deploy\"}"}}` + "\n\n" +
				`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"mcp_call","id":"mcp_1","server_label":"Jira","name":"create_issue","arguments":""}}` + "\n\n" +
				`data: {"type":"response.completed","response":{"id":"resp_1"}}` + "\n\n")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.ctype)
				_, _ = w.Write(tc.resp)
			}))
			defer upstream.Close()
			if strings.HasPrefix(tc.path, "/model/") {
				u, _ := url.Parse(upstream.URL)
				llmplane.SetTransportOverrideForTest(fixedTransport{target: u})
				defer llmplane.SetTransportOverrideForTest(nil)
			}
			cap := &captureRec{}
			core, err := llmplane.Handler(llmplane.Config{
				UpstreamBaseURL: upstream.URL, OpenAIEnabled: true, OpenAIBaseURL: upstream.URL,
				GeminiEnabled: true, GeminiBaseURL: upstream.URL,
				BedrockEnabled: true, BedrockRegion: "us-east-1", BedrockCredentialMode: "passthrough",
				MaxRequestBytes: 1 << 20, MaxConcurrentPerTenant: 8, Authorizer: pkgauth.NewRoleAuthorizer(),
			}, cap)
			if err != nil {
				t.Fatal(err)
			}
			srv := httptest.NewServer(principalMW(core, &pkgauth.Principal{Subject: "u", TenantID: "t", KeyID: "k", Roles: []string{"agent"}}))
			defer srv.Close()
			req, _ := http.NewRequest(http.MethodPost, srv.URL+tc.path, strings.NewReader(tc.reqBody))
			req.Header.Set("Content-Type", "application/json")
			for k, v := range tc.hdr {
				req.Header.Set(k, v)
			}
			resp, err := (&http.Client{Transport: &http.Transport{}}).Do(req)
			if err != nil {
				t.Fatal(err)
			}
			got, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 || string(got) != string(tc.resp) {
				t.Fatalf("status %d; client body altered (%d vs %d bytes)", resp.StatusCode, len(got), len(tc.resp))
			}
			call := waitCall(cap)
			if call == nil {
				t.Fatal("no call recorded")
			}
			if !reflect.DeepEqual(call.SkillsUsed, []string{"deploy"}) || !reflect.DeepEqual(call.MCPToolsUsed, []string{"jira__create_issue"}) {
				t.Errorf("skills=%v mcp=%v", call.SkillsUsed, call.MCPToolsUsed)
			}
		})
	}
}
