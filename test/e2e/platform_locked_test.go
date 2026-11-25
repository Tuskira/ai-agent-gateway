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
	"time"

	dpheaders "github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/headers"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/secrets"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// TestPlatformLockedEndToEnd drives the locked flag through a real gateway
// listener and real Postgres, as a tenant-admin key and as a platform-admin
// key of the same tenant: the tenant admin may use a locked source in
// connector headers but cannot change, rotate, disable, delete or re-point
// a locked credential or source, cannot set or clear the flag, and cannot
// register a source that names a locked credential; the platform key can do
// all of it.
func TestPlatformLockedEndToEnd(t *testing.T) {
	h, secretSvc := newFixture(t)
	ctx := context.Background()

	tenantKey := h.adminKey // role "admin": a tenant admin
	platformKey := newAPIKey(t, h.store, h.tenant.ID, "e2e-platform", pkgauth.RolePlatformAdmin)

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

	do := func(key, method, path string, body any) (int, string) {
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
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw)
	}
	want := func(t *testing.T, what string, key, method, path string, body any, code int, contains string) string {
		t.Helper()
		got, raw := do(key, method, path, body)
		if got != code || !strings.Contains(raw, contains) {
			t.Fatalf("%s: %s %s = %d %s, want %d containing %q", what, method, path, got, raw, code, contains)
		}
		return raw
	}
	const (
		managed  = "managed by the platform"
		permType = "permission_error"
	)

	// --- provisioning: only the platform key may set the flag -------------
	t.Run("tenant admin cannot create locked rows", func(t *testing.T) {
		locked := map[string]any{"name": "sneaky", "type": "generic", "payload": map[string]string{"token": "x"}, "locked": true}
		want(t, "locked credential as tenant admin", tenantKey, http.MethodPost, "/api/v1/credentials", locked, http.StatusForbidden, permType)
		want(t, "locked source as tenant admin", tenantKey, http.MethodPost, "/api/v1/credential-sources", map[string]any{
			"name": "sneaky", "provider": "external_service", "config": map[string]any{"url": cs.URL}, "locked": true,
		}, http.StatusForbidden, permType)
	})

	lockedCred := map[string]any{"name": "plat-auth", "type": "generic", "payload": map[string]string{"token": "svc-token-a"}, "locked": true}
	raw := want(t, "platform creates locked credential", platformKey, http.MethodPost, "/api/v1/credentials", lockedCred, http.StatusCreated, `"locked":true`)
	if strings.Contains(raw, "svc-token-a") {
		t.Fatalf("credential response leaked the payload: %s", raw)
	}
	var src struct {
		ID     string `json:"id"`
		Locked bool   `json:"locked"`
	}
	raw = want(t, "platform creates locked source", platformKey, http.MethodPost, "/api/v1/credential-sources", map[string]any{
		"name": "plat-src", "provider": "external_service",
		"config": map[string]any{"url": cs.URL, "auth_credential": "plat-auth"}, "locked": true,
	}, http.StatusCreated, `"locked":true`)
	if err := json.Unmarshal([]byte(raw), &src); err != nil || src.ID == "" || !src.Locked {
		t.Fatalf("decode source: %v %s", err, raw)
	}
	srcPath := "/api/v1/credential-sources/" + src.ID

	t.Run("tenant admin sees the flag but not the secret", func(t *testing.T) {
		raw := want(t, "get credential", tenantKey, http.MethodGet, "/api/v1/credentials/plat-auth", nil, http.StatusOK, `"locked":true`)
		if strings.Contains(raw, "svc-token-a") {
			t.Fatalf("credential view leaked the payload: %s", raw)
		}
		want(t, "list credentials", tenantKey, http.MethodGet, "/api/v1/credentials", nil, http.StatusOK, `"locked":true`)
		want(t, "get source", tenantKey, http.MethodGet, srcPath, nil, http.StatusOK, `"locked":true`)
		want(t, "list sources", tenantKey, http.MethodGet, "/api/v1/credential-sources", nil, http.StatusOK, `"locked":true`)
	})

	t.Run("tenant admin cannot change a locked credential", func(t *testing.T) {
		want(t, "rotate", tenantKey, http.MethodPut, "/api/v1/credentials/plat-auth", map[string]any{"payload": map[string]string{"token": "evil"}}, http.StatusForbidden, managed)
		want(t, "unlock", tenantKey, http.MethodPut, "/api/v1/credentials/plat-auth", map[string]any{"locked": false}, http.StatusForbidden, managed)
		want(t, "delete", tenantKey, http.MethodDelete, "/api/v1/credentials/plat-auth", nil, http.StatusForbidden, managed)
		// The secret is untouched.
		if got, err := secretSvc.GetField(store.WithLockedAccess(ctx, true), h.tenant.ID, "plat-auth", "token"); err != nil || got != "svc-token-a" {
			t.Fatalf("credential payload after refused rotate = %q, %v", got, err)
		}
	})

	t.Run("tenant admin cannot change a locked source", func(t *testing.T) {
		want(t, "disable", tenantKey, http.MethodPut, srcPath, map[string]any{"enabled": false}, http.StatusForbidden, managed)
		want(t, "re-point", tenantKey, http.MethodPut, srcPath, map[string]any{"config": map[string]any{"url": "https://evil.example.com", "auth_credential": "plat-auth"}}, http.StatusForbidden, managed)
		want(t, "rename", tenantKey, http.MethodPut, srcPath, map[string]any{"name": "mine"}, http.StatusForbidden, managed)
		want(t, "unlock", tenantKey, http.MethodPut, srcPath, map[string]any{"locked": false}, http.StatusForbidden, managed)
		want(t, "delete", tenantKey, http.MethodDelete, srcPath, nil, http.StatusForbidden, managed)
		got, err := h.store.CredentialSources().Get(ctx, h.tenant.ID, src.ID)
		if err != nil || !got.Enabled || !got.Locked || got.Config["url"] != cs.URL || got.Name != "plat-src" {
			t.Fatalf("source after refused changes = %+v, %v", got, err)
		}
	})

	t.Run("tenant admin can still test a locked source", func(t *testing.T) {
		want(t, "test", tenantKey, http.MethodPost, srcPath+"/test", nil, http.StatusOK, `"ok":true`)
	})

	t.Run("tenant admin cannot register a source that names a locked credential", func(t *testing.T) {
		want(t, "create", tenantKey, http.MethodPost, "/api/v1/credential-sources", map[string]any{
			"name": "reuse", "provider": "external_service",
			"config": map[string]any{"url": cs.URL, "auth_credential": "plat-auth"},
		}, http.StatusForbidden, managed)

		// A source of the tenant's own, on an unlocked credential, is fine...
		want(t, "own credential", tenantKey, http.MethodPost, "/api/v1/credentials", map[string]any{
			"name": "own-auth", "type": "generic", "payload": map[string]string{"token": "own"},
		}, http.StatusCreated, `"locked":false`)
		var own struct {
			ID string `json:"id"`
		}
		raw := want(t, "own source", tenantKey, http.MethodPost, "/api/v1/credential-sources", map[string]any{
			"name": "own-src", "provider": "external_service",
			"config": map[string]any{"url": cs.URL, "auth_credential": "own-auth"},
		}, http.StatusCreated, `"locked":false`)
		if err := json.Unmarshal([]byte(raw), &own); err != nil {
			t.Fatal(err)
		}
		// ...but cannot be re-pointed at the locked one.
		want(t, "re-point own source", tenantKey, http.MethodPut, "/api/v1/credential-sources/"+own.ID, map[string]any{
			"config": map[string]any{"url": cs.URL, "auth_credential": "plat-auth"},
		}, http.StatusForbidden, managed)
		// Nor can the tenant admin lock its own rows.
		want(t, "lock own credential", tenantKey, http.MethodPut, "/api/v1/credentials/own-auth", map[string]any{"locked": true}, http.StatusForbidden, permType)
		want(t, "lock own source", tenantKey, http.MethodPut, "/api/v1/credential-sources/"+own.ID, map[string]any{"locked": true}, http.StatusForbidden, permType)
		// Own unlocked rows stay fully editable.
		want(t, "rotate own", tenantKey, http.MethodPut, "/api/v1/credentials/own-auth", map[string]any{"payload": map[string]string{"token": "own2"}}, http.StatusOK, `"locked":false`)
		want(t, "disable own source", tenantKey, http.MethodPut, "/api/v1/credential-sources/"+own.ID, map[string]any{"enabled": false}, http.StatusOK, `"enabled":false`)
		want(t, "delete own source", tenantKey, http.MethodDelete, "/api/v1/credential-sources/"+own.ID, nil, http.StatusNoContent, "")
		want(t, "delete own credential", tenantKey, http.MethodDelete, "/api/v1/credentials/own-auth", nil, http.StatusNoContent, "")
	})

	t.Run("a connector header resolves through the locked source", func(t *testing.T) {
		conn, err := h.store.Connectors().Get(ctx, h.tenant.ID, h.connector.ID)
		if err != nil {
			t.Fatal(err)
		}
		gate := newTokenGate(t, h.backend.Server.Config.Handler, "ext-v1")
		conn.Endpoint = gate.URL
		conn.Metadata["headers"].(map[string]any)["X-Ext-Token"] = map[string]any{
			"type": "external", "provider": "external_service", "source": "plat-src",
			"config": map[string]any{"credential_id": "splunk-prod", "field": "token"},
		}
		if err := h.store.Connectors().Update(ctx, conn); err != nil {
			t.Fatal(err)
		}
		resp, decoded, rawResp := h.rpc(t, mcp.Request{
			JSONRPC: mcp.Version, ID: 1, Method: mcp.MethodInitialize,
			Params: json.RawMessage(`{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"e2e","version":"1"}}`),
		}, rpcOptions{})
		if decoded.Error != nil {
			t.Fatalf("initialize failed: %+v (%s)", decoded.Error, rawResp)
		}
		_, decoded, rawResp = h.rpc(t, mcp.Request{
			JSONRPC: mcp.Version, ID: 2, Method: mcp.MethodToolsCall,
			Params: json.RawMessage(`{"name":"alpha__echo","arguments":{"message":"via locked"}}`),
		}, rpcOptions{SessionID: resp.Header.Get(mcp.HeaderSessionID), ProfileName: "Reader"})
		if decoded.Error != nil {
			t.Fatalf("tools/call failed: %+v (%s)", decoded.Error, rawResp)
		}
		if v := h.backend.LastHeaders().Get("X-Ext-Token"); v != "ext-v1" {
			t.Errorf("backend saw X-Ext-Token = %q, want ext-v1 (resolved through the locked source)", v)
		}
		if last := cs.lastResolve(); last["tenant_id"] != h.tenant.ID || last["credential_id"] != "splunk-prod" {
			t.Errorf("resolve request = %v", last)
		}

		// While a connector header names it, even the platform's delete is
		// the ordinary 409; the tenant admin still gets the 403 first.
		want(t, "tenant delete", tenantKey, http.MethodDelete, srcPath, nil, http.StatusForbidden, managed)
		want(t, "platform delete while used", platformKey, http.MethodDelete, srcPath, nil, http.StatusConflict, "")
		delete(conn.Metadata["headers"].(map[string]any), "X-Ext-Token")
		if err := h.store.Connectors().Update(ctx, conn); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("platform key can change locked rows", func(t *testing.T) {
		want(t, "rotate", platformKey, http.MethodPut, "/api/v1/credentials/plat-auth", map[string]any{"payload": map[string]string{"token": "svc-token-a2"}}, http.StatusOK, `"locked":true`)
		if got, err := secretSvc.GetField(store.WithLockedAccess(ctx, true), h.tenant.ID, "plat-auth", "token"); err != nil || got != "svc-token-a2" {
			t.Fatalf("payload after platform rotate = %q, %v", got, err)
		}
		want(t, "disable source", platformKey, http.MethodPut, srcPath, map[string]any{"enabled": false}, http.StatusOK, `"enabled":false`)
		want(t, "enable source", platformKey, http.MethodPut, srcPath, map[string]any{"enabled": true}, http.StatusOK, `"enabled":true`)
		want(t, "re-point source", platformKey, http.MethodPut, srcPath, map[string]any{"config": map[string]any{"url": cs.URL, "auth_credential": "plat-auth"}}, http.StatusOK, `"locked":true`)

		// Clearing the flag hands the rows back to the tenant admin.
		want(t, "unlock credential", platformKey, http.MethodPut, "/api/v1/credentials/plat-auth", map[string]any{"locked": false}, http.StatusOK, `"locked":false`)
		want(t, "unlock source", platformKey, http.MethodPut, srcPath, map[string]any{"locked": false}, http.StatusOK, `"locked":false`)
		want(t, "tenant rotate after unlock", tenantKey, http.MethodPut, "/api/v1/credentials/plat-auth", map[string]any{"payload": map[string]string{"token": "svc-token-a3"}}, http.StatusOK, `"locked":false`)
		want(t, "tenant disable after unlock", tenantKey, http.MethodPut, srcPath, map[string]any{"enabled": false}, http.StatusOK, `"enabled":false`)

		// Re-lock, then the platform key deletes both.
		want(t, "relock source without config", platformKey, http.MethodPut, srcPath, map[string]any{"locked": true}, http.StatusBadRequest, "config")
		want(t, "relock source", platformKey, http.MethodPut, srcPath, map[string]any{"locked": true, "config": map[string]any{"url": cs.URL, "auth_credential": "plat-auth"}}, http.StatusOK, `"locked":true`)
		want(t, "relock credential", platformKey, http.MethodPut, "/api/v1/credentials/plat-auth", map[string]any{"locked": true}, http.StatusOK, `"locked":true`)
		want(t, "tenant still blocked", tenantKey, http.MethodDelete, "/api/v1/credentials/plat-auth", nil, http.StatusForbidden, managed)
		want(t, "platform delete source", platformKey, http.MethodDelete, srcPath, nil, http.StatusNoContent, "")
		want(t, "platform delete credential", platformKey, http.MethodDelete, "/api/v1/credentials/plat-auth", nil, http.StatusNoContent, "")
	})

	t.Run("PUT with neither payload nor locked is still rejected", func(t *testing.T) {
		want(t, "create", tenantKey, http.MethodPost, "/api/v1/credentials", map[string]any{
			"name": "tmp", "type": "generic", "payload": map[string]string{"a": "b"},
		}, http.StatusCreated, `"locked":false`)
		want(t, "empty PUT", tenantKey, http.MethodPut, "/api/v1/credentials/tmp", map[string]any{}, http.StatusBadRequest, "payload")
		want(t, "cleanup", tenantKey, http.MethodDelete, "/api/v1/credentials/tmp", nil, http.StatusNoContent, "")
	})
}
