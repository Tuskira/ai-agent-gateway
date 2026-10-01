package handlers

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/api/httpx"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/config"
	applog "github.com/Tuskira/tusk-ai-secured-gateway/internal/log"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// MCPCatalog implements the /api/v1/mcp-catalog routes: listing the
// platform entries (seeded from config) and the tenant's own, and "add to
// tenant", which creates an ordinary tenant connector from an entry
// (entry writes are in mcp_catalog_admin.go). Nothing in the catalog is
// callable until it is added.
type MCPCatalog struct{ Deps }

const (
	errTypeOAuthNotSupported = "oauth_not_supported"
	errTypeAlreadyAdded      = "already_added"

	// catalogCredentialType is the credential type stamped on the secret an
	// add creates, so a later add can tell its own leftovers from a
	// credential an operator created by hand.
	catalogCredentialType = "mcp_catalog"

	// addProbeTimeout bounds each of the health probe and the tool
	// discovery an add runs after creating the connector: a little over the
	// connector's own default request timeout, so a slow backend is cut off
	// by that and the outcome can still be recorded.
	addProbeTimeout = 35 * time.Second

	maxCatalogFieldValueLen = 4096
)

type catalogAuthView struct {
	Kind           string                  `json:"kind"`
	Fields         []store.MCPCatalogField `json:"fields"`
	HeaderTemplate *store.MCPCatalogHeader `json:"header_template,omitempty"`
}

type catalogEntryView struct {
	ID             string            `json:"id"`
	Slug           string            `json:"slug"`
	Name           string            `json:"name"`
	Description    string            `json:"description"`
	Icon           string            `json:"icon"`
	Category       string            `json:"category"`
	URL            string            `json:"url"`
	URLOverridable bool              `json:"url_overridable"`
	Transport      string            `json:"transport"`
	Auth           catalogAuthView   `json:"auth"`
	DefaultHeaders map[string]string `json:"default_headers"`
	SuggestedTools []string          `json:"suggested_tools"`
	DocsURL        string            `json:"docs_url,omitempty"`
	Enabled        bool              `json:"enabled"`

	// Scope is "platform" for an entry every tenant sees (seeded from
	// config or written by a platform admin) and "tenant" for one this
	// tenant's admins curate. A tenant entry overrides a platform entry of
	// the same slug for that tenant.
	Scope string `json:"scope"`

	// Supported is false for an entry the gateway cannot add yet (today:
	// oauth); UnsupportedReason says why.
	Supported         bool   `json:"supported"`
	UnsupportedReason string `json:"unsupported_reason,omitempty"`

	// Added is true when the calling tenant already has a live connector
	// created from this entry; ConnectorID names it (the oldest one, when a
	// tenant added the entry more than once under different slugs).
	Added       bool   `json:"added"`
	ConnectorID string `json:"connector_id,omitempty"`
}

const oauthUnsupportedReason = "Requires OAuth, which the gateway does not support yet."

func newCatalogEntryView(e *store.MCPCatalogEntry, added *store.Connector) catalogEntryView {
	fields := e.Auth.Fields
	if fields == nil {
		fields = []store.MCPCatalogField{}
	}
	tools := e.SuggestedTools
	if tools == nil {
		tools = []string{}
	}
	hdrs := e.DefaultHeaders
	if hdrs == nil {
		hdrs = map[string]string{}
	}
	v := catalogEntryView{
		ID: e.ID, Slug: e.Slug, Name: e.Name, Description: e.Description, Icon: e.Icon, Category: e.Category,
		URL: e.URL, URLOverridable: e.URLOverridable, Transport: e.Transport,
		Auth:           catalogAuthView{Kind: e.Auth.Kind, Fields: fields, HeaderTemplate: e.Auth.HeaderTemplate},
		DefaultHeaders: hdrs, SuggestedTools: tools, DocsURL: e.DocsURL, Enabled: e.Enabled,
		Scope:     "platform",
		Supported: e.Auth.Kind != config.MCPAuthOAuth,
	}
	if e.TenantID != "" {
		v.Scope = "tenant"
	}
	if !v.Supported {
		v.UnsupportedReason = oauthUnsupportedReason
	}
	if added != nil {
		v.Added, v.ConnectorID = true, added.ID
	}
	return v
}

