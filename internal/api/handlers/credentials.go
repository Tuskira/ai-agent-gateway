package handlers

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/api/httpx"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// Credentials implements the /api/v1/credentials routes. Every response
// shape here is metadata-only (name, type, field_names, key_id,
// rotated_at, created_at) -- the decrypted payload, ciphertext, and nonce
// are never serialized, matching store.CredentialStore.List's contract
// and internal/secrets.Service's documented behavior.
//
// Like APIKeys, every route here is mounted behind permission
// "admin.manage" rather than "credential.*", so an "agent" principal's
// "*.read" grant can't be used to enumerate credential names/metadata for
// a tenant (see the longer explanation on the APIKeys type).
type Credentials struct{ Deps }

const maxTypeLen = 50

type createCredentialRequest struct {
	Name    string            `json:"name"`
	Type    string            `json:"type"`
	Payload map[string]string `json:"payload"`
}

type rotateCredentialRequest struct {
	Payload map[string]string `json:"payload"`
}

type credentialView struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Type       string   `json:"type"`
	FieldNames []string `json:"field_names"`
	KeyID      string   `json:"key_id"`
	// UsedBy is every connector in the tenant whose metadata.headers
	// references this credential via an external/secret_store header
	// (see credentialUsage). Empty, never null, when nothing references
	// it.
	UsedBy    []connectorRef `json:"used_by"`
	CreatedAt string         `json:"created_at"`
	RotatedAt string         `json:"rotated_at,omitempty"`
}

// connectorRef is the minimal connector identity surfaced on a
// credential's used_by list: enough for a UI to link to the connector
// without re-fetching and masking its full view.
type connectorRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func newCredentialView(c *store.Credential, usedBy []connectorRef) credentialView {
	fields := c.FieldNames
	if fields == nil {
		fields = []string{}
	}
	if usedBy == nil {
		usedBy = []connectorRef{}
	}
	return credentialView{
		ID:         c.ID,
		Name:       c.Name,
		Type:       c.Type,
		FieldNames: fields,
		KeyID:      c.KeyID,
		UsedBy:     usedBy,
		CreatedAt:  formatTime(c.CreatedAt),
		RotatedAt:  formatTimePtr(c.RotatedAt),
	}
}

// credentialUsage indexes conns by which credential name each one's
// secret_store-backed headers reference, so a credential list/get
// handler can attach "used_by" without an O(connectors) scan per
// credential. A header counts when it is shaped
// {"type":"external","provider":"secret_store","config":{"credential":"<name>"}}
// (see maskHeaderConfigs/unmaskHeaderConfigs for the same header shape
// elsewhere); any other header type or provider is ignored.
func credentialUsage(conns []*store.Connector) map[string][]connectorRef {
	usage := make(map[string][]connectorRef)
	for _, c := range conns {
		headers, ok := c.Metadata["headers"].(map[string]any)
		if !ok {
			continue
		}
		for _, raw := range headers {
			cfg, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if cfg["type"] != "external" || cfg["provider"] != "secret_store" {
				continue
			}
			sub, ok := cfg["config"].(map[string]any)
			if !ok {
				continue
			}
			name, _ := sub["credential"].(string)
			if name == "" {
				continue
			}
			usage[name] = append(usage[name], connectorRef{ID: c.ID, Name: c.Name})
		}
	}
	return usage
}

// usageForTenant loads every connector in tid (soft-deleted ones are
// already excluded by ConnectorStore.List) and indexes them by
// credential name via credentialUsage.
func (h Credentials) usageForTenant(ctx context.Context, tid string) (map[string][]connectorRef, error) {
	conns, err := h.Store.Connectors().List(ctx, tid)
	if err != nil {
		return nil, err
	}
	return credentialUsage(conns), nil
}

func validatePayload(payload map[string]string) error {
	if len(payload) == 0 {
		return errRequired("payload")
	}
	for k := range payload {
		if k == "" {
			return errRequired("payload field name")
		}
	}
	return nil
}

// Create handles POST /api/v1/credentials.
func (h Credentials) Create(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}

	var req createCredentialRequest
	if !httpx.Decode(w, r, &req) {
		return
	}
	if err := validateName("name", req.Name); err != nil {
		httpx.ValidationError(w, err.Error())
		return
	}
	if err := validateName("type", req.Type); err != nil {
		httpx.ValidationError(w, err.Error())
		return
	}
	if len([]rune(req.Type)) > maxTypeLen {
		httpx.ValidationError(w, "type must be at most 50 characters")
		return
	}
	if err := validatePayload(req.Payload); err != nil {
		httpx.ValidationError(w, err.Error())
		return
	}

	cred, err := h.Secrets.Create(r.Context(), tid, req.Name, req.Type, req.Payload, principalSubject(r))
	if err != nil {
		writeStoreErr(w, r, "create credential", err)
		return
	}

	usage, err := h.usageForTenant(r.Context(), tid)
	if err != nil {
		writeStoreErr(w, r, "create credential (used_by)", err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, newCredentialView(cred, usage[cred.Name]))
}

// List handles GET /api/v1/credentials.
func (h Credentials) List(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}

	creds, err := h.Secrets.List(r.Context(), tid)
	if err != nil {
		writeStoreErr(w, r, "list credentials", err)
		return
	}
	usage, err := h.usageForTenant(r.Context(), tid)
	if err != nil {
		writeStoreErr(w, r, "list credentials", err)
		return
	}

	page := httpx.ParsePagination(r)
	views := make([]credentialView, 0, len(creds))
	for _, c := range creds {
		views = append(views, newCredentialView(c, usage[c.Name]))
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page{Items: paginate(views, page), Total: len(views)})
}

// Get handles GET /api/v1/credentials/{name}. It reads the row directly
// from the credential store (metadata only, same as List) rather than
// through secrets.Service.Get, which decrypts -- there is no need to
// touch the ring at all to answer a metadata request.
func (h Credentials) Get(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}
	name := chi.URLParam(r, "name")

	cred, err := h.Store.Credentials().Get(r.Context(), tid, name)
	if err != nil {
		writeStoreErr(w, r, "get credential", err)
		return
	}
	usage, err := h.usageForTenant(r.Context(), tid)
	if err != nil {
		writeStoreErr(w, r, "get credential", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, newCredentialView(cred, usage[cred.Name]))
}

// Rotate handles PUT /api/v1/credentials/{name}.
func (h Credentials) Rotate(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}
	name := chi.URLParam(r, "name")

	var req rotateCredentialRequest
	if !httpx.Decode(w, r, &req) {
		return
	}
	if err := validatePayload(req.Payload); err != nil {
		httpx.ValidationError(w, err.Error())
		return
	}

	if err := h.Secrets.Rotate(r.Context(), tid, name, req.Payload); err != nil {
		writeStoreErr(w, r, "rotate credential", err)
		return
	}

	cred, err := h.Store.Credentials().Get(r.Context(), tid, name)
	if err != nil {
		writeStoreErr(w, r, "rotate credential (reload)", err)
		return
	}
	usage, err := h.usageForTenant(r.Context(), tid)
	if err != nil {
		writeStoreErr(w, r, "rotate credential (used_by)", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, newCredentialView(cred, usage[cred.Name]))
}

// Delete handles DELETE /api/v1/credentials/{name}.
func (h Credentials) Delete(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}
	name := chi.URLParam(r, "name")

	if err := h.Secrets.Delete(r.Context(), tid, name); err != nil {
		writeStoreErr(w, r, "delete credential", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
