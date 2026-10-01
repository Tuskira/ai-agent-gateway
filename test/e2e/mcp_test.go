//go:build e2e

// Package e2e drives the gateway the way a client does: over a real
// socket, against a real Postgres, with a real MCP backend on the other
// side.
//
// The unit tests fake one collaborator each. This one fakes nothing
// except the backend server itself, so it is the test that catches a
// wiring mistake -- a header resolved in the wrong order, a profile slug
// computed differently by two packages, a store method that behaves
// differently on Postgres than in a map.
//
// Run it with:
//
//	GATEWAY_TEST_DATABASE_URL=postgres://... \
//	GATEWAY_MASTER_KEY=$(go run ./cmd/gateway secrets genkey) \
//	go test -tags e2e ./test/e2e/...
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/api"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/auth/apikey"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/config"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/dptest"
	dpheaders "github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/headers"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/profile"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/secrets"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/store/postgres"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
	// Session drivers, registered exactly as cmd/gateway registers them:
	// dataplane.New opens sessions.store through pkg/session's registry.
	_ "github.com/Tuskira/tusk-ai-secured-gateway/pkg/session/memory"
	_ "github.com/Tuskira/tusk-ai-secured-gateway/pkg/session/redis"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink/stdout"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// ---------------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------------

type harness struct {
	// baseURL is the MCP plane's; empty when the test runs an api-only
	// instance (ops_test.go), which serves no MCP plane at all.
	baseURL    string
	apiBaseURL string
	backend    *dptest.Backend
	store      store.Store

	tenant    *store.Tenant
	connector *store.Connector
	adminKey  string
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	h, secretSvc := newFixture(t)
	addr, mcpPlane := startPlane(t, h.store, secretSvc)
	h.baseURL = "http://" + addr
	h.apiBaseURL = "http://" + startAPIPlane(t, h.store, mcpPlane)
	return h
}

