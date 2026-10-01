package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProfiles_Create_SlugIsTenantPrefixed(t *testing.T) {
	h := Profiles{Deps: newTestDeps()}

	req := withPrincipal(httptest.NewRequest(http.MethodPost, "/profiles", strings.NewReader(`{"name":"SOC Agent!"}`)), "tenant-a", "admin")
	w := serve(http.MethodPost, "/profiles", h.Create, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var got profileView
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Slug != "tenant-a-soc-agent" {
		t.Errorf("Slug = %q, want %q", got.Slug, "tenant-a-soc-agent")
	}
}

func TestProfiles_Get_TenantIsolation(t *testing.T) {
	deps := newTestDeps()
	h := Profiles{Deps: deps}

	createReq := withPrincipal(httptest.NewRequest(http.MethodPost, "/profiles", strings.NewReader(`{"name":"soc-agent"}`)), "tenant-a", "admin")
	w := serve(http.MethodPost, "/profiles", h.Create, createReq)
	var created profileView
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}

	getReq := withPrincipal(httptest.NewRequest(http.MethodGet, "/profiles/"+created.ID, nil), "tenant-b", "admin")
	w = serve(http.MethodGet, "/profiles/{id}", h.Get, getReq)
	if w.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant get status = %d, want 404", w.Code)
	}
}

func TestProfiles_SetTools_HappyPathAndUnknownConnectorRejected(t *testing.T) {
	deps := newTestDeps()
	profiles := Profiles{Deps: deps}
	connectors := Connectors{Deps: deps}

	connReq := withPrincipal(httptest.NewRequest(http.MethodPost, "/connectors", strings.NewReader(connectorCreateBody)), "tenant-a", "admin")
	w := serve(http.MethodPost, "/connectors", connectors.Create, connReq)
	var conn connectorView
	if err := json.Unmarshal(w.Body.Bytes(), &conn); err != nil {
		t.Fatalf("decode connector: %v", err)
	}

	profReq := withPrincipal(httptest.NewRequest(http.MethodPost, "/profiles", strings.NewReader(`{"name":"soc-agent"}`)), "tenant-a", "admin")
	w = serve(http.MethodPost, "/profiles", profiles.Create, profReq)
	var prof profileView
	if err := json.Unmarshal(w.Body.Bytes(), &prof); err != nil {
		t.Fatalf("decode profile: %v", err)
	}

	// Happy path: a real connector_id from the same tenant.
	setReq := withPrincipal(httptest.NewRequest(http.MethodPut, "/profiles/"+prof.ID+"/tools",
		strings.NewReader(`{"tools":[{"connector_id":"`+conn.ID+`","tool_name":"list_users"}]}`)), "tenant-a", "admin")
	w = serve(http.MethodPut, "/profiles/{id}/tools", profiles.SetTools, setReq)
	if w.Code != http.StatusOK {
		t.Fatalf("SetTools status = %d, body = %s", w.Code, w.Body.String())
	}
	// Same {items,total} shape as GET .../tools (the console reads it so).
	var setPage struct {
		Items []profileToolView `json:"items"`
		Total int               `json:"total"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &setPage); err != nil || setPage.Total != 1 || len(setPage.Items) != 1 {
		t.Fatalf("SetTools body = %s, want {items:[1 tool], total:1}", w.Body.String())
	}

	getReq := withPrincipal(httptest.NewRequest(http.MethodGet, "/profiles/"+prof.ID+"/tools", nil), "tenant-a", "admin")
	w = serve(http.MethodGet, "/profiles/{id}/tools", profiles.GetTools, getReq)
	var page struct {
		Items []profileToolView `json:"items"`
		Total int               `json:"total"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if page.Total != 1 || page.Items[0].ConnectorID != conn.ID || page.Items[0].ToolName != "list_users" {
		t.Errorf("page = %+v", page)
	}

	// Rejected: a connector_id that doesn't exist for this tenant.
	badReq := withPrincipal(httptest.NewRequest(http.MethodPut, "/profiles/"+prof.ID+"/tools",
		strings.NewReader(`{"tools":[{"connector_id":"does-not-exist","tool_name":"x"}]}`)), "tenant-a", "admin")
	w = serve(http.MethodPut, "/profiles/{id}/tools", profiles.SetTools, badReq)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("SetTools with unknown connector status = %d, want 400, body = %s", w.Code, w.Body.String())
	}
	assertErrorType(t, w, "validation_error")

	// Cross-tenant: tenant-b must not be able to allow-list tenant-a's
	// connector into a profile of its own.
	crossReq := withPrincipal(httptest.NewRequest(http.MethodPut, "/profiles/"+prof.ID+"/tools",
		strings.NewReader(`{"tools":[{"connector_id":"`+conn.ID+`","tool_name":"x"}]}`)), "tenant-b", "admin")
	w = serve(http.MethodPut, "/profiles/{id}/tools", profiles.SetTools, crossReq)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("cross-tenant SetTools status = %d, want 400 (connector not visible to tenant-b)", w.Code)
	}
}

