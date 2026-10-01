package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/api/httpx"
	dataplaneheaders "github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/headers"
	applog "github.com/Tuskira/tusk-ai-secured-gateway/internal/log"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/netguard"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// defaultConnectorTimeoutMS mirrors the postgres backend's column default
// (connectors.timeout_ms DEFAULT 30000), applied here in the handler so
// the behavior is identical regardless of which store.Store backend is
// wired in (the fake store used by tests has no column defaults of its
// own).
const defaultConnectorTimeoutMS = 30000

const maxConnectorSlugLen = 100

// reservedConnectorIdentifier is the connector name/slug Phase 2 reserves
// for the gateway's own native MCP surface (the "gateway__skill" tool and
// native command prompts, see internal/dataplane/orchestrator): a
// connector qualified as "gateway" (client.Qualifier -- its slug, or its
// name when no slug is set) would make its own tools ambiguous with
// those. Checked case-insensitively since a connector's Name (unlike its
// Slug) isn't restricted to lowercase.
const reservedConnectorIdentifier = "gateway"

func isReservedConnectorIdentifier(s string) bool {
	return strings.EqualFold(strings.TrimSpace(s), reservedConnectorIdentifier)
}

// Connectors implements the /api/v1/connectors routes.
type Connectors struct{ Deps }

