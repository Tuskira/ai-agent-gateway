package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/api/httpx"
	dataplaneheaders "github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/headers"
	applog "github.com/Tuskira/tusk-ai-secured-gateway/internal/log"
	pkgheaders "github.com/Tuskira/tusk-ai-secured-gateway/pkg/headers"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// CredentialSources implements the /api/v1/credential-sources routes: a
// tenant's registrations of outside credential services that connector
// headers resolve secret values from (see pkg/headers.Source). A source
// holds only non-secret settings; the token it authenticates with lives
// in /credentials and is referenced by name.
//
// Like Credentials, every route is behind permission "admin.manage".
type CredentialSources struct{ Deps }

type createCredentialSourceRequest struct {
	Name     string         `json:"name"`
	Provider string         `json:"provider"`
	Config   map[string]any `json:"config"`
	Enabled  *bool          `json:"enabled"`
}

type updateCredentialSourceRequest struct {
	Name    *string        `json:"name"`
	Config  map[string]any `json:"config"`
	Enabled *bool          `json:"enabled"`
}

type credentialSourceView struct {
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	Provider string         `json:"provider"`
	Config   map[string]any `json:"config"`
	Enabled  bool           `json:"enabled"`
	// UsedBy is every connector whose headers name this source. Empty,
	// never null, when nothing references it.
	UsedBy    []connectorRef `json:"used_by"`
	CreatedAt string         `json:"created_at"`
	UpdatedAt string         `json:"updated_at"`
}

type credentialSourceTestResult struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

func newCredentialSourceView(s *store.CredentialSource, usedBy []connectorRef) credentialSourceView {
	cfg := s.Config
	if cfg == nil {
		cfg = map[string]any{}
	}
	if usedBy == nil {
		usedBy = []connectorRef{}
	}
	return credentialSourceView{
		ID:        s.ID,
		Name:      s.Name,
		Provider:  s.Provider,
		Config:    cfg,
		Enabled:   s.Enabled,
		UsedBy:    usedBy,
		CreatedAt: formatTime(s.CreatedAt),
		UpdatedAt: formatTime(s.UpdatedAt),
	}
}

// sourceUsage indexes conns by the credential source names their external
// headers reference ({"type":"external","source":"<name>",...}).
func sourceUsage(conns []*store.Connector) map[string][]connectorRef {
	usage := make(map[string][]connectorRef)
	for _, c := range conns {
		hdrs, ok := c.Metadata["headers"].(map[string]any)
		if !ok {
			continue
		}
		seen := map[string]bool{}
		for _, raw := range hdrs {
			cfg, ok := raw.(map[string]any)
			if !ok || cfg["type"] != "external" {
				continue
			}
			name, _ := cfg["source"].(string)
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			usage[name] = append(usage[name], connectorRef{ID: c.ID, Name: c.Name})
		}
	}
	for _, refs := range usage {
		sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
	}
	return usage
}

func (h CredentialSources) usageForTenant(ctx context.Context, tid string) (map[string][]connectorRef, error) {
	conns, err := h.Store.Connectors().List(ctx, tid)
	if err != nil {
		return nil, err
	}
	return sourceUsage(conns), nil
}

// sourceProvider returns the registered provider id names when it takes
// credential sources.
func (h CredentialSources) sourceProvider(id string) (pkgheaders.ExternalProvider, pkgheaders.SourceSchemaProvider, error) {
	if h.Headers == nil {
		return nil, nil, errors.New("no header providers are configured")
	}
	p, ok := h.Headers.Provider(id)
	if !ok {
		return nil, nil, fmt.Errorf("provider %q is not enabled on this gateway", id)
	}
	sp, ok := p.(pkgheaders.SourceSchemaProvider)
	if !ok {
		return nil, nil, fmt.Errorf("provider %q does not use credential sources", id)
	}
	return p, sp, nil
}

// invalidate drops cached values the source's provider holds for the
// caller's tenant, so an edited or deleted source takes effect at once.
func (h CredentialSources) invalidate(r *http.Request, providerID string) {
	if h.Headers == nil {
		return
	}
	p, ok := h.Headers.Provider(providerID)
	if !ok {
		return
	}
	if inv, ok := p.(pkgheaders.Invalidator); ok {
		inv.Invalidate(r.Context(), principalSubject(r), "")
	}
}