// newFixture builds everything an e2e test needs below the planes: an
// isolated Postgres schema, a tenant, an admin API key, an encrypted
// credential, the dptest backend registered as a connector, and a profile
// granting one of its two tools. It returns the harness with no plane
// started yet (baseURL/apiBaseURL empty) plus the secrets.Service the
// connector's secret_store header resolves through, so each test can wire
// exactly the planes it is about: newHarness starts both, ops_test.go's
// api-only test starts only the control plane.
func newFixture(t *testing.T) (*harness, *secrets.Service) {
	t.Helper()

	rawURL := os.Getenv("GATEWAY_TEST_DATABASE_URL")
	if rawURL == "" {
		t.Skip("GATEWAY_TEST_DATABASE_URL not set; skipping the end-to-end tests")
	}
	ctx := context.Background()

	backend := dptest.NewBackend(dptest.BackendOptions{
		Tools: []mcp.Tool{
			{Name: "echo", Description: "Echo a message back", InputSchema: dptest.ObjectSchema([]string{"message"}, "message")},
			{Name: "danger", Description: "Should stay out of reach", InputSchema: dptest.ObjectSchema(nil, "target")},
		},
		Handlers: map[string]dptest.ToolFunc{
			"echo": func(args map[string]any) (mcp.ToolsCallResult, error) {
				msg, _ := args["message"].(string)
				return mcp.ToolsCallResult{Content: []mcp.Content{mcp.TextContent("echo: " + msg)}}, nil
			},
		},
		Prompts:           dptest.SamplePrompts(),
		Resources:         dptest.SampleResources(),
		ResourceTemplates: dptest.SampleResourceTemplates(),
		PageSize:          1,
		RequireSession:    true,
	})
	t.Cleanup(backend.Close)

	st := newIsolatedStore(t, rawURL)

	tenant := &store.Tenant{Slug: "e2e-" + uuid.NewString()[:8], Name: "E2E Tenant"}
	if err := st.Tenants().Create(ctx, tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	// An admin API key, created exactly as `gateway bootstrap-key` does:
	// the plaintext exists only here, the row holds only the hash.
	plaintext, hash, prefix, err := apikey.Generate()
	if err != nil {
		t.Fatalf("generate api key: %v", err)
	}
	key := &store.APIKey{
		TenantID: tenant.ID, Name: "e2e", Role: "admin",
		KeyHash: hash, KeyPrefix: prefix, CreatedBy: "e2e",
	}
	if err := st.APIKeys().Create(ctx, key); err != nil {
		t.Fatalf("create api key: %v", err)
	}

	// A credential the connector's token_field-equivalent header reads
	// from, so the secret store is exercised end to end rather than
	// stubbed.
	ring, err := secrets.LoadKeyRing(config.SecretStore{MasterKeyEnv: "GATEWAY_MASTER_KEY", ActiveKeyID: "k1"})
	if err != nil {
		t.Fatalf("load master key ring (set GATEWAY_MASTER_KEY): %v", err)
	}
	secretSvc := secrets.NewService(st.Credentials(), ring)
	if _, err := secretSvc.Create(ctx, tenant.ID, "backend-auth", "generic",
		map[string]string{"api_key": "s3cret-backend-key"}, "e2e"); err != nil {
		t.Fatalf("create credential: %v", err)
	}

	connector := &store.Connector{
		TenantID: tenant.ID, Name: "alpha", Slug: "alpha",
		Endpoint: backend.URL, TimeoutMS: 5000, Status: "unknown",
		Metadata: map[string]any{
			"headers": map[string]any{
				// One static header and one resolved from the encrypted
				// credential store, which is the shape a real connector
				// has.
				"X-Static-Header": map[string]any{"type": "static", "value": "static-value"},
				"X-Backend-Key": map[string]any{
					"type": "external", "provider": "secret_store",
					"config": map[string]any{"credential": "backend-auth", "field": "api_key"},
				},
				// A header the caller's own identity supplies.
				"X-Caller-Subject": map[string]any{"type": "token_field", "field": "subject"},
			},
		},
	}
	if err := st.Connectors().Create(ctx, connector); err != nil {
		t.Fatalf("create connector: %v", err)
	}

	// A profile that grants exactly one of the backend's two tools.
	prof := &store.AgentProfile{
		TenantID: tenant.ID, Name: "Reader", Slug: profile.Slug(tenant.ID, "Reader"),
		Description: "May echo, may not do damage",
	}
	if err := st.AgentProfiles().Create(ctx, prof); err != nil {
		t.Fatalf("create profile: %v", err)
	}
	if err := st.AgentProfiles().SetTools(ctx, tenant.ID, prof.ID, []store.ProfileTool{
		{AgentProfileID: prof.ID, ConnectorID: connector.ID, ToolName: "echo"},
	}); err != nil {
		t.Fatalf("set profile tools: %v", err)
	}

	return &harness{
		backend:   backend,
		store:     st,
		tenant:    tenant,
		connector: connector,
		adminKey:  plaintext,
	}, secretSvc
}

// newHeaderRegistry builds the header-resolver registry the data plane
// reads connector metadata.headers through, with the secret_store provider
// registered so the connector's X-Backend-Key resolves against the real
// encrypted credential rather than a stub.
func newHeaderRegistry(t *testing.T, secretSvc *secrets.Service) *dpheaders.Registry {
	t.Helper()

	registry := dpheaders.NewRegistry()
	if err := registry.RegisterExternal(secrets.NewSecretStoreProvider(secretSvc)); err != nil {
		t.Fatalf("register secret_store provider: %v", err)
	}
	return registry
}

// startPlane builds the MCP plane exactly as cmd/gateway does and serves
// it on a free port, returning the address and the built Plane (so
// startAPIPlane can wire its ConnectorOps/CacheOps into the control-plane
// API, exactly as cmd/gateway does when both planes are enabled).
func startPlane(t *testing.T, st store.Store, secretSvc *secrets.Service) (string, *dataplane.Plane) {
	t.Helper()

	registry := newHeaderRegistry(t, secretSvc)

	cfg := config.Default()
	cfg.Service.Version = "e2e"
	// Off for this test: every assertion below is about what the
	// backends actually answer right now, and a cache would mask a
	// routing change behind a stale hit.
	cfg.ToolCache.Enabled = false

	logSink := stdout.New(io.Discard)
	t.Cleanup(func() { _ = logSink.Close() })

	plane, err := dataplane.New(dataplane.Deps{
		Config:        cfg,
		Store:         st,
		Headers:       registry,
		Authenticator: apikey.New(st.APIKeys(), apikey.Options{}),
		Sink:          logSink,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("build mcp plane: %v", err)
	}
	t.Cleanup(func() { _ = plane.Close() })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: plane.Handler, ReadTimeout: 30 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	})

	return ln.Addr().String(), plane
}

// startAPIPlane builds the control-plane API exactly as cmd/gateway does
// -- ConnectorOps/CacheOps wired from the already-built data plane -- and
// serves it on a free port. The wiring is the same whether or not the MCP
// plane is also served in this process: see ops_test.go.
func startAPIPlane(t *testing.T, st store.Store, mcpPlane *dataplane.Plane) string {
	t.Helper()

	apiHandler := api.NewRouter(api.Deps{
		ServiceVersion: "e2e",
		Authenticator:  apikey.New(st.APIKeys(), apikey.Options{}),
		Authorizer:     pkgauth.NewRoleAuthorizer(),
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		Store:          st,
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
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	})

	return ln.Addr().String()
}

// newIsolatedStore gives each test its own Postgres schema, migrated
// fresh and dropped afterwards, so tests never see each other's rows.
func newIsolatedStore(t *testing.T, rawURL string) store.Store {
	t.Helper()
	st, _ := newIsolatedStoreDSN(t, rawURL)
	return st
}

// newIsolatedStoreDSN is newIsolatedStore plus the schema-scoped DSN, for
// tests that also open the schema through another client (the Postgres
// capture sink in models_test.go).
func newIsolatedStoreDSN(t *testing.T, rawURL string) (store.Store, string) {
	t.Helper()
	ctx := context.Background()

	schema := "e2e_" + strings.ReplaceAll(uuid.NewString(), "-", "")

	admin, err := sql.Open("pgx", rawURL)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	if _, err := admin.ExecContext(ctx, fmt.Sprintf(`CREATE SCHEMA %q`, schema)); err != nil {
		admin.Close()
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), fmt.Sprintf(`DROP SCHEMA %q CASCADE`, schema))
		admin.Close()
	})

	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse database url: %v", err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()

	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatalf("open scoped connection: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	st := postgres.New(db)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return st, u.String()
}

