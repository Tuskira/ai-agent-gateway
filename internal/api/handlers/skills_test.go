package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/api/httpx"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// jsonEscapeNewlines converts a normal multi-line string (real newline
// bytes) into the form safe to splice into a hand-built JSON string
// literal below: "\n" (backslash, n) rather than a raw newline byte.
func jsonEscapeNewlines(s string) string {
	return strings.ReplaceAll(s, "\n", "\\n")
}

func skillMD(name, description, body string) string {
	return jsonEscapeNewlines("---\nname: " + name + "\ndescription: " + description + "\n---\n" + body + "\n")
}

func skillCreateBody(name string) string {
	return `{"name":"` + name + `","kind":"skill","files":[{"path":"SKILL.md","content":"` + skillMD(name, "A recon helper.", "Body text.") + `"}]}`
}

func commandCreateBody(name string) string {
	return `{"name":"` + name + `","kind":"command","arguments":[{"name":"target","description":"the host","required":true}],` +
		`"files":[{"path":"SKILL.md","content":"` + skillMD(name, "Scan a host.", "scan {{target}}") + `"}]}`
}

func postSkill(t *testing.T, h Skills, tenant, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := withPrincipal(httptest.NewRequest(http.MethodPost, "/skills", strings.NewReader(body)), tenant, "admin")
	return serve(http.MethodPost, "/skills", h.Create, req)
}

func decodeSkill(t *testing.T, w *httptest.ResponseRecorder) skillView {
	t.Helper()
	var v skillView
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %s: %v", w.Body.String(), err)
	}
	return v
}

