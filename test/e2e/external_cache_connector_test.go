//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	dpheaders "github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/headers"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/secrets"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// Two connectors whose headers name the same credential id on the same
// source each get their own resolve (and their own cached value) from the
// external credential service.
func TestExternalServiceCacheIsPerConnectorEndToEnd(t *testing.T) {
	h, secretSvc := newFixture(t)
	ctx := context.Background()

	cs := newCredService(t)
	cs.setValue("shared-id", "ext-v1")
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

	extHeaders := map[string]any{
		"X-Ext-Token": map[string]any{
			"type": "external", "provider": "external_service", "source": "vault-a",
			"config": map[string]any{"credential_id": "shared-id", "field": "token"},
		},
	}
	// alpha (from the fixture) and a second connector on the same backend.
	conn, err := h.store.Connectors().Get(ctx, h.tenant.ID, h.connector.ID)
	if err != nil {
		t.Fatal(err)
	}
	conn.Metadata["headers"] = extHeaders
	if err := h.store.Connectors().Update(ctx, conn); err != nil {
		t.Fatal(err)
	}
	beta := &store.Connector{
		TenantID: h.tenant.ID, Name: "beta", Slug: "beta", Endpoint: h.backend.URL, TimeoutMS: 5000,
		Status: "unknown", Metadata: map[string]any{"headers": extHeaders},
	}
	if err := h.store.Connectors().Create(ctx, beta); err != nil {
		t.Fatal(err)
	}

	echo := func(id int, tool string) {
		t.Helper()
		_, decoded, raw := h.rpc(t, mcp.Request{
			JSONRPC: mcp.Version, ID: id, Method: mcp.MethodToolsCall,
			Params: json.RawMessage(`{"name":"` + tool + `","arguments":{"message":"hi"}}`),
		}, rpcOptions{})
		if decoded.Error != nil {
			t.Fatalf("%s: %+v (%s)", tool, decoded.Error, raw)
		}
	}

	echo(1, "alpha__echo")
	echo(2, "beta__echo")
	if n := cs.resolveCount("shared-id"); n != 2 {
		t.Fatalf("resolve calls = %d, want 2: the second connector reused the first one's cached value", n)
	}
	if got := cs.lastResolve()["connector_slug"]; got != "beta" {
		t.Errorf("last resolve was for connector %q, want beta", got)
	}

	// Each connector now hits its own cache entry.
	echo(3, "alpha__echo")
	echo(4, "beta__echo")
	if n := cs.resolveCount("shared-id"); n != 2 {
		t.Errorf("resolve calls = %d after repeat calls, want 2 (cached per connector)", n)
	}
}
