package handlers

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/api/httpx"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/auth/apikey"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// APIKeys implements POST/GET /api/v1/api-keys, PATCH and DELETE
// /api/v1/api-keys/{id}, and POST /api/v1/api-keys/{id}/rotate.
//
// Every route here is mounted behind permission "admin.manage" (see
// router.go), not "apikey.*": the built-in "agent" role's "*.read" grant
// would otherwise satisfy an "apikey.read"-style permission and let an
// agent key list every key (including admin keys) for its tenant.
// "admin.manage" only matches the "admin" role's "*" grant, so api-key
// management stays admin-only regardless of what future apikey.* naming
// gets added.
//
// Revocation: revoking or rotating a key updates the store immediately
// (store.APIKeyStore.Revoke) and, when Deps.KeyInvalidator is non-nil
// (wired in cmd/gateway/main.go from the same *apikey.Authenticator
// instance the auth chain runs), evicts that key's cached lookup from
// the running Authenticator in the same request -- so a revoked/
// rotated-out key stops authenticating immediately rather than surviving
// up to its CacheTTL (default 60s, see
// internal/auth/apikey.Options.CacheTTL). KeyInvalidator is nil (a valid
// no-op) in tests and in any deployment that doesn't wire the api-key
// Authenticator through.
type APIKeys struct{ Deps }

type createAPIKeyRequest struct {
	Name      string        `json:"name"`
	Role      string        `json:"role"`
	ExpiresAt *string       `json:"expires_at,omitempty"`
	Limits    *store.Limits `json:"limits,omitempty"`
	// ProfileID binds the key to an agent profile of the caller's tenant.
	ProfileID *string `json:"profile_id,omitempty"`
}

// updateAPIKeyRequest is the PATCH body. Limits and ProfileID are
// json.RawMessage so an explicit `null` (clear) is distinguishable from an
// absent field.
type updateAPIKeyRequest struct {
	Limits    json.RawMessage `json:"limits"`
	ProfileID json.RawMessage `json:"profile_id"`
}

type apiKeyView struct {
	ID         string        `json:"id"`
	Name       string        `json:"name"`
	Role       string        `json:"role"`
	Prefix     string        `json:"prefix"`
	CreatedAt  string        `json:"created_at"`
	LastUsedAt string        `json:"last_used_at,omitempty"`
	ExpiresAt  string        `json:"expires_at,omitempty"`
	RevokedAt  string        `json:"revoked_at,omitempty"`
	Limits     *store.Limits `json:"limits,omitempty"`
	// ProfileID / ProfileName identify the profile the key is bound to.
	ProfileID   string `json:"profile_id,omitempty"`
	ProfileName string `json:"profile_name,omitempty"`
}

type apiKeyCreateView struct {
	apiKeyView
	Key string `json:"key"`
}

func newAPIKeyView(k *store.APIKey) apiKeyView {
	v := apiKeyView{
		ID:         k.ID,
		Name:       k.Name,
		Role:       k.Role,
		Prefix:     k.KeyPrefix,
		CreatedAt:  formatTime(k.CreatedAt),
		LastUsedAt: formatTimePtr(k.LastUsedAt),
		ExpiresAt:  formatTimePtr(k.ExpiresAt),
		RevokedAt:  formatTimePtr(k.RevokedAt),
		Limits:     k.Limits,
	}
	if k.ProfileID != nil {
		v.ProfileID = *k.ProfileID
	}
	return v
}

// withProfileName fills v.ProfileName from the tenant's profiles (best
// effort: a lookup failure just leaves the name empty).
func (h APIKeys) withProfileName(r *http.Request, tenantID string, v apiKeyView) apiKeyView {
	if v.ProfileID == "" {
		return v
	}
	if p, err := h.Store.AgentProfiles().Get(r.Context(), tenantID, v.ProfileID); err == nil {
		v.ProfileName = p.Name
	}
	return v
}

// checkProfile verifies profileID is a live profile of tenantID. It writes
// a 400 and returns false when it is not.
func (h APIKeys) checkProfile(w http.ResponseWriter, r *http.Request, tenantID, profileID string) bool {
	if _, err := h.Store.AgentProfiles().Get(r.Context(), tenantID, profileID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			httpx.ValidationError(w, "profile_id must be the id of a profile in this tenant")
			return false
		}
		writeStoreErr(w, r, "look up profile", err)
		return false
	}
	return true
}