// ---------------------------------------------------------------------------
// request helpers
// ---------------------------------------------------------------------------

type rpcOptions struct {
	SessionID   string
	ProfileName string
	NoAuth      bool
}

func (h *harness) rpc(t *testing.T, req mcp.Request, opts rpcOptions) (*http.Response, mcp.Response, []byte) {
	t.Helper()

	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	httpReq, err := http.NewRequest(http.MethodPost, h.baseURL+"/mcp", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if !opts.NoAuth {
		httpReq.Header.Set("Authorization", "Bearer "+h.adminKey)
	}
	if opts.SessionID != "" {
		httpReq.Header.Set(mcp.HeaderSessionID, opts.SessionID)
	}
	if opts.ProfileName != "" {
		httpReq.Header.Set(profile.Header, opts.ProfileName)
	}

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatalf("POST /mcp: %v", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	var decoded mcp.Response
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("response is not JSON-RPC: %v (%s)", err, raw)
		}
	}
	return resp, decoded, raw
}

// apiRequest issues one control-plane API request as h.adminKey and, when
// dst is non-nil, decodes the JSON response body into it.
func (h *harness) apiRequest(t *testing.T, method, path string, dst any) *http.Response {
	t.Helper()

	req, err := http.NewRequest(method, h.apiBaseURL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+h.adminKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { resp.Body.Close() })

	if dst != nil {
		if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
			t.Fatalf("%s %s: decode response: %v", method, path, err)
		}
	}
	return resp
}

// ---------------------------------------------------------------------------
// the tests
// ---------------------------------------------------------------------------

