//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	dpheaders "github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/headers"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/secrets"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
)

// A connector with metadata.headers_required refuses a call, with JSON-RPC
// -32002, when its credential header cannot be resolved because the external
// credential service is down, and the backend receives nothing. Without the
// flag the same call goes out without the header.
func TestHeadersRequiredEndToEnd(t *testing.T) {
	h, secretSvc := newFixture(t)
	ctx := context.Background()

	cs := newCredService(t)
	cs.setValue("splunk-prod", "ext-v1")
	cs.setToken(h.tenant.ID, "svc-token-a")

	registry := dpheaders.NewRegistry()
	if err := registry.RegisterExternal(secrets.NewSecretStoreProvider(secretSvc)); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterExternal(secrets.NewExternalServiceProvider(secretSvc.Fetcher(), 5*time.Second, 5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	registry.SetSourceLookup(dpheaders.StoreSourceLookup(h.store.CredentialSources()))

	addr, plane := startPlaneWithRegistry(t, h.store, registry, nil)
	h.baseURL = "http://" + addr
	h.apiBaseURL = "http://" + startSourcesAPIPlane(t, h.store, plane, secretSvc, registry)

	if code := h.apiJSON(t, http.MethodPost, "/api/v1/credentials", map[string]any{
		"name": "vault-auth", "type": "generic", "payload": map[string]string{"token": "svc-token-a"},
	}, nil); code != http.StatusCreated {
		t.Fatalf("create credential: %d", code)
	}
	if code := h.apiJSON(t, http.MethodPost, "/api/v1/credential-sources", map[string]any{
		"name": "vault-a", "provider": "external_service",
		"config": map[string]any{"url": cs.URL, "auth_credential": "vault-auth"},
	}, nil); code != http.StatusCreated {
		t.Fatalf("create source: %d", code)
	}

	// Count every request that reaches the backend.
	var reached atomic.Int32
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		h.backend.Server.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(front.Close)

	conn, err := h.store.Connectors().Get(ctx, h.tenant.ID, h.connector.ID)
	if err != nil {
		t.Fatal(err)
	}
	conn.Endpoint = front.URL
	conn.Metadata["headers"] = map[string]any{
		"X-Ext-Token": map[string]any{
			"type": "external", "provider": "external_service", "source": "vault-a",
			"config": map[string]any{"credential_id": "splunk-prod", "field": "token"},
		},
	}
	conn.Metadata["headers_required"] = true
	if err := h.store.Connectors().Update(ctx, conn); err != nil {
		t.Fatal(err)
	}

	// The credential service is stopped before the first resolve, so
	// nothing is cached.
	cs.Close()

	call := func(id int) mcp.Response {
		t.Helper()
		_, decoded, _ := h.rpc(t, mcp.Request{
			JSONRPC: mcp.Version, ID: id, Method: mcp.MethodToolsCall,
			Params: json.RawMessage(`{"name":"alpha__echo","arguments":{"message":"hi"}}`),
		}, rpcOptions{ProfileName: "Reader"})
		return decoded
	}

	got := call(1)
	if got.Error == nil || got.Error.Code != -32002 || got.Error.Message != "credential header X-Ext-Token unavailable" {
		t.Fatalf("required: response = %+v %s; want -32002 credential header X-Ext-Token unavailable", got.Error, got.Result)
	}
	if n := reached.Load(); n != 0 {
		t.Fatalf("required: the backend received %d requests; want 0", n)
	}

	// Same connector, flag off: the call goes out without the header.
	conn, err = h.store.Connectors().Get(ctx, h.tenant.ID, h.connector.ID)
	if err != nil {
		t.Fatal(err)
	}
	conn.Metadata["headers_required"] = false
	if err := h.store.Connectors().Update(ctx, conn); err != nil {
		t.Fatal(err)
	}
	got = call(2)
	if got.Error != nil {
		t.Fatalf("not required: call refused: %+v", got.Error)
	}
	if reached.Load() == 0 {
		t.Error("not required: the backend received nothing")
	}
	if v := h.backend.LastHeaders().Get("X-Ext-Token"); v != "" {
		t.Errorf("not required: backend saw X-Ext-Token = %q", v)
	}
}