func TestProfiles_Delete_SoftDelete(t *testing.T) {
	deps := newTestDeps()
	h := Profiles{Deps: deps}

	createReq := withPrincipal(httptest.NewRequest(http.MethodPost, "/profiles", strings.NewReader(`{"name":"soc-agent"}`)), "tenant-a", "admin")
	w := serve(http.MethodPost, "/profiles", h.Create, createReq)
	var created profileView
	json.Unmarshal(w.Body.Bytes(), &created)

	delReq := withPrincipal(httptest.NewRequest(http.MethodDelete, "/profiles/"+created.ID, nil), "tenant-a", "admin")
	w = serve(http.MethodDelete, "/profiles/{id}", h.Delete, delReq)
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d", w.Code)
	}

	getReq := withPrincipal(httptest.NewRequest(http.MethodGet, "/profiles/"+created.ID, nil), "tenant-a", "admin")
	w = serve(http.MethodGet, "/profiles/{id}", h.Get, getReq)
	if w.Code != http.StatusNotFound {
		t.Fatalf("get after delete status = %d, want 404", w.Code)
	}
}

func TestProfiles_InstructionsRoundTripAndCap(t *testing.T) {
	deps := newTestDeps()
	h := Profiles{Deps: deps}

	createReq := withPrincipal(httptest.NewRequest(http.MethodPost, "/profiles",
		strings.NewReader(`{"name":"soc-agent","instructions":"Always cite the alert id."}`)), "tenant-a", "admin")
	w := serve(http.MethodPost, "/profiles", h.Create, createReq)
	if w.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", w.Code, w.Body.String())
	}
	var created profileView
	json.Unmarshal(w.Body.Bytes(), &created)
	if created.Instructions != "Always cite the alert id." {
		t.Errorf("Instructions = %q", created.Instructions)
	}

	tooLong := `{"name":"soc-agent","instructions":"` + strings.Repeat("x", 8*1024+1) + `"}`
	badReq := withPrincipal(httptest.NewRequest(http.MethodPut, "/profiles/"+created.ID, strings.NewReader(tooLong)), "tenant-a", "admin")
	w = serve(http.MethodPut, "/profiles/{id}", h.Update, badReq)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("over-cap instructions status = %d, want 400, body = %s", w.Code, w.Body.String())
	}

	updReq := withPrincipal(httptest.NewRequest(http.MethodPut, "/profiles/"+created.ID,
		strings.NewReader(`{"name":"soc-agent","instructions":"Updated text."}`)), "tenant-a", "admin")
	w = serve(http.MethodPut, "/profiles/{id}", h.Update, updReq)
	if w.Code != http.StatusOK {
		t.Fatalf("update status = %d, body = %s", w.Code, w.Body.String())
	}

	getReq := withPrincipal(httptest.NewRequest(http.MethodGet, "/profiles/"+created.ID, nil), "tenant-a", "admin")
	w = serve(http.MethodGet, "/profiles/{id}", h.Get, getReq)
	var got profileView
	json.Unmarshal(w.Body.Bytes(), &got)
	if got.Instructions != "Updated text." {
		t.Errorf("GET Instructions = %q, want %q", got.Instructions, "Updated text.")
	}
}