// checkPlatformRole refuses (403) a request that would mint a key holding
// platform permissions unless the caller holds platform.admin itself.
func (h APIKeys) checkPlatformRole(w http.ResponseWriter, r *http.Request, role string) bool {
	if ra, ok := h.Authorizer.(*pkgauth.RoleAuthorizer); ok {
		if !ra.RoleGrantsPlatform(role) {
			return true
		}
	} else if role != pkgauth.RolePlatformAdmin {
		return true // nil or custom Authorizer: only the built-in name is special
	}
	p, _ := pkgauth.PrincipalFrom(r.Context())
	if h.Authorizer != nil && h.Authorizer.Allow(r.Context(), p, pkgauth.PermPlatformAdmin) {
		return true
	}
	httpx.WriteError(w, http.StatusForbidden, httpx.TypePermission,
		"creating a key with role "+role+" requires platform.admin")
	return false
}

// maxRPM bounds limits.rpm: the LLM plane's sliding window keeps one
// timestamp per admitted request per key, so the bound caps its memory.
const maxRPM = 100000

// validateLimits checks a limits object (an API key's or a model's): at
// least one field set, every
// set field non-negative (0 is a real limit: it blocks), rpm <= maxRPM.
func validateLimits(l *store.Limits) error {
	if l.Empty() {
		return errors.New("limits must set at least one of daily_usd, monthly_usd, rpm, max_tokens")
	}
	for name, v := range map[string]*float64{"daily_usd": l.DailyUSD, "monthly_usd": l.MonthlyUSD} {
		if v != nil && (*v < 0 || math.IsNaN(*v) || math.IsInf(*v, 0)) {
			return fmt.Errorf("limits.%s must be a non-negative number", name)
		}
	}
	for name, v := range map[string]*int{"rpm": l.RPM, "max_tokens": l.MaxTokens} {
		if v != nil && *v < 0 {
			return fmt.Errorf("limits.%s must be a non-negative integer", name)
		}
	}
	if l.RPM != nil && *l.RPM > maxRPM {
		return fmt.Errorf("limits.rpm must be at most %d", maxRPM)
	}
	return nil
}

// Create handles POST /api/v1/api-keys.
func (h APIKeys) Create(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}

	var req createAPIKeyRequest
	if !httpx.Decode(w, r, &req) {
		return
	}
	if err := validateName("name", req.Name); err != nil {
		httpx.ValidationError(w, err.Error())
		return
	}
	if err := validateRole(req.Role, h.AllowedRoles); err != nil {
		httpx.ValidationError(w, err.Error())
		return
	}
	if !h.checkPlatformRole(w, r, req.Role) {
		return
	}
	if req.Limits != nil {
		if err := validateLimits(req.Limits); err != nil {
			httpx.ValidationError(w, err.Error())
			return
		}
	}
	if req.ProfileID != nil && !h.checkProfile(w, r, tid, *req.ProfileID) {
		return
	}

	var expiresAt *time.Time
	if req.ExpiresAt != nil && *req.ExpiresAt != "" {
		t, err := time.Parse(time.RFC3339, *req.ExpiresAt)
		if err != nil {
			// Fall back to a bare YYYY-MM-DD date (what the console sends
			// today, independent of the frontend fix to send full
			// RFC3339): treat it as the end of that day in UTC so a key
			// created with expires_at=2026-01-15 stays valid through all
			// of that day.
			var dateErr error
			t, dateErr = time.Parse("2006-01-02", *req.ExpiresAt)
			if dateErr != nil {
				httpx.ValidationError(w, "expires_at must be an RFC3339 timestamp or a YYYY-MM-DD date")
				return
			}
			t = time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 59, 0, time.UTC)
		}
		if !t.After(time.Now().UTC()) {
			httpx.ValidationError(w, "expires_at must be in the future")
			return
		}
		expiresAt = &t
	}

	plaintext, hash, prefix, err := apikey.Generate()
	if err != nil {
		writeStoreErr(w, r, "generate api key", err)
		return
	}

	k := &store.APIKey{
		TenantID:  tid,
		Name:      req.Name,
		Role:      req.Role,
		KeyHash:   hash,
		KeyPrefix: prefix,
		ExpiresAt: expiresAt,
		Limits:    req.Limits,
		ProfileID: req.ProfileID,
		CreatedBy: principalSubject(r),
	}
	if err := h.Store.APIKeys().Create(r.Context(), k); err != nil {
		writeStoreErr(w, r, "create api key", err)
		return
	}

	httpx.WriteJSON(w, http.StatusCreated, apiKeyCreateView{apiKeyView: h.withProfileName(r, tid, newAPIKeyView(k)), Key: plaintext})
}

// List handles GET /api/v1/api-keys.
func (h APIKeys) List(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}

	keys, err := h.Store.APIKeys().List(r.Context(), tid)
	if err != nil {
		writeStoreErr(w, r, "list api keys", err)
		return
	}

	page := httpx.ParsePagination(r)
	views := make([]apiKeyView, 0, len(keys))
	for _, k := range keys {
		views = append(views, h.withProfileName(r, tid, newAPIKeyView(k)))
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page{Items: paginate(views, page), Total: len(views)})
}

