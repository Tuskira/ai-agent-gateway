//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/auth/apikey"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// A restricted key (containment level restrict) is refused every tool
// call, except on a connector an admin marked restrict_exempt through the
// API: that call is forwarded to the backend, over a real socket and a real
// Postgres.
func TestRestrictedKeyReachesExemptConnector(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// An agent key, created as the API creates one.
	agentKey, hash, prefix, err := apikey.Generate()
	if err != nil {
		t.Fatal(err)
	}
	k := &store.APIKey{TenantID: h.tenant.ID, Name: "agent", Role: "agent", KeyHash: hash, KeyPrefix: prefix, CreatedBy: "e2e"}
	if err := h.store.APIKeys().Create(ctx, k); err != nil {
		t.Fatal(err)
	}

	api := func(method, path, body string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(method, h.apiBaseURL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+h.adminKey)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := api(http.MethodPost, "/api/v1/api-keys/"+k.ID+"/containment", `{"level":"restrict","note":"e2e"}`); code != http.StatusOK {
		t.Fatalf("restrict: %d %s", code, body)
	}

	callEcho := func() mcp.Response {
		t.Helper()
		b, _ := json.Marshal(mcp.Request{JSONRPC: mcp.Version, ID: 7, Method: mcp.MethodToolsCall,
			Params: json.RawMessage(`{"name":"alpha__echo","arguments":{"message":"help"}}`)})
		req, err := http.NewRequest(http.MethodPost, h.baseURL+"/mcp", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+agentKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out mcp.Response
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	if r := callEcho(); r.Error == nil || r.Error.Code != mcp.ErrorCodeToolNotAllowed {
		t.Fatalf("restricted key on a connector that is not exempt: %+v", r)
	}
	before := h.backend.Calls(mcp.MethodToolsCall)

	// The admin marks the connector restrict_exempt (metadata is replaced
	// as a whole: send what is stored, with the flag).
	meta := map[string]any{}
	for k, v := range h.connector.Metadata {
		meta[k] = v
	}
	meta["restrict_exempt"] = true
	mb, _ := json.Marshal(map[string]any{"name": "alpha", "endpoint": h.connector.Endpoint, "metadata": meta})
	if code, body := api(http.MethodPut, "/api/v1/connectors/"+h.connector.ID, string(mb)); code != http.StatusOK || !strings.Contains(body, `"restrict_exempt":true`) {
		t.Fatalf("set restrict_exempt: %d %s", code, body)
	}

	r := callEcho()
	if r.Error != nil {
		t.Fatalf("restricted key on an exempt connector refused: %+v", r.Error)
	}
	if !strings.Contains(string(r.Result), "echo: help") {
		t.Errorf("result = %s", r.Result)
	}
	if n := h.backend.Calls(mcp.MethodToolsCall) - before; n != 1 {
		t.Errorf("backend tools/call count = %d; want 1", n)
	}
}
