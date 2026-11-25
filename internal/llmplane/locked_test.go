package llmplane

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// A platform-locked credential authenticates a platform model (operator
// seeded) but never a tenant model, whose base_url a tenant admin can
// point anywhere: the tenant model's upstream is not called with it.
func TestRegistry_LockedCredentialOnlyForPlatformModels(t *testing.T) {
	f := newRegistryFixture(t)
	tenantUp := newAnthropicUpstream(t, http.StatusOK)
	platformUp := newAnthropicUpstream(t, http.StatusOK)
	if _, err := f.secrets.CreateLocked(context.Background(), f.tenant, "plat-key", "api_key", map[string]string{"api_key": "sk-platform"}, "platform", true); err != nil {
		t.Fatal(err)
	}
	f.model(t, "tenant-row", nil, store.ModelTarget{Vendor: "anthropic", Model: "m", BaseURL: tenantUp.URL, Credential: "plat-key"})
	if err := f.models.Create(context.Background(), &store.Model{TenantID: "", Name: "platform-row", Enabled: true,
		Targets: []store.ModelTarget{{Vendor: "anthropic", Model: "m", BaseURL: platformUp.URL, Credential: "plat-key"}}}); err != nil {
		t.Fatal(err)
	}

	gw := f.gateway(t, Config{UpstreamBaseURL: tenantUp.URL})
	call := func(model string) (*http.Response, string) {
		return do(t, "POST", gw.URL+"/v1/messages", map[string]string{"x-api-key": "sk-client"}, strings.Replace(aliasBody, "sonnet", model, 1))
	}

	resp, body := call("tenant-row")
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("tenant model with a locked credential answered 200: %s", body)
	}
	if n := tenantUp.hits.Load(); n != 0 {
		_, h, _ := tenantUp.got()
		t.Errorf("tenant model's upstream was called %d times (x-api-key %q)", n, h.Get("x-api-key"))
	}

	resp, body = call("platform-row")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("platform model: status %d body %s", resp.StatusCode, body)
	}
	if _, h, _ := platformUp.got(); h.Get("x-api-key") != "sk-platform" {
		t.Errorf("platform model x-api-key = %q, want the locked credential", h.Get("x-api-key"))
	}
}
