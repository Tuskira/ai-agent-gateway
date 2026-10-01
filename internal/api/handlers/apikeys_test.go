package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/auth/apikey"
)

func TestAPIKeys_Create_HappyPath_ReturnsPlaintextOnceAndNeverHash(t *testing.T) {
	h := APIKeys{Deps: newTestDeps()}

	req := withPrincipal(httptest.NewRequest(http.MethodPost, "/api-keys", strings.NewReader(`{"name":"ci-bot","role":"agent"}`)), "tenant-a", "admin")
	w := serve(http.MethodPost, "/api-keys", h.Create, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "key_hash") || strings.Contains(w.Body.String(), "KeyHash") {
		t.Fatalf("response leaked key_hash: %s", w.Body.String())
	}

	var got apiKeyCreateView
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.HasPrefix(got.Key, apikey.Prefix) {
		t.Errorf("Key = %q, want prefix %q", got.Key, apikey.Prefix)
	}
	if got.Role != "agent" || got.Name != "ci-bot" || got.Prefix == "" {
		t.Errorf("got = %+v", got)
	}
	if apikey.Hash(got.Key) == "" {
		t.Fatal("sanity: Hash(plaintext) is empty")
	}
}

func TestAPIKeys_Create_ValidationError_BadRole(t *testing.T) {
	h := APIKeys{Deps: newTestDeps()}

	req := withPrincipal(httptest.NewRequest(http.MethodPost, "/api-keys", strings.NewReader(`{"name":"x","role":"superuser"}`)), "tenant-a", "admin")
	w := serve(http.MethodPost, "/api-keys", h.Create, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", w.Code, w.Body.String())
	}
	assertErrorType(t, w, "validation_error")
}