func TestSkills_Create_DefaultsAndShape(t *testing.T) {
	deps := newTestDeps()
	h := Skills{Deps: deps}

	w := postSkill(t, h, "tenant-a", skillCreateBody("recon"))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	got := decodeSkill(t, w)
	if got.ID == "" || got.Name != "recon" || got.Kind != "skill" || !got.Enabled || got.Scope != "tenant" {
		t.Errorf("view = %+v", got)
	}
	if got.LatestVersion != 1 {
		t.Errorf("LatestVersion = %d, want 1", got.LatestVersion)
	}
	if got.Description != "A recon helper." {
		t.Errorf("Description = %q, want it defaulted from SKILL.md frontmatter", got.Description)
	}
	if got.Frontmatter["name"] != "recon" {
		t.Errorf("Frontmatter = %+v", got.Frontmatter)
	}

	// A "description" in the request body is accepted (no "unknown
	// field" error) but always ignored -- description is derived from
	// SKILL.md's own frontmatter, never client-supplied.
	w = postSkill(t, h, "tenant-a", `{"name":"recon2","kind":"skill","description":"client-supplied, must be ignored",`+
		`"files":[{"path":"SKILL.md","content":"`+skillMD("recon2", "Frontmatter description.", "Body.")+`"}]}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create with body description: status = %d, body = %s", w.Code, w.Body.String())
	}
	got = decodeSkill(t, w)
	if got.Description != "Frontmatter description." {
		t.Errorf("Description = %q, want the frontmatter's description (body description must be ignored)", got.Description)
	}

	// A command with valid arguments and a matching template.
	w = postSkill(t, h, "tenant-a", commandCreateBody("scan-host"))
	if w.Code != http.StatusCreated {
		t.Fatalf("command create: status = %d, body = %s", w.Code, w.Body.String())
	}
	got = decodeSkill(t, w)
	if got.Kind != "command" || len(got.Arguments) != 1 || got.Arguments[0].Name != "target" {
		t.Errorf("command view = %+v", got)
	}
}

func TestSkills_Create_Validation(t *testing.T) {
	deps := newTestDeps()
	h := Skills{Deps: deps}

	cases := map[string]string{
		"uppercase name":            `{"name":"Recon","kind":"skill","files":[{"path":"SKILL.md","content":"` + skillMD("Recon", "x", "y") + `"}]}`,
		"double underscore":         `{"name":"re__con","kind":"skill","files":[{"path":"SKILL.md","content":"` + skillMD("re__con", "x", "y") + `"}]}`,
		"reserved name":             `{"name":"gateway","kind":"skill","files":[{"path":"SKILL.md","content":"` + skillMD("gateway", "x", "y") + `"}]}`,
		"bad kind":                  `{"name":"recon","kind":"bogus","files":[{"path":"SKILL.md","content":"` + skillMD("recon", "x", "y") + `"}]}`,
		"missing SKILL.md":          `{"name":"recon","kind":"skill","files":[{"path":"notes.md","content":"x"}]}`,
		"disallowed extension":      `{"name":"recon","kind":"skill","files":[{"path":"SKILL.md","content":"` + skillMD("recon", "x", "y") + `"},{"path":"run.sh","content":"echo hi"}]}`,
		"secret in file":            `{"name":"recon","kind":"skill","files":[{"path":"SKILL.md","content":"` + skillMD("recon", "x", "y") + `"},{"path":"notes.txt","content":"key AKIAAAAAAAAAAAAAAAAA"}]}`,
		"frontmatter name mismatch": `{"name":"recon","kind":"skill","files":[{"path":"SKILL.md","content":"` + skillMD("other", "x", "y") + `"}]}`,
		"hooks rejected": `{"name":"recon","kind":"skill","files":[{"path":"SKILL.md","content":"` +
			jsonEscapeNewlines("---\nname: recon\ndescription: x\nhooks:\n  pre: echo hi\n---\ny\n") + `"}]}`,
		"unknown frontmatter key": `{"name":"recon","kind":"skill","files":[{"path":"SKILL.md","content":"` +
			jsonEscapeNewlines("---\nname: recon\ndescription: x\nrun: y\n---\ny\n") + `"}]}`,
		"frontmatter description too long": `{"name":"recon","kind":"skill","files":[{"path":"SKILL.md","content":"` + skillMD("recon", strings.Repeat("a", 1025), "y") + `"}]}`,
		"arguments on kind skill":          `{"name":"recon","kind":"skill","arguments":[{"name":"target"}],"files":[{"path":"SKILL.md","content":"` + skillMD("recon", "x", "y") + `"}]}`,
		"bad argument name":                `{"name":"scan","kind":"command","arguments":[{"name":"Target"}],"files":[{"path":"SKILL.md","content":"` + skillMD("scan", "x", "scan {{Target}}") + `"}]}`,
		"undeclared placeholder":           `{"name":"scan","kind":"command","arguments":[{"name":"target"}],"files":[{"path":"SKILL.md","content":"` + skillMD("scan", "x", "scan {{target}} for {{cve}}") + `"}]}`,
	}
	for name, body := range cases {
		w := postSkill(t, h, "tenant-a", body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (%s)", name, w.Code, w.Body.String())
			continue
		}
		var e struct {
			Error struct {
				Type string `json:"type"`
			} `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil || e.Error.Type != httpx.TypeValidation {
			t.Errorf("%s: body = %s", name, w.Body.String())
		}
	}
}

func TestSkills_Create_DuplicateAndPlatformShadow(t *testing.T) {
	deps := newTestDeps()
	h := Skills{Deps: deps}

	if w := postSkill(t, h, "tenant-a", skillCreateBody("recon")); w.Code != http.StatusCreated {
		t.Fatalf("first create: %d %s", w.Code, w.Body.String())
	}
	if w := postSkill(t, h, "tenant-a", skillCreateBody("recon")); w.Code != http.StatusConflict {
		t.Errorf("duplicate name: status = %d, want 409 (%s)", w.Code, w.Body.String())
	}
	if w := postSkill(t, h, "tenant-b", skillCreateBody("recon")); w.Code != http.StatusCreated {
		t.Errorf("same name, other tenant: status = %d (%s)", w.Code, w.Body.String())
	}
}

func TestSkills_List_MergesPlatformRows(t *testing.T) {
	deps := newTestDeps()
	fs := deps.Store.(*fakeStore)
	h := Skills{Deps: deps}

	// Seed a platform row directly through the store (mirrors how
	// TestModels_List_MergesPlatformRows seeds "haiku").
	plat := &store.Skill{Name: "triage", Kind: "skill", Enabled: true}
	platFiles := []store.SkillFile{{Path: "SKILL.md", Content: "---\nname: triage\ndescription: Platform triage.\n---\nBody.\n"}}
	if err := fs.Skills().Create(context.Background(), plat, platFiles, "seed"); err != nil {
		t.Fatalf("seed platform skill: %v", err)
	}

	if w := postSkill(t, h, "tenant-a", skillCreateBody("recon")); w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}

	list := func(tenant string) []skillView {
		req := withPrincipal(httptest.NewRequest(http.MethodGet, "/skills", nil), tenant, "agent")
		w := serve(http.MethodGet, "/skills", h.List, req)
		if w.Code != http.StatusOK {
			t.Fatalf("list(%s): %d %s", tenant, w.Code, w.Body.String())
		}
		var page struct {
			Items []skillView `json:"items"`
			Total int         `json:"total"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		if page.Total != len(page.Items) {
			t.Errorf("total = %d, items = %d", page.Total, len(page.Items))
		}
		return page.Items
	}

	a := list("tenant-a")
	if len(a) != 2 {
		t.Fatalf("tenant-a list = %+v, want 2 (platform triage + tenant recon)", a)
	}
	b := list("tenant-b")
	if len(b) != 1 || b[0].Scope != "platform" {
		t.Errorf("tenant-b list = %+v; want only the platform row", b)
	}
}

func TestSkills_GetUpdateDelete(t *testing.T) {
	deps := newTestDeps()
	h := Skills{Deps: deps}

	created := decodeSkill(t, postSkill(t, h, "tenant-a", skillCreateBody("recon")))

	get := func(tenant, id string) *httptest.ResponseRecorder {
		req := withPrincipal(httptest.NewRequest(http.MethodGet, "/skills/"+id, nil), tenant, "agent")
		return serve(http.MethodGet, "/skills/{id}", h.Get, req)
	}

	w := get("tenant-a", created.ID)
	if w.Code != http.StatusOK {
		t.Fatalf("get: %d %s", w.Code, w.Body.String())
	}
	var detail skillDetailView
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Latest.Version != 1 || len(detail.Latest.Files) != 1 || detail.Latest.Files[0].Path != "SKILL.md" {
		t.Errorf("Get latest = %+v", detail.Latest)
	}
	if detail.Latest.Files[0].SHA256 == "" || detail.Latest.Files[0].Size == 0 {
		t.Errorf("Get latest file missing sha256/size: %+v", detail.Latest.Files[0])
	}

	// Tenant isolation.
	if w := get("tenant-b", created.ID); w.Code != http.StatusNotFound {
		t.Errorf("get(tenant-b, tenant-a's id) = %d, want 404", w.Code)
	}

	// Update: partial semantics -- an unrelated field change must not
	// touch description.
	updateReq := func(tenant, id, body string) *httptest.ResponseRecorder {
		req := withPrincipal(httptest.NewRequest(http.MethodPut, "/skills/"+id, strings.NewReader(body)), tenant, "admin")
		return serve(http.MethodPut, "/skills/{id}", h.Update, req)
	}
	w = updateReq("tenant-a", created.ID, `{"enabled":false}`)
	if w.Code != http.StatusOK {
		t.Fatalf("update enabled: %d %s", w.Code, w.Body.String())
	}
	updated := decodeSkill(t, w)
	if updated.Enabled {
		t.Error("update did not disable")
	}
	if updated.Description != "A recon helper." {
		t.Errorf("update(enabled only) must not touch description: %q", updated.Description)
	}

	// description in the body is accepted but ignored -- it is not
	// settable through this API (only AddVersion changes it, from a new
	// SKILL.md).
	w = updateReq("tenant-a", created.ID, `{"description":"new desc","metadata":{"k":"v"}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("update metadata: %d %s", w.Code, w.Body.String())
	}
	updated = decodeSkill(t, w)
	if updated.Description != "A recon helper." || updated.Metadata["k"] != "v" || updated.Enabled {
		t.Errorf("update = %+v, want description unchanged (ignored), metadata changed, enabled still false", updated)
	}

	if w := updateReq("tenant-b", created.ID, `{"enabled":true}`); w.Code != http.StatusNotFound {
		t.Errorf("update(tenant-b, tenant-a's id) = %d, want 404", w.Code)
	}

	// Delete.
	del := func(tenant, id string) *httptest.ResponseRecorder {
		req := withPrincipal(httptest.NewRequest(http.MethodDelete, "/skills/"+id, nil), tenant, "admin")
		return serve(http.MethodDelete, "/skills/{id}", h.Delete, req)
	}
	if w := del("tenant-b", created.ID); w.Code != http.StatusNotFound {
		t.Errorf("delete(tenant-b) = %d, want 404", w.Code)
	}
	if w := del("tenant-a", created.ID); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	if w := get("tenant-a", created.ID); w.Code != http.StatusNotFound {
		t.Errorf("get after delete = %d, want 404", w.Code)
	}
}

func TestSkills_PlatformRowsReadOnly(t *testing.T) {
	deps := newTestDeps()
	fs := deps.Store.(*fakeStore)
	h := Skills{Deps: deps}

	plat := &store.Skill{Name: "triage", Kind: "skill", Enabled: true}
	platFiles := []store.SkillFile{{Path: "SKILL.md", Content: "---\nname: triage\ndescription: Platform triage.\n---\nBody.\n"}}
	if err := fs.Skills().Create(context.Background(), plat, platFiles, "seed"); err != nil {
		t.Fatalf("seed platform skill: %v", err)
	}

	update := withPrincipal(httptest.NewRequest(http.MethodPut, "/skills/"+plat.ID, strings.NewReader(`{"enabled":false}`)), "tenant-a", "admin")
	if w := serve(http.MethodPut, "/skills/{id}", h.Update, update); w.Code != http.StatusForbidden {
		t.Errorf("update platform row = %d, want 403 (%s)", w.Code, w.Body.String())
	}

	del := withPrincipal(httptest.NewRequest(http.MethodDelete, "/skills/"+plat.ID, nil), "tenant-a", "admin")
	if w := serve(http.MethodDelete, "/skills/{id}", h.Delete, del); w.Code != http.StatusForbidden {
		t.Errorf("delete platform row = %d, want 403 (%s)", w.Code, w.Body.String())
	}

	addVerBody := `{"files":[{"path":"SKILL.md","content":"` + skillMD("triage", "Platform triage v2.", "Body.") + `"}]}`
	addVer := withPrincipal(httptest.NewRequest(http.MethodPost, "/skills/"+plat.ID+"/versions", strings.NewReader(addVerBody)), "tenant-a", "admin")
	if w := serve(http.MethodPost, "/skills/{id}/versions", h.AddVersion, addVer); w.Code != http.StatusForbidden {
		t.Errorf("add version to platform row = %d, want 403 (%s)", w.Code, w.Body.String())
	}
}

func TestSkills_Versions(t *testing.T) {
	deps := newTestDeps()
	h := Skills{Deps: deps}

	created := decodeSkill(t, postSkill(t, h, "tenant-a", skillCreateBody("recon")))

	addVersion := func(tenant, id, body string) *httptest.ResponseRecorder {
		req := withPrincipal(httptest.NewRequest(http.MethodPost, "/skills/"+id+"/versions", strings.NewReader(body)), tenant, "admin")
		return serve(http.MethodPost, "/skills/{id}/versions", h.AddVersion, req)
	}

	v2Body := `{"files":[{"path":"SKILL.md","content":"` + skillMD("recon", "Updated recon description.", "Body v2.") + `"}]}`
	w := addVersion("tenant-a", created.ID, v2Body)
	if w.Code != http.StatusCreated {
		t.Fatalf("add version: %d %s", w.Code, w.Body.String())
	}
	var v2 skillVersionDetailView
	if err := json.Unmarshal(w.Body.Bytes(), &v2); err != nil {
		t.Fatal(err)
	}
	if v2.Version != 2 || len(v2.Files) != 1 {
		t.Errorf("add version response = %+v", v2)
	}

	// Tenant isolation on AddVersion.
	if w := addVersion("tenant-b", created.ID, v2Body); w.Code != http.StatusNotFound {
		t.Errorf("add version (tenant-b) = %d, want 404", w.Code)
	}

	// The skill's description was refreshed from the new SKILL.md.
	getReq := withPrincipal(httptest.NewRequest(http.MethodGet, "/skills/"+created.ID, nil), "tenant-a", "agent")
	getResp := serve(http.MethodGet, "/skills/{id}", h.Get, getReq)
	var detail skillDetailView
	if err := json.Unmarshal(getResp.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Description != "Updated recon description." || detail.LatestVersion != 2 || detail.Latest.Version != 2 {
		t.Errorf("after AddVersion, Get = %+v", detail)
	}

	// ListVersions: newest first, with file_count, no bodies.
	listReq := withPrincipal(httptest.NewRequest(http.MethodGet, "/skills/"+created.ID+"/versions", nil), "tenant-a", "agent")
	listResp := serve(http.MethodGet, "/skills/{id}/versions", h.ListVersions, listReq)
	if listResp.Code != http.StatusOK {
		t.Fatalf("list versions: %d %s", listResp.Code, listResp.Body.String())
	}
	var page struct {
		Items []skillVersionListView `json:"items"`
		Total int                    `json:"total"`
	}
	if err := json.Unmarshal(listResp.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || page.Items[0].Version != 2 || page.Items[1].Version != 1 {
		t.Fatalf("ListVersions = %+v, want [2, 1]", page.Items)
	}
	if page.Items[0].FileCount != 1 {
		t.Errorf("ListVersions[0].FileCount = %d, want 1", page.Items[0].FileCount)
	}

	// GetVersion: a specific version's files.
	verReq := withPrincipal(httptest.NewRequest(http.MethodGet, "/skills/"+created.ID+"/versions/1", nil), "tenant-a", "agent")
	verResp := serve(http.MethodGet, "/skills/{id}/versions/{v}", h.GetVersion, verReq)
	if verResp.Code != http.StatusOK {
		t.Fatalf("get version 1: %d %s", verResp.Code, verResp.Body.String())
	}
	var v1 skillVersionDetailView
	if err := json.Unmarshal(verResp.Body.Bytes(), &v1); err != nil {
		t.Fatal(err)
	}
	if v1.Version != 1 || len(v1.Files) != 1 {
		t.Errorf("GetVersion(1) = %+v", v1)
	}

	// A missing version is 404.
	missingReq := withPrincipal(httptest.NewRequest(http.MethodGet, "/skills/"+created.ID+"/versions/99", nil), "tenant-a", "agent")
	missingResp := serve(http.MethodGet, "/skills/{id}/versions/{v}", h.GetVersion, missingReq)
	if missingResp.Code != http.StatusNotFound {
		t.Errorf("get version 99 = %d, want 404", missingResp.Code)
	}
}

// TestSkills_WritesInvalidateTenantProfileCache asserts that Update,
// AddVersion and Delete all call Deps.ProfileOps.InvalidateTenantProfiles
// -- the "simplest acceptable" response to a skill/command write that
// could change what a profile attaching it resolves to (see
// pkg/ops.ProfileOps and docs/profiles.md). Create is deliberately not
// asserted here: a brand-new skill cannot yet be attached to anything.
func TestSkills_WritesInvalidateTenantProfileCache(t *testing.T) {
	deps := newTestDeps()
	fake := &fakeProfileOps{store: deps.Store.AgentProfiles()}
	deps.ProfileOps = fake
	h := Skills{Deps: deps}

	w := postSkill(t, h, "tenant-a", skillCreateBody("recon"))
	created := decodeSkill(t, w)
	if fake.invalidateTenantCalls != 0 {
		t.Errorf("Create called InvalidateTenantProfiles %d time(s), want 0", fake.invalidateTenantCalls)
	}

	updateReq := withPrincipal(httptest.NewRequest(http.MethodPut, "/skills/"+created.ID, strings.NewReader(`{"enabled":false}`)), "tenant-a", "admin")
	if w := serve(http.MethodPut, "/skills/{id}", h.Update, updateReq); w.Code != http.StatusOK {
		t.Fatalf("update: %d %s", w.Code, w.Body.String())
	}
	if fake.invalidateTenantCalls != 1 || fake.lastInvalidateTenantID != "tenant-a" {
		t.Errorf("after Update: calls=%d tenant=%q", fake.invalidateTenantCalls, fake.lastInvalidateTenantID)
	}

	versionReq := withPrincipal(httptest.NewRequest(http.MethodPost, "/skills/"+created.ID+"/versions",
		strings.NewReader(`{"files":[{"path":"SKILL.md","content":"`+skillMD("recon", "v2", "v2 body")+`"}]}`)), "tenant-a", "admin")
	if w := serve(http.MethodPost, "/skills/{id}/versions", h.AddVersion, versionReq); w.Code != http.StatusCreated {
		t.Fatalf("add version: %d %s", w.Code, w.Body.String())
	}
	if fake.invalidateTenantCalls != 2 {
		t.Errorf("after AddVersion: calls=%d, want 2", fake.invalidateTenantCalls)
	}

	deleteReq := withPrincipal(httptest.NewRequest(http.MethodDelete, "/skills/"+created.ID, nil), "tenant-a", "admin")
	if w := serve(http.MethodDelete, "/skills/{id}", h.Delete, deleteReq); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	if fake.invalidateTenantCalls != 3 {
		t.Errorf("after Delete: calls=%d, want 3", fake.invalidateTenantCalls)
	}
}