type connectorRequest struct {
	Name      string         `json:"name"`
	Endpoint  string         `json:"endpoint"`
	TimeoutMS *int           `json:"timeout_ms,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	// Description, when non-nil, is persisted at metadata.description
	// (there is no dedicated column). A pointer distinguishes "omitted,
	// leave unchanged" (nil) from "set to empty string" (non-nil, "").
	Description *string `json:"description,omitempty"`
	// Slug, when non-empty, overrides the name-derived default. Must be
	// lowercase [a-z0-9-]+ (see validateSlug) and unique per tenant.
	Slug string `json:"slug,omitempty"`
}

type connectorView struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Slug      string `json:"slug"`
	Endpoint  string `json:"endpoint"`
	TimeoutMS int    `json:"timeout_ms"`
	Status    string `json:"status"`
	// Capabilities is whatever the last successful health probe recorded
	// from the backend's MCP initialize result: "tools"/"resources"/
	// "prompts" -> bool, "protocol_version" -> string, "server_info" ->
	// {"name","version"} (see internal/dataplane/ops.go's opsAdapter.Probe).
	// Empty until the connector has been probed at least once.
	Capabilities map[string]any `json:"capabilities"`
	// Description mirrors metadata.description for convenient display;
	// it is not a separate stored field.
	// CatalogID is the MCP catalog entry this connector was added from; empty
	// for a hand-registered connector.
	CatalogID   string         `json:"catalog_id,omitempty"`
	Description string         `json:"description,omitempty"`
	Metadata    map[string]any `json:"metadata"`
	CreatedAt   string         `json:"created_at"`
	UpdatedAt   string         `json:"updated_at"`
}

func newConnectorView(c *store.Connector) connectorView {
	caps := c.Capabilities
	if caps == nil {
		caps = map[string]any{}
	}
	desc, _ := c.Metadata["description"].(string)
	return connectorView{
		ID:           c.ID,
		Name:         c.Name,
		Slug:         c.Slug,
		Endpoint:     c.Endpoint,
		TimeoutMS:    c.TimeoutMS,
		Status:       c.Status,
		Capabilities: caps,
		Description:  desc,
		CatalogID:    c.CatalogID,
		Metadata:     maskConnectorMetadata(c.Metadata),
		CreatedAt:    formatTime(c.CreatedAt),
		UpdatedAt:    formatTime(c.UpdatedAt),
	}
}

// maskConnectorMetadata returns a copy of meta with every header config's
// secret material replaced by "***":
//   - a "static" header's "value"
//   - every value inside an "external" header's "config" object (e.g.
//     secret_store's "credential"/"field" args aren't secret themselves,
//     but the mask is applied uniformly rather than trying to
//     second-guess which external providers' config is sensitive)
//
// Everything else in meta (retry, tool_arg_overrides, tls, ...) passes
// through unmodified.
func maskConnectorMetadata(meta map[string]any) map[string]any {
	if meta == nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(meta))
	for k, v := range meta {
		if k == "headers" {
			if headers, ok := v.(map[string]any); ok {
				out[k] = maskHeaderConfigs(headers)
				continue
			}
		}
		out[k] = v
	}
	return out
}

func maskHeaderConfigs(headers map[string]any) map[string]any {
	out := make(map[string]any, len(headers))
	for name, raw := range headers {
		cfg, ok := raw.(map[string]any)
		if !ok {
			out[name] = raw
			continue
		}
		masked := make(map[string]any, len(cfg))
		for k, v := range cfg {
			masked[k] = v
		}
		if masked["type"] == "static" {
			if _, present := masked["value"]; present {
				masked["value"] = "***"
			}
		}
		if sub, ok := masked["config"].(map[string]any); ok {
			maskedSub := make(map[string]any, len(sub))
			for k := range sub {
				maskedSub[k] = "***"
			}
			masked["config"] = maskedSub
		}
		out[name] = masked
	}
	return out
}

// maskedValue is what reads substitute for secret material. A client that
// round-trips a connector through the UI sends it back verbatim for
// fields the user did not change; Update treats it as "keep the stored
// value" so a read-modify-write can never overwrite a real secret with
// the placeholder.
const maskedValue = "***"

// changedHeaders is meta with only the header entries that differ from
// stored. A non-object "headers" is passed through for the shape check.
func changedHeaders(meta map[string]any, stored map[string]any) map[string]any {
	in, ok := meta["headers"].(map[string]any)
	if !ok {
		return meta
	}
	out := map[string]any{}
	for name, cfg := range in {
		if !reflect.DeepEqual(cfg, stored[name]) {
			out[name] = cfg
		}
	}
	return map[string]any{"headers": out}
}

// unmaskHeaderConfigs returns incoming with every maskedValue in
// headers[*].value and headers[*].config[*] replaced by the value at the
// same path in stored (when present). Everything else is left as sent.
func unmaskHeaderConfigs(incoming, stored map[string]any) map[string]any {
	inHeaders, ok := incoming["headers"].(map[string]any)
	if !ok {
		return incoming
	}
	stHeaders, _ := stored["headers"].(map[string]any)
	for name, raw := range inHeaders {
		cfg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		old, _ := stHeaders[name].(map[string]any)
		if cfg["value"] == maskedValue {
			if v, ok := old["value"]; ok {
				cfg["value"] = v
			}
		}
		if sub, ok := cfg["config"].(map[string]any); ok {
			oldSub, _ := old["config"].(map[string]any)
			for k, v := range sub {
				if v == maskedValue {
					if ov, ok := oldSub[k]; ok {
						sub[k] = ov
					}
				}
			}
		}
	}
	return incoming
}

// serverRequestFields are the only keys permitted inside
// metadata.server_requests: which server-initiated MCP requests
// (sampling/createMessage, elicitation/create, roots/list -- see
// internal/dataplane/client.ServerRequestPolicy, which this shape
// feeds) this connector may send back to an agent. Every one defaults
// to false when its key, or the whole object, is absent.
var serverRequestFields = map[string]bool{
	"sampling":    true,
	"elicitation": true,
	"roots":       true,
}

// validateServerRequests rejects an unrecognized key or a non-boolean
// value inside meta["server_requests"]. A missing server_requests
// object, or a missing key within one that's present, is not an error
// -- internal/dataplane/client.ServerRequests treats both as false.
// Unlike headers, this field carries no secret material and is never
// masked (see maskConnectorMetadata).
func validateServerRequests(meta map[string]any) error {
	raw, present := meta["server_requests"]
	if !present {
		return nil
	}
	sr, ok := raw.(map[string]any)
	if !ok {
		return fmt.Errorf("metadata.server_requests must be an object")
	}
	for key, v := range sr {
		if !serverRequestFields[key] {
			return fmt.Errorf("metadata.server_requests: unknown key %q", key)
		}
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("metadata.server_requests.%s must be a boolean", key)
		}
	}
	return nil
}

func (h Connectors) validateAndBuild(ctx context.Context, req connectorRequest) (name, endpoint string, timeoutMS int, meta map[string]any, err error) {
	if err = validateName("name", req.Name); err != nil {
		return
	}
	if isReservedConnectorIdentifier(req.Name) {
		err = fmt.Errorf("name %q is reserved for the gateway's own native tools", req.Name)
		return
	}
	if err = validateAbsoluteHTTPURL(ctx, "endpoint", req.Endpoint); err != nil {
		return
	}
	timeoutMS = h.connectorTimeoutMS()
	if req.TimeoutMS != nil {
		if verr := validateTimeoutMS(*req.TimeoutMS); verr != nil {
			err = verr
			return
		}
		timeoutMS = *req.TimeoutMS
	}
	if req.Slug != "" {
		if verr := validateSlug("slug", req.Slug, maxConnectorSlugLen); verr != nil {
			err = verr
			return
		}
		if isReservedConnectorIdentifier(req.Slug) {
			err = fmt.Errorf("slug %q is reserved for the gateway's own native tools", req.Slug)
			return
		}
	}
	meta = req.Metadata
	if meta == nil {
		meta = map[string]any{}
	}
	if verr := validateServerRequests(meta); verr != nil {
		err = verr
		return
	}
	return req.Name, req.Endpoint, timeoutMS, meta, nil
}

// writeConnectorValidationErr maps a validateAndBuild error to its status:
// egress_blocked and bearer_forwarding_disabled get their own error types,
// everything else is a plain validation_error.
func writeConnectorValidationErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, netguard.ErrBlocked):
		httpx.WriteError(w, http.StatusBadRequest, "egress_blocked", err.Error())
	case errors.Is(err, dataplaneheaders.ErrBearerForwardingDisabled):
		httpx.WriteError(w, http.StatusBadRequest, "bearer_forwarding_disabled", err.Error())
	default:
		httpx.ValidationError(w, err.Error())
	}
}

// Create handles POST /api/v1/connectors. slug, if provided, overrides
// the name-derived default (see connectorRequest.Slug); a duplicate
// slug within the tenant is a 409.
func (h Connectors) Create(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}

	var req connectorRequest
	if !httpx.Decode(w, r, &req) {
		return
	}
	name, endpoint, timeoutMS, meta, err := h.validateAndBuild(r.Context(), req)
	if err == nil {
		err = h.Headers.CheckConnectorHeaders(meta)
	}
	if err != nil {
		writeConnectorValidationErr(w, err)
		return
	}
	if req.Description != nil {
		meta["description"] = *req.Description
	}

	slug := slugify(name, maxConnectorSlugLen)
	if req.Slug != "" {
		slug = req.Slug
	}

	c := &store.Connector{
		TenantID:  tid,
		Name:      name,
		Slug:      slug,
		Endpoint:  endpoint,
		TimeoutMS: timeoutMS,
		Metadata:  meta,
	}
	if err := h.Store.Connectors().Create(r.Context(), c); err != nil {
		writeStoreErr(w, r, "create connector", err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, newConnectorView(c))
}

// List handles GET /api/v1/connectors.
func (h Connectors) List(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}

	connectors, err := h.Store.Connectors().List(r.Context(), tid)
	if err != nil {
		writeStoreErr(w, r, "list connectors", err)
		return
	}

	page := httpx.ParsePagination(r)
	views := make([]connectorView, 0, len(connectors))
	for _, c := range connectors {
		views = append(views, newConnectorView(c))
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page{Items: paginate(views, page), Total: len(views)})
}

// Get handles GET /api/v1/connectors/{id}.
func (h Connectors) Get(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}
	c, err := h.Store.Connectors().Get(r.Context(), tid, chi.URLParam(r, "id"))
	if err != nil {
		writeStoreErr(w, r, "get connector", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, newConnectorView(c))
}

// Update handles PUT /api/v1/connectors/{id}. Name/endpoint/timeout_ms
// follow full-replace PUT semantics; metadata, if omitted from the
// request body, is left as-is rather than wiped (JSON null/absence both
// decode to a nil map, indistinguishable from "not provided", so this is
// the safer default for a field that commonly holds hand-authored header
// configs nobody wants to accidentally erase). description and slug are
// both optional overrides applied independently of metadata: omitting
// either leaves it unchanged. A slug that collides with another
// connector in the tenant is a 409.
func (h Connectors) Update(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")

	existing, err := h.Store.Connectors().Get(r.Context(), tid, id)
	if err != nil {
		writeStoreErr(w, r, "update connector", err)
		return
	}

	var req connectorRequest
	if !httpx.Decode(w, r, &req) {
		return
	}
	name, endpoint, timeoutMS, meta, verr := h.validateAndBuild(r.Context(), req)
	if verr != nil {
		writeConnectorValidationErr(w, verr)
		return
	}

	existing.Name = name
	existing.Endpoint = endpoint
	existing.TimeoutMS = timeoutMS
	if req.Metadata != nil {
		storedHeaders, _ := existing.Metadata["headers"].(map[string]any)
		existing.Metadata = unmaskHeaderConfigs(meta, existing.Metadata)
		// Checked after unmasking (a read's "***" sent back passes), and only
		// the headers this request adds or changes: one saved before save-time
		// validation existed must not block an edit or Enable/Disable, which
		// send every header back.
		if err := h.Headers.CheckConnectorHeaders(changedHeaders(existing.Metadata, storedHeaders)); err != nil {
			writeConnectorValidationErr(w, err)
			return
		}
	}
	if req.Description != nil {
		if existing.Metadata == nil {
			existing.Metadata = map[string]any{}
		}
		existing.Metadata["description"] = *req.Description
	}
	if req.Slug != "" {
		existing.Slug = req.Slug
	}

	if err := h.Store.Connectors().Update(r.Context(), existing); err != nil {
		writeStoreErr(w, r, "update connector", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, newConnectorView(existing))
}

// Delete handles DELETE /api/v1/connectors/{id} (soft delete).
//
// It also invalidates the connector's tool_cache rows (Deps.CacheOps),
// so a deleted connector's tools don't keep answering MCP tools/list for
// up to the cache's TTL. That call is made *before* the soft delete, not
// after: CacheOps.Invalidate re-resolves the connector through the same
// tenant-scoped store Get the soft delete flips to "not found" (see
// ConnectorStore.Get's deleted_at IS NULL filter), so calling it once the
// connector is already gone would just fail every time. It is
// best-effort either way -- a failure here is logged, not returned, since
// the delete itself is what the caller is waiting on. If it's missed
// entirely (CacheOps nil) or the process crashes between the two writes,
// the store's reads (internal/store/postgres/tool_cache.go) exclude a
// soft-deleted connector's rows on their own, so a stale tools/list is
// never served.
func (h Connectors) Delete(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")

	if h.CacheOps != nil {
		if err := h.CacheOps.Invalidate(r.Context(), tid, id); err != nil {
			applog.From(r.Context()).Warn("invalidate tool cache before connector delete failed", "connector_id", id, "error", err)
		}
	}

	if err := h.Store.Connectors().SoftDelete(r.Context(), tid, id); err != nil {
		writeStoreErr(w, r, "delete connector", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type cachedToolView struct {
	ConnectorID   string         `json:"connector_id"`
	ToolNamespace string         `json:"tool_namespace,omitempty"`
	ToolName      string         `json:"tool_name"`
	Description   string         `json:"description,omitempty"`
	InputSchema   map[string]any `json:"input_schema,omitempty"`
	CachedAt      string         `json:"cached_at"`
	ExpiresAt     string         `json:"expires_at"`
	Stale         bool           `json:"stale"`
}

// Tools handles GET /api/v1/connectors/{id}/tools: the cached tool list
// advertised by this connector (internal/dataplane populates the cache;
// this route only reads it).
func (h Connectors) Tools(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")

	// Scope/existence check: a tools list for a connector the caller
	// can't see (wrong tenant, deleted) should 404, not return an
	// (empty) list.
	if _, err := h.Store.Connectors().Get(r.Context(), tid, id); err != nil {
		writeStoreErr(w, r, "get connector tools", err)
		return
	}

	tools, err := h.Store.ToolCache().ListByConnector(r.Context(), tid, id)
	if err != nil {
		writeStoreErr(w, r, "list connector tools", err)
		return
	}

	views := make([]cachedToolView, 0, len(tools))
	for _, t := range tools {
		views = append(views, newCachedToolView(t))
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page{Items: views, Total: len(views)})
}

// newCachedToolView renders one store.CachedTool row for a JSON response.
func newCachedToolView(t *store.CachedTool) cachedToolView {
	return cachedToolView{
		ConnectorID:   t.ConnectorID,
		ToolNamespace: t.ToolNamespace,
		ToolName:      t.ToolName,
		Description:   t.Description,
		InputSchema:   t.InputSchema,
		CachedAt:      formatTime(t.CachedAt),
		ExpiresAt:     formatTime(t.ExpiresAt),
		Stale:         t.Stale,
	}
}

// healthView is the JSON shape returned by GET /connectors/{id}/health.
type healthView struct {
	Status       string         `json:"status"`
	LatencyMS    int64          `json:"latency_ms"`
	Capabilities map[string]any `json:"capabilities,omitempty"`
	CheckedAt    string         `json:"checked_at"`
	Error        string         `json:"error,omitempty"`
}

// Health handles GET /api/v1/connectors/{id}/health: an MCP initialize
// probe against the connector, which also persists its health status via
// the router's MarkHealthy/MarkUnhealthy (see internal/dataplane/ops.go).
// Answers 503 when no data plane was wired in (see opsUnavailableMessage).
func (h Connectors) Health(w http.ResponseWriter, r *http.Request) {
	if h.ConnectorOps == nil {
		writeOpsUnavailable(w)
		return
	}
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}

	result, err := h.ConnectorOps.Probe(r.Context(), tid, chi.URLParam(r, "id"))
	if err != nil {
		writeStoreErr(w, r, "probe connector", err)
		return
	}

	httpx.WriteJSON(w, http.StatusOK, healthView{
		Status:       result.Status,
		LatencyMS:    result.LatencyMS,
		Capabilities: result.Capabilities,
		CheckedAt:    formatTime(result.CheckedAt),
		Error:        result.Error,
	})
}

// Discover handles POST /api/v1/connectors/{id}/discover: a live
// tools/list against the connector, written to the tool cache. Answers
// 503 when no data plane was wired in (see opsUnavailableMessage).
func (h Connectors) Discover(w http.ResponseWriter, r *http.Request) {
	if h.ConnectorOps == nil {
		writeOpsUnavailable(w)
		return
	}
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}

	tools, err := h.ConnectorOps.Discover(r.Context(), tid, chi.URLParam(r, "id"))
	if err != nil {
		writeStoreErr(w, r, "discover connector tools", err)
		return
	}

	views := make([]cachedToolView, 0, len(tools))
	for i := range tools {
		views = append(views, newCachedToolView(&tools[i]))
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page{Items: views, Total: len(views)})
}
