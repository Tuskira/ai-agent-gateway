//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	dpheaders "github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/headers"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/secrets"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// lockAfterRead is the real Postgres store, except that the first read of
// an armed credential or source returns the row as it was and then locks it
// in Postgres, the way a platform admin's lock lands between a tenant
// handler's own check and its write. It makes the lost-update window
// deterministic.
type lockAfterRead struct {
	store.Store
	mu    sync.Mutex
	creds map[string]bool // armed credential names
	srcs  map[string]bool // armed source ids
	// onLock, when set, runs right after a source is locked through
	// SetLocked: it sees the state other readers see at that moment.
	onLock func(tenantID, id string)
}

func (s *lockAfterRead) armCredential(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.creds[name] = true
}

func (s *lockAfterRead) armSource(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.srcs[id] = true
}

func (s *lockAfterRead) take(m map[string]bool, k string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m[k] {
		delete(m, k)
		return true
	}
	return false
}

type lockAfterReadCreds struct {
	store.CredentialStore
	s *lockAfterRead
}

func (c lockAfterReadCreds) Get(ctx context.Context, tenantID, name string) (*store.Credential, error) {
	row, err := c.CredentialStore.Get(ctx, tenantID, name)
	if err == nil && c.s.take(c.s.creds, name) {
		if lerr := c.CredentialStore.SetLocked(ctx, tenantID, name, true); lerr != nil {
			return nil, lerr
		}
	}
	return row, err
}

type lockAfterReadSources struct {
	store.CredentialSourceStore
	s *lockAfterRead
}

func (c lockAfterReadSources) Get(ctx context.Context, tenantID, id string) (*store.CredentialSource, error) {
	row, err := c.CredentialSourceStore.Get(ctx, tenantID, id)
	if err == nil && c.s.take(c.s.srcs, id) {
		if lerr := c.CredentialSourceStore.SetLocked(ctx, tenantID, id, true); lerr != nil {
			return nil, lerr
		}
	}
	return row, err
}

func (c lockAfterReadSources) SetLocked(ctx context.Context, tenantID, id string, locked bool) error {
	if err := c.CredentialSourceStore.SetLocked(ctx, tenantID, id, locked); err != nil {
		return err
	}
	c.s.mu.Lock()
	hook := c.s.onLock
	c.s.mu.Unlock()
	if locked && hook != nil {
		hook(tenantID, id)
	}
	return nil
}

func (s *lockAfterRead) Credentials() store.CredentialStore {
	return lockAfterReadCreds{CredentialStore: s.Store.Credentials(), s: s}
}

func (s *lockAfterRead) CredentialSources() store.CredentialSourceStore {
	return lockAfterReadSources{CredentialSourceStore: s.Store.CredentialSources(), s: s}
}

