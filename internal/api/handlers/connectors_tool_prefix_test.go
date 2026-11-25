package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type notifyingCacheOps struct {
	*fakeCacheOps
	notified []string
}

func (n *notifyingCacheOps) NotifyToolsListChanged(tenantID string) {
	n.notified = append(n.notified, tenantID)
}

func createPrefixed(t *testing.T, h Connectors, name, prefixJSON string) (int, connectorView) {
	t.Helper()
	body := `{"name":"` + name + `","endpoint":"https://` + name + `.example.com/mcp"` + prefixJSON + `}`
	w := serve(http.MethodPost, "/connectors", h.Create, connectorReq(http.MethodPost, "/connectors", body, "admin"))
	var v connectorView
	if w.Code == http.StatusCreated {
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
	}
	return w.Code, v
}

func TestValidateToolPrefix(t *testing.T) {
	valid := []string{"a", "virustotal.threat_intel", "Vendor-1.Feed_two_three", "x.y", "A-B.c", strings.Repeat("a", 100)}
	invalid := []string{
		"", "has space", "a/b", "a__b", "_a", "a_", "a__", "__a", "a:b", "gateway", "Gateway", "GATEWAY",
		"ünï", "a,b", strings.Repeat("a", 101),
	}
	for _, p := range valid {
		if err := validateToolPrefix(p); err != nil {
			t.Errorf("validateToolPrefix(%q) = %v, want nil", p, err)
		}
	}
	for _, p := range invalid {
		if err := validateToolPrefix(p); err == nil {
			t.Errorf("validateToolPrefix(%q) = nil, want an error", p)
		}
	}
}

