//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/api"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/auth/apikey"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane"
	dpheaders "github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/headers"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/secrets"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// credService is a real HTTP server speaking the credential-service
// contract the external_service provider calls. It accepts one bearer
// token per tenant and serves one current value per credential id.
type credService struct {
	*httptest.Server

	mu       sync.Mutex
	tokens   map[string]string // tenant id -> accepted bearer token
	values   map[string]string // credential id -> value
	resolves []map[string]string
}

func newCredService(t *testing.T) *credService {
	t.Helper()
	cs := &credService{tokens: map[string]string{}, values: map[string]string{}}
	cs.Server = httptest.NewServer(http.HandlerFunc(cs.serve))
	t.Cleanup(cs.Close)
	return cs
}

func (cs *credService) serve(w http.ResponseWriter, r *http.Request) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	authz := r.Header.Get("Authorization")
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/credentials/resolve":
		var req map[string]string
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if authz != "Bearer "+cs.tokens[req["tenant_id"]] {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		cs.resolves = append(cs.resolves, req)
		v, ok := cs.values[req["credential_id"]]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"value": v, "ttl_seconds": 300})
	case r.Method == http.MethodGet && r.URL.Path == "/v1/credentials/options":
		if authz != "Bearer "+cs.tokens[r.URL.Query().Get("tenant_id")] || r.URL.Query().Get("field") != "credential_id" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"options": []map[string]string{{"id": "splunk-prod"}}})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (cs *credService) setToken(tenantID, token string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.tokens[tenantID] = token
}

func (cs *credService) setValue(id, v string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.values[id] = v
}

// resolveCount counts resolve calls for credential id (all, when "").
func (cs *credService) resolveCount(id string) int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	n := 0
	for _, r := range cs.resolves {
		if id == "" || r["credential_id"] == id {
			n++
		}
	}
	return n
}

func (cs *credService) lastResolve() map[string]string {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if len(cs.resolves) == 0 {
		return nil
	}
	return cs.resolves[len(cs.resolves)-1]
}

// tokenGate fronts the MCP backend and answers 401 unless X-Ext-Token
// carries the value the credential service currently issues, the way a
// backend rejects a rotated-out credential.
type tokenGate struct {
	*httptest.Server
	mu   sync.Mutex
	want string
	n401 int
}

func newTokenGate(t *testing.T, backend http.Handler, want string) *tokenGate {
	t.Helper()
	g := &tokenGate{want: want}
	g.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		ok := r.Header.Get("X-Ext-Token") == g.want
		if !ok {
			g.n401++
		}
		g.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		backend.ServeHTTP(w, r)
	}))
	t.Cleanup(g.Close)
	return g
}

func (g *tokenGate) setWant(v string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.want = v
}

func (g *tokenGate) unauthorized() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.n401
}

