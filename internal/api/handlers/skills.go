package handlers

import (
	"context"
	"fmt"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/api/httpx"
	applog "github.com/Tuskira/tusk-ai-secured-gateway/internal/log"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/skills"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// Skills implements the /api/v1/skills routes: the skills & commands
// registry. Reads return the tenant's own rows merged with the platform
// defaults (scope "platform"); writes address tenant rows only -- a
// platform row can be shadowed by creating a tenant row of the same
// name, never edited or deleted through this API (same convention as
// Models).
type Skills struct{ Deps }

// invalidateTenantProfiles best-effort drops every cached MCP profile
// resolution for tenantID (internal/dataplane/profile.Enforcer) after a
// skill/command write that could change what an attached profile
// resolves to (a new version, enable/disable, delete): finding exactly
// which profiles reference this skill isn't worth a query, so every
// profile in the tenant is simply dropped from the cache -- see
// pkg/ops.ProfileOps.InvalidateTenantProfiles. A nil Deps.ProfileOps or a
// failure is logged, not returned; the write itself already succeeded.
func (h Skills) invalidateTenantProfiles(ctx context.Context, tenantID string) {
	if h.ProfileOps == nil {
		return
	}
	if err := h.ProfileOps.InvalidateTenantProfiles(ctx, tenantID); err != nil {
		applog.From(ctx).Warn("invalidate tenant profile cache failed", "tenant_id", tenantID, "error", err)
	}
}

type skillFileRequest struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type skillFileView struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	SHA256  string `json:"sha256"`
	Size    int    `json:"size"`
}

func newSkillFileViews(files []store.SkillFile) []skillFileView {
	out := make([]skillFileView, 0, len(files))
	for _, f := range files {
		out = append(out, skillFileView{Path: f.Path, Content: f.Content, SHA256: f.SHA256, Size: f.Size})
	}
	return out
}

type createSkillRequest struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	// Description is accepted (so a client that echoes back a GET
	// response doesn't get an "unknown field" 400) but always ignored:
	// a skill/command's description is derived from SKILL.md's own
	// frontmatter (see prepareSkillFiles) and is read-only through this
	// API.
	Description string                  `json:"description,omitempty"`
	Enabled     *bool                   `json:"enabled,omitempty"`
	Metadata    map[string]any          `json:"metadata,omitempty"`
	Arguments   []store.CommandArgument `json:"arguments,omitempty"`
	Files       []skillFileRequest      `json:"files"`
}

// updateSkillRequest's fields are all optional (a caller PUTs only what
// it wants to change): a nil field is left as-is. This is a genuine
// partial update, not a full-replace like Models.Update, so an unrelated
// field (enabled, metadata) can be changed without echoing back other
// state. Description is accepted (for the same "unknown field" reason as
// createSkillRequest's) but always ignored -- it is never settable
// through this API; only AddVersion (from the new version's SKILL.md)
// changes it.
type updateSkillRequest struct {
	Description string                  `json:"description,omitempty"`
	Enabled     *bool                   `json:"enabled,omitempty"`
	Metadata    map[string]any          `json:"metadata,omitempty"`
	Arguments   []store.CommandArgument `json:"arguments,omitempty"`
}

type addVersionRequest struct {
	Files []skillFileRequest `json:"files"`
}

type skillView struct {
	ID          string                  `json:"id"`
	Name        string                  `json:"name"`
	Kind        string                  `json:"kind"`
	Description string                  `json:"description"`
	Frontmatter map[string]any          `json:"frontmatter"`
	Arguments   []store.CommandArgument `json:"arguments"`
	// LatestVersion is the highest version number this skill has.
	LatestVersion int  `json:"latest_version"`
	Enabled       bool `json:"enabled"`
	// Scope is "tenant" for the caller's own row, "platform" for a
	// default every tenant sees (read-only through this API).
	Scope     string         `json:"scope"`
	Metadata  map[string]any `json:"metadata"`
	CreatedAt string         `json:"created_at"`
	UpdatedAt string         `json:"updated_at"`
}

