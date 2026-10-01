package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/api/httpx"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/config"
	applog "github.com/Tuskira/tusk-ai-secured-gateway/internal/log"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/modelcatalog/prices"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/netguard"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// ModelCatalog implements the /api/v1/model-catalog routes: the
// platform-wide catalog of known providers and their models (read by every
// tenant, written only by platform.catalog.manage -- see docs/security-
// model.md), the provider connectivity test, and Connect, which turns a
// tenant's selection of catalog models into real, callable tenant Model
// rows (Models handler) plus a credential (Credentials handler's own
// encrypted store, via the same cipher path -- see secrets.Service.Seal).
type ModelCatalog struct{ Deps }

// maxSuggestedNameLen mirrors config.ReCatalogSuggestedName's length cap.
const maxSuggestedNameLen = 64

// maxCatalogFieldLen bounds model_id/display_name/docs_url -- generous but
// not unbounded, matching the spirit of validateName elsewhere.
const maxCatalogFieldLen = 512

// ---------------------------------------------------------------------------
// Shapes
// ---------------------------------------------------------------------------

type catalogModelView struct {
	ID            string            `json:"id"`
	ModelID       string            `json:"model_id"`
	DisplayName   string            `json:"display_name"`
	SuggestedName string            `json:"suggested_name"`
	Price         *store.ModelPrice `json:"price"`
	Capabilities  map[string]any    `json:"capabilities"`
	// Notes explains a false/limited capability above (e.g. tool use not
	// yet supported through the current translation path); "" when there
	// is nothing to say.
	Notes           string  `json:"notes"`
	Enabled         bool    `json:"enabled"`
	TenantModelID   *string `json:"tenant_model_id"`
	TenantModelName *string `json:"tenant_model_name"`
}

func newCatalogModelView(m store.ModelCatalogModel) catalogModelView {
	caps := m.Capabilities
	if caps == nil {
		caps = map[string]any{}
	}
	return catalogModelView{
		ID: m.ID, ModelID: m.ModelID, DisplayName: m.DisplayName, SuggestedName: m.SuggestedName,
		Price: m.Price, Capabilities: caps, Notes: m.Notes, Enabled: m.Enabled,
	}
}

type catalogProviderView struct {
	ID          string             `json:"id"`
	Slug        string             `json:"slug"`
	DisplayName string             `json:"display_name"`
	Vendor      string             `json:"vendor"`
	BaseURL     string             `json:"base_url"`
	DocsURL     string             `json:"docs_url"`
	Enabled     bool               `json:"enabled"`
	Models      []catalogModelView `json:"models"`
	// Warnings is present (never null) only on Create/Update, where a
	// non-fatal base_url observation belongs; List/Get omit it.
	Warnings []string `json:"warnings,omitempty"`
}

func newCatalogProviderView(p *store.ModelCatalogProvider) catalogProviderView {
	models := make([]catalogModelView, 0, len(p.Models))
	for _, m := range p.Models {
		models = append(models, newCatalogModelView(m))
	}
	return catalogProviderView{
		ID: p.ID, Slug: p.Slug, DisplayName: p.DisplayName, Vendor: p.Vendor,
		BaseURL: p.BaseURL, DocsURL: p.DocsURL, Enabled: p.Enabled, Models: models,
	}
}

// ---------------------------------------------------------------------------
// GET /model-catalog
// ---------------------------------------------------------------------------

type catalogListResponse struct {
	Providers []catalogProviderView `json:"providers"`
}