// startSourcesAPIPlane is startAPIPlane with the secret store and header
// registry wired, which /credentials, /credential-sources and
// /headers/providers need.
func startSourcesAPIPlane(t *testing.T, st store.Store, mcpPlane *dataplane.Plane, svc *secrets.Service, registry *dpheaders.Registry) string {
	t.Helper()
	apiHandler := api.NewRouter(api.Deps{
		ServiceVersion: "e2e",
		Authenticator:  apikey.New(st.APIKeys(), apikey.Options{}),
		Authorizer:     pkgauth.NewRoleAuthorizer(),
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		Store:          st,
		Secrets:        svc,
		Headers:        registry,
		ConnectorOps:   mcpPlane.ConnectorOps,
		CacheOps:       mcpPlane.CacheOps,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: apiHandler, ReadTimeout: 30 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return ln.Addr().String()
}

func (h *harness) apiJSON(t *testing.T, method, path string, body any, dst any) int {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, h.apiBaseURL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+h.adminKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if dst != nil && resp.StatusCode < 300 {
		if err := json.Unmarshal(raw, dst); err != nil {
			t.Fatalf("%s %s: decode %s: %v", method, path, raw, err)
		}
	}
	return resp.StatusCode
}

func TestCredentialSourcesEndToEnd(t *testing.T) {
	h, secretSvc := newFixture(t)
	ctx := context.Background()

	cs := newCredService(t)
	cs.setValue("splunk-prod", "ext-v1")
	cs.setValue("cross-id", "must-not-leak")
	cs.setValue("disabled-id", "must-not-resolve")
	cs.setToken(h.tenant.ID, "svc-token-a")

	// The registry exactly as buildSecretsDeps builds it with
	// secret_store.external_service.enabled: true.
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

	// The gateway's token for the service, in tenant A's encrypted store.
	if code := h.apiJSON(t, http.MethodPost, "/api/v1/credentials", map[string]any{
		"name": "vault-auth", "type": "generic", "payload": map[string]string{"token": "svc-token-a"},
	}, nil); code != http.StatusCreated {
		t.Fatalf("create credential: %d", code)
	}

	// Sources through the API: one live, one disabled.
	var src struct {
		ID      string `json:"id"`
		Enabled bool   `json:"enabled"`
	}
	sourceCfg := map[string]any{"url": cs.URL, "auth_credential": "vault-auth"}
	if code := h.apiJSON(t, http.MethodPost, "/api/v1/credential-sources", map[string]any{
		"name": "vault-a", "provider": "external_service", "config": sourceCfg,
	}, &src); code != http.StatusCreated || !src.Enabled {
		t.Fatalf("create source: %d %+v", code, src)
	}
	if code := h.apiJSON(t, http.MethodPost, "/api/v1/credential-sources", map[string]any{
		"name": "vault-off", "provider": "external_service", "config": sourceCfg, "enabled": false,
	}, nil); code != http.StatusCreated {
		t.Fatalf("create disabled source: %d", code)
	}

	// Tenant B has its own source; tenant A must never resolve through it.
	tenantB := &store.Tenant{Slug: h.tenant.Slug + "-b", Name: "E2E Tenant B"}
	if err := h.store.Tenants().Create(ctx, tenantB); err != nil {
		t.Fatal(err)
	}
	if _, err := secretSvc.Create(ctx, tenantB.ID, "vault-auth", "generic", map[string]string{"token": "svc-token-b"}, "e2e"); err != nil {
		t.Fatal(err)
	}
	cs.setToken(tenantB.ID, "svc-token-b")
	if err := h.store.CredentialSources().Create(ctx, &store.CredentialSource{
		TenantID: tenantB.ID, Name: "vault-b", Provider: "external_service", Enabled: true, Config: sourceCfg,
	}); err != nil {
		t.Fatal(err)
	}

	t.Run("test endpoint reports ok without returning secrets", func(t *testing.T) {
		var res map[string]any
		if code := h.apiJSON(t, http.MethodPost, "/api/v1/credential-sources/"+src.ID+"/test", nil, &res); code != http.StatusOK {
			t.Fatalf("test: %d", code)
		}
		if res["ok"] != true || len(res) != 1 {
			t.Errorf("test result = %v, want exactly {ok: true}", res)
		}
	})

	t.Run("headers/providers lists the schema and the tenant's enabled sources", func(t *testing.T) {
		var res struct {
			Providers []struct {
				ID           string         `json:"id"`
				SourceSchema map[string]any `json:"source_schema"`
				Sources      []string       `json:"sources"`
			} `json:"providers"`
		}
		if code := h.apiJSON(t, http.MethodGet, "/api/v1/headers/providers", nil, &res); code != http.StatusOK {
			t.Fatalf("providers: %d", code)
		}
		found := false
		for _, p := range res.Providers {
			if p.ID == "external_service" {
				found = true
				if p.SourceSchema == nil || len(p.Sources) != 1 || p.Sources[0] != "vault-a" {
					t.Errorf("external_service = %+v, want source_schema and sources [vault-a]", p)
				}
			}
		}
		if !found {
			t.Error("external_service missing from /headers/providers")
		}
	})

	// Point the connector at a gate that demands the current external
	// value, and give it three external_service headers: one through the
	// live source, one naming tenant B's source, one naming the disabled
	// source. The last two are written straight to the store, as the API
	// refuses them at save time.
	gate := newTokenGate(t, h.backend.Server.Config.Handler, "ext-v1")
	conn, err := h.store.Connectors().Get(ctx, h.tenant.ID, h.connector.ID)
	if err != nil {
		t.Fatal(err)
	}
	conn.Endpoint = gate.URL
	hdrs := conn.Metadata["headers"].(map[string]any)
	extHeader := func(source, credentialID string) map[string]any {
		return map[string]any{
			"type": "external", "provider": "external_service", "source": source,
			"config": map[string]any{"credential_id": credentialID, "field": "token"},
		}
	}
	hdrs["X-Ext-Token"] = extHeader("vault-a", "splunk-prod")
	hdrs["X-Cross-Tenant"] = extHeader("vault-b", "cross-id")
	hdrs["X-Disabled"] = extHeader("vault-off", "disabled-id")
	if err := h.store.Connectors().Update(ctx, conn); err != nil {
		t.Fatal(err)
	}

	t.Run("API refuses a connector header naming another tenant's source", func(t *testing.T) {
		code := h.apiJSON(t, http.MethodPost, "/api/v1/connectors", map[string]any{
			"name": "cross", "endpoint": "https://cross.example.com/mcp",
			"metadata": map[string]any{"headers": map[string]any{"X-Cross": extHeader("vault-b", "cross-id")}},
		}, nil)
		if code != http.StatusBadRequest {
			t.Errorf("create connector naming vault-b = %d, want 400", code)
		}
	})

	var sessionID string
	call := func(t *testing.T, id int) {
		t.Helper()
		_, decoded, raw := h.rpc(t, mcp.Request{
			JSONRPC: mcp.Version, ID: id, Method: mcp.MethodToolsCall,
			Params: json.RawMessage(`{"name":"alpha__echo","arguments":{"message":"via source"}}`),
		}, rpcOptions{SessionID: sessionID, ProfileName: "Reader"})
		if decoded.Error != nil {
			t.Fatalf("tools/call failed: %+v (%s)", decoded.Error, raw)
		}
		var result mcp.ToolsCallResult
		if err := json.Unmarshal(decoded.Result, &result); err != nil {
			t.Fatal(err)
		}
		if len(result.Content) != 1 || result.Content[0].Text != "echo: via source" || result.IsError {
			t.Fatalf("tools/call result = %s", decoded.Result)
		}
	}

	t.Run("tools/call injects the value resolved through the source", func(t *testing.T) {
		resp, decoded, raw := h.rpc(t, mcp.Request{
			JSONRPC: mcp.Version, ID: 1, Method: mcp.MethodInitialize,
			Params: json.RawMessage(`{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"e2e","version":"1"}}`),
		}, rpcOptions{})
		if decoded.Error != nil {
			t.Fatalf("initialize failed: %+v (%s)", decoded.Error, raw)
		}
		sessionID = resp.Header.Get(mcp.HeaderSessionID)

		call(t, 2)
		got := h.backend.LastHeaders()
		if v := got.Get("X-Ext-Token"); v != "ext-v1" {
			t.Errorf("backend saw X-Ext-Token = %q, want ext-v1", v)
		}
		if v := got.Get("X-Backend-Key"); v != "s3cret-backend-key" {
			t.Errorf("secret_store header regressed: X-Backend-Key = %q", v)
		}
		last := cs.lastResolve()
		if last["tenant_id"] != h.tenant.ID || last["connector_slug"] != "alpha" || last["header_name"] != "X-Ext-Token" || last["field"] != "token" {
			t.Errorf("resolve request = %v", last)
		}
	})

	t.Run("cross-tenant and disabled sources are refused", func(t *testing.T) {
		got := h.backend.LastHeaders()
		if v := got.Get("X-Cross-Tenant"); v != "" {
			t.Errorf("backend saw X-Cross-Tenant = %q; another tenant's source resolved", v)
		}
		if v := got.Get("X-Disabled"); v != "" {
			t.Errorf("backend saw X-Disabled = %q; a disabled source resolved", v)
		}
		if n := cs.resolveCount("cross-id"); n != 0 {
			t.Errorf("credential service was asked for cross-id %d times", n)
		}
		if n := cs.resolveCount("disabled-id"); n != 0 {
			t.Errorf("credential service was asked for disabled-id %d times", n)
		}
	})

	t.Run("a 401 after rotation evicts the cached value and replays once", func(t *testing.T) {
		before := cs.resolveCount("splunk-prod")
		call(t, 3) // still cached: no new resolve
		if n := cs.resolveCount("splunk-prod"); n != before {
			t.Fatalf("resolve count went %d -> %d on a cached call", before, n)
		}

		cs.setValue("splunk-prod", "ext-v2")
		gate.setWant("ext-v2")
		rejected := gate.unauthorized()

		call(t, 4)
		if v := h.backend.LastHeaders().Get("X-Ext-Token"); v != "ext-v2" {
			t.Errorf("backend saw X-Ext-Token = %q after replay, want ext-v2", v)
		}
		if n := gate.unauthorized() - rejected; n != 1 {
			t.Errorf("backend answered 401 %d times, want exactly 1 before the replay", n)
		}
		if n := cs.resolveCount("splunk-prod"); n != before+1 {
			t.Errorf("resolve count = %d, want %d (one re-resolve after eviction)", n, before+1)
		}
	})

	t.Run("delete is refused while a connector names the source", func(t *testing.T) {
		if code := h.apiJSON(t, http.MethodDelete, "/api/v1/credential-sources/"+src.ID, nil, nil); code != http.StatusConflict {
			t.Errorf("delete referenced source = %d, want 409", code)
		}
	})
}
