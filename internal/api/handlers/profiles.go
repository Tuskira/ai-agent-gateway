package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/api/httpx"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/profile"
	applog "github.com/Tuskira/tusk-ai-secured-gateway/internal/log"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// maxInstructionsBytes bounds AgentProfile.Instructions: free text
// prepended to the resolved profile's MCP initialize instructions (see
// internal/dataplane/orchestrator), not a place for a skill's worth of
// prose.
const maxInstructionsBytes = 8 * 1024

func validateInstructions(s string) error {
	if len(s) > maxInstructionsBytes {
		return fmt.Errorf("instructions must be at most %d bytes", maxInstructionsBytes)
	}
	return nil
}

// invalidateProfile best-effort drops a profile's cached MCP resolution
// (internal/dataplane/profile.Enforcer) so a write to its tools,
// instructions or skills takes effect immediately on this replica rather
// than waiting out the enforcer's 30s TTL. A nil Deps.ProfileOps (an api
// pod without the MCP plane wired in) or a failed lookup is logged, not
// returned -- the write itself already succeeded and is what the caller
// is waiting on.
func (h Profiles) invalidateProfile(ctx context.Context, tenantID, profileID string) {
	if h.ProfileOps == nil {
		return
	}
	if err := h.ProfileOps.InvalidateProfile(ctx, tenantID, profileID); err != nil {
		applog.From(ctx).Warn("invalidate profile cache failed", "profile_id", profileID, "error", err)
	}
}

// Profiles implements the /api/v1/profiles routes.
type Profiles struct{ Deps }

type profileRequest struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
	// Instructions is free text prepended to the resolved profile's MCP
	// initialize instructions (see internal/dataplane/orchestrator); at
	// most 8 KiB (validateInstructions). Omitted/empty means none.
	Instructions string `json:"instructions,omitempty"`
}

type profileView struct {
	ID           string         `json:"id"`
	Name         string         `json:"name"`
	Slug         string         `json:"slug"`
	Description  string         `json:"description,omitempty"`
	Instructions string         `json:"instructions,omitempty"`
	Metadata     map[string]any `json:"metadata"`
	CreatedAt    string         `json:"created_at"`
	UpdatedAt    string         `json:"updated_at"`
}

func newProfileView(p *store.AgentProfile) profileView {
	meta := p.Metadata
	if meta == nil {
		meta = map[string]any{}
	}
	return profileView{
		ID:           p.ID,
		Name:         p.Name,
		Slug:         p.Slug,
		Description:  p.Description,
		Instructions: p.Instructions,
		Metadata:     meta,
		CreatedAt:    formatTime(p.CreatedAt),
		UpdatedAt:    formatTime(p.UpdatedAt),
	}
}

// Create handles POST /api/v1/profiles.
func (h Profiles) Create(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}

	var req profileRequest
	if !httpx.Decode(w, r, &req) {
		return
	}
	if err := validateName("name", req.Name); err != nil {
		httpx.ValidationError(w, err.Error())
		return
	}
	if err := validateInstructions(req.Instructions); err != nil {
		httpx.ValidationError(w, err.Error())
		return
	}

	p := &store.AgentProfile{
		TenantID:     tid,
		Name:         req.Name,
		Slug:         profile.Slug(tid, req.Name),
		Description:  req.Description,
		Metadata:     req.Metadata,
		Instructions: req.Instructions,
	}
	if err := h.Store.AgentProfiles().Create(r.Context(), p); err != nil {
		writeStoreErr(w, r, "create profile", err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, newProfileView(p))
}

// List handles GET /api/v1/profiles.
func (h Profiles) List(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}

	profiles, err := h.Store.AgentProfiles().List(r.Context(), tid)
	if err != nil {
		writeStoreErr(w, r, "list profiles", err)
		return
	}

	page := httpx.ParsePagination(r)
	views := make([]profileView, 0, len(profiles))
	for _, p := range profiles {
		views = append(views, newProfileView(p))
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page{Items: paginate(views, page), Total: len(views)})
}

// Get handles GET /api/v1/profiles/{id}.
func (h Profiles) Get(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}
	p, err := h.Store.AgentProfiles().Get(r.Context(), tid, chi.URLParam(r, "id"))
	if err != nil {
		writeStoreErr(w, r, "get profile", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, newProfileView(p))
}