func newSkillView(sk *store.Skill) skillView {
	scope := "tenant"
	if sk.TenantID == "" {
		scope = "platform"
	}
	fm := sk.Frontmatter
	if fm == nil {
		fm = map[string]any{}
	}
	args := sk.Arguments
	if args == nil {
		args = []store.CommandArgument{}
	}
	meta := sk.Metadata
	if meta == nil {
		meta = map[string]any{}
	}
	return skillView{
		ID: sk.ID, Name: sk.Name, Kind: sk.Kind, Description: sk.Description,
		Frontmatter: fm, Arguments: args, LatestVersion: sk.LatestVersion, Enabled: sk.Enabled,
		Scope: scope, Metadata: meta, CreatedAt: formatTime(sk.CreatedAt), UpdatedAt: formatTime(sk.UpdatedAt),
	}
}

// skillDetailView is GET /skills/{id}'s shape: the skill view plus its
// latest version's files.
type skillDetailView struct {
	skillView
	Latest skillLatestView `json:"latest"`
}

type skillLatestView struct {
	Version int             `json:"version"`
	Files   []skillFileView `json:"files"`
}

// skillVersionDetailView is the shape POST .../versions and GET
// .../versions/{v} both return: a version's files plus who/when.
type skillVersionDetailView struct {
	Version   int             `json:"version"`
	Files     []skillFileView `json:"files"`
	CreatedBy string          `json:"created_by"`
	CreatedAt string          `json:"created_at"`
}

func newSkillVersionDetailView(v *store.SkillVersion) skillVersionDetailView {
	return skillVersionDetailView{Version: v.Version, Files: newSkillFileViews(v.Files), CreatedBy: v.CreatedBy, CreatedAt: formatTime(v.CreatedAt)}
}

// skillVersionListView is one entry of GET .../versions: no file bodies,
// just enough to pick a version to inspect further.
type skillVersionListView struct {
	Version   int    `json:"version"`
	CreatedBy string `json:"created_by"`
	CreatedAt string `json:"created_at"`
	FileCount int    `json:"file_count"`
}

// prepareSkillFiles builds the store.SkillFile slice from the request's
// (path, content) pairs, running every file rule (internal/skills.
// ValidateFiles -- count, SKILL.md present, path/extension shape, UTF-8,
// sizes, secret scan) and parsing+validating SKILL.md's frontmatter
// against skillName (allowed keys, hooks rejection, name match,
// description bounds). Returns the completed files (SHA256/Size filled
// in) and the parsed frontmatter, or the first validation failure as a
// plain error meant to surface as a 400.
func prepareSkillFiles(reqFiles []skillFileRequest, skillName string) ([]store.SkillFile, map[string]any, error) {
	files := make([]store.SkillFile, len(reqFiles))
	for i, f := range reqFiles {
		files[i] = store.SkillFile{Path: f.Path, Content: f.Content}
	}
	validated, err := skills.ValidateFiles(files)
	if err != nil {
		return nil, nil, err
	}
	skillMD, err := skills.FindSkillMD(validated)
	if err != nil {
		return nil, nil, err
	}
	fm, err := skills.ParseFrontmatter(skillMD, skillName)
	if err != nil {
		return nil, nil, err
	}
	return validated, fm, nil
}

// validateCommandArguments applies the command-only argument rules: kind
// "skill" must carry none; kind "command" must pass ValidateArguments and
// have every {{placeholder}} in its SKILL.md body declared.
func validateCommandArguments(kind string, args []store.CommandArgument, files []store.SkillFile) error {
	if kind != "command" {
		if len(args) > 0 {
			return fmt.Errorf("arguments are only valid for kind \"command\"")
		}
		return nil
	}
	if err := skills.ValidateArguments(args); err != nil {
		return err
	}
	skillMD, err := skills.FindSkillMD(files)
	if err != nil {
		return err
	}
	_, body, err := skills.SplitFrontmatter(skillMD)
	if err != nil {
		return err
	}
	return skills.ValidatePlaceholders(body, args)
}

