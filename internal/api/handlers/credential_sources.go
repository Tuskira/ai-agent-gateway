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
	// Locked marks the source platform-managed; setting it needs
	// platform.admin.
	Locked bool `json:"locked"`
}

type updateCredentialSourceRequest struct {
	Name    *string        `json:"name"`
	Config  map[string]any `json:"config"`
	Enabled *bool          `json:"enabled"`
	// Locked sets or clears the platform-managed flag (platform.admin only).
	Locked *bool `json:"locked"`
}

type credentialSourceView struct {
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	Provider string         `json:"provider"`
	Config   map[string]any `json:"config"`
	Enabled  bool           `json:"enabled"`
	// Locked is true for a platform-managed source: connector headers may
	// use it, but it cannot be changed without platform.admin.
	Locked bool `json:"locked"`
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

// credentialSourceOptions is the answer of the options route: the values
// the source's service offers, or a failure reason (with no options) when
// it could not be asked, like credentialSourceTestResult.
type credentialSourceOptions struct {
	Options []credentialSourceOption `json:"options"`
	Error   string                   `json:"error,omitempty"`
}

type credentialSourceOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
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
		Locked:    s.Locked,
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

// denyLockedAuthCredential refuses a source config whose auth_credential
// names a platform-locked credential of the tenant: 403 for a caller
// without platform.admin (a tenant must not point a source at a URL of its
// choosing and have the gateway send the platform's token there), and 400
// for a platform admin when the source would not be locked itself (an
// unlocked source can be re-pointed by the tenant later; at request time
// such a source cannot read the credential anyway). It reports whether it
// refused.
func (h CredentialSources) denyLockedAuthCredential(w http.ResponseWriter, r *http.Request, tid string, cfg map[string]any, sourceLocked bool) bool {
	name, _ := cfg["auth_credential"].(string)
	if name == "" {
		return false
	}
	cred, err := h.Store.Credentials().Get(r.Context(), tid, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false
		}
		writeStoreErr(w, r, "check credential source auth credential", err)
		return true
	}
	if !cred.Locked {
		return false
	}
	if h.denyLockedChange(w, r, true) {
		return true
	}
	if !sourceLocked {
		httpx.ValidationError(w, fmt.Sprintf("auth_credential %q is managed by the platform; a source that uses it must be locked too (\"locked\": true)", name))
		return true
	}
	return false
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
	if h.denySetLocked(w, r, req.Locked, false) {
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
	if h.denyLockedAuthCredential(w, r, tid, req.Config, req.Locked) {
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
		Locked:   req.Locked,
	}
	if err := h.Store.CredentialSources().Create(r.Context(), src); err != nil {
		if req.Locked && errors.Is(err, store.ErrConflict) {
			// The name is taken, possibly by the tenant itself. Never take
			// it over silently: the caller decides, with an explicit PUT.
			httpx.Conflict(w, fmt.Sprintf("a credential source named %q already exists in this tenant; to take it over, PUT /api/v1/credential-sources/{id} with \"locked\": true and the config", req.Name))
			return
		}
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

// Update handles PUT /api/v1/credential-sources/{id}. name, config,
// enabled and locked are each optional; provider cannot change. Renaming a
// source that connector headers still reference is refused with 409. A
// lock is applied before the other fields are written and an unlock after
// them; the store write carries the lock check itself, so a lock set by a
// platform admin after this handler's read is never bypassed or cleared.
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
	if h.denyLockedChange(w, r, src.Locked) {
		return
	}
	if req.Locked != nil && h.denySetLocked(w, r, *req.Locked, src.Locked) {
		return
	}
	finalLocked := src.Locked
	if req.Locked != nil {
		finalLocked = *req.Locked
	}
	if req.Config != nil && h.denyLockedAuthCredential(w, r, tid, req.Config, finalLocked) {
		return
	}
	if req.Config == nil && src.Locked && !finalLocked && h.denyLockedAuthCredential(w, r, tid, src.Config, false) {
		return
	}
	if finalLocked && !src.Locked && req.Config == nil {
		httpx.ValidationError(w, "locking a source needs its config in the same request (\"config\"), so it is never locked with a config the platform did not set")
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

	guard := store.LockGuard(h.isPlatformAdmin(r))
	if finalLocked && !src.Locked {
		// Locking an unlocked source (for example one a tenant created
		// under a name the platform takes over) writes the platform's
		// config and the lock in one statement: a locked source with the
		// tenant's URL would get a locked credential's token.
		if err := h.Store.CredentialSources().UpdateLocked(r.Context(), src); err != nil {
			writeStoreErr(w, r, "lock credential source", err)
			return
		}
		h.invalidate(r, src.Provider)
		httpx.WriteJSON(w, http.StatusOK, newCredentialSourceView(src, usage[src.Name]))
		return
	}
	if err := h.Store.CredentialSources().Update(r.Context(), src, guard); err != nil {
		writeStoreErr(w, r, "update credential source", err)
		return
	}
	if !finalLocked && src.Locked {
		if err := h.Store.CredentialSources().SetLocked(r.Context(), tid, src.ID, false); err != nil {
			writeStoreErr(w, r, "unlock credential source", err)
			return
		}
	}
	src.Locked = finalLocked
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
	if h.denyLockedChange(w, r, src.Locked) {
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
	if err := h.Store.CredentialSources().SoftDelete(r.Context(), tid, src.ID, store.LockGuard(h.isPlatformAdmin(r))); err != nil {
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

// Options handles GET /api/v1/credential-sources/{id}/options?field=<name>:
// it asks the source's provider which values its service offers for one
// header config field (for external_service, the credential ids it holds
// for the tenant), so a console can offer a picker. field must be one the
// provider lists options for (see "option_fields" on GET
// /headers/providers), else 400. Like Test, a source that cannot be asked
// (disabled, a provider that lists nothing, the service failing) answers
// 200 with {"options":[],"error":"..."}; only an unknown source is 404.
// Values are credential ids, never secrets.
//
// The route needs connector.update, not admin.manage: it serves the
// connector editor, whose users can already name any credential id in a
// header they save.
func (h CredentialSources) Options(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}
	field := strings.TrimSpace(r.URL.Query().Get("field"))
	if field == "" {
		httpx.ValidationError(w, errRequired("field").Error())
		return
	}
	src, err := h.Store.CredentialSources().Get(r.Context(), tid, chi.URLParam(r, "id"))
	if err != nil {
		writeStoreErr(w, r, "list credential source options", err)
		return
	}
	fail := func(msg string) {
		httpx.WriteJSON(w, http.StatusOK, credentialSourceOptions{Options: []credentialSourceOption{}, Error: msg})
	}
	p, _, err := h.sourceProvider(src.Provider)
	if err != nil {
		fail(err.Error())
		return
	}
	lister, ok := p.(pkgheaders.SourceOptionLister)
	if !ok {
		fail(fmt.Sprintf("provider %q cannot list options", src.Provider))
		return
	}
	known := false
	for _, f := range lister.OptionFields() {
		known = known || f == field
	}
	if !known {
		httpx.ValidationError(w, fmt.Sprintf("provider %q lists no options for field %q", src.Provider, field))
		return
	}
	if !src.Enabled {
		fail(fmt.Sprintf("credential source %q is disabled", src.Name))
		return
	}
	opts, err := lister.ListOptions(r.Context(), *dataplaneheaders.SourceFromStore(src), field)
	if err != nil {
		applog.From(r.Context()).Warn("credential source options failed", "source", src.Name, "field", field, "error", err)
		fail(err.Error())
		return
	}
	out := credentialSourceOptions{Options: make([]credentialSourceOption, 0, len(opts))}
	for _, o := range opts {
		out.Options = append(out.Options, credentialSourceOption{Value: o.Value, Label: o.Label})
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func refNames(refs []connectorRef) string {
	names := make([]string, len(refs))
	for i, r := range refs {
		names[i] = fmt.Sprintf("%q", r.Name)
	}
	return "connector(s) " + strings.Join(names, ", ")
}