func TestMCPPlaneEndToEnd(t *testing.T) {
	h := newHarness(t)

	var sessionID string

	t.Run("initialize issues a session", func(t *testing.T) {
		resp, decoded, raw := h.rpc(t, mcp.Request{
			JSONRPC: mcp.Version, ID: 1, Method: mcp.MethodInitialize,
			Params: json.RawMessage(`{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"e2e","version":"1"}}`),
		}, rpcOptions{})
		t.Logf("initialize -> %d %s", resp.StatusCode, raw)

		if decoded.Error != nil {
			t.Fatalf("initialize failed: %+v", decoded.Error)
		}
		sessionID = resp.Header.Get(mcp.HeaderSessionID)
		if sessionID == "" {
			t.Fatal("no Mcp-Session-Id header on the initialize response")
		}

		var result mcp.InitializeResult
		if err := json.Unmarshal(decoded.Result, &result); err != nil {
			t.Fatal(err)
		}
		if result.Capabilities.Tools == nil || !result.Capabilities.Tools.ListChanged {
			t.Fatalf("capabilities = %+v, want tools.listChanged", result.Capabilities)
		}

		// The fan-out handshook with the connector and recorded it.
		conn, err := h.store.Connectors().Get(context.Background(), h.tenant.ID, h.connector.ID)
		if err != nil {
			t.Fatal(err)
		}
		if conn.Status != "healthy" {
			t.Errorf("connector status = %q, want healthy", conn.Status)
		}
	})

	t.Run("tools/list with the profile returns only the granted tool", func(t *testing.T) {
		_, decoded, raw := h.rpc(t, mcp.Request{JSONRPC: mcp.Version, ID: 2, Method: mcp.MethodToolsList},
			rpcOptions{SessionID: sessionID, ProfileName: "Reader"})
		t.Logf("tools/list (profile=Reader) -> %s", raw)

		if decoded.Error != nil {
			t.Fatalf("tools/list failed: %+v", decoded.Error)
		}
		var result mcp.ToolsListResult
		if err := json.Unmarshal(decoded.Result, &result); err != nil {
			t.Fatal(err)
		}
		if len(result.Tools) != 1 {
			t.Fatalf("got %d tools, want exactly the one the profile grants: %+v", len(result.Tools), result.Tools)
		}
		if result.Tools[0].Name != "alpha__echo" {
			t.Fatalf("tool name = %q, want %q", result.Tools[0].Name, "alpha__echo")
		}
	})

	t.Run("tools/call on the granted tool reaches the backend with the right headers", func(t *testing.T) {
		resp, decoded, raw := h.rpc(t, mcp.Request{
			JSONRPC: mcp.Version, ID: 3, Method: mcp.MethodToolsCall,
			Params: json.RawMessage(`{"name":"alpha__echo","arguments":{"message":"hello gateway"}}`),
		}, rpcOptions{SessionID: sessionID, ProfileName: "Reader"})
		t.Logf("tools/call alpha__echo -> %s", raw)

		if decoded.Error != nil {
			t.Fatalf("tools/call failed: %+v", decoded.Error)
		}
		var result mcp.ToolsCallResult
		if err := json.Unmarshal(decoded.Result, &result); err != nil {
			t.Fatal(err)
		}
		if len(result.Content) != 1 || result.Content[0].Text != "echo: hello gateway" {
			t.Fatalf("content = %+v", result.Content)
		}
		if got := resp.Header.Get("X-Connector-ID"); got != h.connector.ID {
			t.Errorf("X-Connector-ID = %q, want %q", got, h.connector.ID)
		}

		headers := h.backend.LastHeaders()
		if got := headers.Get("X-Tenant-Id"); got != h.tenant.ID {
			t.Errorf("backend saw X-Tenant-Id = %q, want the tenant id %q", got, h.tenant.ID)
		}
		if got := headers.Get("X-Static-Header"); got != "static-value" {
			t.Errorf("backend saw X-Static-Header = %q", got)
		}
		if got := headers.Get("X-Backend-Key"); got != "s3cret-backend-key" {
			t.Errorf("backend saw X-Backend-Key = %q, want the decrypted credential", got)
		}
		if got := headers.Get("X-Caller-Subject"); got == "" {
			t.Error("backend saw no X-Caller-Subject")
		}
		if got := headers.Get(mcp.HeaderSessionID); got != h.backend.SessionID() {
			t.Errorf("backend saw mcp-session-id = %q, want its own %q", got, h.backend.SessionID())
		}
	})

	t.Run("tools/call on the ungranted tool is denied", func(t *testing.T) {
		_, decoded, raw := h.rpc(t, mcp.Request{
			JSONRPC: mcp.Version, ID: 4, Method: mcp.MethodToolsCall,
			Params: json.RawMessage(`{"name":"alpha__danger","arguments":{"target":"prod"}}`),
		}, rpcOptions{SessionID: sessionID, ProfileName: "Reader"})
		t.Logf("tools/call alpha__danger -> %s", raw)

		if decoded.Error == nil || decoded.Error.Code != mcp.ErrorCodeToolNotAllowed {
			t.Fatalf("error = %+v, want -32003", decoded.Error)
		}
		if h.backend.ToolArgs("danger") != nil {
			t.Fatal("the denied tool still reached the backend")
		}
	})

	t.Run("an unknown profile yields an empty list", func(t *testing.T) {
		_, decoded, raw := h.rpc(t, mcp.Request{JSONRPC: mcp.Version, ID: 5, Method: mcp.MethodToolsList},
			rpcOptions{SessionID: sessionID, ProfileName: "Does Not Exist"})
		t.Logf("tools/list (profile=Does Not Exist) -> %s", raw)

		if decoded.Error != nil {
			t.Fatalf("tools/list failed: %+v", decoded.Error)
		}
		if !bytes.Contains(decoded.Result, []byte(`"tools":[]`)) {
			t.Fatalf("result = %s, want an empty tool array", decoded.Result)
		}
	})

	t.Run("no profile header yields every tool in the tenant", func(t *testing.T) {
		_, decoded, raw := h.rpc(t, mcp.Request{JSONRPC: mcp.Version, ID: 6, Method: mcp.MethodToolsList},
			rpcOptions{SessionID: sessionID})
		t.Logf("tools/list (no profile) -> %s", raw)

		var result mcp.ToolsListResult
		if err := json.Unmarshal(decoded.Result, &result); err != nil {
			t.Fatal(err)
		}
		if len(result.Tools) != 2 {
			t.Fatalf("got %d tools, want both", len(result.Tools))
		}
	})

	t.Run("prompts and resources round-trip through the gateway namespace", func(t *testing.T) {
		// Reader grants one of alpha's tools, so every one of alpha's
		// prompts and resources is in scope for it.
		reader := rpcOptions{SessionID: sessionID, ProfileName: "Reader"}

		_, decoded, raw := h.rpc(t, mcp.Request{JSONRPC: mcp.Version, ID: 20, Method: mcp.MethodPromptsList}, reader)
		t.Logf("prompts/list (profile=Reader) -> %s", raw)
		var prompts mcp.PromptsListResult
		if decoded.Error != nil || json.Unmarshal(decoded.Result, &prompts) != nil {
			t.Fatalf("prompts/list failed: %+v", decoded.Error)
		}
		if len(prompts.Prompts) != 2 || prompts.Prompts[0].Name != "alpha__greet" || prompts.Prompts[1].Name != "alpha__simple" {
			t.Fatalf("prompts = %+v, want alpha__greet and alpha__simple", prompts.Prompts)
		}

		resp, decoded, raw := h.rpc(t, mcp.Request{
			JSONRPC: mcp.Version, ID: 21, Method: mcp.MethodPromptsGet,
			Params: json.RawMessage(`{"name":"alpha__greet","arguments":{"who":"e2e"}}`),
		}, reader)
		t.Logf("prompts/get alpha__greet -> %s", raw)
		var got mcp.PromptsGetResult
		if decoded.Error != nil || json.Unmarshal(decoded.Result, &got) != nil {
			t.Fatalf("prompts/get failed: %+v", decoded.Error)
		}
		if len(got.Messages) != 1 {
			t.Fatalf("messages = %+v, want one", got.Messages)
		}
		if args := h.backend.PromptArgs("greet"); args["who"] != "e2e" {
			t.Errorf("backend saw prompt arguments %v, want who=e2e", args)
		}
		if got := resp.Header.Get("X-Connector-ID"); got != h.connector.ID {
			t.Errorf("X-Connector-ID = %q, want %q", got, h.connector.ID)
		}
		// The same connector headers reach the backend as on tools/call.
		if got := h.backend.LastHeaders().Get("X-Backend-Key"); got != "s3cret-backend-key" {
			t.Errorf("backend saw X-Backend-Key = %q, want the decrypted credential", got)
		}

		_, decoded, raw = h.rpc(t, mcp.Request{JSONRPC: mcp.Version, ID: 22, Method: mcp.MethodResourcesList}, reader)
		t.Logf("resources/list (profile=Reader) -> %s", raw)
		var resources mcp.ResourcesListResult
		if decoded.Error != nil || json.Unmarshal(decoded.Result, &resources) != nil {
			t.Fatalf("resources/list failed: %+v", decoded.Error)
		}
		if len(resources.Resources) != 2 {
			t.Fatalf("resources = %+v, want two", resources.Resources)
		}
		for _, r := range resources.Resources {
			if !strings.HasPrefix(r.URI, "gw://alpha/") {
				t.Errorf("resource uri %q is not in the gw://alpha/ namespace", r.URI)
			}
		}

		_, decoded, raw = h.rpc(t, mcp.Request{JSONRPC: mcp.Version, ID: 23, Method: mcp.MethodResourcesTemplatesList}, reader)
		t.Logf("resources/templates/list (profile=Reader) -> %s", raw)
		var templates mcp.ResourceTemplatesListResult
		if decoded.Error != nil || json.Unmarshal(decoded.Result, &templates) != nil {
			t.Fatalf("resources/templates/list failed: %+v", decoded.Error)
		}
		if len(templates.ResourceTemplates) != 1 || templates.ResourceTemplates[0].URITemplate != "gw://alpha/test://dynamic/resource/{id}" {
			t.Fatalf("templates = %+v", templates.ResourceTemplates)
		}

		params, _ := json.Marshal(mcp.ResourcesReadParams{URI: "gw://alpha/test://static/resource/1"})
		_, decoded, raw = h.rpc(t, mcp.Request{JSONRPC: mcp.Version, ID: 24, Method: mcp.MethodResourcesRead, Params: params}, reader)
		t.Logf("resources/read -> %s", raw)
		var read mcp.ResourcesReadResult
		if decoded.Error != nil || json.Unmarshal(decoded.Result, &read) != nil {
			t.Fatalf("resources/read failed: %+v", decoded.Error)
		}
		if len(read.Contents) != 1 || read.Contents[0].Text == nil || *read.Contents[0].Text == "" {
			t.Fatalf("contents = %+v", read.Contents)
		}
		if uris := h.backend.ReadURIs(); len(uris) == 0 || uris[len(uris)-1] != "test://static/resource/1" {
			t.Errorf("backend read %v, want the un-namespaced uri last", uris)
		}
	})

	t.Run("an unknown profile sees no prompts and may not read resources", func(t *testing.T) {
		nobody := rpcOptions{SessionID: sessionID, ProfileName: "Does Not Exist"}

		_, decoded, raw := h.rpc(t, mcp.Request{JSONRPC: mcp.Version, ID: 25, Method: mcp.MethodPromptsList}, nobody)
		t.Logf("prompts/list (profile=Does Not Exist) -> %s", raw)
		if decoded.Error != nil || !bytes.Contains(decoded.Result, []byte(`"prompts":[]`)) {
			t.Fatalf("result = %s / %+v, want an empty prompt array", decoded.Result, decoded.Error)
		}

		_, decoded, raw = h.rpc(t, mcp.Request{
			JSONRPC: mcp.Version, ID: 26, Method: mcp.MethodResourcesRead,
			Params: json.RawMessage(`{"uri":"gw://alpha/test://static/resource/1"}`),
		}, nobody)
		t.Logf("resources/read (profile=Does Not Exist) -> %s", raw)
		if decoded.Error == nil || decoded.Error.Code != mcp.ErrorCodeToolNotAllowed {
			t.Fatalf("error = %+v, want -32003", decoded.Error)
		}
	})

	t.Run("an unauthenticated request is rejected with 401 and -32001", func(t *testing.T) {
		resp, decoded, raw := h.rpc(t, mcp.Request{JSONRPC: mcp.Version, ID: 7, Method: mcp.MethodToolsList},
			rpcOptions{NoAuth: true})
		t.Logf("tools/list (no credential) -> %d %s", resp.StatusCode, raw)

		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
		if decoded.Error == nil || decoded.Error.Code != mcp.ErrorCodeUnauthorized {
			t.Fatalf("error = %+v, want -32001", decoded.Error)
		}
	})

	t.Run("an unknown session id is rejected with -32000", func(t *testing.T) {
		_, decoded, raw := h.rpc(t, mcp.Request{JSONRPC: mcp.Version, ID: 8, Method: mcp.MethodToolsList},
			rpcOptions{SessionID: "not-a-real-session"})
		t.Logf("tools/list (bogus session) -> %s", raw)

		if decoded.Error == nil || decoded.Error.Code != mcp.ErrorCodeSessionNotFound {
			t.Fatalf("error = %+v, want -32000", decoded.Error)
		}
	})

	t.Run("a stateless caller works without ever initializing", func(t *testing.T) {
		_, decoded, raw := h.rpc(t, mcp.Request{
			JSONRPC: mcp.Version, ID: 9, Method: mcp.MethodToolsCall,
			Params: json.RawMessage(`{"name":"alpha__echo","arguments":{"message":"stateless"}}`),
		}, rpcOptions{ProfileName: "Reader"})
		t.Logf("tools/call (no session) -> %s", raw)

		if decoded.Error != nil {
			t.Fatalf("tools/call failed: %+v", decoded.Error)
		}
		var result mcp.ToolsCallResult
		if err := json.Unmarshal(decoded.Result, &result); err != nil {
			t.Fatal(err)
		}
		if len(result.Content) != 1 || result.Content[0].Text != "echo: stateless" {
			t.Fatalf("content = %+v", result.Content)
		}
	})
}

