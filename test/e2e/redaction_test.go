//go:build e2e

package e2e

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/google/uuid"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/auth/apikey"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/dptest"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/profile"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/store/postgres"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/gateway"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// redactAWSKeyID is the documented AWS example key id, not a credential.
const redactAWSKeyID = "AKIAIOSFODNN7EXAMPLE"

// redactJWT builds a JWT-shaped token at run time, so the source holds no
// token literal.
func redactJWT() string {
	e := base64.RawURLEncoding.EncodeToString
	return e([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + e([]byte(`{"sub":"e2e-redaction"}`)) + "." + e([]byte("e2e-signature-bytes"))
}

// The whole gateway (gateway.Run) with body capture on for both planes and
// the ClickHouse sink on: an LLM call whose prompt and reply carry an AWS
// key id, and an MCP tool call whose arguments and result carry a JWT. The
// upstream, the connector and the client all see the real values; the
// Postgres capture row and the ClickHouse llm_calls / mcp_access_logs rows
// hold only [REDACTED:...] markers.
func TestCapturedSecretsAreRedactedInEveryStore(t *testing.T) {
	chAddr := os.Getenv("GATEWAY_TEST_CLICKHOUSE_ADDR")
	if chAddr == "" {
		t.Skip("GATEWAY_TEST_CLICKHOUSE_ADDR not set; skipping the redaction end-to-end test")
	}
	a := setupRunEnv(t)
	chHost, chPort, _ := strings.Cut(chAddr, ":")
	jwt := redactJWT()

	// LLM upstream: records what it was sent, replies with the key.
	reply := `{"id":"msg_redact","type":"message","role":"assistant","model":"claude-test-1","content":[{"type":"text","text":"found ` +
		redactAWSKeyID + `"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`
	var upMu sync.Mutex
	var upGot string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		upMu.Lock()
		upGot = string(b)
		upMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(upstream.Close)

	// MCP connector: a real MCP server whose echo tool returns its input.
	backend := dptest.NewBackend(dptest.BackendOptions{
		Tools: []mcp.Tool{{Name: "echo", Description: "Echo", InputSchema: dptest.ObjectSchema([]string{"message"}, "message")}},
		Handlers: map[string]dptest.ToolFunc{
			"echo": func(args map[string]any) (mcp.ToolsCallResult, error) {
				msg, _ := args["message"].(string)
				return mcp.ToolsCallResult{Content: []mcp.Content{mcp.TextContent("echo: " + msg)}}, nil
			},
		},
	})
	t.Cleanup(backend.Close)

	for k, v := range map[string]string{
		"GATEWAY_LLM_PROXY_UPSTREAM_BASE_URL":                upstream.URL,
		"GATEWAY_EGRESS_ALLOWED_CIDRS":                       "127.0.0.0/8",
		"GATEWAY_CAPTURE_STORE_BODIES":                       "true",
		"GATEWAY_LLM_PROXY_CAPTURE_STORE_BODIES":             "true",
		"GATEWAY_SINKS_CLICKHOUSE_ENABLED":                   "true",
		"GATEWAY_SINKS_CLICKHOUSE_HOST":                      chHost,
		"GATEWAY_SINKS_CLICKHOUSE_PORT":                      chPort,
		"GATEWAY_SINKS_CLICKHOUSE_FLUSH_INTERVAL":            "200ms",
		"GATEWAY_SINKS_CLICKHOUSE_ASYNC_INSERT_BUSY_TIMEOUT": "200ms",
		"GATEWAY_TOOL_CACHE_ENABLED":                         "false",
	} {
		t.Setenv(k, v)
	}
	startRun(t, a, gateway.Options{Version: "e2e-redaction"})

	// Tenant, admin key, connector and profile, written straight into the
	// database the running gateway uses.
	ctx := context.Background()
	runDSN, err := url.Parse(os.Getenv("GATEWAY_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	runDSN.Path = "/" + os.Getenv("GATEWAY_DATABASE_DATABASE")
	db, err := sql.Open("pgx", runDSN.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	st := postgres.New(db)
	tenant := &store.Tenant{Slug: "e2e-redact-" + uuid.NewString()[:8], Name: "E2E Redaction"}
	if err := st.Tenants().Create(ctx, tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	plaintext, hash, prefix, err := apikey.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.APIKeys().Create(ctx, &store.APIKey{TenantID: tenant.ID, Name: "e2e", Role: "admin",
		KeyHash: hash, KeyPrefix: prefix, CreatedBy: "e2e"}); err != nil {
		t.Fatalf("create api key: %v", err)
	}
	conn := &store.Connector{TenantID: tenant.ID, Name: "alpha", Slug: "alpha", Endpoint: backend.URL, TimeoutMS: 5000, Status: "unknown"}
	if err := st.Connectors().Create(ctx, conn); err != nil {
		t.Fatalf("create connector: %v", err)
	}
	prof := &store.AgentProfile{TenantID: tenant.ID, Name: "Reader", Slug: profile.Slug(tenant.ID, "Reader")}
	if err := st.AgentProfiles().Create(ctx, prof); err != nil {
		t.Fatalf("create profile: %v", err)
	}
	if err := st.AgentProfiles().SetTools(ctx, tenant.ID, prof.ID, []store.ProfileTool{
		{AgentProfileID: prof.ID, ConnectorID: conn.ID, ToolName: "echo"},
	}); err != nil {
		t.Fatalf("set profile tools: %v", err)
	}

	// LLM plane.
	llmBody := `{"model":"claude-test-1","max_tokens":16,"system":"tool output follows","messages":[{"role":"user","content":"the key is ` + redactAWSKeyID + `"}]}`
	req, _ := http.NewRequest(http.MethodPost, "http://"+a.llm+"/v1/messages", strings.NewReader(llmBody))
	req.Header.Set("Authorization", "Bearer "+plaintext)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(got) != reply {
		t.Fatalf("llm call: %d %s, want the upstream reply unchanged", resp.StatusCode, got)
	}
	upMu.Lock()
	if upGot != llmBody {
		t.Errorf("upstream got %q, want the request unchanged", upGot)
	}
	upMu.Unlock()

	// MCP plane.
	h := &harness{baseURL: "http://" + a.mcp, adminKey: plaintext}
	params, _ := json.Marshal(map[string]any{"name": "alpha__echo", "arguments": map[string]any{"message": "token " + jwt}})
	_, decoded, raw := h.rpc(t, mcp.Request{JSONRPC: mcp.Version, ID: 7, Method: mcp.MethodToolsCall, Params: params},
		rpcOptions{ProfileName: "Reader"})
	if decoded.Error != nil || !strings.Contains(string(decoded.Result), "echo: token "+jwt) {
		t.Fatalf("tools/call: %s, want the echo with the token unchanged", raw)
	}
	if args := backend.ToolArgs("echo"); args == nil || args["message"] != "token "+jwt {
		t.Errorf("connector got %v, want the token unchanged", args)
	}

	// Postgres capture row (the durable store).
	var pgReq, pgResp []byte
	var pgMsgs sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT request_body, response_body, messages FROM llm_calls WHERE tenant_id = $1 AND status_code = 200`,
		tenant.ID).Scan(&pgReq, &pgResp, &pgMsgs); err != nil {
		t.Fatalf("read postgres llm_calls: %v", err)
	}
	assertRedacted(t, "postgres llm_calls", map[string]string{
		"request_body": string(pgReq), "response_body": string(pgResp), "messages": pgMsgs.String,
	}, redactAWSKeyID, "[REDACTED:aws_key_id]")
	if !json.Valid(pgReq) {
		t.Errorf("postgres request_body is not valid JSON: %s", pgReq)
	}

	// ClickHouse rows (the analytics sink, written asynchronously).
	ch, err := clickhouse.Open(&clickhouse.Options{Addr: []string{chAddr}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ch.Close() })
	var chReq, chResp, chMsgs string
	waitRow(t, func() error {
		return ch.QueryRow(ctx, `SELECT request_body, response_body, messages FROM llm_calls WHERE tenant_id = ? AND status_code = 200 LIMIT 1`,
			tenant.ID).Scan(&chReq, &chResp, &chMsgs)
	})
	assertRedacted(t, "clickhouse llm_calls", map[string]string{
		"request_body": chReq, "response_body": chResp, "messages": chMsgs,
	}, redactAWSKeyID, "[REDACTED:aws_key_id]")

	var accReq, accResp string
	waitRow(t, func() error {
		return ch.QueryRow(ctx, `SELECT request_body, response_body FROM mcp_access_logs WHERE tenant_id = ? AND method = 'tools/call' LIMIT 1`,
			tenant.ID).Scan(&accReq, &accResp)
	})
	assertRedacted(t, "clickhouse mcp_access_logs", map[string]string{
		"request_body": accReq, "response_body": accResp,
	}, jwt, "[REDACTED:jwt]")
	if !json.Valid([]byte(accReq)) || !json.Valid([]byte(accResp)) {
		t.Errorf("stored mcp payloads are not valid JSON: %s / %s", accReq, accResp)
	}

	// The redactions were counted, by plane and kind.
	mresp, err := http.Get("http://" + a.metrics + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	metricsBody, _ := io.ReadAll(mresp.Body)
	mresp.Body.Close()
	for _, series := range []string{
		`gateway_capture_redactions_total{kind="aws_key_id",plane="llm"}`,
		`gateway_capture_redactions_total{kind="jwt",plane="mcp"}`,
	} {
		if v := metricValue(string(metricsBody), series); v < 1 {
			t.Errorf("%s = %v, want >= 1", series, v)
		}
	}
	if strings.Contains(string(metricsBody), jwt) || strings.Contains(string(metricsBody), redactAWSKeyID) {
		t.Error("a secret value reached /metrics")
	}
}

// assertRedacted fails when any field holds secret or no field holds marker.
func assertRedacted(t *testing.T, where string, fields map[string]string, secret, marker string) {
	t.Helper()
	marked := false
	for name, v := range fields {
		if strings.Contains(v, secret) {
			t.Errorf("%s.%s stored the raw secret: %s", where, name, v)
		}
		marked = marked || strings.Contains(v, marker)
	}
	if !marked {
		t.Errorf("%s: no %s marker in %v", where, marker, fields)
	}
}

// waitRow retries query until it finds a row (ClickHouse inserts are
// batched and asynchronous) or 30s pass.
func waitRow(t *testing.T, query func() error) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		err := query()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("row never appeared in ClickHouse: %v", err)
		}
		time.Sleep(250 * time.Millisecond)
	}
}