// TestPlatformLockRaceEndToEnd: a platform lock that lands after a tenant
// handler checked the row but before it wrote is never bypassed (rotate,
// delete, source update, source delete) and never cleared (source update
// used to write the stale locked=false back).
func TestPlatformLockRaceEndToEnd(t *testing.T) {
	h, secretSvc := newFixture(t)
	ctx := context.Background()
	st := &lockAfterRead{Store: h.store, creds: map[string]bool{}, srcs: map[string]bool{}}

	cs := newCredService(t)
	registry := dpheaders.NewRegistry()
	if err := registry.RegisterExternal(secrets.NewSecretStoreProvider(secretSvc)); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterExternal(secrets.NewExternalServiceProvider(secretSvc.Fetcher(), 5*time.Second, time.Minute)); err != nil {
		t.Fatal(err)
	}
	registry.SetSourceLookup(dpheaders.StoreSourceLookup(h.store.CredentialSources()))
	_, plane := startPlaneWithRegistry(t, h.store, registry, nil)
	h.apiBaseURL = "http://" + startSourcesAPIPlane(t, st, plane, secretSvc, registry)
	tenantKey := h.adminKey
	platformKey := newAPIKey(t, h.store, h.tenant.ID, "e2e-platform-race", pkgauth.RolePlatformAdmin)

	doAs := func(key, method, path string, body any) (int, string) {
		t.Helper()
		var rdr io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rdr = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, h.apiBaseURL+path, rdr)
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
	do := func(method, path string, body any) (int, string) {
		t.Helper()
		return doAs(tenantKey, method, path, body)
	}
	allowed := store.WithLockedAccess(ctx, true)

	newCred := func(t *testing.T, name string) {
		t.Helper()
		if code, raw := do(http.MethodPost, "/api/v1/credentials", map[string]any{"name": name, "type": "generic", "payload": map[string]string{"token": "orig"}}); code != http.StatusCreated {
			t.Fatalf("create %s = %d %s", name, code, raw)
		}
	}
	newSource := func(t *testing.T, name string) string {
		t.Helper()
		code, raw := do(http.MethodPost, "/api/v1/credential-sources", map[string]any{
			"name": name, "provider": "external_service", "config": map[string]any{"url": cs.URL, "auth_credential": "none"},
		})
		if code != http.StatusCreated {
			t.Fatalf("create source %s = %d %s", name, code, raw)
		}
		var v struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal([]byte(raw), &v)
		return v.ID
	}
	forbidden := func(t *testing.T, what string, code int, raw string) {
		t.Helper()
		if code != http.StatusForbidden || !strings.Contains(raw, "managed by the platform") {
			t.Fatalf("%s = %d %s, want 403 managed by the platform", what, code, raw)
		}
	}

	t.Run("rotate", func(t *testing.T) {
		newCred(t, "race-rotate")
		st.armCredential("race-rotate")
		code, raw := do(http.MethodPut, "/api/v1/credentials/race-rotate", map[string]any{"payload": map[string]string{"token": "evil"}})
		forbidden(t, "tenant rotate racing a lock", code, raw)
		if v, err := secretSvc.GetField(allowed, h.tenant.ID, "race-rotate", "token"); err != nil || v != "orig" {
			t.Fatalf("payload after the race = %q, %v; want orig", v, err)
		}
	})

	t.Run("delete", func(t *testing.T) {
		newCred(t, "race-delete")
		st.armCredential("race-delete")
		code, raw := do(http.MethodDelete, "/api/v1/credentials/race-delete", nil)
		forbidden(t, "tenant delete racing a lock", code, raw)
		if c, err := h.store.Credentials().Get(ctx, h.tenant.ID, "race-delete"); err != nil || !c.Locked {
			t.Fatalf("credential after the race = %+v, %v; want present and locked", c, err)
		}
	})

	t.Run("source update", func(t *testing.T) {
		id := newSource(t, "race-src-update")
		st.armSource(id)
		code, raw := do(http.MethodPut, "/api/v1/credential-sources/"+id, map[string]any{"enabled": false, "config": map[string]any{"url": "https://evil.example.com", "auth_credential": "none"}})
		forbidden(t, "tenant source update racing a lock", code, raw)
		got, err := h.store.CredentialSources().Get(ctx, h.tenant.ID, id)
		if err != nil || !got.Locked || !got.Enabled || got.Config["url"] != cs.URL {
			t.Fatalf("source after the race = %+v, %v; want locked, enabled, url unchanged", got, err)
		}
	})

	t.Run("source delete", func(t *testing.T) {
		id := newSource(t, "race-src-delete")
		st.armSource(id)
		code, raw := do(http.MethodDelete, "/api/v1/credential-sources/"+id, nil)
		forbidden(t, "tenant source delete racing a lock", code, raw)
		if got, err := h.store.CredentialSources().Get(ctx, h.tenant.ID, id); err != nil || !got.Locked {
			t.Fatalf("source after the race = %+v, %v; want present and locked", got, err)
		}
	})

	// A platform takeover of a tenant's source must never leave it locked
	// with the tenant's config, not even for an instant: a locked source
	// may authenticate with a locked credential, so its token would go to
	// the tenant's URL.
	t.Run("source takeover is atomic", func(t *testing.T) {
		tenantSvc := newCredService(t) // the tenant's own endpoint
		code, raw := do(http.MethodPost, "/api/v1/credential-sources", map[string]any{
			"name": "race-takeover", "provider": "external_service", "config": map[string]any{"url": tenantSvc.URL, "auth_credential": "future-auth"},
		})
		if code != http.StatusCreated {
			t.Fatalf("tenant source = %d %s", code, raw)
		}
		var src struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal([]byte(raw), &src)
		if code, raw := doAs(platformKey, http.MethodPost, "/api/v1/credentials", map[string]any{
			"name": "future-auth", "type": "generic", "payload": map[string]string{"token": "plat-token"}, "locked": true,
		}); code != http.StatusCreated {
			t.Fatalf("platform credential = %d %s", code, raw)
		}

		var seen []string
		st.mu.Lock()
		st.onLock = func(tenantID, id string) {
			got, err := h.store.CredentialSources().Get(ctx, tenantID, id)
			if err == nil && got.Locked {
				seen = append(seen, got.Config["url"].(string))
			}
		}
		st.mu.Unlock()
		t.Cleanup(func() { st.mu.Lock(); st.onLock = nil; st.mu.Unlock() })

		if code, raw := doAs(platformKey, http.MethodPut, "/api/v1/credential-sources/"+src.ID, map[string]any{"locked": true}); code != http.StatusBadRequest || !strings.Contains(raw, "config") {
			t.Fatalf("lock without config = %d %s, want 400 asking for config", code, raw)
		}
		if code, raw := doAs(platformKey, http.MethodPut, "/api/v1/credential-sources/"+src.ID, map[string]any{
			"locked": true, "config": map[string]any{"url": cs.URL, "auth_credential": "future-auth"},
		}); code != http.StatusOK || !strings.Contains(raw, `"locked":true`) {
			t.Fatalf("platform takeover = %d %s", code, raw)
		}
		for _, u := range seen {
			if u == tenantSvc.URL {
				t.Fatalf("the source was locked while still pointing at the tenant's URL")
			}
		}
		got, err := h.store.CredentialSources().Get(ctx, h.tenant.ID, src.ID)
		if err != nil || !got.Locked || got.Config["url"] != cs.URL {
			t.Fatalf("source after takeover = %+v, %v; want locked with the platform's url", got, err)
		}
		if n := tenantSvc.resolveCount(""); n != 0 {
			t.Fatalf("the tenant's endpoint got %d resolve calls", n)
		}
	})
}
