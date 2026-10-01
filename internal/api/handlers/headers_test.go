package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHeaders_List_IncludesProvidersAndBuiltinResolverTypes(t *testing.T) {
	h := Headers{Deps: newTestDeps()}

	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/headers/providers", nil), "tenant-a", "agent")
	w := serve(http.MethodGet, "/headers/providers", h.List, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var got providersResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(got.Providers) != 1 || got.Providers[0].ID != "secret_store" {
		t.Errorf("Providers = %+v, want exactly [secret_store] (only provider registered in newTestDeps)", got.Providers)
	}
	if got.Providers[0].ConfigSchema == nil {
		t.Error("secret_store provider's ConfigSchema is nil")
	}

	want := map[string]bool{"static": false, "token_field": false, "incoming_field": false, "external": false}
	for _, rt := range got.ResolverTypes {
		if _, ok := want[rt]; !ok {
			t.Errorf("unexpected resolver type %q", rt)
		}
		want[rt] = true
	}
	for rt, seen := range want {
		if !seen {
			t.Errorf("resolver_types missing built-in %q", rt)
		}
	}
}