// addedByCatalogID indexes the tenant's live catalog-derived connectors by
// catalog id, keeping the oldest (List is ordered by creation).
func addedByCatalogID(conns []*store.Connector) map[string]*store.Connector {
	out := map[string]*store.Connector{}
	for _, c := range conns {
		if c.CatalogID == "" {
			continue
		}
		if _, ok := out[c.CatalogID]; !ok {
			out[c.CatalogID] = c
		}
	}
	return out
}

// canWriteCatalog reports whether the caller may manage the tenant's own
// catalog entries (the same grant that creates connectors). Used to decide
// whether disabled entries are shown.
func (h MCPCatalog) canWriteCatalog(r *http.Request) bool {
	if h.Authorizer == nil {
		return false
	}
	p, _ := pkgauth.PrincipalFrom(r.Context())
	return h.Authorizer.Allow(r.Context(), p, "connector.create")
}

// canWritePlatformCatalog reports whether the caller may write PLATFORM
// entries: the platform.admin grant, which is API-key only.
func (h MCPCatalog) canWritePlatformCatalog(r *http.Request) bool {
	if h.Authorizer == nil {
		return false
	}
	p, _ := pkgauth.PrincipalFrom(r.Context())
	return h.Authorizer.Allow(r.Context(), p, "platform.admin")
}

// List handles GET /api/v1/mcp-catalog: the platform entries plus the
// tenant's own (the tenant's wins on a shared slug). Disabled entries are
// shown only to callers who can manage the catalog.
func (h MCPCatalog) List(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}
	entries, err := h.Store.MCPCatalog().List(r.Context(), tid)
	if err != nil {
		writeStoreErr(w, r, "list mcp catalog", err)
		return
	}
	conns, err := h.Store.Connectors().List(r.Context(), tid)
	if err != nil {
		writeStoreErr(w, r, "list mcp catalog (connectors)", err)
		return
	}
	added := addedByCatalogID(conns)
	showDisabled := h.canWriteCatalog(r)

	views := make([]catalogEntryView, 0, len(entries))
	for _, e := range entries {
		if e.Enabled || showDisabled {
			views = append(views, newCatalogEntryView(e, added[e.ID]))
		}
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page{Items: paginate(views, httpx.ParsePagination(r)), Total: len(views)})
}

// Get handles GET /api/v1/mcp-catalog/{slug}.
func (h MCPCatalog) Get(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}
	e, err := h.Store.MCPCatalog().GetBySlug(r.Context(), tid, chi.URLParam(r, "slug"))
	if err == nil && !e.Enabled && !h.canWriteCatalog(r) {
		err = store.ErrNotFound
	}
	if err != nil {
		writeStoreErr(w, r, "get mcp catalog entry", err)
		return
	}
	conns, err := h.Store.Connectors().List(r.Context(), tid)
	if err != nil {
		writeStoreErr(w, r, "get mcp catalog entry (connectors)", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, newCatalogEntryView(e, addedByCatalogID(conns)[e.ID]))
}

// enabledEntry resolves the {slug} an add targets: the tenant's entry, else
// the platform's, and only while enabled.
func (h MCPCatalog) enabledEntry(w http.ResponseWriter, r *http.Request) (*store.MCPCatalogEntry, bool) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return nil, false
	}
	e, err := h.Store.MCPCatalog().GetBySlug(r.Context(), tid, chi.URLParam(r, "slug"))
	if err == nil && !e.Enabled {
		err = store.ErrNotFound
	}
	if err != nil {
		writeStoreErr(w, r, "get mcp catalog entry", err)
		return nil, false
	}
	return e, true
}