// TestAPIOpsRoutesEndToEnd drives the control-plane API's connector
// health/discover and tool-cache routes over a real HTTP socket, against
// the same Postgres-backed harness (tenant, connector, backend) as
// TestMCPPlaneEndToEnd. It is the one test that catches a wiring mistake
// between internal/api/handlers and internal/dataplane's ops.go adapter --
// a permission gated wrong, a JSON field renamed on one side only, a
// tenant-scoping check skipped -- that a handler test with a fake
// ConnectorOps/CacheOps cannot.
func TestAPIOpsRoutesEndToEnd(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	t.Run("health probes the connector and persists status", func(t *testing.T) {
		var got struct {
			Status       string         `json:"status"`
			LatencyMS    int64          `json:"latency_ms"`
			Capabilities map[string]any `json:"capabilities"`
			CheckedAt    string         `json:"checked_at"`
		}
		resp := h.apiRequest(t, http.MethodGet, "/api/v1/connectors/"+h.connector.ID+"/health", &got)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		if got.Status != "healthy" {
			t.Fatalf("health status = %q, want healthy", got.Status)
		}
		if got.CheckedAt == "" {
			t.Error("checked_at is empty")
		}

		conn, err := h.store.Connectors().Get(ctx, h.tenant.ID, h.connector.ID)
		if err != nil {
			t.Fatal(err)
		}
		if conn.Status != "healthy" {
			t.Errorf("persisted connector status = %q, want healthy", conn.Status)
		}
	})

	t.Run("a wrong-tenant connector id 404s", func(t *testing.T) {
		// A syntactically valid but nonexistent id: the connectors.id
		// column is a Postgres uuid, and a non-UUID string like
		// "does-not-exist" fails to parse before the tenant-scoping
		// WHERE clause is even evaluated -- exactly like every other
		// connector-scoped route (Get, Update, Delete, Tools), not
		// something particular to health.
		req, err := http.NewRequest(http.MethodGet, h.apiBaseURL+"/api/v1/connectors/"+uuid.NewString()+"/health", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+h.adminKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
	})

	t.Run("discover writes both backend tools to the cache", func(t *testing.T) {
		var got struct {
			Items []struct {
				ToolName string `json:"tool_name"`
			} `json:"items"`
			Total int `json:"total"`
		}
		resp := h.apiRequest(t, http.MethodPost, "/api/v1/connectors/"+h.connector.ID+"/discover", &got)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		if got.Total != 2 {
			t.Fatalf("total = %d, want 2 (echo, danger)", got.Total)
		}
	})

	t.Run("cache stats and search see the discovered tools", func(t *testing.T) {
		var stats struct {
			TotalTools int `json:"total_tools"`
			Connectors []struct {
				ConnectorID string `json:"connector_id"`
				Tools       int    `json:"tools"`
			} `json:"connectors"`
		}
		resp := h.apiRequest(t, http.MethodGet, "/api/v1/cache/stats", &stats)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		if stats.TotalTools != 2 || len(stats.Connectors) != 1 || stats.Connectors[0].Tools != 2 {
			t.Fatalf("stats = %+v", stats)
		}

		var search struct {
			Items []struct {
				ToolName string `json:"tool_name"`
			} `json:"items"`
			Total int `json:"total"`
		}
		resp = h.apiRequest(t, http.MethodGet, "/api/v1/cache/search?q=echo", &search)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		if search.Total != 1 || search.Items[0].ToolName != "echo" {
			t.Fatalf("search = %+v", search)
		}
	})

	t.Run("delete then refresh the cache", func(t *testing.T) {
		resp := h.apiRequest(t, http.MethodDelete, "/api/v1/cache", nil)
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("delete status = %d, want 204", resp.StatusCode)
		}

		var stats struct {
			TotalTools int `json:"total_tools"`
		}
		resp = h.apiRequest(t, http.MethodGet, "/api/v1/cache/stats", &stats)
		if resp.StatusCode != http.StatusOK || stats.TotalTools != 0 {
			t.Fatalf("stats after delete = %+v, status = %d", stats, resp.StatusCode)
		}

		var refresh struct {
			Refreshed int `json:"refreshed"`
		}
		resp = h.apiRequest(t, http.MethodPost, "/api/v1/cache/refresh", &refresh)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("refresh status = %d", resp.StatusCode)
		}
		if refresh.Refreshed != 2 {
			t.Fatalf("refreshed = %d, want 2", refresh.Refreshed)
		}
	})
}