// TestAPIKeys_Create_ExpiresAt_RFC3339_Succeeds pins the primary format:
// a full RFC3339 timestamp is accepted as-is.
func TestAPIKeys_Create_ExpiresAt_RFC3339_Succeeds(t *testing.T) {
	h := APIKeys{Deps: newTestDeps()}

	req := withPrincipal(httptest.NewRequest(http.MethodPost, "/api-keys", strings.NewReader(`{"name":"ci-bot","role":"agent","expires_at":"2099-01-01T00:00:00Z"}`)), "tenant-a", "admin")
	w := serve(http.MethodPost, "/api-keys", h.Create, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var got apiKeyCreateView
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ExpiresAt != "2099-01-01T00:00:00Z" {
		t.Errorf("ExpiresAt = %q, want %q", got.ExpiresAt, "2099-01-01T00:00:00Z")
	}
}

// TestAPIKeys_Create_ExpiresAt_BareDate_EndOfDayUTC pins the fallback
// format the console sends today: a bare YYYY-MM-DD date must be treated
// as the end of that day in UTC, not midnight (which would expire the key
// before its creator's day was even over).
func TestAPIKeys_Create_ExpiresAt_BareDate_EndOfDayUTC(t *testing.T) {
	h := APIKeys{Deps: newTestDeps()}

	req := withPrincipal(httptest.NewRequest(http.MethodPost, "/api-keys", strings.NewReader(`{"name":"ci-bot","role":"agent","expires_at":"2099-06-15"}`)), "tenant-a", "admin")
	w := serve(http.MethodPost, "/api-keys", h.Create, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var got apiKeyCreateView
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ExpiresAt != "2099-06-15T23:59:59Z" {
		t.Errorf("ExpiresAt = %q, want %q (bare date must mean end-of-day UTC)", got.ExpiresAt, "2099-06-15T23:59:59Z")
	}
}

// TestAPIKeys_Create_ExpiresAt_Unparseable_ValidationError proves a
// string that is neither RFC3339 nor a bare YYYY-MM-DD date is rejected.
func TestAPIKeys_Create_ExpiresAt_Unparseable_ValidationError(t *testing.T) {
	h := APIKeys{Deps: newTestDeps()}

	req := withPrincipal(httptest.NewRequest(http.MethodPost, "/api-keys", strings.NewReader(`{"name":"ci-bot","role":"agent","expires_at":"not-a-date"}`)), "tenant-a", "admin")
	w := serve(http.MethodPost, "/api-keys", h.Create, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", w.Code, w.Body.String())
	}
	assertErrorType(t, w, "validation_error")
}

// TestAPIKeys_Create_ExpiresAt_InThePast_ValidationError proves a
// well-formed but already-elapsed expires_at is rejected rather than
// silently creating an already-expired key.
func TestAPIKeys_Create_ExpiresAt_InThePast_ValidationError(t *testing.T) {
	h := APIKeys{Deps: newTestDeps()}

	req := withPrincipal(httptest.NewRequest(http.MethodPost, "/api-keys", strings.NewReader(`{"name":"ci-bot","role":"agent","expires_at":"2020-01-01T00:00:00Z"}`)), "tenant-a", "admin")
	w := serve(http.MethodPost, "/api-keys", h.Create, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", w.Code, w.Body.String())
	}
	assertErrorType(t, w, "validation_error")
}

func TestAPIKeys_List_TenantIsolation(t *testing.T) {
	deps := newTestDeps()
	h := APIKeys{Deps: deps}

	create := func(tenant string) {
		req := withPrincipal(httptest.NewRequest(http.MethodPost, "/api-keys", strings.NewReader(`{"name":"k","role":"agent"}`)), tenant, "admin")
		if w := serve(http.MethodPost, "/api-keys", h.Create, req); w.Code != http.StatusCreated {
			t.Fatalf("seed create for %q: status %d", tenant, w.Code)
		}
	}
	create("tenant-a")
	create("tenant-a")
	create("tenant-b")

	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/api-keys", nil), "tenant-a", "admin")
	w := serve(http.MethodGet, "/api-keys", h.List, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var page struct {
		Items []apiKeyView `json:"items"`
		Total int          `json:"total"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if page.Total != 2 {
		t.Errorf("tenant-a sees %d keys, want 2 (tenant isolation broken)", page.Total)
	}
}

func TestAPIKeys_Revoke_NotFoundAcrossTenants(t *testing.T) {
	deps := newTestDeps()
	h := APIKeys{Deps: deps}

	createReq := withPrincipal(httptest.NewRequest(http.MethodPost, "/api-keys", strings.NewReader(`{"name":"k","role":"agent"}`)), "tenant-a", "admin")
	w := serve(http.MethodPost, "/api-keys", h.Create, createReq)
	var created apiKeyCreateView
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}

	// tenant-b must not be able to revoke tenant-a's key.
	revokeReq := withPrincipal(httptest.NewRequest(http.MethodDelete, "/api-keys/"+created.ID, nil), "tenant-b", "admin")
	w = serve(http.MethodDelete, "/api-keys/{id}", h.Revoke, revokeReq)
	if w.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant revoke status = %d, want 404 (body=%s)", w.Code, w.Body.String())
	}

	// tenant-a can revoke its own key.
	revokeReq = withPrincipal(httptest.NewRequest(http.MethodDelete, "/api-keys/"+created.ID, nil), "tenant-a", "admin")
	w = serve(http.MethodDelete, "/api-keys/{id}", h.Revoke, revokeReq)
	if w.Code != http.StatusNoContent {
		t.Fatalf("own-tenant revoke status = %d, want 204", w.Code)
	}
}

func TestAPIKeys_Rotate_RevokesOldAndReturnsNewPlaintext(t *testing.T) {
	deps := newTestDeps()
	h := APIKeys{Deps: deps}

	createReq := withPrincipal(httptest.NewRequest(http.MethodPost, "/api-keys", strings.NewReader(`{"name":"ci-bot","role":"agent"}`)), "tenant-a", "admin")
	w := serve(http.MethodPost, "/api-keys", h.Create, createReq)
	var original apiKeyCreateView
	if err := json.Unmarshal(w.Body.Bytes(), &original); err != nil {
		t.Fatalf("decode create: %v", err)
	}

	rotateReq := withPrincipal(httptest.NewRequest(http.MethodPost, "/api-keys/"+original.ID+"/rotate", nil), "tenant-a", "admin")
	w = serve(http.MethodPost, "/api-keys/{id}/rotate", h.Rotate, rotateReq)
	if w.Code != http.StatusOK {
		t.Fatalf("rotate status = %d, body = %s", w.Code, w.Body.String())
	}
	var rotated apiKeyCreateView
	if err := json.Unmarshal(w.Body.Bytes(), &rotated); err != nil {
		t.Fatalf("decode rotate: %v", err)
	}
	if rotated.Name != "ci-bot" || rotated.Role != "agent" {
		t.Errorf("rotated = %+v, want same name/role as original", rotated)
	}
	if rotated.Key == original.Key || rotated.ID == original.ID {
		t.Error("rotate did not produce a new key/id")
	}

	// The old key id is now revoked.
	listReq := withPrincipal(httptest.NewRequest(http.MethodGet, "/api-keys", nil), "tenant-a", "admin")
	w = serve(http.MethodGet, "/api-keys", h.List, listReq)
	var page struct {
		Items []apiKeyView `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	var oldRevoked bool
	for _, k := range page.Items {
		if k.ID == original.ID {
			oldRevoked = k.RevokedAt != ""
		}
	}
	if !oldRevoked {
		t.Error("original key was not marked revoked after rotate")
	}
}

func TestAPIKeys_Create_WithLimits_ShownInCreateAndList(t *testing.T) {
	h := APIKeys{Deps: newTestDeps()}

	body := `{"name":"budgeted","role":"agent","limits":{"daily_usd":5,"monthly_usd":100.5,"rpm":60,"max_tokens":4096}}`
	w := serve(http.MethodPost, "/api-keys", h.Create,
		withPrincipal(httptest.NewRequest(http.MethodPost, "/api-keys", strings.NewReader(body)), "tenant-a", "admin"))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"limits":{"daily_usd":5,"monthly_usd":100.5,"rpm":60,"max_tokens":4096}`) {
		t.Errorf("create response limits missing/wrong: %s", w.Body.String())
	}

	w = serve(http.MethodGet, "/api-keys", h.List,
		withPrincipal(httptest.NewRequest(http.MethodGet, "/api-keys", nil), "tenant-a", "admin"))
	if !strings.Contains(w.Body.String(), `"limits":{"daily_usd":5,"monthly_usd":100.5,"rpm":60,"max_tokens":4096}`) {
		t.Errorf("list limits missing/wrong: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "key_hash") {
		t.Fatalf("list leaked key_hash: %s", w.Body.String())
	}
}

func TestAPIKeys_Create_WithoutLimits_OmitsField(t *testing.T) {
	h := APIKeys{Deps: newTestDeps()}
	w := serve(http.MethodPost, "/api-keys", h.Create,
		withPrincipal(httptest.NewRequest(http.MethodPost, "/api-keys", strings.NewReader(`{"name":"k","role":"agent"}`)), "tenant-a", "admin"))
	if w.Code != http.StatusCreated || strings.Contains(w.Body.String(), "limits") {
		t.Fatalf("status = %d, body = %s; want 201 with no limits field", w.Code, w.Body.String())
	}
}

func TestAPIKeys_Create_InvalidLimits(t *testing.T) {
	for name, limits := range map[string]string{
		"empty":            `{}`,
		"negative daily":   `{"daily_usd":-1}`,
		"negative monthly": `{"monthly_usd":-0.01}`,
		"negative rpm":     `{"rpm":-1}`,
		"negative max":     `{"max_tokens":-5}`,
		"rpm too large":    `{"rpm":100001}`,
		"unknown field":    `{"daily":1}`,
		"fractional rpm":   `{"rpm":1.5}`,
	} {
		t.Run(name, func(t *testing.T) {
			h := APIKeys{Deps: newTestDeps()}
			body := `{"name":"k","role":"agent","limits":` + limits + `}`
			w := serve(http.MethodPost, "/api-keys", h.Create,
				withPrincipal(httptest.NewRequest(http.MethodPost, "/api-keys", strings.NewReader(body)), "tenant-a", "admin"))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400, body = %s", w.Code, w.Body.String())
			}
			assertErrorType(t, w, "validation_error")
		})
	}
}

func TestAPIKeys_Update_SetsReplacesAndClearsLimits(t *testing.T) {
	h := APIKeys{Deps: newTestDeps()}
	w := serve(http.MethodPost, "/api-keys", h.Create,
		withPrincipal(httptest.NewRequest(http.MethodPost, "/api-keys", strings.NewReader(`{"name":"k","role":"agent","limits":{"rpm":10}}`)), "tenant-a", "admin"))
	var created apiKeyCreateView
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}

	patch := func(tenant, body string) *httptest.ResponseRecorder {
		req := withPrincipal(httptest.NewRequest(http.MethodPatch, "/api-keys/"+created.ID, strings.NewReader(body)), tenant, "admin")
		return serve(http.MethodPatch, "/api-keys/{id}", h.Update, req)
	}

	// Replace wholesale: rpm is dropped, daily_usd set.
	w = patch("tenant-a", `{"limits":{"daily_usd":0.01}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("patch status = %d, body = %s", w.Code, w.Body.String())
	}
	var got apiKeyView
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode patch: %v", err)
	}
	if got.Limits == nil || got.Limits.DailyUSD == nil || *got.Limits.DailyUSD != 0.01 || got.Limits.RPM != nil {
		t.Fatalf("patched limits = %+v, want daily_usd=0.01 only", got.Limits)
	}
	if strings.Contains(w.Body.String(), `"key"`) || strings.Contains(w.Body.String(), "key_hash") {
		t.Fatalf("patch response leaked key material: %s", w.Body.String())
	}

	// Validation.
	for _, bad := range []string{`{}`, `{"limits":{}}`, `{"limits":{"rpm":-1}}`, `{"limits":{"nope":1}}`, `{"name":"x"}`} {
		if w := patch("tenant-a", bad); w.Code != http.StatusBadRequest {
			t.Errorf("patch %s status = %d, want 400 (body=%s)", bad, w.Code, w.Body.String())
		}
	}

	// Cross-tenant -> 404, and the limits are untouched.
	if w := patch("tenant-b", `{"limits":{"rpm":1}}`); w.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant patch status = %d, want 404", w.Code)
	}

	// Explicit null clears.
	w = patch("tenant-a", `{"limits":null}`)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "limits") {
		t.Fatalf("clear: status = %d, body = %s; want 200 with no limits", w.Code, w.Body.String())
	}
}

func TestAPIKeys_Rotate_CarriesLimits(t *testing.T) {
	h := APIKeys{Deps: newTestDeps()}
	w := serve(http.MethodPost, "/api-keys", h.Create,
		withPrincipal(httptest.NewRequest(http.MethodPost, "/api-keys", strings.NewReader(`{"name":"k","role":"agent","limits":{"monthly_usd":20}}`)), "tenant-a", "admin"))
	var created apiKeyCreateView
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	w = serve(http.MethodPost, "/api-keys/{id}/rotate", h.Rotate,
		withPrincipal(httptest.NewRequest(http.MethodPost, "/api-keys/"+created.ID+"/rotate", nil), "tenant-a", "admin"))
	var rotated apiKeyCreateView
	if err := json.Unmarshal(w.Body.Bytes(), &rotated); err != nil {
		t.Fatalf("decode rotate: %v", err)
	}
	if rotated.Limits == nil || rotated.Limits.MonthlyUSD == nil || *rotated.Limits.MonthlyUSD != 20 {
		t.Fatalf("rotated limits = %+v, want monthly_usd=20 carried over", rotated.Limits)
	}
}