type addCatalogRequest struct {
	// Name and Slug override the entry's own (the defaults). A different
	// Slug is how a tenant adds the same entry a second time.
	Name string `json:"name,omitempty"`
	Slug string `json:"slug,omitempty"`
	// Fields holds the values for the entry's auth.fields, by field name.
	Fields map[string]string `json:"fields,omitempty"`
	// URL replaces the entry's URL; only accepted when url_overridable.
	URL string `json:"url,omitempty"`
}

// discoveryView reports what the post-create health probe and tool
// discovery found. Neither failing fails the add: a connector with bad
// credentials is still created, and its health says so.
type discoveryView struct {
	Status          string `json:"status,omitempty"`
	Error           string `json:"error,omitempty"`
	ToolsDiscovered int    `json:"tools_discovered"`
	DiscoveryError  string `json:"discovery_error,omitempty"`
}

type addCatalogResponse struct {
	Connector connectorView `json:"connector"`
	Discovery discoveryView `json:"discovery"`
}

// Add handles POST /api/v1/mcp-catalog/{slug}/add.
func (h MCPCatalog) Add(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}
	entry, ok := h.enabledEntry(w, r)
	if !ok {
		return
	}
	if entry.Auth.Kind == config.MCPAuthOAuth {
		httpx.WriteError(w, http.StatusUnprocessableEntity, errTypeOAuthNotSupported, oauthUnsupportedReason)
		return
	}

	var req addCatalogRequest
	if !httpx.Decode(w, r, &req) {
		return
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = entry.Name
	}
	if err := validateName("name", name); err != nil {
		httpx.ValidationError(w, err.Error())
		return
	}
	if isReservedConnectorIdentifier(name) {
		httpx.ValidationError(w, fmt.Sprintf("name %q is reserved for the gateway's own native tools", name))
		return
	}
	slug := entry.Slug
	if req.Slug != "" {
		slug = req.Slug
		if err := validateSlug("slug", slug, maxConnectorSlugLen); err != nil {
			httpx.ValidationError(w, err.Error())
			return
		}
	}
	if isReservedConnectorIdentifier(slug) {
		httpx.ValidationError(w, fmt.Sprintf("slug %q is reserved for the gateway's own native tools", slug))
		return
	}

	endpoint, credFields, err := resolveCatalogInputs(r.Context(), entry, req)
	if err != nil {
		httpx.ValidationError(w, err.Error())
		return
	}

	conns, err := h.Store.Connectors().List(r.Context(), tid)
	if err != nil {
		writeStoreErr(w, r, "add mcp catalog entry (connectors)", err)
		return
	}
	existingFromEntry := false
	for _, c := range conns {
		if c.CatalogID == entry.ID {
			existingFromEntry = true
		}
		if c.Slug == slug {
			if c.CatalogID == entry.ID {
				httpx.WriteError(w, http.StatusConflict, errTypeAlreadyAdded, fmt.Sprintf("%s is already added as %q; pass a different slug to add it again", entry.Name, c.Slug))
			} else {
				httpx.Conflict(w, fmt.Sprintf("a connector with slug %q already exists", slug))
			}
			return
		}
	}
	if existingFromEntry && req.Slug == "" {
		httpx.WriteError(w, http.StatusConflict, errTypeAlreadyAdded, fmt.Sprintf("%s is already added; pass a different slug to add it again", entry.Name))
		return
	}

	meta := map[string]any{"description": entry.Description}
	headers := map[string]any{}
	for k, v := range entry.DefaultHeaders {
		headers[k] = map[string]any{"type": "static", "value": v}
	}

	var createdCred string
	if hdrName, hdrValue, has := buildAuthHeader(entry.Auth, credFields); has {
		credName, created, err := h.storeCatalogCredential(r.Context(), tid, slug, hdrName, hdrValue, principalSubject(r))
		if err != nil {
			writeStoreErr(w, r, "add mcp catalog entry (credential)", err)
			return
		}
		if created {
			createdCred = credName
		}
		headers[hdrName] = map[string]any{
			"type":     "external",
			"provider": "secret_store",
			"config":   map[string]any{"credential": credName, "field": credentialFieldFor(hdrName)},
		}
	}
	if len(headers) > 0 {
		meta["headers"] = headers
	}

	c := &store.Connector{
		TenantID: tid, Name: name, Slug: slug, Endpoint: endpoint, TimeoutMS: h.connectorTimeoutMS(),
		Metadata: meta, CatalogID: entry.ID,
	}
	if err := h.Store.Connectors().Create(r.Context(), c); err != nil {
		if createdCred != "" {
			if derr := h.Secrets.Delete(r.Context(), tid, createdCred); derr != nil {
				applog.From(r.Context()).Warn("remove credential after failed catalog add", "credential", createdCred, "error", derr)
			}
		}
		writeStoreErr(w, r, "add mcp catalog entry", err)
		return
	}

	disc := h.probeAndDiscover(r.Context(), tid, c.ID)
	if fresh, err := h.Store.Connectors().Get(r.Context(), tid, c.ID); err == nil {
		c = fresh
	}
	httpx.WriteJSON(w, http.StatusCreated, addCatalogResponse{Connector: newConnectorView(c), Discovery: disc})
}