func TestProfiles_SetSkills_HappyPathAndValidation(t *testing.T) {
	deps := newTestDeps()
	profiles := Profiles{Deps: deps}
	skillsH := Skills{Deps: deps}

	skillResp := postSkill(t, skillsH, "tenant-a", skillCreateBody("runbook"))
	sk := decodeSkill(t, skillResp)

	profReq := withPrincipal(httptest.NewRequest(http.MethodPost, "/profiles", strings.NewReader(`{"name":"soc-agent"}`)), "tenant-a", "admin")
	w := serve(http.MethodPost, "/profiles", profiles.Create, profReq)
	var prof profileView
	json.Unmarshal(w.Body.Bytes(), &prof)

	// Happy path: attach the skill unpinned.
	setReq := withPrincipal(httptest.NewRequest(http.MethodPut, "/profiles/"+prof.ID+"/skills",
		strings.NewReader(`{"items":[{"skill_id":"`+sk.ID+`"}]}`)), "tenant-a", "admin")
	w = serve(http.MethodPut, "/profiles/{id}/skills", profiles.SetSkills, setReq)
	if w.Code != http.StatusOK {
		t.Fatalf("set skills status = %d, body = %s", w.Code, w.Body.String())
	}
	decodeProfileSkills := func(w *httptest.ResponseRecorder) []profileSkillView {
		t.Helper()
		var page struct {
			Items []profileSkillView `json:"items"`
			Total int                `json:"total"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		return page.Items
	}
	items := decodeProfileSkills(w)
	if len(items) != 1 || items[0].SkillID != sk.ID || items[0].Name != "runbook" || items[0].Kind != "skill" {
		t.Fatalf("SetSkills view = %+v", items)
	}

	getReq := withPrincipal(httptest.NewRequest(http.MethodGet, "/profiles/"+prof.ID+"/skills", nil), "tenant-a", "admin")
	w = serve(http.MethodGet, "/profiles/{id}/skills", profiles.GetSkills, getReq)
	if w.Code != http.StatusOK {
		t.Fatalf("get skills status = %d, body = %s", w.Code, w.Body.String())
	}
	items = decodeProfileSkills(w)
	if len(items) != 1 || items[0].SkillID != sk.ID {
		t.Fatalf("GetSkills = %+v", items)
	}

	// Replace semantics: an empty set clears it.
	clearReq := withPrincipal(httptest.NewRequest(http.MethodPut, "/profiles/"+prof.ID+"/skills", strings.NewReader(`{"items":[]}`)), "tenant-a", "admin")
	serve(http.MethodPut, "/profiles/{id}/skills", profiles.SetSkills, clearReq)
	w = serve(http.MethodGet, "/profiles/{id}/skills", profiles.GetSkills, getReq)
	items = decodeProfileSkills(w)
	if len(items) != 0 {
		t.Fatalf("after clearing: GetSkills = %+v, want none", items)
	}

	// Rejected: a skill_id that doesn't exist/isn't visible.
	badReq := withPrincipal(httptest.NewRequest(http.MethodPut, "/profiles/"+prof.ID+"/skills",
		strings.NewReader(`{"items":[{"skill_id":"does-not-exist"}]}`)), "tenant-a", "admin")
	w = serve(http.MethodPut, "/profiles/{id}/skills", profiles.SetSkills, badReq)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown skill_id status = %d, want 400, body = %s", w.Code, w.Body.String())
	}

	// Rejected: a pinned version that doesn't exist for the skill.
	badVersion := withPrincipal(httptest.NewRequest(http.MethodPut, "/profiles/"+prof.ID+"/skills",
		strings.NewReader(`{"items":[{"skill_id":"`+sk.ID+`","version":99}]}`)), "tenant-a", "admin")
	w = serve(http.MethodPut, "/profiles/{id}/skills", profiles.SetSkills, badVersion)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing pinned version status = %d, want 400, body = %s", w.Code, w.Body.String())
	}

	// Cross-tenant: tenant-b must not be able to attach tenant-a's skill.
	crossProfReq := withPrincipal(httptest.NewRequest(http.MethodPost, "/profiles", strings.NewReader(`{"name":"other"}`)), "tenant-b", "admin")
	w = serve(http.MethodPost, "/profiles", profiles.Create, crossProfReq)
	var crossProf profileView
	json.Unmarshal(w.Body.Bytes(), &crossProf)

	crossReq := withPrincipal(httptest.NewRequest(http.MethodPut, "/profiles/"+crossProf.ID+"/skills",
		strings.NewReader(`{"items":[{"skill_id":"`+sk.ID+`"}]}`)), "tenant-b", "admin")
	w = serve(http.MethodPut, "/profiles/{id}/skills", profiles.SetSkills, crossReq)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("cross-tenant attach status = %d, want 400 (skill not visible to tenant-b)", w.Code)
	}
}

// TestProfiles_WritesInvalidateProfileCache asserts that Update, SetTools,
// SetSkills and Delete all call Deps.ProfileOps.InvalidateProfile with
// this profile's id (see pkg/ops.ProfileOps and docs/profiles.md). Delete
// invalidates BEFORE the soft delete, mirroring Connectors.Delete, so the
// fake's tenant-scoped lookup still resolves the (not yet deleted) row.
func TestProfiles_WritesInvalidateProfileCache(t *testing.T) {
	deps := newTestDeps()
	fake := &fakeProfileOps{store: deps.Store.AgentProfiles()}
	deps.ProfileOps = fake
	h := Profiles{Deps: deps}

	createReq := withPrincipal(httptest.NewRequest(http.MethodPost, "/profiles", strings.NewReader(`{"name":"soc-agent"}`)), "tenant-a", "admin")
	w := serve(http.MethodPost, "/profiles", h.Create, createReq)
	var created profileView
	json.Unmarshal(w.Body.Bytes(), &created)
	if fake.invalidateCalls != 0 {
		t.Errorf("Create called InvalidateProfile %d time(s), want 0", fake.invalidateCalls)
	}

	updReq := withPrincipal(httptest.NewRequest(http.MethodPut, "/profiles/"+created.ID, strings.NewReader(`{"name":"soc-agent-2"}`)), "tenant-a", "admin")
	serve(http.MethodPut, "/profiles/{id}", h.Update, updReq)
	if fake.invalidateCalls != 1 || fake.lastInvalidateProfileID != created.ID {
		t.Errorf("after Update: calls=%d id=%q", fake.invalidateCalls, fake.lastInvalidateProfileID)
	}

	toolsReq := withPrincipal(httptest.NewRequest(http.MethodPut, "/profiles/"+created.ID+"/tools", strings.NewReader(`{"tools":[]}`)), "tenant-a", "admin")
	serve(http.MethodPut, "/profiles/{id}/tools", h.SetTools, toolsReq)
	if fake.invalidateCalls != 2 {
		t.Errorf("after SetTools: calls=%d, want 2", fake.invalidateCalls)
	}

	skillsReq := withPrincipal(httptest.NewRequest(http.MethodPut, "/profiles/"+created.ID+"/skills", strings.NewReader(`{"items":[]}`)), "tenant-a", "admin")
	serve(http.MethodPut, "/profiles/{id}/skills", h.SetSkills, skillsReq)
	if fake.invalidateCalls != 3 {
		t.Errorf("after SetSkills: calls=%d, want 3", fake.invalidateCalls)
	}

	delReq := withPrincipal(httptest.NewRequest(http.MethodDelete, "/profiles/"+created.ID, nil), "tenant-a", "admin")
	w = serve(http.MethodDelete, "/profiles/{id}", h.Delete, delReq)
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, body = %s", w.Code, w.Body.String())
	}
	if fake.invalidateCalls != 4 || fake.lastInvalidateProfileID != created.ID {
		t.Errorf("after Delete: calls=%d id=%q", fake.invalidateCalls, fake.lastInvalidateProfileID)
	}
}
