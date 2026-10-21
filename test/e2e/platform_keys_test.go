//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/api"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/auth/apikey"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// TestPlatformAdminMintsKeyInNamedTenant drives the real API router over
// real Postgres: a platform-admin key of tenant A finds tenant B by slug,
// mints an agent key in B through /tenants/{id}/api-keys, and that key
// authenticates as tenant B (and sees only B's data). A tenant admin gets
// 403 on the same route; an unknown tenant is 404.
func TestPlatformAdminMintsKeyInNamedTenant(t *testing.T) {
	rawURL := os.Getenv("GATEWAY_TEST_DATABASE_URL")
	if rawURL == "" {
		t.Skip("GATEWAY_TEST_DATABASE_URL not set; skipping the platform tenant-keys end-to-end test")
	}
	ctx := context.Background()
	st, _ := newIsolatedStoreDSN(t, rawURL)

	mk := func(slug string) *store.Tenant {
		tn := &store.Tenant{Slug: slug + "-" + uuid.NewString()[:8], Name: slug}
		if err := st.Tenants().Create(ctx, tn); err != nil {
			t.Fatalf("create tenant: %v", err)
		}
		return tn
	}
	home, target := mk("home"), mk("target")
	platformKey := newAPIKey(t, st, home.ID, "platform", pkgauth.RolePlatformAdmin)
	homeAdminKey := newAPIKey(t, st, home.ID, "home-admin", "admin")

	authenticator := apikey.New(st.APIKeys(), apikey.Options{})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	base := serve(t, api.NewRouter(api.Deps{
		ServiceVersion: "e2e",
		Authenticator:  authenticator,
		Authorizer:     pkgauth.NewRoleAuthorizer(),
		Logger:         logger,
		Store:          st,
		// As cmd/gateway wires it: revoke/rotate evict the running
		// authenticator's cached lookup immediately.
		KeyInvalidator: authenticator,
	}))

	call := func(key, method, path, body string) (int, []byte) {
		t.Helper()
		req, _ := http.NewRequest(method, base+"/api/v1"+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}

	// Look the tenant up by slug.
	code, body := call(platformKey, http.MethodGet, "/tenants?slug="+target.Slug, "")
	var found struct {
		Items []struct{ ID string } `json:"items"`
	}
	if err := json.Unmarshal(body, &found); code != 200 || err != nil || len(found.Items) != 1 || found.Items[0].ID != target.ID {
		t.Fatalf("GET /tenants?slug = %d %s (%v)", code, body, err)
	}

	// An empty slug filter is an error, not "every tenant".
	if code, _ := call(platformKey, http.MethodGet, "/tenants?slug=", ""); code != http.StatusBadRequest {
		t.Errorf("GET /tenants?slug= = %d, want 400", code)
	}

	// Mint.
	path := "/tenants/" + target.ID + "/api-keys"
	code, body = call(platformKey, http.MethodPost, path, `{"name":"provisioned","role":"agent","limits":{"rpm":10}}`)
	var minted struct {
		ID, Key, Role string
		Limits        struct{ RPM *int }
	}
	if err := json.Unmarshal(body, &minted); code != http.StatusCreated || err != nil || minted.Key == "" || minted.Role != "agent" || minted.Limits.RPM == nil || *minted.Limits.RPM != 10 {
		t.Fatalf("POST %s = %d %s (%v)", path, code, body, err)
	}

	// The new key belongs to the target tenant, created by the platform key.
	p, err := authenticator.Authenticate(ctx, mustReq(t, minted.Key))
	if err != nil || p.TenantID != target.ID {
		t.Fatalf("minted key authenticates as %+v (%v), want tenant %s", p, err, target.ID)
	}
	if keys, _ := st.APIKeys().List(ctx, target.ID); len(keys) != 1 || keys[0].CreatedBy == "" {
		t.Errorf("target tenant keys = %+v, want exactly the minted one with created_by set", keys)
	}
	for _, k := range mustList(t, st, home.ID) {
		if k.ID == minted.ID {
			t.Error("minted key listed under the caller's own tenant")
		}
	}

	// Listing through the platform route shows it; the plaintext is not repeated.
	code, body = call(platformKey, http.MethodGet, path, "")
	if code != 200 || !strings.Contains(string(body), minted.ID) || strings.Contains(string(body), minted.Key) {
		t.Errorf("GET %s = %d %s", path, code, body)
	}

	// Tenant admins are refused, for the other tenant and for their own.
	for _, tid := range []string{target.ID, home.ID} {
		if code, _ := call(homeAdminKey, http.MethodPost, "/tenants/"+tid+"/api-keys", `{"name":"x","role":"agent"}`); code != http.StatusForbidden {
			t.Errorf("tenant admin POST /tenants/%s/api-keys = %d, want 403", tid, code)
		}
		if code, _ := call(homeAdminKey, http.MethodGet, "/tenants/"+tid+"/api-keys", ""); code != http.StatusForbidden {
			t.Errorf("tenant admin GET /tenants/%s/api-keys = %d, want 403", tid, code)
		}
	}
	// The minted agent key is refused too.
	if code, _ := call(minted.Key, http.MethodGet, path, ""); code != http.StatusForbidden {
		t.Errorf("agent key GET %s = %d, want 403", path, code)
	}

	// Unknown tenant.
	if code, _ := call(platformKey, http.MethodPost, "/tenants/"+uuid.NewString()+"/api-keys", `{"name":"x","role":"agent"}`); code != http.StatusNotFound {
		t.Errorf("unknown tenant = %d, want 404", code)
	}

	// Change the key's limits through the platform route; a request naming
	// the caller's own tenant cannot reach it.
	if code, body := call(platformKey, http.MethodPatch, path+"/"+minted.ID, `{"limits":{"rpm":99}}`); code != http.StatusOK || !strings.Contains(string(body), `"rpm":99`) {
		t.Errorf("PATCH = %d %s", code, body)
	}
	if code, _ := call(platformKey, http.MethodPatch, "/tenants/"+home.ID+"/api-keys/"+minted.ID, `{"limits":{"rpm":1}}`); code != http.StatusNotFound {
		t.Errorf("PATCH via the wrong tenant = %d, want 404", code)
	}
	if code, _ := call(homeAdminKey, http.MethodPatch, path+"/"+minted.ID, `{"limits":{"rpm":1}}`); code != http.StatusForbidden {
		t.Errorf("tenant admin PATCH = %d, want 403", code)
	}

	// Rotate, then revoke; the old and revoked keys stop authenticating.
	code, body = call(platformKey, http.MethodPost, path+"/"+minted.ID+"/rotate", "")
	var rotated struct{ ID, Key string }
	if err := json.Unmarshal(body, &rotated); code != 200 || err != nil || rotated.Key == "" {
		t.Fatalf("rotate = %d %s", code, body)
	}
	if _, err := authenticator.Authenticate(ctx, mustReq(t, minted.Key)); err == nil {
		t.Error("rotated-out key still authenticates")
	}
	if code, _ := call(platformKey, http.MethodDelete, path+"/"+rotated.ID, ""); code != http.StatusNoContent {
		t.Errorf("revoke = %d, want 204", code)
	}
	if _, err := authenticator.Authenticate(ctx, mustReq(t, rotated.Key)); err == nil {
		t.Error("revoked key still authenticates")
	}
}

func mustReq(t *testing.T, key string) *http.Request {
	t.Helper()
	r, _ := http.NewRequest(http.MethodGet, "http://x/", nil)
	r.Header.Set("Authorization", "Bearer "+key)
	return r
}

func mustList(t *testing.T, st store.Store, tenantID string) []*store.APIKey {
	t.Helper()
	keys, err := st.APIKeys().List(context.Background(), tenantID)
	if err != nil {
		t.Fatal(err)
	}
	return keys
}