// probeAndDiscover runs the same health probe and tool discovery the
// connector's own /health and /discover routes do, each under its own
// timeout. Discovery is skipped when the probe did not come back healthy
// (there is nothing to list from a backend that just refused the
// credentials). Best-effort: with no data plane wired in, or a backend that
// rejects the credentials, it just reports what happened.
func (h MCPCatalog) probeAndDiscover(ctx context.Context, tid, id string) discoveryView {
	var d discoveryView
	if h.ConnectorOps == nil {
		return d
	}

	probeCtx, cancelProbe := context.WithTimeout(ctx, addProbeTimeout)
	res, err := h.ConnectorOps.Probe(probeCtx, tid, id)
	cancelProbe()
	if err != nil {
		d.Status, d.Error = "unknown", "health probe failed"
		applog.From(ctx).Warn("catalog add: health probe failed", "connector_id", id, "error", err)
		return d
	}
	d.Status, d.Error = res.Status, res.Error
	if res.Status != "healthy" {
		return d
	}

	discoverCtx, cancelDiscover := context.WithTimeout(ctx, addProbeTimeout)
	defer cancelDiscover()
	tools, err := h.ConnectorOps.Discover(discoverCtx, tid, id)
	if err != nil {
		d.DiscoveryError = "tool discovery failed"
		applog.From(ctx).Warn("catalog add: tool discovery failed", "connector_id", id, "error", err)
		return d
	}
	d.ToolsDiscovered = len(tools)
	return d
}

// resolveCatalogInputs validates the add request against the entry's field
// list and returns the connector endpoint (the entry's or the tenant's
// override, with query-routed fields applied) and the credential-bound
// field values by name.
func resolveCatalogInputs(ctx context.Context, entry *store.MCPCatalogEntry, req addCatalogRequest) (endpoint string, cred map[string]string, err error) {
	known := make(map[string]store.MCPCatalogField, len(entry.Auth.Fields))
	for _, f := range entry.Auth.Fields {
		known[f.Name] = f
	}
	names := make([]string, 0, len(req.Fields))
	for n := range req.Fields {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if _, ok := known[n]; !ok {
			return "", nil, fmt.Errorf("fields: unknown field %q", n)
		}
	}

	base := entry.URL
	if req.URL != "" {
		if !entry.URLOverridable {
			return "", nil, errors.New("url cannot be overridden for this entry")
		}
		base = strings.TrimSpace(req.URL)
	}
	if verr := config.ValidateMCPCatalogURL(ctx, "url", base); verr != nil {
		return "", nil, verr
	}
	u, _ := url.Parse(base)

	cred = map[string]string{}
	query := u.Query()
	for _, f := range entry.Auth.Fields {
		v := strings.TrimSpace(req.Fields[f.Name])
		if v == "" {
			if f.Required {
				return "", nil, fmt.Errorf("fields.%s (%s) is required", f.Name, f.Label)
			}
			continue
		}
		if len(v) > maxCatalogFieldValueLen {
			return "", nil, fmt.Errorf("fields.%s is too long", f.Name)
		}
		if strings.ContainsAny(v, "\r\n\x00") {
			return "", nil, fmt.Errorf("fields.%s must not contain control characters", f.Name)
		}
		if f.Query != "" {
			query.Set(f.Query, v)
		} else {
			cred[f.Name] = v
		}
	}
	u.RawQuery = query.Encode()
	return u.String(), cred, nil
}