// sessionStream opens GET /mcp/stream bound to a session and returns its
// data frames. The handler subscribes before it sends its headers, so the
// stream is live once this returns.
func (h *harness) sessionStream(t *testing.T, ctx context.Context, sessionID string) <-chan string {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.baseURL+"/mcp/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+h.adminKey)
	req.Header.Set(mcp.HeaderSessionID, sessionID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /mcp/stream: %v", err)
	}
	if ct := resp.Header.Get("Content-Type"); resp.StatusCode != http.StatusOK || ct != "text/event-stream" {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("GET /mcp/stream = %d %q: %s", resp.StatusCode, ct, body)
	}
	frames := make(chan string, 16)
	go func() {
		defer close(frames)
		defer resp.Body.Close()
		lines := bufio.NewScanner(resp.Body)
		for lines.Scan() {
			if data, ok := strings.CutPrefix(lines.Text(), "data: "); ok {
				frames <- data
			}
		}
	}()
	return frames
}

// TestMCPProgressRelayEndToEnd drives a tools/call that carries a
// progressToken to a backend that streams notifications/progress, and
// reads the progress back off the session's real GET /mcp/stream.
func TestMCPProgressRelayEndToEnd(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	resp, decoded, raw := h.rpc(t, mcp.Request{
		JSONRPC: mcp.Version, ID: 1, Method: mcp.MethodInitialize,
		Params: json.RawMessage(`{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"e2e","version":"1"}}`),
	}, rpcOptions{})
	if decoded.Error != nil {
		t.Fatalf("initialize failed: %s", raw)
	}
	sessionID := resp.Header.Get(mcp.HeaderSessionID)

	frames := h.sessionStream(t, ctx, sessionID)

	// dptest.SlowTool is served without being advertised, and no profile
	// header means every tool in the tenant is in scope.
	_, decoded, raw = h.rpc(t, mcp.Request{
		JSONRPC: mcp.Version, ID: 2, Method: mcp.MethodToolsCall,
		Params: json.RawMessage(`{"name":"alpha__` + dptest.SlowTool + `","arguments":{"steps":3},"_meta":{"progressToken":"e2e-progress"}}`),
	}, rpcOptions{SessionID: sessionID})
	t.Logf("tools/call alpha__slow -> %s", raw)
	if decoded.Error != nil {
		t.Fatalf("tools/call failed: %+v", decoded.Error)
	}

	for want := 1; want <= 3; want++ {
		select {
		case frame := <-frames:
			t.Logf("stream <- %s", frame)
			var note mcp.Request
			if err := json.Unmarshal([]byte(frame), &note); err != nil {
				t.Fatalf("frame %q: %v", frame, err)
			}
			var p mcp.ProgressParams
			if err := json.Unmarshal(note.Params, &p); err != nil {
				t.Fatal(err)
			}
			if note.Method != mcp.NotificationProgress || string(p.ProgressToken) != `"e2e-progress"` || p.Progress != float64(want) {
				t.Fatalf("frame = %s, want progress %d for the caller's token", frame, want)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("progress %d never reached the session's stream", want)
		}
	}
}

// TestMCPResourceSubscriptionEndToEnd subscribes to a gateway resource
// URI over real HTTP, has the backend push an update on the stream the
// gateway opened to it, and reads the update -- re-namespaced -- back off
// the session's real GET /mcp/stream. It also proves the gateway opens
// that stream with the connector's resolved credentials, like any call.
func TestMCPResourceSubscriptionEndToEnd(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	resp, decoded, raw := h.rpc(t, mcp.Request{
		JSONRPC: mcp.Version, ID: 1, Method: mcp.MethodInitialize,
		Params: json.RawMessage(`{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"e2e","version":"1"}}`),
	}, rpcOptions{})
	if decoded.Error != nil {
		t.Fatalf("initialize failed: %s", raw)
	}
	t.Logf("initialize -> %s", raw)
	var init mcp.InitializeResult
	if err := json.Unmarshal(decoded.Result, &init); err != nil {
		t.Fatal(err)
	}
	if init.Capabilities.Resources == nil || !init.Capabilities.Resources.Subscribe {
		t.Fatalf("resources capability = %+v, want subscribe advertised", init.Capabilities.Resources)
	}
	sessionID := resp.Header.Get(mcp.HeaderSessionID)
	frames := h.sessionStream(t, ctx, sessionID)

	const uri = "test://static/resource/1"
	params, _ := json.Marshal(mcp.ResourcesSubscribeParams{URI: "gw://alpha/" + uri})
	_, decoded, raw = h.rpc(t, mcp.Request{
		JSONRPC: mcp.Version, ID: 2, Method: mcp.MethodResourcesSubscribe, Params: params,
	}, rpcOptions{SessionID: sessionID, ProfileName: "Reader"})
	t.Logf("resources/subscribe (profile=Reader) -> %s", raw)
	if decoded.Error != nil {
		t.Fatalf("resources/subscribe failed: %+v", decoded.Error)
	}

	streamHeaders := h.backend.StreamHeaders()
	for name, want := range map[string]string{
		"X-Backend-Key":     "s3cret-backend-key",
		"X-Static-Header":   "static-value",
		mcp.HeaderSessionID: h.backend.SessionID(),
	} {
		if got := streamHeaders.Get(name); got != want {
			t.Errorf("connector stream GET %s = %q, want %q", name, got, want)
		}
	}

	if !h.backend.ResourceUpdated(uri) {
		t.Fatal("the backend holds no subscription for the original uri")
	}
	for {
		select {
		case frame := <-frames:
			t.Logf("stream <- %s", frame)
			var note mcp.Request
			if err := json.Unmarshal([]byte(frame), &note); err != nil {
				t.Fatalf("frame %q: %v", frame, err)
			}
			if note.Method == mcp.NotificationToolsListChanged {
				continue
			}
			var p mcp.ResourceUpdatedParams
			if err := json.Unmarshal(note.Params, &p); err != nil {
				t.Fatal(err)
			}
			if note.Method != mcp.NotificationResourcesUpdated || p.URI != "gw://alpha/"+uri {
				t.Fatalf("frame = %s, want resources/updated for gw://alpha/%s", frame, uri)
			}
			return
		case <-time.After(10 * time.Second):
			t.Fatal("the update never reached the session's stream")
		}
	}
}

// TestMCPElicitationRelayEndToEnd turns on a connector's elicitation
// policy, initializes as an agent that declares elicitation, and calls a
// tool that asks for input mid-call. The request is read off the
// session's real GET /mcp/stream, answered with a real POST /mcp, and the
// answer comes back in the tool's result -- all over real sockets, with
// the connector row in Postgres.
func TestMCPElicitationRelayEndToEnd(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h.connector.Metadata["server_requests"] = map[string]any{"elicitation": true}
	if err := h.store.Connectors().Update(ctx, h.connector); err != nil {
		t.Fatalf("enable the elicitation policy: %v", err)
	}

	resp, decoded, raw := h.rpc(t, mcp.Request{
		JSONRPC: mcp.Version, ID: 1, Method: mcp.MethodInitialize,
		Params: json.RawMessage(`{"protocolVersion":"2025-06-18","capabilities":{"elicitation":{},"sampling":{}},"clientInfo":{"name":"e2e","version":"1"}}`),
	}, rpcOptions{})
	if decoded.Error != nil {
		t.Fatalf("initialize failed: %s", raw)
	}
	sessionID := resp.Header.Get(mcp.HeaderSessionID)
	// The connector is declared elicitation -- the agent declared it and
	// the policy allows it -- and not sampling, which the policy does not.
	if caps := h.backend.InitializeCapabilities(); len(caps) == 0 || string(caps[len(caps)-1]) != `{"elicitation":{}}` {
		t.Fatalf("connector was declared %s, want exactly elicitation", caps)
	}

	frames := h.sessionStream(t, ctx, sessionID)

	type outcome struct {
		decoded mcp.Response
		raw     []byte
	}
	done := make(chan outcome, 1)
	go func() {
		// Not h.rpc: t.Fatal must not run off the test goroutine.
		body := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"alpha__` + dptest.AskTool +
			`","arguments":{"method":"elicitation/create","params":{"message":"Which project?","requestedSchema":{"type":"object","properties":{"project":{"type":"string"}}}}}}}`
		req, _ := http.NewRequest(http.MethodPost, h.baseURL+"/mcp", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+h.adminKey)
		req.Header.Set(mcp.HeaderSessionID, sessionID)
		var out outcome
		if resp, err := http.DefaultClient.Do(req); err == nil {
			out.raw, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
			_ = json.Unmarshal(out.raw, &out.decoded)
		}
		done <- out
	}()

	var request mcp.Request
	for request.Method == "" {
		select {
		case frame := <-frames:
			t.Logf("stream <- %s", frame)
			var msg mcp.Request
			if err := json.Unmarshal([]byte(frame), &msg); err != nil {
				t.Fatalf("frame %q: %v", frame, err)
			}
			if msg.Method == "elicitation/create" {
				request = msg
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the elicitation never reached the session's stream")
		}
	}
	id, _ := request.ID.(string)
	if !strings.HasPrefix(id, "gw-") || !strings.Contains(string(request.Params), "Which project?") {
		t.Fatalf("relayed request = %+v, want a gw- id and the connector's params", request)
	}

	answer := `{"jsonrpc":"2.0","id":"` + id + `","result":{"action":"accept","content":{"project":"apollo"}}}`
	req, err := http.NewRequest(http.MethodPost, h.baseURL+"/mcp", strings.NewReader(answer))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.adminKey)
	req.Header.Set(mcp.HeaderSessionID, sessionID)
	ack, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST the answer: %v", err)
	}
	ack.Body.Close()
	if ack.StatusCode != http.StatusAccepted {
		t.Fatalf("POST the answer = %d, want 202", ack.StatusCode)
	}

	select {
	case out := <-done:
		t.Logf("tools/call alpha__ask -> %s", out.raw)
		if out.decoded.Error != nil || !strings.Contains(string(out.decoded.Result), `apollo`) ||
			!strings.Contains(string(out.decoded.Result), `accept`) {
			t.Fatalf("tools/call = %s, want the agent's answer in the tool result", out.raw)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the tools/call never returned")
	}
}