func TestConnectors_ToolPrefix_CreateAndView(t *testing.T) {
	h := Connectors{Deps: restrictExemptDeps()}

	code, v := createPrefixed(t, h, "vt", `,"tool_prefix":"virustotal.threat_intel"`)
	if code != http.StatusCreated || v.ToolPrefix != "virustotal.threat_intel" {
		t.Fatalf("create with prefix: %d %+v", code, v)
	}
	// Absent and "" both mean no prefix, and the view says "".
	for name, extra := range map[string]string{"plain": "", "empty": `,"tool_prefix":""`} {
		code, v = createPrefixed(t, h, name, extra)
		if code != http.StatusCreated || v.ToolPrefix != "" {
			t.Errorf("create %s: %d prefix %q, want 201 and no prefix", name, code, v.ToolPrefix)
		}
	}
	// The key is always present in the JSON view.
	w := serve(http.MethodGet, "/connectors/{id}", h.Get, connectorReq(http.MethodGet, "/connectors/"+v.ID, "", "admin"))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"tool_prefix":""`) {
		t.Errorf("get: %d %s; want tool_prefix \"\" in the body", w.Code, w.Body.String())
	}
}

func TestConnectors_ToolPrefix_Validation(t *testing.T) {
	h := Connectors{Deps: restrictExemptDeps()}
	for _, bad := range []string{"has space", "a/b", "a__b", "gateway", "GateWay", strings.Repeat("a", 101), "trailing_"} {
		code, _ := createPrefixed(t, h, "bad", `,"tool_prefix":"`+bad+`"`)
		if code != http.StatusBadRequest {
			t.Errorf("create with tool_prefix %q: status %d, want 400", bad, code)
		}
	}
}

func TestConnectors_ToolPrefix_Conflicts(t *testing.T) {
	h := Connectors{Deps: restrictExemptDeps()}
	if code, _ := createPrefixed(t, h, "first", `,"tool_prefix":"Vendor.Feed"`); code != http.StatusCreated {
		t.Fatalf("create first: %d", code)
	}
	// Same prefix, any case; and a prefix equal to another connector's
	// slug, in any case.
	for _, p := range []string{"Vendor.Feed", "vendor.feed", "VENDOR.FEED", "first", "FIRST"} {
		if code, _ := createPrefixed(t, h, "second"+strings.ToLower(strings.ReplaceAll(p, ".", "")), `,"tool_prefix":"`+p+`"`); code != http.StatusConflict {
			t.Errorf("create with tool_prefix %q: status %d, want 409", p, code)
		}
	}
}

func TestConnectors_ToolPrefix_UpdateSemantics(t *testing.T) {
	deps := restrictExemptDeps()
	cache := &notifyingCacheOps{fakeCacheOps: &fakeCacheOps{store: deps.Store.Connectors()}}
	pops := &fakeProfileOps{}
	deps.CacheOps, deps.ProfileOps = cache, pops
	h := Connectors{Deps: deps}

	_, c := createPrefixed(t, h, "vt", `,"tool_prefix":"vt.feed"`)
	put := func(extra string) (int, connectorView) {
		body := `{"name":"vt","endpoint":"https://vt.example.com/mcp"` + extra + `}`
		w := serve(http.MethodPut, "/connectors/{id}", h.Update, connectorReq(http.MethodPut, "/connectors/"+c.ID, body, "admin"))
		var v connectorView
		if w.Code == http.StatusOK {
			if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
				t.Fatal(err)
			}
		}
		return w.Code, v
	}

	// Absent keeps the prefix, and does not touch caches or notify.
	if code, v := put(""); code != http.StatusOK || v.ToolPrefix != "vt.feed" {
		t.Fatalf("PUT without tool_prefix: %d %+v", code, v)
	}
	if cache.lastInvalidateConnectorID != "" || len(cache.notified) != 0 || pops.invalidateTenantCalls != 0 {
		t.Errorf("an unchanged prefix touched caches: invalidate %q, notified %v, profiles %d",
			cache.lastInvalidateConnectorID, cache.notified, pops.invalidateTenantCalls)
	}
	// The same value is no change either.
	if code, _ := put(`,"tool_prefix":"vt.feed"`); code != http.StatusOK || len(cache.notified) != 0 {
		t.Errorf("PUT with the same prefix: %d, notified %v", code, cache.notified)
	}
	// A new value replaces it: cache rows dropped, profiles dropped, streams told.
	if code, v := put(`,"tool_prefix":"vt.other"`); code != http.StatusOK || v.ToolPrefix != "vt.other" {
		t.Fatalf("PUT new prefix: %d %+v", code, v)
	}
	if cache.lastRefreshConnectorID != c.ID {
		t.Errorf("a prefix change refreshed connector %q, want %q", cache.lastRefreshConnectorID, c.ID)
	}
	if cache.lastInvalidateConnectorID != c.ID || pops.invalidateTenantCalls != 1 || len(cache.notified) != 1 || cache.notified[0] != "tenant-a" {
		t.Errorf("a prefix change: invalidate %q, profiles %d, notified %v; want the connector, 1, [tenant-a]",
			cache.lastInvalidateConnectorID, pops.invalidateTenantCalls, cache.notified)
	}
	// "" clears it.
	if code, v := put(`,"tool_prefix":""`); code != http.StatusOK || v.ToolPrefix != "" {
		t.Fatalf("PUT clearing the prefix: %d %+v", code, v)
	}
	if len(cache.notified) != 2 {
		t.Errorf("clearing the prefix notified %d times, want 2 in total", len(cache.notified))
	}
	// An invalid value is refused and changes nothing.
	if code, _ := put(`,"tool_prefix":"a b"`); code != http.StatusBadRequest {
		t.Errorf("PUT invalid prefix: %d, want 400", code)
	}
}

// A failed re-discovery after a prefix change does not fail the PUT (the
// update is already stored), and the streams are still told; a disabled
// connector's rows are dropped but its backend is not dialled.
func TestConnectors_ToolPrefix_ChangeRefreshIsBestEffort(t *testing.T) {
	deps := restrictExemptDeps()
	cache := &notifyingCacheOps{fakeCacheOps: &fakeCacheOps{store: deps.Store.Connectors(), refreshErr: errors.New("backend down")}}
	deps.CacheOps, deps.ProfileOps = cache, &fakeProfileOps{}
	h := Connectors{Deps: deps}

	_, c := createPrefixed(t, h, "vt", `,"tool_prefix":"vt.feed"`)
	put := func(extra string) *httptest.ResponseRecorder {
		body := `{"name":"vt","endpoint":"https://vt.example.com/mcp"` + extra + `}`
		return serve(http.MethodPut, "/connectors/{id}", h.Update, connectorReq(http.MethodPut, "/connectors/"+c.ID, body, "admin"))
	}

	if w := put(`,"tool_prefix":"vt.other"`); w.Code != http.StatusOK {
		t.Fatalf("PUT with a failing refresh: %d %s, want 200", w.Code, w.Body.String())
	}
	if cache.lastRefreshConnectorID != c.ID || len(cache.notified) != 1 {
		t.Errorf("failing refresh: refreshed %q, notified %v; want the connector and one push", cache.lastRefreshConnectorID, cache.notified)
	}
	if got, err := h.Store.Connectors().Get(t.Context(), "tenant-a", c.ID); err != nil || got.ToolPrefix != "vt.other" {
		t.Errorf("stored prefix after a failing refresh = %+v, %v; want vt.other", got, err)
	}

	cache.lastRefreshConnectorID, cache.lastInvalidateConnectorID = "", ""
	if w := put(`,"tool_prefix":"vt.third","metadata":{"enabled":false}`); w.Code != http.StatusOK {
		t.Fatalf("PUT disabling with a new prefix: %d %s", w.Code, w.Body.String())
	}
	if cache.lastInvalidateConnectorID != c.ID || cache.lastRefreshConnectorID != "" || len(cache.notified) != 2 {
		t.Errorf("disabled connector: invalidate %q, refresh %q, notified %v; want invalidate only, plus a push",
			cache.lastInvalidateConnectorID, cache.lastRefreshConnectorID, cache.notified)
	}
}

// Changing the prefix of a restrict_exempt connector renames what a
// contained key may reach, so it takes the same permission as repointing it.
func TestConnectors_ToolPrefix_ChangeOnExemptNeedsAdmin(t *testing.T) {
	h := Connectors{Deps: restrictExemptDeps()}
	body := `{"name":"help","endpoint":"https://help.example.com/mcp","metadata":{"restrict_exempt":true}}`
	w := serve(http.MethodPost, "/connectors", h.Create, connectorReq(http.MethodPost, "/connectors", body, "admin"))
	var c connectorView
	if err := json.Unmarshal(w.Body.Bytes(), &c); err != nil || w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	put := func(prefix, role string) *httptest.ResponseRecorder {
		body := `{"name":"help","endpoint":"https://help.example.com/mcp","metadata":{"restrict_exempt":true},"tool_prefix":"` + prefix + `"}`
		return serve(http.MethodPut, "/connectors/{id}", h.Update, connectorReq(http.MethodPut, "/connectors/"+c.ID, body, role))
	}
	if w := put("help.desk", "editor"); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), restrictExemptMessage) {
		t.Errorf("editor setting a prefix on an exempt connector: %d %s; want 403", w.Code, w.Body.String())
	}
	if got, _ := h.Store.Connectors().Get(t.Context(), "tenant-a", c.ID); got == nil || got.ToolPrefix != "" {
		t.Errorf("refused write stored prefix %+v", got)
	}
	if w := put("help.desk", "admin"); w.Code != http.StatusOK {
		t.Errorf("admin setting a prefix on an exempt connector: %d %s", w.Code, w.Body.String())
	}
	if w := put("help.desk", "editor"); w.Code != http.StatusOK {
		t.Errorf("editor resending the same prefix: %d %s; want 200", w.Code, w.Body.String())
	}
}