// Update handles PUT /api/v1/profiles/{id}. Slug is immutable once
// created (matching Connectors.Update's treatment of slug).
func (h Profiles) Update(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")

	existing, err := h.Store.AgentProfiles().Get(r.Context(), tid, id)
	if err != nil {
		writeStoreErr(w, r, "update profile", err)
		return
	}

	var req profileRequest
	if !httpx.Decode(w, r, &req) {
		return
	}
	if err := validateName("name", req.Name); err != nil {
		httpx.ValidationError(w, err.Error())
		return
	}
	if err := validateInstructions(req.Instructions); err != nil {
		httpx.ValidationError(w, err.Error())
		return
	}

	existing.Name = req.Name
	existing.Description = req.Description
	existing.Instructions = req.Instructions
	if req.Metadata != nil {
		existing.Metadata = req.Metadata
	}

	if err := h.Store.AgentProfiles().Update(r.Context(), existing); err != nil {
		writeStoreErr(w, r, "update profile", err)
		return
	}
	h.invalidateProfile(r.Context(), tid, id)
	httpx.WriteJSON(w, http.StatusOK, newProfileView(existing))
}

// Delete handles DELETE /api/v1/profiles/{id} (soft delete).
//
// The cache invalidation runs BEFORE the soft delete, mirroring
// Connectors.Delete: InvalidateProfile re-resolves the profile through
// the same tenant-scoped Get the soft delete flips to "not found", so
// calling it after would just fail every time.
func (h Profiles) Delete(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")

	h.invalidateProfile(r.Context(), tid, id)

	if err := h.Store.AgentProfiles().SoftDelete(r.Context(), tid, id); err != nil {
		writeStoreErr(w, r, "delete profile", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type profileToolRequest struct {
	ConnectorID   string `json:"connector_id"`
	ToolName      string `json:"tool_name"`
	ToolNamespace string `json:"tool_namespace,omitempty"`
}

type setProfileToolsRequest struct {
	Tools []profileToolRequest `json:"tools"`
}

type profileToolView struct {
	ConnectorID   string `json:"connector_id"`
	ToolName      string `json:"tool_name"`
	ToolNamespace string `json:"tool_namespace,omitempty"`
}

// SetTools handles PUT /api/v1/profiles/{id}/tools: replaces the
// profile's entire tool allow-list. Every connector_id referenced must
// exist and belong to the caller's own tenant -- store.AgentProfileStore.
// SetTools itself only enforces a DB foreign key (connector_id must exist
// in SOME tenant), which on its own would let one tenant's profile
// allow-list reference another tenant's connector by id; this handler
// closes that gap by checking ownership before calling SetTools.
func (h Profiles) SetTools(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")

	var req setProfileToolsRequest
	if !httpx.Decode(w, r, &req) {
		return
	}

	tools := make([]store.ProfileTool, 0, len(req.Tools))
	for i, t := range req.Tools {
		if t.ConnectorID == "" {
			httpx.ValidationError(w, fmt.Sprintf("tools[%d].connector_id is required", i))
			return
		}
		if err := validateName("tool_name", t.ToolName); err != nil {
			httpx.ValidationError(w, fmt.Sprintf("tools[%d].%s", i, err.Error()))
			return
		}
		if _, err := h.Store.Connectors().Get(r.Context(), tid, t.ConnectorID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				httpx.ValidationError(w, fmt.Sprintf("tools[%d].connector_id %q does not exist for this tenant", i, t.ConnectorID))
				return
			}
			writeStoreErr(w, r, "set profile tools (verify connector)", err)
			return
		}
		tools = append(tools, store.ProfileTool{
			AgentProfileID: id,
			ConnectorID:    t.ConnectorID,
			ToolNamespace:  t.ToolNamespace,
			ToolName:       t.ToolName,
		})
	}

	if err := h.Store.AgentProfiles().SetTools(r.Context(), tid, id, tools); err != nil {
		writeStoreErr(w, r, "set profile tools", err)
		return
	}
	h.invalidateProfile(r.Context(), tid, id)

	httpx.WriteJSON(w, http.StatusOK, httpx.Page{Items: newProfileToolViews(tools), Total: len(tools)})
}

// GetTools handles GET /api/v1/profiles/{id}/tools.
func (h Profiles) GetTools(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")

	// Existence/scope check: GetTools itself doesn't distinguish
	// "profile has no tools" from "profile doesn't exist" (both return
	// an empty slice, nil error -- see agentProfileStore.GetTools), so
	// confirm the profile exists for this tenant first.
	if _, err := h.Store.AgentProfiles().Get(r.Context(), tid, id); err != nil {
		writeStoreErr(w, r, "get profile tools", err)
		return
	}

	tools, err := h.Store.AgentProfiles().GetTools(r.Context(), tid, id)
	if err != nil {
		writeStoreErr(w, r, "get profile tools", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page{Items: newProfileToolViews(tools), Total: len(tools)})
}

func newProfileToolViews(tools []store.ProfileTool) []profileToolView {
	views := make([]profileToolView, 0, len(tools))
	for _, t := range tools {
		views = append(views, profileToolView{
			ConnectorID:   t.ConnectorID,
			ToolName:      t.ToolName,
			ToolNamespace: t.ToolNamespace,
		})
	}
	return views
}

type profileSkillRequest struct {
	SkillID string `json:"skill_id"`
	// Version pins the attachment to one SkillVersion; omitted/nil means
	// "always resolve to the skill's current latest version" (see
	// store.ProfileSkill).
	Version *int `json:"version,omitempty"`
}

type setProfileSkillsRequest struct {
	Items []profileSkillRequest `json:"items"`
}

type profileSkillView struct {
	SkillID string `json:"skill_id"`
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	// Version is the pinned version, omitted when the attachment floats
	// to LatestVersion.
	Version       *int   `json:"version,omitempty"`
	LatestVersion int    `json:"latest_version"`
	Description   string `json:"description"`
}

// SetSkills handles PUT /api/v1/profiles/{id}/skills: replaces the
// profile's entire skill/command attachment set, the same replace
// semantics as SetTools. Every skill_id must be visible to the caller's
// tenant (a tenant row, or a platform row) -- store.AgentProfileStore.
// SetSkills is not itself tenant-scoped (see its doc comment), so this
// handler is what actually closes that gap, the same division of
// responsibility SetTools' handler applies for connector_id ownership. A
// pinned version must already exist for its skill.
func (h Profiles) SetSkills(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")

	if _, err := h.Store.AgentProfiles().Get(r.Context(), tid, id); err != nil {
		writeStoreErr(w, r, "set profile skills", err)
		return
	}

	var req setProfileSkillsRequest
	if !httpx.Decode(w, r, &req) {
		return
	}

	items := make([]store.ProfileSkill, 0, len(req.Items))
	skillsByID := make(map[string]*store.Skill, len(req.Items))
	for i, it := range req.Items {
		if it.SkillID == "" {
			httpx.ValidationError(w, fmt.Sprintf("items[%d].skill_id is required", i))
			return
		}
		sk, err := h.Store.Skills().Get(r.Context(), tid, it.SkillID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				httpx.ValidationError(w, fmt.Sprintf("items[%d].skill_id %q is not visible to this tenant", i, it.SkillID))
				return
			}
			writeStoreErr(w, r, "set profile skills (verify skill)", err)
			return
		}
		if it.Version != nil {
			if _, err := h.Store.Skills().GetVersion(r.Context(), sk.ID, *it.Version); err != nil {
				if errors.Is(err, store.ErrNotFound) {
					httpx.ValidationError(w, fmt.Sprintf("items[%d].version %d does not exist for skill %q", i, *it.Version, sk.Name))
					return
				}
				writeStoreErr(w, r, "set profile skills (verify version)", err)
				return
			}
		}
		skillsByID[it.SkillID] = sk
		items = append(items, store.ProfileSkill{AgentProfileID: id, SkillID: it.SkillID, Version: it.Version})
	}

	if err := h.Store.AgentProfiles().SetSkills(r.Context(), id, items); err != nil {
		writeStoreErr(w, r, "set profile skills", err)
		return
	}
	h.invalidateProfile(r.Context(), tid, id)

	views := make([]profileSkillView, 0, len(items))
	for _, it := range items {
		sk := skillsByID[it.SkillID]
		views = append(views, profileSkillView{
			SkillID: it.SkillID, Name: sk.Name, Kind: sk.Kind,
			Version: it.Version, LatestVersion: sk.LatestVersion, Description: sk.Description,
		})
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page{Items: views, Total: len(views)})
}

// GetSkills handles GET /api/v1/profiles/{id}/skills.
func (h Profiles) GetSkills(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")

	// Existence/scope check, same reasoning as GetTools.
	if _, err := h.Store.AgentProfiles().Get(r.Context(), tid, id); err != nil {
		writeStoreErr(w, r, "get profile skills", err)
		return
	}

	items, err := h.Store.AgentProfiles().GetSkills(r.Context(), id)
	if err != nil {
		writeStoreErr(w, r, "get profile skills", err)
		return
	}

	views := make([]profileSkillView, 0, len(items))
	for _, it := range items {
		sk, err := h.Store.Skills().Get(r.Context(), tid, it.SkillID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				// Attached earlier, since deleted or no longer visible
				// to this tenant -- omit it rather than fail the read.
				continue
			}
			writeStoreErr(w, r, "get profile skills", err)
			return
		}
		views = append(views, profileSkillView{
			SkillID: it.SkillID, Name: sk.Name, Kind: sk.Kind,
			Version: it.Version, LatestVersion: sk.LatestVersion, Description: sk.Description,
		})
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page{Items: views, Total: len(views)})
}