// Create handles POST /api/v1/credential-sources.
func (h CredentialSources) Create(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}
	var req createCredentialSourceRequest
	if !httpx.Decode(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if err := validateName("name", req.Name); err != nil {
		httpx.ValidationError(w, err.Error())
		return
	}
	if req.Provider == "" {
		httpx.ValidationError(w, errRequired("provider").Error())
		return
	}
	_, sp, err := h.sourceProvider(req.Provider)
	if err != nil {
		httpx.ValidationError(w, err.Error())
		return
	}
	if req.Config == nil {
		req.Config = map[string]any{}
	}
	if err := sp.ValidateSource(req.Config); err != nil {
		httpx.ValidationError(w, "config: "+err.Error())
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	src := &store.CredentialSource{
		TenantID: tid,
		Name:     req.Name,
		Provider: req.Provider,
		Config:   req.Config,
		Enabled:  enabled,
	}
	if err := h.Store.CredentialSources().Create(r.Context(), src); err != nil {
		writeStoreErr(w, r, "create credential source", err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, newCredentialSourceView(src, nil))
}

// List handles GET /api/v1/credential-sources.
func (h CredentialSources) List(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}
	srcs, err := h.Store.CredentialSources().List(r.Context(), tid)
	if err != nil {
		writeStoreErr(w, r, "list credential sources", err)
		return
	}
	usage, err := h.usageForTenant(r.Context(), tid)
	if err != nil {
		writeStoreErr(w, r, "list credential sources", err)
		return
	}
	page := httpx.ParsePagination(r)
	views := make([]credentialSourceView, 0, len(srcs))
	for _, s := range srcs {
		views = append(views, newCredentialSourceView(s, usage[s.Name]))
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page{Items: paginate(views, page), Total: len(views)})
}

// Get handles GET /api/v1/credential-sources/{id}.
func (h CredentialSources) Get(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}
	src, err := h.Store.CredentialSources().Get(r.Context(), tid, chi.URLParam(r, "id"))
	if err != nil {
		writeStoreErr(w, r, "get credential source", err)
		return
	}
	usage, err := h.usageForTenant(r.Context(), tid)
	if err != nil {
		writeStoreErr(w, r, "get credential source", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, newCredentialSourceView(src, usage[src.Name]))
}

// Update handles PUT /api/v1/credential-sources/{id}. name, config and
// enabled are each optional; provider cannot change. Renaming a source
// that connector headers still reference is refused with 409.
func (h CredentialSources) Update(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}
	var req updateCredentialSourceRequest
	if !httpx.Decode(w, r, &req) {
		return
	}
	src, err := h.Store.CredentialSources().Get(r.Context(), tid, chi.URLParam(r, "id"))
	if err != nil {
		writeStoreErr(w, r, "update credential source", err)
		return
	}
	usage, err := h.usageForTenant(r.Context(), tid)
	if err != nil {
		writeStoreErr(w, r, "update credential source", err)
		return
	}

	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if err := validateName("name", name); err != nil {
			httpx.ValidationError(w, err.Error())
			return
		}
		if name != src.Name && len(usage[src.Name]) > 0 {
			httpx.Conflict(w, fmt.Sprintf("credential source %q is used by %s; point them elsewhere before renaming it", src.Name, refNames(usage[src.Name])))
			return
		}
		src.Name = name
	}
	if req.Config != nil {
		_, sp, err := h.sourceProvider(src.Provider)
		if err != nil {
			httpx.ValidationError(w, err.Error())
			return
		}
		if err := sp.ValidateSource(req.Config); err != nil {
			httpx.ValidationError(w, "config: "+err.Error())
			return
		}
		src.Config = req.Config
	}
	if req.Enabled != nil {
		src.Enabled = *req.Enabled
	}

	if err := h.Store.CredentialSources().Update(r.Context(), src); err != nil {
		writeStoreErr(w, r, "update credential source", err)
		return
	}
	h.invalidate(r, src.Provider)
	httpx.WriteJSON(w, http.StatusOK, newCredentialSourceView(src, usage[src.Name]))
}

// Delete handles DELETE /api/v1/credential-sources/{id}. It answers 409
// while any connector header still names the source.
func (h CredentialSources) Delete(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}
	src, err := h.Store.CredentialSources().Get(r.Context(), tid, chi.URLParam(r, "id"))
	if err != nil {
		writeStoreErr(w, r, "delete credential source", err)
		return
	}
	usage, err := h.usageForTenant(r.Context(), tid)
	if err != nil {
		writeStoreErr(w, r, "delete credential source", err)
		return
	}
	if refs := usage[src.Name]; len(refs) > 0 {
		httpx.Conflict(w, fmt.Sprintf("credential source %q is used by %s", src.Name, refNames(refs)))
		return
	}
	if err := h.Store.CredentialSources().SoftDelete(r.Context(), tid, src.ID); err != nil {
		writeStoreErr(w, r, "delete credential source", err)
		return
	}
	h.invalidate(r, src.Provider)
	w.WriteHeader(http.StatusNoContent)
}

// Test handles POST /api/v1/credential-sources/{id}/test: it asks the
// source's provider to check the service is reachable and accepts the
// gateway's token. The answer is ok or a failure reason; no secret value
// is ever returned.
func (h CredentialSources) Test(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}
	src, err := h.Store.CredentialSources().Get(r.Context(), tid, chi.URLParam(r, "id"))
	if err != nil {
		writeStoreErr(w, r, "test credential source", err)
		return
	}
	p, _, err := h.sourceProvider(src.Provider)
	if err != nil {
		httpx.WriteJSON(w, http.StatusOK, credentialSourceTestResult{Error: err.Error()})
		return
	}
	tester, ok := p.(pkgheaders.SourceTester)
	if !ok {
		httpx.WriteJSON(w, http.StatusOK, credentialSourceTestResult{Error: fmt.Sprintf("provider %q cannot test a source", src.Provider)})
		return
	}
	if err := tester.TestSource(r.Context(), *dataplaneheaders.SourceFromStore(src)); err != nil {
		applog.From(r.Context()).Warn("credential source test failed", "source", src.Name, "error", err)
		httpx.WriteJSON(w, http.StatusOK, credentialSourceTestResult{Error: err.Error()})
		return
	}
	httpx.WriteJSON(w, http.StatusOK, credentialSourceTestResult{OK: true})
}

func refNames(refs []connectorRef) string {
	names := make([]string, len(refs))
	for i, r := range refs {
		names[i] = fmt.Sprintf("%q", r.Name)
	}
	return "connector(s) " + strings.Join(names, ", ")
}