// Create handles POST /api/v1/skills. enabled defaults to true;
// description is always derived from SKILL.md's own frontmatter -- any
// "description" in the request body is accepted but ignored (see
// createSkillRequest).
func (h Skills) Create(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}

	var req createSkillRequest
	if !httpx.Decode(w, r, &req) {
		return
	}

	if err := skills.ValidateName(req.Name); err != nil {
		httpx.ValidationError(w, err.Error())
		return
	}
	if req.Kind != "skill" && req.Kind != "command" {
		httpx.ValidationError(w, "kind must be \"skill\" or \"command\"")
		return
	}

	files, fm, err := prepareSkillFiles(req.Files, req.Name)
	if err != nil {
		httpx.ValidationError(w, err.Error())
		return
	}
	if err := validateCommandArguments(req.Kind, req.Arguments, files); err != nil {
		httpx.ValidationError(w, err.Error())
		return
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	meta := req.Metadata
	if meta == nil {
		meta = map[string]any{}
	}

	sk := &store.Skill{
		TenantID:    tid,
		Name:        req.Name,
		Kind:        req.Kind,
		Description: skills.DescriptionFrom(fm),
		Frontmatter: fm,
		Arguments:   req.Arguments,
		Enabled:     enabled,
		Metadata:    meta,
	}
	if err := h.Store.Skills().Create(r.Context(), sk, files, principalSubject(r)); err != nil {
		writeStoreErr(w, r, "create skill", err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, newSkillView(sk))
}

// List handles GET /api/v1/skills?kind=skill|command: the tenant's rows
// plus the platform defaults (the tenant's row wins on a shared name).
func (h Skills) List(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}

	kind := r.URL.Query().Get("kind")
	if kind != "" && kind != "skill" && kind != "command" {
		httpx.ValidationError(w, "kind must be \"skill\" or \"command\"")
		return
	}

	list, _, err := h.Store.Skills().List(r.Context(), tid, store.SkillListOptions{Kind: kind})
	if err != nil {
		writeStoreErr(w, r, "list skills", err)
		return
	}

	page := httpx.ParsePagination(r)
	views := make([]skillView, 0, len(list))
	for i := range list {
		views = append(views, newSkillView(&list[i]))
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page{Items: paginate(views, page), Total: len(views)})
}

// Get handles GET /api/v1/skills/{id} (tenant or platform row), including
// the latest version's files.
func (h Skills) Get(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}
	sk, err := h.Store.Skills().Get(r.Context(), tid, chi.URLParam(r, "id"))
	if err != nil {
		writeStoreErr(w, r, "get skill", err)
		return
	}
	latest, err := h.Store.Skills().GetVersion(r.Context(), sk.ID, sk.LatestVersion)
	if err != nil {
		writeStoreErr(w, r, "get skill latest version", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, skillDetailView{
		skillView: newSkillView(sk),
		Latest:    skillLatestView{Version: latest.Version, Files: newSkillFileViews(latest.Files)},
	})
}

// Update handles PUT /api/v1/skills/{id}: a partial update of
// enabled/metadata/arguments only (see updateSkillRequest; description is
// derived from SKILL.md and not settable here). A platform row is 403:
// shadow it with a tenant row instead.
func (h Skills) Update(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")

	existing, err := h.Store.Skills().Get(r.Context(), tid, id)
	if err != nil {
		writeStoreErr(w, r, "update skill", err)
		return
	}
	if existing.TenantID == "" {
		httpx.WriteError(w, http.StatusForbidden, httpx.TypePermission, "platform skills are read-only; create a tenant skill with the same name to override it")
		return
	}

	var req updateSkillRequest
	if !httpx.Decode(w, r, &req) {
		return
	}

	if req.Enabled != nil {
		existing.Enabled = *req.Enabled
	}
	if req.Metadata != nil {
		existing.Metadata = req.Metadata
	}
	if req.Arguments != nil {
		latest, err := h.Store.Skills().GetVersion(r.Context(), existing.ID, existing.LatestVersion)
		if err != nil {
			writeStoreErr(w, r, "update skill (load current version)", err)
			return
		}
		if err := validateCommandArguments(existing.Kind, req.Arguments, latest.Files); err != nil {
			httpx.ValidationError(w, err.Error())
			return
		}
		existing.Arguments = req.Arguments
	}

	if err := h.Store.Skills().Update(r.Context(), existing); err != nil {
		writeStoreErr(w, r, "update skill", err)
		return
	}
	h.invalidateTenantProfiles(r.Context(), tid)
	httpx.WriteJSON(w, http.StatusOK, newSkillView(existing))
}