// List handles GET /api/v1/model-catalog: every provider and its models,
// each model annotated with the CALLER'S OWN registered model (if any)
// created from it -- tenant_model_id/tenant_model_name, both null when the
// tenant hasn't connected that model.
func (h ModelCatalog) List(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}

	providers, err := h.Store.ModelCatalog().ListProviders(r.Context())
	if err != nil {
		writeStoreErr(w, r, "list model catalog", err)
		return
	}
	tenantModels, err := h.Store.Models().List(r.Context(), tid, store.ListOptions{})
	if err != nil {
		writeStoreErr(w, r, "list model catalog (tenant models)", err)
		return
	}
	byCatalogID := make(map[string]*store.Model, len(tenantModels))
	for _, m := range tenantModels {
		if m.CatalogModelID != "" {
			byCatalogID[m.CatalogModelID] = m
		}
	}

	resp := catalogListResponse{Providers: make([]catalogProviderView, 0, len(providers))}
	for _, p := range providers {
		pv := newCatalogProviderView(p)
		for i := range pv.Models {
			if tm, ok := byCatalogID[pv.Models[i].ID]; ok {
				id, name := tm.ID, tm.Name
				pv.Models[i].TenantModelID, pv.Models[i].TenantModelName = &id, &name
			}
		}
		resp.Providers = append(resp.Providers, pv)
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// Providers: create / update / delete
// ---------------------------------------------------------------------------

type catalogProviderRequest struct {
	Slug        string `json:"slug"`
	DisplayName string `json:"display_name"`
	BaseURL     string `json:"base_url"`
	DocsURL     string `json:"docs_url,omitempty"`
	Enabled     *bool  `json:"enabled,omitempty"`
}

// validateProvider validates req's fixed fields and the base_url
// convention, returning the normalized (trailing-slash-trimmed) base_url
// and any non-fatal warning.
func validateProvider(ctx context.Context, req catalogProviderRequest) (baseURL, warning string, err error) {
	if err := config.ValidateCatalogProviderSlug(req.Slug); err != nil {
		return "", "", err
	}
	if err := validateName("display_name", req.DisplayName); err != nil {
		return "", "", err
	}
	if len([]rune(req.DocsURL)) > maxCatalogFieldLen {
		return "", "", fmt.Errorf("docs_url must be at most %d characters", maxCatalogFieldLen)
	}
	baseURL, warning, err = config.ValidateCatalogBaseURL(ctx, req.BaseURL)
	if err != nil {
		return "", "", fmt.Errorf("base_url: %w", err)
	}
	return baseURL, warning, nil
}

// CreateProvider handles POST /api/v1/model-catalog/providers.
func (h ModelCatalog) CreateProvider(w http.ResponseWriter, r *http.Request) {
	var req catalogProviderRequest
	if !httpx.Decode(w, r, &req) {
		return
	}
	baseURL, warning, err := validateProvider(r.Context(), req)
	if err != nil {
		httpx.ValidationError(w, err.Error())
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	p := &store.ModelCatalogProvider{
		Slug: req.Slug, DisplayName: req.DisplayName, Vendor: "openai_compat",
		BaseURL: baseURL, DocsURL: req.DocsURL, Enabled: enabled,
	}
	if err := h.Store.ModelCatalog().CreateProvider(r.Context(), p); err != nil {
		writeStoreErr(w, r, "create catalog provider", err)
		return
	}
	view := newCatalogProviderView(p)
	view.Warnings = warningsOf(warning)
	httpx.WriteJSON(w, http.StatusCreated, view)
}

// warningsOf returns a non-nil slice: empty when w is "", else a
// single-element slice. Kept as a helper so every write endpoint's
// "warnings" field is consistently [] rather than null.
func warningsOf(w string) []string {
	if w == "" {
		return []string{}
	}
	return []string{w}
}

// UpdateProvider handles PUT /api/v1/model-catalog/providers/{id}.
func (h ModelCatalog) UpdateProvider(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	existing, err := h.Store.ModelCatalog().GetProvider(r.Context(), id)
	if err != nil {
		writeStoreErr(w, r, "update catalog provider", err)
		return
	}
	var req catalogProviderRequest
	if !httpx.Decode(w, r, &req) {
		return
	}
	baseURL, warning, err := validateProvider(r.Context(), req)
	if err != nil {
		httpx.ValidationError(w, err.Error())
		return
	}
	existing.Slug, existing.DisplayName, existing.BaseURL, existing.DocsURL = req.Slug, req.DisplayName, baseURL, req.DocsURL
	if req.Enabled != nil {
		existing.Enabled = *req.Enabled
	}
	if err := h.Store.ModelCatalog().UpdateProvider(r.Context(), existing); err != nil {
		writeStoreErr(w, r, "update catalog provider", err)
		return
	}
	// Re-fetch for the response so Models is populated (UpdateProvider
	// itself only touches the provider row).
	updated, err := h.Store.ModelCatalog().GetProvider(r.Context(), id)
	if err != nil {
		writeStoreErr(w, r, "update catalog provider (reload)", err)
		return
	}
	view := newCatalogProviderView(updated)
	view.Warnings = warningsOf(warning)
	httpx.WriteJSON(w, http.StatusOK, view)
}

// DeleteProvider handles DELETE /api/v1/model-catalog/providers/{id}.
// ?force=true bypasses the usage confirmation.
func (h ModelCatalog) DeleteProvider(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	p, err := h.Store.ModelCatalog().GetProvider(r.Context(), id)
	if err != nil {
		writeStoreErr(w, r, "delete catalog provider", err)
		return
	}
	if r.URL.Query().Get("force") != "true" {
		usage, err := h.totalUsage(r.Context(), p.Models)
		if err != nil {
			writeStoreErr(w, r, "delete catalog provider (usage)", err)
			return
		}
		if usage > 0 {
			writeUsageConflict(w, usage, "provider has models in use by tenant-registered models; pass ?force=true to delete anyway")
			return
		}
	}
	if err := h.Store.ModelCatalog().DeleteProvider(r.Context(), id); err != nil {
		writeStoreErr(w, r, "delete catalog provider", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// totalUsage sums CountTenantUsage across models.
func (h ModelCatalog) totalUsage(ctx context.Context, models []store.ModelCatalogModel) (int, error) {
	var total int
	for _, m := range models {
		n, err := h.Store.ModelCatalog().CountTenantUsage(ctx, m.ID)
		if err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}

// writeUsageConflict writes the 409-with-usage-count body every catalog
// delete confirmation shares: the standard error envelope plus
// tenant_models, matching GET .../usage's own shape for that field.
func writeUsageConflict(w http.ResponseWriter, tenantModels int, message string) {
	httpx.WriteJSON(w, http.StatusConflict, struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
		TenantModels int `json:"tenant_models"`
	}{
		Error: struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		}{Type: httpx.TypeConflict, Message: message},
		TenantModels: tenantModels,
	})
}

// ---------------------------------------------------------------------------
// Models: create / update / delete / usage
// ---------------------------------------------------------------------------

type catalogModelRequest struct {
	ModelID       string            `json:"model_id"`
	DisplayName   string            `json:"display_name,omitempty"`
	SuggestedName string            `json:"suggested_name,omitempty"`
	Price         *store.ModelPrice `json:"price,omitempty"`
	Capabilities  map[string]any    `json:"capabilities,omitempty"`
	Notes         string            `json:"notes,omitempty"`
	Enabled       *bool             `json:"enabled,omitempty"`
}

// catalogCapabilityKeys is the closed set of keys ModelCatalogModel.
// Capabilities may carry -- see the contract's shape,
// {"tools": bool|null, "vision": bool|null, "streaming": bool|null,
// "max_context": int|null}.
var catalogCapabilityKeys = map[string]string{"tools": "bool", "vision": "bool", "streaming": "bool", "max_context": "number"}

func validateCapabilities(caps map[string]any) error {
	for k, v := range caps {
		typ, known := catalogCapabilityKeys[k]
		if !known {
			return fmt.Errorf("capabilities: unknown field %q", k)
		}
		if v == nil {
			continue
		}
		switch typ {
		case "bool":
			if _, ok := v.(bool); !ok {
				return fmt.Errorf("capabilities.%s must be a boolean or null", k)
			}
		case "number":
			n, ok := v.(float64)
			if !ok || n != float64(int64(n)) || n < 0 {
				return fmt.Errorf("capabilities.%s must be a non-negative integer or null", k)
			}
		}
	}
	return nil
}

// sanitizeModelSegment lowercases s and keeps [a-z0-9._-], collapsing any
// other run of characters into a single "-", trimming leading/trailing
// "-._", and ensuring the first character is alphanumeric (required by
// config.ReCatalogSuggestedName). Falls back to "model" if nothing usable
// remains.
func sanitizeModelSegment(s string) string {
	var b strings.Builder
	prevDash := false
	for _, c := range strings.ToLower(s) {
		switch {
		case (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '.' || c == '_' || c == '-':
			b.WriteRune(c)
			prevDash = c == '-'
		case !prevDash:
			b.WriteByte('-')
			prevDash = true
		}
	}
	out := strings.Trim(b.String(), "-._")
	for len(out) > 0 {
		c := out[0]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			break
		}
		out = out[1:]
	}
	if out == "" {
		out = "model"
	}
	return out
}

// defaultSuggestedName is <sanitized last path segment of modelID>-<slug>,
// truncated to config.ReCatalogSuggestedName's length cap -- see the
// contract's catalog seed for worked examples (e.g. "zai-org/GLM-5.3" on
// "nebius" -> "glm-5.3-nebius").
func defaultSuggestedName(modelID, slug string) string {
	seg := modelID
	if i := strings.LastIndexByte(modelID, '/'); i >= 0 {
		seg = modelID[i+1:]
	}
	name := sanitizeModelSegment(seg) + "-" + slug
	if len(name) > maxSuggestedNameLen {
		name = name[:maxSuggestedNameLen]
	}
	return strings.TrimRight(name, "-._")
}

// validateCatalogModel validates req and resolves its effective
// suggested_name (defaulted from providerSlug when req.SuggestedName is
// empty).
func validateCatalogModel(req catalogModelRequest, providerSlug string) (suggestedName string, err error) {
	if strings.TrimSpace(req.ModelID) == "" || len([]rune(req.ModelID)) > maxCatalogFieldLen {
		return "", fmt.Errorf("model_id is required and must be at most %d characters", maxCatalogFieldLen)
	}
	if len([]rune(req.DisplayName)) > maxCatalogFieldLen {
		return "", fmt.Errorf("display_name must be at most %d characters", maxCatalogFieldLen)
	}
	if len([]rune(req.Notes)) > maxCatalogFieldLen {
		return "", fmt.Errorf("notes must be at most %d characters", maxCatalogFieldLen)
	}
	suggestedName = req.SuggestedName
	if suggestedName == "" {
		suggestedName = defaultSuggestedName(req.ModelID, providerSlug)
	}
	if err := config.ValidateCatalogSuggestedName(suggestedName); err != nil {
		return "", err
	}
	if req.Price != nil {
		if err := config.ValidateModelPrice(req.Price.Input, req.Price.Output, req.Price.CacheRead, req.Price.CacheWrite); err != nil {
			return "", fmt.Errorf("price: %w", err)
		}
	}
	if err := validateCapabilities(req.Capabilities); err != nil {
		return "", err
	}
	return suggestedName, nil
}

// CreateModel handles POST /api/v1/model-catalog/providers/{id}/models.
func (h ModelCatalog) CreateModel(w http.ResponseWriter, r *http.Request) {
	providerID := chi.URLParam(r, "id")
	provider, err := h.Store.ModelCatalog().GetProvider(r.Context(), providerID)
	if err != nil {
		writeStoreErr(w, r, "create catalog model", err)
		return
	}
	var req catalogModelRequest
	if !httpx.Decode(w, r, &req) {
		return
	}
	suggestedName, err := validateCatalogModel(req, provider.Slug)
	if err != nil {
		httpx.ValidationError(w, err.Error())
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	m := &store.ModelCatalogModel{
		ProviderID: providerID, ModelID: req.ModelID, DisplayName: req.DisplayName, SuggestedName: suggestedName,
		Price: req.Price, Capabilities: req.Capabilities, Notes: req.Notes, Enabled: enabled,
	}
	if err := h.Store.ModelCatalog().CreateModel(r.Context(), m); err != nil {
		writeStoreErr(w, r, "create catalog model", err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, newCatalogModelView(*m))
}

// UpdateModel handles PUT /api/v1/model-catalog/models/{id}.
func (h ModelCatalog) UpdateModel(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	existing, err := h.Store.ModelCatalog().GetModel(r.Context(), id)
	if err != nil {
		writeStoreErr(w, r, "update catalog model", err)
		return
	}
	provider, err := h.Store.ModelCatalog().GetProvider(r.Context(), existing.ProviderID)
	if err != nil {
		writeStoreErr(w, r, "update catalog model (provider)", err)
		return
	}
	var req catalogModelRequest
	if !httpx.Decode(w, r, &req) {
		return
	}
	suggestedName, err := validateCatalogModel(req, provider.Slug)
	if err != nil {
		httpx.ValidationError(w, err.Error())
		return
	}
	existing.ModelID, existing.DisplayName, existing.SuggestedName = req.ModelID, req.DisplayName, suggestedName
	existing.Price, existing.Capabilities, existing.Notes = req.Price, req.Capabilities, req.Notes
	if req.Enabled != nil {
		existing.Enabled = *req.Enabled
	}
	if err := h.Store.ModelCatalog().UpdateModel(r.Context(), existing); err != nil {
		writeStoreErr(w, r, "update catalog model", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, newCatalogModelView(*existing))
}

// DeleteModel handles DELETE /api/v1/model-catalog/models/{id}.
// ?force=true bypasses the usage confirmation.
func (h ModelCatalog) DeleteModel(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if _, err := h.Store.ModelCatalog().GetModel(r.Context(), id); err != nil {
		writeStoreErr(w, r, "delete catalog model", err)
		return
	}
	if r.URL.Query().Get("force") != "true" {
		usage, err := h.Store.ModelCatalog().CountTenantUsage(r.Context(), id)
		if err != nil {
			writeStoreErr(w, r, "delete catalog model (usage)", err)
			return
		}
		if usage > 0 {
			writeUsageConflict(w, usage, "model is in use by tenant-registered models; pass ?force=true to delete anyway")
			return
		}
	}
	if err := h.Store.ModelCatalog().DeleteModel(r.Context(), id); err != nil {
		writeStoreErr(w, r, "delete catalog model", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ModelUsage handles GET /api/v1/model-catalog/models/{id}/usage.
func (h ModelCatalog) ModelUsage(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if _, err := h.Store.ModelCatalog().GetModel(r.Context(), id); err != nil {
		writeStoreErr(w, r, "get catalog model usage", err)
		return
	}
	n, err := h.Store.ModelCatalog().CountTenantUsage(r.Context(), id)
	if err != nil {
		writeStoreErr(w, r, "get catalog model usage", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, struct {
		TenantModels int `json:"tenant_models"`
	}{TenantModels: n})
}

// ---------------------------------------------------------------------------
// POST /model-catalog/providers/{id}/test
// ---------------------------------------------------------------------------

const (
	// testTimeout bounds the outbound call to the provider.
	testTimeout = 10 * time.Second
	// testMaxBody caps how much of the provider's response we read.
	testMaxBody = 1 << 20 // 1 MiB
	// testMaxModelsFound caps how many model ids the response echoes.
	testMaxModelsFound = 200
)

type catalogTestRequest struct {
	Credential string `json:"credential,omitempty"`
	APIKey     string `json:"api_key,omitempty"`
}

type catalogTestResponse struct {
	OK             bool            `json:"ok"`
	Status         int             `json:"status"`
	Error          string          `json:"error,omitempty"`
	ModelsFound    []string        `json:"models_found"`
	CatalogMatches map[string]bool `json:"catalog_matches"`
}

// noRedirectClient never follows a redirect: the provider's response is
// returned as-is (status 3xx, ok:false), which trivially satisfies "do not
// follow redirects to other hosts" without needing to inspect the Location
// host at all.
var noRedirectClient = &http.Client{
	Timeout:       testTimeout,
	Transport:     netguard.Transport(),
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// Test handles POST /api/v1/model-catalog/providers/{id}/test: calls GET
// {base_url}/models with the given (or decrypted existing) key and reports
// whether it answered, without ever echoing the key. Every outcome short
// of a caller error (bad request shape, unknown provider) is a 200 with
// ok:false -- a network/HTTP failure talking to a third-party provider is
// not this API's own error.
func (h ModelCatalog) Test(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}
	provider, err := h.Store.ModelCatalog().GetProvider(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeStoreErr(w, r, "test catalog provider", err)
		return
	}

	var req catalogTestRequest
	if !httpx.Decode(w, r, &req) {
		return
	}
	if (req.Credential == "") == (req.APIKey == "") {
		httpx.ValidationError(w, "exactly one of credential or api_key is required")
		return
	}

	key := req.APIKey
	if req.Credential != "" {
		payload, err := h.Secrets.Get(r.Context(), tid, req.Credential)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				httpx.ValidationError(w, "credential not found")
				return
			}
			writeStoreErr(w, r, "test catalog provider (credential)", err)
			return
		}
		key, err = apiKeyFromPayload(payload)
		if err != nil {
			httpx.ValidationError(w, "credential "+req.Credential+": "+err.Error())
			return
		}
	}

	resp := h.callProviderModels(r.Context(), provider.BaseURL, key)
	// Keyed by the vendor model id (catalog model_id), not the catalog
	// row's uuid -- the console looks this up by the id it shows the user.
	matches := make(map[string]bool, len(provider.Models))
	for _, m := range provider.Models {
		matches[m.ModelID] = containsFold(resp.ModelsFound, m.ModelID)
	}
	resp.CatalogMatches = matches
	httpx.WriteJSON(w, http.StatusOK, resp)
}

// apiKeyFromPayload mirrors internal/llmplane's own credential resolution
// (registry.go's apiKeyOf): the "api_key" field, or the sole field of a
// single-field credential, so /test resolves an existing credential
// exactly as a real LLM-plane call through it would.
func apiKeyFromPayload(payload map[string]string) (string, error) {
	if v, ok := payload["api_key"]; ok && v != "" {
		return v, nil
	}
	if len(payload) == 1 {
		for _, v := range payload {
			if v != "" {
				return v, nil
			}
		}
	}
	return "", errors.New("has no api_key field")
}

// callProviderModels makes the actual GET {baseURL}/models call. Every
// return path sets ModelsFound to a non-nil (possibly empty) slice.
func (h ModelCatalog) callProviderModels(ctx context.Context, baseURL, key string) catalogTestResponse {
	out := catalogTestResponse{ModelsFound: []string{}}

	ctx, cancel := context.WithTimeout(ctx, testTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/models", nil)
	if err != nil {
		out.Error = "could not build request"
		return out
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")

	resp, err := noRedirectClient.Do(req)
	if err != nil {
		out.Error = classifyTestErr(err)
		return out
	}
	defer resp.Body.Close()
	out.Status = resp.StatusCode

	body, err := io.ReadAll(io.LimitReader(resp.Body, testMaxBody))
	if err != nil {
		out.Error = "failed reading the provider's response"
		return out
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		out.Error = fmt.Sprintf("provider responded with HTTP %d", resp.StatusCode)
		return out
	}

	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		out.Error = "unexpected response shape from provider"
		return out
	}
	ids := make([]string, 0, len(parsed.Data))
	for _, d := range parsed.Data {
		if d.ID != "" {
			ids = append(ids, d.ID)
		}
	}
	sort.Strings(ids)
	if len(ids) > testMaxModelsFound {
		ids = ids[:testMaxModelsFound]
	}
	out.ModelsFound = ids
	out.OK = true
	return out
}

// classifyTestErr turns a transport-level error into a short, key-free
// message -- net/http error text never contains header values, but this
// keeps the wording stable and readable regardless.
func classifyTestErr(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "request timed out"
	case isTimeout(err):
		return "request timed out"
	default:
		return "request failed: could not reach the provider"
	}
}

func isTimeout(err error) bool {
	var ne interface{ Timeout() bool }
	return errors.As(err, &ne) && ne.Timeout()
}

func containsFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// POST /model-catalog/providers/{id}/connect
// ---------------------------------------------------------------------------

type catalogConnectCredentialRequest struct {
	Name string                              `json:"name,omitempty"`
	New  *catalogConnectNewCredentialRequest `json:"new,omitempty"`
}

type catalogConnectNewCredentialRequest struct {
	Name   string `json:"name,omitempty"`
	APIKey string `json:"api_key"`
}

type catalogConnectRequest struct {
	Credential catalogConnectCredentialRequest `json:"credential"`
	Models     []string                        `json:"models"`
}

type catalogConnectCredentialView struct {
	Name    string `json:"name"`
	Created bool   `json:"created"`
}

type catalogConnectModelView struct {
	CatalogModelID string `json:"catalog_model_id"`
	ModelID        string `json:"model_id"`
	Name           string `json:"name"`
	Status         string `json:"status"`
}

type catalogConnectResponse struct {
	Credential catalogConnectCredentialView `json:"credential"`
	Models     []catalogConnectModelView    `json:"models"`
}

// requirePermission denies with 403 unless h.Authorizer grants perm to the
// request's Principal. Used for Connect's second and third permission
// checks (credential.create, credential.read), which router.go's single-
// Permission routeSpec can't express (see Deps.Authorizer).
func (h ModelCatalog) requirePermission(w http.ResponseWriter, r *http.Request, perm string) bool {
	p, _ := pkgauth.PrincipalFrom(r.Context())
	if h.Authorizer == nil || !h.Authorizer.Allow(r.Context(), p, perm) {
		httpx.WriteError(w, http.StatusForbidden, httpx.TypePermission, "insufficient permissions")
		return false
	}
	return true
}

// Connect handles POST /api/v1/model-catalog/providers/{id}/connect.
func (h ModelCatalog) Connect(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}
	if !h.requirePermission(w, r, "credential.create") {
		return
	}

	provider, err := h.Store.ModelCatalog().GetProvider(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeStoreErr(w, r, "connect catalog provider", err)
		return
	}
	if !provider.Enabled {
		httpx.ValidationError(w, "provider is disabled")
		return
	}

	var req catalogConnectRequest
	if !httpx.Decode(w, r, &req) {
		return
	}
	haveExisting, haveNew := req.Credential.Name != "", req.Credential.New != nil
	if haveExisting == haveNew {
		httpx.ValidationError(w, "credential must be exactly one of {\"name\": \"<existing>\"} or {\"new\": {...}}")
		return
	}
	if len(req.Models) == 0 {
		httpx.ValidationError(w, "models: at least one catalog model id is required")
		return
	}

	byID := make(map[string]store.ModelCatalogModel, len(provider.Models))
	for _, m := range provider.Models {
		byID[m.ID] = m
	}
	connectModels := make([]store.CatalogConnectModel, 0, len(req.Models))
	for _, id := range req.Models {
		cm, ok := byID[id]
		if !ok {
			httpx.ValidationError(w, fmt.Sprintf("models: %q is not a model of this provider", id))
			return
		}
		if !cm.Enabled {
			httpx.ValidationError(w, fmt.Sprintf("models: %q is disabled", id))
			return
		}
		connectModels = append(connectModels, store.CatalogConnectModel{
			CatalogModelID: cm.ID, Name: cm.SuggestedName, Description: cm.DisplayName,
			ModelID: cm.ModelID, BaseURL: provider.BaseURL, Label: provider.Slug, Price: cm.Price, Capabilities: cm.Capabilities,
		})
	}

	var credSpec store.CatalogConnectCredential
	if haveExisting {
		if !h.requirePermission(w, r, "credential.read") {
			return
		}
		if err := validateName("credential.name", req.Credential.Name); err != nil {
			httpx.ValidationError(w, err.Error())
			return
		}
		credSpec.Existing = req.Credential.Name
	} else {
		name := req.Credential.New.Name
		if name == "" {
			name = provider.Slug + "-api-key"
		}
		if err := validateName("credential.new.name", name); err != nil {
			httpx.ValidationError(w, err.Error())
			return
		}
		if strings.TrimSpace(req.Credential.New.APIKey) == "" {
			httpx.ValidationError(w, "credential.new.api_key is required")
			return
		}
		// type/payload match the console's own "Accept API key" credential
		// exactly (web/src/components/app/models/ModelFormDialog.tsx), so
		// a catalog-connected model's credential and a hand-created one
		// are indistinguishable.
		ciphertext, nonce, keyID, fieldNames, err := h.Secrets.Seal(map[string]string{"api_key": req.Credential.New.APIKey})
		if err != nil {
			applog.From(r.Context()).Error("connect catalog provider: seal credential failed", "error", err)
			httpx.Internal(w, "internal error")
			return
		}
		credSpec.New = &store.CatalogConnectNewCredential{
			Name: name, Type: "api_key", Ciphertext: ciphertext, Nonce: nonce, KeyID: keyID, FieldNames: fieldNames,
		}
	}

	result, err := h.Store.ModelCatalog().Connect(r.Context(), tid, credSpec, connectModels)
	if err != nil {
		h.writeConnectErr(w, r, err)
		return
	}
	h.invalidate(tid)

	resp := catalogConnectResponse{
		Credential: catalogConnectCredentialView{Name: result.CredentialName, Created: result.CredentialCreated},
		Models:     make([]catalogConnectModelView, 0, len(result.Models)),
	}
	for _, m := range result.Models {
		resp.Models = append(resp.Models, catalogConnectModelView{CatalogModelID: m.CatalogModelID, ModelID: m.ModelID, Name: m.Name, Status: m.Status})
	}
	httpx.WriteJSON(w, http.StatusCreated, resp)
}

// writeConnectErr maps Connect's error cases onto the shared HTTP error
// envelope: a named conflict (credential or model name clash) is a 409
// naming it; ErrNotFound (an "existing" credential name that doesn't
// exist) is a 400, since it is the caller's request that is wrong, not a
// missing resource addressed by the URL; everything else falls back to
// writeStoreErr.
func (h ModelCatalog) writeConnectErr(w http.ResponseWriter, r *http.Request, err error) {
	var credConflict *store.CredentialNameConflictError
	var modelConflict *store.ModelNameConflictError
	switch {
	case errors.As(err, &credConflict):
		httpx.Conflict(w, credConflict.Error())
	case errors.As(err, &modelConflict):
		httpx.Conflict(w, modelConflict.Error())
	case errors.Is(err, store.ErrNotFound):
		httpx.ValidationError(w, "credential not found")
	default:
		writeStoreErr(w, r, "connect catalog provider", err)
	}
}

// invalidate tells the LLM plane's registry cache (when wired) that the
// tenant's rows changed -- Connect creates new ones, same as Models'
// write routes.
func (h ModelCatalog) invalidate(tenantID string) {
	if h.ModelInvalidator != nil {
		h.ModelInvalidator.Invalidate(tenantID)
	}
}

// ---------------------------------------------------------------------------
// POST /model-catalog/providers/{id}/prices/preview
// POST /model-catalog/providers/{id}/prices/apply
// ---------------------------------------------------------------------------

type catalogPricesSourceRequest struct {
	Credential string `json:"credential,omitempty"`
	APIKey     string `json:"api_key,omitempty"`
}

// resolvePriceAPIKey resolves req into a plain key for a prices.Source
// call: an existing credential's key, a raw key, or "" when source needs
// neither (Nebius's public feed). Mirrors Test's credential/api_key
// resolution (apiKeyFromPayload) except both may be empty here. Writes a
// response itself and returns ok=false on any failure.
func (h ModelCatalog) resolvePriceAPIKey(w http.ResponseWriter, r *http.Request, tid string, source prices.Source, req catalogPricesSourceRequest) (key string, ok bool) {
	haveCred, haveKey := req.Credential != "", req.APIKey != ""
	if haveCred && haveKey {
		httpx.ValidationError(w, "at most one of credential or api_key may be set")
		return "", false
	}
	switch {
	case haveCred:
		payload, err := h.Secrets.Get(r.Context(), tid, req.Credential)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				httpx.ValidationError(w, "credential not found")
				return "", false
			}
			writeStoreErr(w, r, "refresh catalog prices (credential)", err)
			return "", false
		}
		key, err = apiKeyFromPayload(payload)
		if err != nil {
			httpx.ValidationError(w, "credential "+req.Credential+": "+err.Error())
			return "", false
		}
		return key, true
	case haveKey:
		return req.APIKey, true
	case !source.RequiresAPIKey():
		return "", true
	default:
		httpx.ValidationError(w, "credential or api_key is required")
		return "", false
	}
}

// pricesEqual is a nil-safe comparison: two nil prices are equal, a nil
// and a non-nil never are, two non-nil prices compare field-by-field.
func pricesEqual(a, b *store.ModelPrice) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

type catalogPriceItemView struct {
	CatalogModelID string            `json:"catalog_model_id"`
	ModelID        string            `json:"model_id"`
	CurrentPrice   *store.ModelPrice `json:"current_price"`
	// NewPrice is null when the provider's pricing feed didn't mention
	// this catalog model's vendor model_id.
	NewPrice *store.ModelPrice `json:"new_price"`
	// Changed is false whenever NewPrice is null -- there is nothing to
	// apply for a model the feed didn't return.
	Changed bool `json:"changed"`
}

type catalogPricesPreviewResponse struct {
	// SourceURL is the exact URL Fetch called -- no key, no query secret.
	SourceURL string                 `json:"source_url"`
	FetchedAt time.Time              `json:"fetched_at"`
	Items     []catalogPriceItemView `json:"items"`
	// UnmatchedProviderModels is how many vendor model ids the feed
	// returned a price for that match none of this provider's catalog
	// models -- the provider offers more than the catalog currently
	// tracks.
	UnmatchedProviderModels int `json:"unmatched_provider_models"`
}

// PreviewPrices handles
// POST /api/v1/model-catalog/providers/{id}/prices/preview: fetches the
// provider's CURRENT prices from ITS OWN pricing API (prices.Source) and
// diffs them against the catalog's stored prices. Read-only -- nothing is
// written.
func (h ModelCatalog) PreviewPrices(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}
	provider, err := h.Store.ModelCatalog().GetProvider(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeStoreErr(w, r, "preview catalog prices", err)
		return
	}
	source, ok := prices.Lookup(provider.Slug)
	if !ok {
		httpx.ValidationError(w, fmt.Sprintf("price refresh not supported for %s", provider.Slug))
		return
	}

	var req catalogPricesSourceRequest
	if !httpx.Decode(w, r, &req) {
		return
	}
	key, ok := h.resolvePriceAPIKey(w, r, tid, source, req)
	if !ok {
		return
	}

	result, err := source.Fetch(r.Context(), provider, key)
	if err != nil {
		applog.From(r.Context()).Warn("preview catalog prices: fetch failed", "provider", provider.Slug, "error", err)
		httpx.WriteError(w, http.StatusBadGateway, httpx.TypeUnavailable, err.Error())
		return
	}

	resp := catalogPricesPreviewResponse{
		SourceURL: result.SourceURL,
		FetchedAt: time.Now().UTC(),
		Items:     make([]catalogPriceItemView, 0, len(provider.Models)),
	}
	matchedProviderIDs := make(map[string]bool, len(result.Prices))
	for _, m := range provider.Models {
		item := catalogPriceItemView{CatalogModelID: m.ID, ModelID: m.ModelID, CurrentPrice: m.Price}
		if newPrice, found := result.Prices[m.ModelID]; found {
			matchedProviderIDs[m.ModelID] = true
			np := newPrice
			item.NewPrice = &np
			item.Changed = !pricesEqual(m.Price, &np)
		}
		resp.Items = append(resp.Items, item)
	}
	for modelID := range result.Prices {
		if !matchedProviderIDs[modelID] {
			resp.UnmatchedProviderModels++
		}
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

type catalogPricesApplyItemRequest struct {
	CatalogModelID string            `json:"catalog_model_id"`
	Price          *store.ModelPrice `json:"price"`
}

type catalogPricesApplyRequest struct {
	Items              []catalogPricesApplyItemRequest `json:"items"`
	UpdateTenantModels bool                            `json:"update_tenant_models"`
}

type catalogPricesApplyResponse struct {
	CatalogUpdated int `json:"catalog_updated"`
	TenantUpdated  int `json:"tenant_updated"`
}

// ApplyPrices handles POST /api/v1/model-catalog/providers/{id}/prices/apply:
// writes the given per-model prices onto the catalog and, when
// update_tenant_models is true, onto every tenant model (across every
// tenant) still using that catalog model's OLD price -- see
// store.ModelCatalogStore.ApplyPrices for the exact matching rule.
func (h ModelCatalog) ApplyPrices(w http.ResponseWriter, r *http.Request) {
	provider, err := h.Store.ModelCatalog().GetProvider(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeStoreErr(w, r, "apply catalog prices", err)
		return
	}

	var req catalogPricesApplyRequest
	if !httpx.Decode(w, r, &req) {
		return
	}
	if len(req.Items) == 0 {
		httpx.ValidationError(w, "items: at least one price update is required")
		return
	}
	byID := make(map[string]store.ModelCatalogModel, len(provider.Models))
	for _, m := range provider.Models {
		byID[m.ID] = m
	}
	updates := make([]store.PriceUpdate, 0, len(req.Items))
	for _, item := range req.Items {
		if _, ok := byID[item.CatalogModelID]; !ok {
			httpx.ValidationError(w, fmt.Sprintf("items: %q is not a model of this provider", item.CatalogModelID))
			return
		}
		if item.Price != nil {
			if err := config.ValidateModelPrice(item.Price.Input, item.Price.Output, item.Price.CacheRead, item.Price.CacheWrite); err != nil {
				httpx.ValidationError(w, fmt.Sprintf("items: %s: price: %s", item.CatalogModelID, err.Error()))
				return
			}
		}
		updates = append(updates, store.PriceUpdate{CatalogModelID: item.CatalogModelID, Price: item.Price})
	}

	result, err := h.Store.ModelCatalog().ApplyPrices(r.Context(), updates, req.UpdateTenantModels)
	if err != nil {
		writeStoreErr(w, r, "apply catalog prices", err)
		return
	}
	for _, tenantID := range result.TenantIDs {
		h.invalidate(tenantID)
	}
	httpx.WriteJSON(w, http.StatusOK, catalogPricesApplyResponse{CatalogUpdated: result.CatalogUpdated, TenantUpdated: result.TenantUpdated})
}
