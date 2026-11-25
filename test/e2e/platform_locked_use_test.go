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
	pkgheaders "github.com/Tuskira/tusk-ai-secured-gateway/pkg/headers"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// TestPlatformLockedUseEndToEnd checks, through real listeners and real
// Postgres, that a platform-locked credential cannot be USED by
// configuration a tenant admin wrote (where the tenant picks the endpoint
// the secret goes to), and what happens when the platform takes over a
// name the tenant already used:
//   - a connector header the tenant saved stops sending the value as soon
//     as the credential is locked, and the tenant cannot (re)save one, even
//     with a forged stamp;
//   - a platform POST of a taken name is a clear 409, and the explicit
//     takeover is a PUT with locked:true;
//   - a header the platform admin saves resolves, and the tenant can no
//     longer re-point that connector or change its metadata (headers, TLS),
//     but can still disable and enable it and change its description;
//   - an unlocked source cannot authenticate with a locked credential;
//   - a tenant model cannot name a locked credential.
func TestPlatformLockedUseEndToEnd(t *testing.T) {
	h, secretSvc := newFixture(t)
	ctx := context.Background()

	tenantKey := h.adminKey // role "admin": a tenant admin
	platformKey := newAPIKey(t, h.store, h.tenant.ID, "e2e-platform-use", pkgauth.RolePlatformAdmin)

	cs := newCredService(t)
	cs.setValue("splunk-prod", "ext-v1")
	cs.setToken(h.tenant.ID, "tenant-svc-token")

	registry := dpheaders.NewRegistry()
	if err := registry.RegisterExternal(secrets.NewSecretStoreProvider(secretSvc)); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterExternal(secrets.NewExternalServiceProvider(secretSvc.Fetcher(), 5*time.Second, time.Millisecond)); err != nil {
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
	want := func(t *testing.T, what, key, method, path string, body any, code int, contains string) string {
		t.Helper()
		got, raw := do(key, method, path, body)
		if got != code || !strings.Contains(raw, contains) {
			t.Fatalf("%s: %s %s = %d %s, want %d containing %q", what, method, path, got, raw, code, contains)
		}
		return raw
	}
	const managed = "managed by the platform"
	connPath := "/api/v1/connectors/" + h.connector.ID

	// connector returns the connector as the API shows it (masked).
	type connView struct {
		Name     string         `json:"name"`
		Endpoint string         `json:"endpoint"`
		Metadata map[string]any `json:"metadata"`
	}
	connector := func(t *testing.T) connView {
		t.Helper()
		raw := want(t, "get connector", platformKey, http.MethodGet, connPath, nil, http.StatusOK, "")
		var v connView
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	withHeader := func(v connView, name string, cfg map[string]any) map[string]any {
		meta := map[string]any{}
		for k, x := range v.Metadata {
			meta[k] = x
		}
		hdrs := map[string]any{}
		for k, x := range v.Metadata["headers"].(map[string]any) {
			hdrs[k] = x
		}
		hdrs[name] = cfg
		meta["headers"] = hdrs
		return map[string]any{"name": v.Name, "endpoint": v.Endpoint, "metadata": meta}
	}
	secretStore := func(cred string) map[string]any {
		return map[string]any{"type": "external", "provider": "secret_store", "config": map[string]any{"credential": cred, "field": "token"}}
	}
	session := ""
	call := func(t *testing.T) {
		t.Helper()
		if session == "" {
			resp, decoded, raw := h.rpc(t, mcp.Request{
				JSONRPC: mcp.Version, ID: 1, Method: mcp.MethodInitialize,
				Params: json.RawMessage(`{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"e2e","version":"1"}}`),
			}, rpcOptions{})
			if decoded.Error != nil {
				t.Fatalf("initialize failed: %+v (%s)", decoded.Error, raw)
			}
			session = resp.Header.Get(mcp.HeaderSessionID)
		}
		_, decoded, raw := h.rpc(t, mcp.Request{
			JSONRPC: mcp.Version, ID: 2, Method: mcp.MethodToolsCall,
			Params: json.RawMessage(`{"name":"alpha__echo","arguments":{"message":"locked use"}}`),
		}, rpcOptions{SessionID: session, ProfileName: "Reader"})
		if decoded.Error != nil {
			t.Fatalf("tools/call failed: %+v (%s)", decoded.Error, raw)
		}
	}

	// --- name squatting: the tenant used "plat-tok" first --------------------
	t.Run("a tenant header on its own credential works before the takeover", func(t *testing.T) {
		want(t, "tenant credential", tenantKey, http.MethodPost, "/api/v1/credentials", map[string]any{
			"name": "plat-tok", "type": "generic", "payload": map[string]string{"token": "tenant-value"},
		}, http.StatusCreated, `"locked":false`)
		want(t, "tenant header", tenantKey, http.MethodPut, connPath, withHeader(connector(t), "X-Plat", secretStore("plat-tok")), http.StatusOK, "")
		call(t)
		if v := h.backend.LastHeaders().Get("X-Plat"); v != "tenant-value" {
			t.Fatalf("backend X-Plat = %q, want tenant-value", v)
		}
	})

	t.Run("platform POST of a taken name is a clear 409, takeover is an explicit PUT", func(t *testing.T) {
		want(t, "platform create", platformKey, http.MethodPost, "/api/v1/credentials", map[string]any{
			"name": "plat-tok", "type": "generic", "payload": map[string]string{"token": "plat-secret"}, "locked": true,
		}, http.StatusConflict, "take it over")
		want(t, "platform takeover", platformKey, http.MethodPut, "/api/v1/credentials/plat-tok", map[string]any{
			"payload": map[string]string{"token": "plat-secret"}, "locked": true,
		}, http.StatusOK, `"locked":true`)
		if got, err := secretSvc.GetField(store.WithLockedAccess(ctx, true), h.tenant.ID, "plat-tok", "token"); err != nil || got != "plat-secret" {
			t.Fatalf("payload after takeover = %q, %v", got, err)
		}
	})

	t.Run("the tenant-saved header no longer sends the locked value", func(t *testing.T) {
		call(t)
		if v := h.backend.LastHeaders().Get("X-Plat"); v != "" {
			t.Fatalf("backend X-Plat = %q after the takeover, want no header (the tenant saved it)", v)
		}
	})

	t.Run("tenant cannot reference the locked credential, even with a forged stamp", func(t *testing.T) {
		forged := secretStore("plat-tok")
		forged[pkgheaders.AllowLockedKey] = true
		want(t, "forged stamp", tenantKey, http.MethodPut, connPath, withHeader(connector(t), "X-Plat", forged), http.StatusForbidden, managed)
		want(t, "new header", tenantKey, http.MethodPut, connPath, withHeader(connector(t), "X-Other", secretStore("plat-tok")), http.StatusForbidden, managed)
		want(t, "new connector", tenantKey, http.MethodPost, "/api/v1/connectors", map[string]any{
			"name": "exfil", "endpoint": h.backend.URL,
			"metadata": map[string]any{"headers": map[string]any{"X-Plat": secretStore("plat-tok")}},
		}, http.StatusForbidden, managed)
		// A forged stamp on a header naming an unlocked credential is dropped.
		want(t, "own credential", tenantKey, http.MethodPost, "/api/v1/credentials", map[string]any{
			"name": "own-tok", "type": "generic", "payload": map[string]string{"token": "own"},
		}, http.StatusCreated, "")
		ownForged := secretStore("own-tok")
		ownForged[pkgheaders.AllowLockedKey] = true
		raw := want(t, "forged stamp on own", tenantKey, http.MethodPut, connPath, withHeader(connector(t), "X-Own", ownForged), http.StatusOK, "")
		if strings.Contains(raw, pkgheaders.AllowLockedKey) {
			t.Fatalf("a tenant's stamp was kept: %s", raw)
		}
		// Locking own-tok later does not make that header resolve it.
		want(t, "platform locks own-tok", platformKey, http.MethodPut, "/api/v1/credentials/own-tok", map[string]any{"locked": true}, http.StatusOK, `"locked":true`)
		call(t)
		if v := h.backend.LastHeaders().Get("X-Own"); v != "" {
			t.Fatalf("backend X-Own = %q, want no header", v)
		}
	})

	t.Run("a header the platform admin saves resolves; the tenant cannot re-point it", func(t *testing.T) {
		raw := want(t, "platform saves", platformKey, http.MethodPut, connPath, withHeader(connector(t), "X-Plat", secretStore("plat-tok")), http.StatusOK, "")
		var v connView
		_ = json.Unmarshal([]byte(raw), &v)
		if hdr, _ := v.Metadata["headers"].(map[string]any)["X-Plat"].(map[string]any); hdr[pkgheaders.AllowLockedKey] != true {
			t.Fatalf("platform header not stamped: %s", raw)
		}
		call(t)
		if got := h.backend.LastHeaders().Get("X-Plat"); got != "plat-secret" {
			t.Fatalf("backend X-Plat = %q, want plat-secret", got)
		}

		cur := connector(t)
		want(t, "tenant re-points", tenantKey, http.MethodPut, connPath, map[string]any{
			"name": cur.Name, "endpoint": "http://127.0.0.1:1/elsewhere",
		}, http.StatusForbidden, managed)
		want(t, "tenant changes a header", tenantKey, http.MethodPut, connPath, withHeader(cur, "X-Static-Header", map[string]any{"type": "static", "value": "x"}), http.StatusForbidden, managed)
		tlsMeta := map[string]any{}
		for k, x := range cur.Metadata {
			tlsMeta[k] = x
		}
		tlsMeta["tls"] = map[string]any{"insecure_skip_verify": true}
		want(t, "tenant turns off TLS verification", tenantKey, http.MethodPut, connPath, map[string]any{"name": cur.Name, "endpoint": cur.Endpoint, "metadata": tlsMeta}, http.StatusForbidden, managed)
		descMeta := map[string]any{}
		for k, x := range cur.Metadata {
			descMeta[k] = x
		}
		descMeta["description"] = "renamed by the tenant"
		want(t, "tenant changes the description", tenantKey, http.MethodPut, connPath, map[string]any{"name": cur.Name, "endpoint": cur.Endpoint, "metadata": descMeta}, http.StatusOK, "renamed by the tenant")
		// Disable and enable send the metadata back verbatim: allowed, and
		// the stamp survives.
		meta := map[string]any{}
		for k, x := range cur.Metadata {
			meta[k] = x
		}
		meta["enabled"] = false
		want(t, "tenant disables", tenantKey, http.MethodPut, connPath, map[string]any{"name": cur.Name, "endpoint": cur.Endpoint, "metadata": meta}, http.StatusOK, `"enabled":false`)
		delete(meta, "enabled")
		raw = want(t, "tenant enables", tenantKey, http.MethodPut, connPath, map[string]any{"name": cur.Name, "endpoint": cur.Endpoint, "metadata": meta}, http.StatusOK, "")
		if !strings.Contains(raw, `"allow_locked":true`) {
			t.Fatalf("stamp lost on a verbatim save: %s", raw)
		}
		call(t)
		if got := h.backend.LastHeaders().Get("X-Plat"); got != "plat-secret" {
			t.Fatalf("backend X-Plat after disable/enable = %q, want plat-secret", got)
		}
		stored, err := h.store.Connectors().Get(ctx, h.tenant.ID, h.connector.ID)
		if err != nil || stored.Endpoint != h.backend.URL {
			t.Fatalf("connector endpoint = %q, %v; want unchanged", stored.Endpoint, err)
		}
	})

	t.Run("sources: taken name, unlocked source on a locked credential", func(t *testing.T) {
		raw := want(t, "tenant source", tenantKey, http.MethodPost, "/api/v1/credential-sources", map[string]any{
			"name": "plat-src", "provider": "external_service", "config": map[string]any{"url": cs.URL, "auth_credential": "t-auth"},
		}, http.StatusCreated, `"locked":false`)
		var src struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal([]byte(raw), &src)
		want(t, "platform create taken source", platformKey, http.MethodPost, "/api/v1/credential-sources", map[string]any{
			"name": "plat-src", "provider": "external_service", "config": map[string]any{"url": cs.URL, "auth_credential": "plat-tok"}, "locked": true,
		}, http.StatusConflict, "take it over")
		want(t, "platform unlocked source on locked credential", platformKey, http.MethodPost, "/api/v1/credential-sources", map[string]any{
			"name": "loose", "provider": "external_service", "config": map[string]any{"url": cs.URL, "auth_credential": "plat-tok"},
		}, http.StatusBadRequest, "must be locked too")
		want(t, "platform takeover source", platformKey, http.MethodPut, "/api/v1/credential-sources/"+src.ID, map[string]any{
			"config": map[string]any{"url": cs.URL, "auth_credential": "plat-tok"}, "locked": true,
		}, http.StatusOK, `"locked":true`)
		want(t, "platform cannot unlock it while it uses a locked credential", platformKey, http.MethodPut, "/api/v1/credential-sources/"+src.ID, map[string]any{
			"locked": false,
		}, http.StatusBadRequest, "must be locked too")
		want(t, "tenant edits taken-over source", tenantKey, http.MethodPut, "/api/v1/credential-sources/"+src.ID, map[string]any{"enabled": false}, http.StatusForbidden, managed)
	})

	t.Run("a tenant source stops authenticating once its credential is locked", func(t *testing.T) {
		want(t, "tenant auth credential", tenantKey, http.MethodPost, "/api/v1/credentials", map[string]any{
			"name": "t-auth2", "type": "generic", "payload": map[string]string{"token": "tenant-svc-token"},
		}, http.StatusCreated, "")
		want(t, "tenant source", tenantKey, http.MethodPost, "/api/v1/credential-sources", map[string]any{
			"name": "t-src", "provider": "external_service", "config": map[string]any{"url": cs.URL, "auth_credential": "t-auth2"},
		}, http.StatusCreated, `"locked":false`)
		// Wire the header through the store: the connector's headers are the
		// platform admin's now, and this is about request time only.
		conn, err := h.store.Connectors().Get(ctx, h.tenant.ID, h.connector.ID)
		if err != nil {
			t.Fatal(err)
		}
		conn.Metadata["headers"].(map[string]any)["X-Ext-Token"] = map[string]any{
			"type": "external", "provider": "external_service", "source": "t-src",
			"config": map[string]any{"credential_id": "splunk-prod", "field": "token"},
		}
		if err := h.store.Connectors().Update(ctx, conn); err != nil {
			t.Fatal(err)
		}
		call(t)
		if v := h.backend.LastHeaders().Get("X-Ext-Token"); v != "ext-v1" {
			t.Fatalf("backend X-Ext-Token = %q before the lock, want ext-v1", v)
		}
		before := cs.resolveCount("")
		want(t, "platform locks t-auth2", platformKey, http.MethodPut, "/api/v1/credentials/t-auth2", map[string]any{"locked": true}, http.StatusOK, `"locked":true`)
		time.Sleep(5 * time.Millisecond) // the provider's value cache TTL in this test
		call(t)
		if v := h.backend.LastHeaders().Get("X-Ext-Token"); v != "" {
			t.Fatalf("backend X-Ext-Token = %q after the lock, want no header", v)
		}
		if n := cs.resolveCount(""); n != before {
			t.Fatalf("the service got %d resolve calls through an unlocked source after the lock", n-before)
		}
	})

	t.Run("a tenant model cannot name a locked credential", func(t *testing.T) {
		want(t, "tenant model", tenantKey, http.MethodPost, "/api/v1/models", map[string]any{
			"name": "exfil-model",
			"targets": []map[string]any{{
				"vendor": "openai_compat", "model": "m", "base_url": "https://api.example.com/v1", "credential": "plat-tok",
			}},
		}, http.StatusForbidden, managed)
	})
}