// Delete handles DELETE /api/v1/skills/{id} (soft). Platform rows are
// 403.
func (h Skills) Delete(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")

	existing, err := h.Store.Skills().Get(r.Context(), tid, id)
	if err != nil {
		writeStoreErr(w, r, "delete skill", err)
		return
	}
	if existing.TenantID == "" {
		httpx.WriteError(w, http.StatusForbidden, httpx.TypePermission, "platform skills are read-only")
		return
	}
	if err := h.Store.Skills().SoftDelete(r.Context(), tid, id); err != nil {
		writeStoreErr(w, r, "delete skill", err)
		return
	}
	h.invalidateTenantProfiles(r.Context(), tid)
	applog.From(r.Context()).Info("skill deleted", "skill_id", id, "name", existing.Name)
	w.WriteHeader(http.StatusNoContent)
}

// AddVersion handles POST /api/v1/skills/{id}/versions: appends a new
// version. Platform rows are 403. The skill's name and (for commands) its
// current Arguments carry over unchanged -- see Update for changing
// those.
func (h Skills) AddVersion(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")

	existing, err := h.Store.Skills().Get(r.Context(), tid, id)
	if err != nil {
		writeStoreErr(w, r, "add skill version", err)
		return
	}
	if existing.TenantID == "" {
		httpx.WriteError(w, http.StatusForbidden, httpx.TypePermission, "platform skills are read-only")
		return
	}

	var req addVersionRequest
	if !httpx.Decode(w, r, &req) {
		return
	}

	files, _, err := prepareSkillFiles(req.Files, existing.Name)
	if err != nil {
		httpx.ValidationError(w, err.Error())
		return
	}
	if err := validateCommandArguments(existing.Kind, existing.Arguments, files); err != nil {
		httpx.ValidationError(w, err.Error())
		return
	}

	v, err := h.Store.Skills().AddVersion(r.Context(), tid, id, files, principalSubject(r))
	if err != nil {
		writeStoreErr(w, r, "add skill version", err)
		return
	}
	h.invalidateTenantProfiles(r.Context(), tid)
	httpx.WriteJSON(w, http.StatusCreated, newSkillVersionDetailView(v))
}

// ListVersions handles GET /api/v1/skills/{id}/versions.
func (h Skills) ListVersions(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")

	if _, err := h.Store.Skills().Get(r.Context(), tid, id); err != nil {
		writeStoreErr(w, r, "list skill versions", err)
		return
	}

	versions, err := h.Store.Skills().ListVersions(r.Context(), id)
	if err != nil {
		writeStoreErr(w, r, "list skill versions", err)
		return
	}

	// ListVersions leaves Files nil (see pkg/store.SkillStore's doc
	// comment); file_count is looked up per version with GetVersion. A
	// skill's version history is short (single digits in practice), so
	// this N+1 is not worth a store-interface change to avoid.
	views := make([]skillVersionListView, 0, len(versions))
	for _, v := range versions {
		full, err := h.Store.Skills().GetVersion(r.Context(), id, v.Version)
		if err != nil {
			writeStoreErr(w, r, "list skill versions (load file count)", err)
			return
		}
		views = append(views, skillVersionListView{
			Version: v.Version, CreatedBy: v.CreatedBy, CreatedAt: formatTime(v.CreatedAt), FileCount: len(full.Files),
		})
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page{Items: views, Total: len(views)})
}

// GetVersion handles GET /api/v1/skills/{id}/versions/{v}.
func (h Skills) GetVersion(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")

	if _, err := h.Store.Skills().Get(r.Context(), tid, id); err != nil {
		writeStoreErr(w, r, "get skill version", err)
		return
	}

	version, err := strconv.Atoi(chi.URLParam(r, "v"))
	if err != nil {
		httpx.NotFound(w, "not found")
		return
	}
	v, err := h.Store.Skills().GetVersion(r.Context(), id, version)
	if err != nil {
		writeStoreErr(w, r, "get skill version", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, newSkillVersionDetailView(v))
}
