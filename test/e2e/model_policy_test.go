//go:build e2e

package e2e

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/store/postgres"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/gateway"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// policyUpstream answers as Anthropic does and remembers every request
// body it received, so the test can prove what was (and was not)
// forwarded.
type policyUpstream struct {
	*httptest.Server
	hits atomic.Int32
	mu   sync.Mutex
	last []byte
}

func newPolicyUpstream(t *testing.T) *policyUpstream {
	t.Helper()
	u := &policyUpstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.hits.Add(1)
		b, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.last = b
		u.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_policy","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":2}}`)
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *policyUpstream) lastBody() []byte {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.last
}

// TestModelPolicyEndToEnd runs the real gateway (gateway.Run, every plane,
// Postgres store and Postgres LLM capture) and drives the model policy
// through the control-plane API:
//
//   - a disabled registry row is refused with 403 and a model_disabled
//     row in llm_calls, and nothing reaches the upstream;
//   - with the tenant's strict policy on, an unregistered model is refused
//     with 403 model_not_registered, while an enabled registered model is
//     still routed;
//   - a key override of false lets that key pass through on a strict
//     tenant, and an override of true refuses on a non-strict tenant;
//   - with strict off (the default) an unregistered model is forwarded
//     byte-for-byte, as before.
func TestModelPolicyEndToEnd(t *testing.T) {
	a := setupRunEnv(t)
	up := newPolicyUpstream(t)
	t.Setenv("GATEWAY_LLM_PROXY_UPSTREAM_BASE_URL", up.URL)
	t.Setenv("GATEWAY_LLM_PROXY_CAPTURE_STORE", "postgres")
	t.Setenv("GATEWAY_EGRESS_ALLOWED_CIDRS", "127.0.0.0/8")
	startRun(t, a, gateway.Options{Version: "e2e-policy"})

	// The same database gateway.Run migrated, for seeding keys and reading
	// capture rows.
	u, err := url.Parse(os.Getenv("GATEWAY_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + os.Getenv("GATEWAY_DATABASE_DATABASE")
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	st := postgres.New(db)
	ctx := context.Background()

	tenant := &store.Tenant{Slug: "e2e-policy-" + shortID(), Name: "E2E Policy"}
	if err := st.Tenants().Create(ctx, tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	other := &store.Tenant{Slug: "e2e-policy-home-" + shortID(), Name: "E2E Policy Home"}
	if err := st.Tenants().Create(ctx, other); err != nil {
		t.Fatalf("create platform tenant: %v", err)
	}
	platformKey := newAPIKey(t, st, other.ID, "e2e-platform", "platform-admin")
	adminKey := newAPIKey(t, st, tenant.ID, "e2e-admin", "admin")
	agentKey := newAPIKey(t, st, tenant.ID, "e2e-agent", "agent")
	optOutKey := newAPIKey(t, st, tenant.ID, "e2e-agent-optout", "agent")

	apiURL := "http://" + a.api + "/api/v1"
	apiCall := func(key, method, path, body string, dst any) int {
		t.Helper()
		req, err := http.NewRequest(method, apiURL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode/100 != 2 {
			t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, raw)
		}
		if dst != nil {
			if err := json.Unmarshal(raw, dst); err != nil {
				t.Fatalf("%s %s: decode %s: %v", method, path, raw, err)
			}
		}
		return resp.StatusCode
	}
	keyID := func(name string) string {
		t.Helper()
		var page struct {
			Items []struct{ ID, Name string } `json:"items"`
		}
		apiCall(adminKey, http.MethodGet, "/api-keys", "", &page)
		for _, k := range page.Items {
			if k.Name == name {
				return k.ID
			}
		}
		t.Fatalf("no key named %q", name)
		return ""
	}

	llm := func(key, model string) (int, string) {
		t.Helper()
		body := `{"model":"` + model + `","max_tokens":8,"messages":[{"role":"user","content":"policy e2e"}]}`
		req, err := http.NewRequest(http.MethodPost, "http://"+a.llm+"/v1/messages", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("x-api-key", "sk-ant-caller")
		req.Header.Set("anthropic-version", "2023-06-01")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	// refusal checks a 403 permission_error, no upstream call, and the
	// llm_calls row the refusal left behind.
	refusal := func(step, key, model, wantErr string) {
		t.Helper()
		before := up.hits.Load()
		code, body := llm(key, model)
		if code != http.StatusForbidden || !strings.Contains(body, `"permission_error"`) {
			t.Fatalf("%s: %d %s, want 403 permission_error", step, code, body)
		}
		if up.hits.Load() != before {
			t.Fatalf("%s: the upstream was called; a refusal must not reach it", step)
		}
		var status, fallback int
		var errText string
		deadline := time.Now().Add(5 * time.Second)
		for {
			err := db.QueryRow(`SELECT status_code, fallback_index, coalesce(error, '') FROM llm_calls
				WHERE tenant_id = $1 AND requested_model = $2 AND status_code = 403 ORDER BY timestamp DESC LIMIT 1`,
				tenant.ID, model).Scan(&status, &fallback, &errText)
			if err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: no 403 llm_calls row for %q: %v", step, model, err)
			}
			time.Sleep(50 * time.Millisecond)
		}
		if errText != wantErr || fallback != -1 {
			t.Errorf("%s: llm_calls row error %q fallback %d, want %q -1", step, errText, fallback, wantErr)
		}
	}
	forwarded := func(step, key, model, wantUpstreamModel string) {
		t.Helper()
		before := up.hits.Load()
		code, body := llm(key, model)
		if code != http.StatusOK || up.hits.Load() != before+1 {
			t.Fatalf("%s: %d %s (upstream hits %d -> %d), want 200 from the upstream", step, code, body, before, up.hits.Load())
		}
		var got struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(up.lastBody(), &got); err != nil || got.Model != wantUpstreamModel {
			t.Errorf("%s: upstream saw model %q (%v), want %q", step, got.Model, err, wantUpstreamModel)
		}
	}

	// AC-3: strict is off by default; an unregistered model passes through
	// with the client's own bytes.
	forwarded("default passthrough", agentKey, "claude-unregistered-e2e", "claude-unregistered-e2e")
	if want := `{"model":"claude-unregistered-e2e","max_tokens":8,"messages":[{"role":"user","content":"policy e2e"}]}`; string(up.lastBody()) != want {
		t.Errorf("passthrough body changed:\n got %s\nwant %s", up.lastBody(), want)
	}

	// Register two models on the upstream (same host as the configured
	// upstream, so the caller's key may reach them).
	target := `[{"vendor":"anthropic","model":"claude-target-e2e","base_url":"` + up.URL + `"}]`
	var gated, open struct {
		ID string `json:"id"`
	}
	apiCall(adminKey, http.MethodPost, "/models", `{"name":"gated-e2e","targets":`+target+`}`, &gated)
	apiCall(adminKey, http.MethodPost, "/models", `{"name":"open-e2e","targets":`+target+`}`, &open)
	forwarded("enabled registered model", agentKey, "gated-e2e", "claude-target-e2e")

	// AC-1: disable the row; it is refused, not passed through.
	apiCall(adminKey, http.MethodPut, "/models/"+gated.ID, `{"name":"gated-e2e","enabled":false,"targets":`+target+`}`, nil)
	refusal("disabled row", agentKey, "gated-e2e", "model_disabled")

	// AC-2: strict tenant. Unregistered refused; enabled registered routed.
	var tv struct {
		StrictModels bool `json:"strict_models"`
	}
	apiCall(platformKey, http.MethodPatch, "/tenants/"+tenant.ID, `{"strict_models":true}`, &tv)
	if !tv.StrictModels {
		t.Fatal("PATCH /tenants did not report strict_models true")
	}
	refusal("strict tenant, unregistered", agentKey, "claude-strict-e2e", "model_not_registered")
	forwarded("strict tenant, enabled registered", agentKey, "open-e2e", "claude-target-e2e")

	// Key override false on a strict tenant: that key passes through.
	var kv struct {
		StrictModels *bool `json:"strict_models"`
	}
	apiCall(adminKey, http.MethodPatch, "/api-keys/"+keyID("e2e-agent-optout"), `{"strict_models":false}`, &kv)
	if kv.StrictModels == nil || *kv.StrictModels {
		t.Fatalf("PATCH /api-keys strict_models = %v, want false", kv.StrictModels)
	}
	forwarded("strict tenant, key override false", optOutKey, "claude-optout-e2e", "claude-optout-e2e")

	// Tenant back to non-strict; key override true refuses for that key only.
	apiCall(platformKey, http.MethodPatch, "/tenants/"+tenant.ID, `{"strict_models":false}`, nil)
	apiCall(adminKey, http.MethodPatch, "/api-keys/"+keyID("e2e-agent"), `{"strict_models":true}`, nil)
	refusal("non-strict tenant, key override true", agentKey, "claude-keystrict-e2e", "model_not_registered") // gitleaks:allow (test model name, not a secret)
	forwarded("non-strict tenant, inheriting key", optOutKey, "claude-keystrict-e2e", "claude-keystrict-e2e") // gitleaks:allow (test model name, not a secret)

	// Clearing the override (null) inherits the non-strict tenant again.
	apiCall(adminKey, http.MethodPatch, "/api-keys/"+keyID("e2e-agent"), `{"strict_models":null}`, nil)
	forwarded("override cleared", agentKey, "claude-cleared-e2e", "claude-cleared-e2e")

	// The disabled row stays refused whatever the policy.
	refusal("disabled row, non-strict", agentKey, "gated-e2e", "model_disabled")
}
