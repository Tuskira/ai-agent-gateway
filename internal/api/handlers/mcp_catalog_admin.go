package handlers

import (
	"context"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/api/httpx"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/config"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// catalogEntryRequest is the body of POST and PUT on the catalog. Slug and
// Scope are create-only (a PUT addresses the row by its path slug and
// cannot move it); everything else is full-replace.
type catalogEntryRequest struct {
	Slug string `json:"slug,omitempty"`
	// Scope is "tenant" (default) or "platform", which needs platform.admin.
	Scope          string               `json:"scope,omitempty"`
	Name           string               `json:"name"`
	Description    string               `json:"description"`
	Icon           string               `json:"icon"`
	Category       string               `json:"category"`
	URL            string               `json:"url"`
	URLOverridable bool                 `json:"url_overridable"`
	Transport      string               `json:"transport,omitempty"`
	Auth           store.MCPCatalogAuth `json:"auth"`
	DefaultHeaders map[string]string    `json:"default_headers"`
	SuggestedTools []string             `json:"suggested_tools"`
	DocsURL        string               `json:"docs_url"`
	Enabled        *bool                `json:"enabled,omitempty"`
}

const platformCatalogReadOnly = "platform catalog entries are read-only without platform.admin; create a tenant entry with the same slug to override it"

// validate runs the same checks as the config seed, so an entry written
// through the API is held to exactly the rules a seeded one is.
func (req catalogEntryRequest) validate(ctx context.Context, slug string) error {
	seed := config.MCPCatalogSeed{
		Slug: slug, Name: strings.TrimSpace(req.Name), Description: req.Description, Icon: req.Icon, Category: req.Category,
		URL: req.URL, URLOverridable: req.URLOverridable, Transport: req.Transport, DefaultHeaders: req.DefaultHeaders,
		SuggestedTools: req.SuggestedTools, DocsURL: req.DocsURL,
		Auth: config.MCPCatalogAuth{Kind: req.Auth.Kind},
	}
	for _, f := range req.Auth.Fields {
		seed.Auth.Fields = append(seed.Auth.Fields, config.MCPCatalogField(f))
	}
	if ht := req.Auth.HeaderTemplate; ht != nil {
		seed.Auth.HeaderTemplate = &config.MCPCatalogHeaderSpec{Name: ht.Name, Prefix: ht.Prefix}
	}
	return config.ValidateMCPCatalogSeed(ctx, seed)
}

func (req catalogEntryRequest) apply(e *store.MCPCatalogEntry) {
	e.Name = strings.TrimSpace(req.Name)
	e.Description, e.Icon, e.Category = req.Description, req.Icon, req.Category
	e.URL, e.URLOverridable = req.URL, req.URLOverridable
	e.Transport = req.Transport
	e.Auth = req.Auth
	e.DefaultHeaders, e.SuggestedTools, e.DocsURL = req.DefaultHeaders, req.SuggestedTools, req.DocsURL
	if req.Enabled != nil {
		e.Enabled = *req.Enabled
	}
}

// Create handles POST /api/v1/mcp-catalog: a new entry owned by the
// caller's tenant, or (scope "platform", platform.admin only) a platform
// entry every tenant sees. A slug already live in that scope is 409.
func (h MCPCatalog) Create(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}
	var req catalogEntryRequest
	if !httpx.Decode(w, r, &req) {
		return
	}
	owner := tid
	switch req.Scope {
	case "", "tenant":
	case "platform":
		if !h.canWritePlatformCatalog(r) {
			httpx.WriteError(w, http.StatusForbidden, httpx.TypePermission, "writing platform catalog entries requires platform.admin")
			return
		}
		owner = ""
	default:
		httpx.ValidationError(w, `scope must be "tenant" or "platform"`)
		return
	}
	if err := validateSlug("slug", req.Slug, maxConnectorSlugLen); err != nil {
		httpx.ValidationError(w, err.Error())
		return
	}
	if err := req.validate(r.Context(), req.Slug); err != nil {
		httpx.ValidationError(w, err.Error())
		return
	}

	e := &store.MCPCatalogEntry{TenantID: owner, Slug: req.Slug, Enabled: true}
	req.apply(e)
	if err := h.Store.MCPCatalog().Create(r.Context(), e); err != nil {
		writeStoreErr(w, r, "create mcp catalog entry", err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, newCatalogEntryView(e, nil))
}

// writableEntry resolves the {slug} a PUT/DELETE targets and enforces who
// may touch it. By default that is the row the caller sees for the slug
// (their own tenant entry, else the platform one); ?scope=platform or
// ?scope=tenant picks explicitly. A platform row needs platform.admin.
func (h MCPCatalog) writableEntry(w http.ResponseWriter, r *http.Request, tid string) (*store.MCPCatalogEntry, bool) {
	slug := chi.URLParam(r, "slug")
	var (
		e   *store.MCPCatalogEntry
		err error
	)
	switch r.URL.Query().Get("scope") {
	case "platform":
		e, err = h.Store.MCPCatalog().GetBySlug(r.Context(), "", slug)
	case "tenant":
		e, err = h.Store.MCPCatalog().GetBySlug(r.Context(), tid, slug)
		if err == nil && e.TenantID == "" {
			err = store.ErrNotFound
		}
	case "":
		e, err = h.Store.MCPCatalog().GetBySlug(r.Context(), tid, slug)
	default:
		httpx.ValidationError(w, `scope must be "tenant" or "platform"`)
		return nil, false
	}
	if err != nil {
		writeStoreErr(w, r, "get mcp catalog entry", err)
		return nil, false
	}
	if e.TenantID == "" && !h.canWritePlatformCatalog(r) {
		httpx.WriteError(w, http.StatusForbidden, httpx.TypePermission, platformCatalogReadOnly)
		return nil, false
	}
	return e, true
}

// Update handles PUT /api/v1/mcp-catalog/{slug}: full replace of the
// entry's fields (the slug and owner never change). An omitted "enabled"
// keeps the current value.
func (h MCPCatalog) Update(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}
	e, ok := h.writableEntry(w, r, tid)
	if !ok {
		return
	}
	var req catalogEntryRequest
	if !httpx.Decode(w, r, &req) {
		return
	}
	if req.Slug != "" && req.Slug != e.Slug {
		httpx.ValidationError(w, "slug cannot be changed")
		return
	}
	if req.Scope != "" {
		httpx.ValidationError(w, "scope is create-only; a PUT cannot move an entry")
		return
	}
	if err := req.validate(r.Context(), e.Slug); err != nil {
		httpx.ValidationError(w, err.Error())
		return
	}
	req.apply(e)
	if err := h.Store.MCPCatalog().Update(r.Context(), e); err != nil {
		writeStoreErr(w, r, "update mcp catalog entry", err)
		return
	}
	conns, _ := h.Store.Connectors().List(r.Context(), tid)
	httpx.WriteJSON(w, http.StatusOK, newCatalogEntryView(e, addedByCatalogID(conns)[e.ID]))
}

// Delete handles DELETE /api/v1/mcp-catalog/{slug} (soft). Connectors
// already added from the entry are untouched; deleting a tenant entry that
// shadows a platform one makes the platform entry visible again.
func (h MCPCatalog) Delete(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}
	e, ok := h.writableEntry(w, r, tid)
	if !ok {
		return
	}
	if err := h.Store.MCPCatalog().SoftDelete(r.Context(), e.TenantID, e.ID); err != nil {
		writeStoreErr(w, r, "delete mcp catalog entry", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