// buildAuthHeader returns the outbound auth header an entry's kind calls
// for, given the validated credential field values. has is false for kinds
// that send none (none, and oauth, which Add has already refused).
func buildAuthHeader(auth store.MCPCatalogAuth, cred map[string]string) (name, value string, has bool) {
	name, prefix := "Authorization", ""
	if ht := auth.HeaderTemplate; ht != nil {
		if ht.Name != "" {
			name = ht.Name
		}
		prefix = ht.Prefix
	}
	var ordered []string
	for _, f := range auth.Fields {
		if f.Query != "" {
			continue
		}
		if cred[f.Name] == "" {
			// An optional credential left blank (Context7's API key, say):
			// send no auth header at all rather than an empty one.
			return "", "", false
		}
		ordered = append(ordered, cred[f.Name])
	}
	switch auth.Kind {
	case config.MCPAuthBearer:
		if len(ordered) != 1 {
			return "", "", false
		}
		if prefix == "" {
			prefix = "Bearer "
		}
		return name, prefix + ordered[0], true
	case config.MCPAuthBasic:
		if len(ordered) != 2 {
			return "", "", false
		}
		return name, "Basic " + base64.StdEncoding.EncodeToString([]byte(ordered[0]+":"+ordered[1])), true
	case config.MCPAuthHeader:
		if len(ordered) != 1 {
			return "", "", false
		}
		return name, prefix + ordered[0], true
	}
	return "", "", false
}

func credentialFieldFor(headerName string) string { return strings.ToLower(headerName) }

// storeCatalogCredential saves the computed header value as a tenant
// credential named "mcp-<slug>" and returns the name used. The value is the
// finished header (e.g. "Basic ..."), so the connector's header is a plain
// secret_store reference and the secret is never echoed. created is false
// when an unused leftover of an earlier add (same credential type, no
// connector referencing it) was rotated in place instead; a credential an
// operator made by hand, or one a live connector uses, is never touched and
// the next free "-N" name is used.
func (h MCPCatalog) storeCatalogCredential(ctx context.Context, tid, slug, headerName, value, createdBy string) (name string, created bool, err error) {
	payload := map[string]string{credentialFieldFor(headerName): value}
	base := "mcp-" + slug
	for i := 1; i <= 20; i++ {
		name = base
		if i > 1 {
			name = fmt.Sprintf("%s-%d", base, i)
		}
		_, cerr := h.Secrets.Create(ctx, tid, name, catalogCredentialType, payload, createdBy)
		if cerr == nil {
			return name, true, nil
		}
		if !errors.Is(cerr, store.ErrConflict) {
			return "", false, cerr
		}
		reusable, rerr := h.reusableCatalogCredential(ctx, tid, name)
		if rerr != nil {
			return "", false, rerr
		}
		if reusable {
			if err := h.Secrets.Rotate(ctx, tid, name, payload); err != nil {
				return "", false, err
			}
			return name, false, nil
		}
	}
	return "", false, fmt.Errorf("no free credential name for %q", base)
}

func (h MCPCatalog) reusableCatalogCredential(ctx context.Context, tid, name string) (bool, error) {
	creds, err := h.Secrets.List(ctx, tid)
	if err != nil {
		return false, err
	}
	for _, c := range creds {
		if c.Name != name {
			continue
		}
		if c.Type != catalogCredentialType {
			return false, nil
		}
		conns, err := h.Store.Connectors().List(ctx, tid)
		if err != nil {
			return false, err
		}
		_, inUse := credentialUsage(conns)[name]
		return !inUse, nil
	}
	return false, nil
}