// Update handles PATCH /api/v1/api-keys/{id}. Today the only mutable field
// is limits: an object replaces the key's limits wholesale, null clears
// them. The LLM plane re-reads a key's limits at most every 10 s (see
// internal/llmplane/limits.go), so a change takes effect within that.
func (h APIKeys) Update(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")

	var req updateAPIKeyRequest
	if !httpx.Decode(w, r, &req) {
		return
	}
	if len(req.Limits) == 0 && len(req.ProfileID) == 0 {
		httpx.ValidationError(w, "limits or profile_id is required (a value to set, or null to clear)")
		return
	}
	var limits *store.Limits
	if len(req.Limits) != 0 && string(req.Limits) != "null" {
		dec := json.NewDecoder(bytes.NewReader(req.Limits))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&limits); err != nil {
			httpx.ValidationError(w, "invalid limits: "+err.Error())
			return
		}
		if err := validateLimits(limits); err != nil {
			httpx.ValidationError(w, err.Error())
			return
		}
	}

	var profileID *string
	if len(req.ProfileID) != 0 && string(req.ProfileID) != "null" {
		if err := json.Unmarshal(req.ProfileID, &profileID); err != nil || profileID == nil {
			httpx.ValidationError(w, "profile_id must be a string (the profile id) or null")
			return
		}
		if !h.checkProfile(w, r, tid, *profileID) {
			return
		}
	}

	if len(req.Limits) != 0 {
		if err := h.Store.APIKeys().SetLimits(r.Context(), tid, id, limits); err != nil {
			writeStoreErr(w, r, "update api key", err)
			return
		}
	}
	if len(req.ProfileID) != 0 {
		if err := h.Store.APIKeys().SetProfile(r.Context(), tid, id, profileID); err != nil {
			writeStoreErr(w, r, "update api key", err)
			return
		}
		// The data plane resolves a key's profile from its cached row.
		if existing, err := h.Store.APIKeys().GetByID(r.Context(), tid, id); err == nil && h.KeyInvalidator != nil {
			h.KeyInvalidator.InvalidateKey(existing.KeyHash)
		}
	}
	k, err := h.Store.APIKeys().GetByID(r.Context(), tid, id)
	if err != nil {
		writeStoreErr(w, r, "update api key", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, h.withProfileName(r, tid, newAPIKeyView(k)))
}

// Revoke handles DELETE /api/v1/api-keys/{id}.
func (h APIKeys) Revoke(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")

	// Fetched before the write so its KeyHash is available to invalidate
	// the Authenticator's cache below; also doubles as the tenant-scoped
	// existence check (Revoke alone would 404 identically, but this way
	// there's exactly one lookup).
	existing, err := h.Store.APIKeys().GetByID(r.Context(), tid, id)
	if err != nil {
		writeStoreErr(w, r, "revoke api key", err)
		return
	}

	if err := h.Store.APIKeys().Revoke(r.Context(), tid, id); err != nil {
		writeStoreErr(w, r, "revoke api key", err)
		return
	}
	if h.KeyInvalidator != nil {
		h.KeyInvalidator.InvalidateKey(existing.KeyHash)
	}
	w.WriteHeader(http.StatusNoContent)
}

// Rotate handles POST /api/v1/api-keys/{id}/rotate: revoke the named key
// and create a new one with the same name/role, returning its plaintext
// once.
func (h APIKeys) Rotate(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")

	existing, err := h.Store.APIKeys().GetByID(r.Context(), tid, id)
	if err != nil {
		writeStoreErr(w, r, "rotate api key", err)
		return
	}

	if !h.checkPlatformRole(w, r, existing.Role) {
		return
	}

	if err := h.Store.APIKeys().Revoke(r.Context(), tid, id); err != nil {
		writeStoreErr(w, r, "rotate api key (revoke old)", err)
		return
	}
	if h.KeyInvalidator != nil {
		h.KeyInvalidator.InvalidateKey(existing.KeyHash)
	}

	plaintext, hash, prefix, err := apikey.Generate()
	if err != nil {
		writeStoreErr(w, r, "rotate api key (generate new)", err)
		return
	}

	k := &store.APIKey{
		TenantID:  tid,
		Name:      existing.Name,
		Role:      existing.Role,
		KeyHash:   hash,
		KeyPrefix: prefix,
		// Limits carry over; spend is tracked per key id, so the new key
		// starts its day/month at $0.
		Limits:    existing.Limits,
		ProfileID: existing.ProfileID,
		CreatedBy: principalSubject(r),
	}
	if err := h.Store.APIKeys().Create(r.Context(), k); err != nil {
		writeStoreErr(w, r, "rotate api key (create new)", err)
		return
	}

	httpx.WriteJSON(w, http.StatusOK, apiKeyCreateView{apiKeyView: h.withProfileName(r, tid, newAPIKeyView(k)), Key: plaintext})
}
