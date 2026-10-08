package llmplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/capture"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/store/postgres"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	pkgsink "github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink"
	pgsink "github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink/postgres"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// limitsEnv is a real Postgres (GATEWAY_TEST_DATABASE_URL) in a throwaway
// schema: the gateway store (api_keys with limits), the real Postgres LLM
// sink (llm_calls, the SpendReader), and one tenant.
type limitsEnv struct {
	db     *sql.DB // scoped to the schema, for assertions
	store  store.Store
	sink   *pgsink.Sink
	tenant *store.Tenant
}

func newLimitsEnv(t *testing.T) *limitsEnv {
	t.Helper()
	rawURL := os.Getenv("GATEWAY_TEST_DATABASE_URL")
	if rawURL == "" {
		t.Skip("GATEWAY_TEST_DATABASE_URL not set; skipping Postgres-backed limits test")
	}
	ctx := context.Background()
	schema := "gwlim_" + strings.ReplaceAll(uuid.NewString(), "-", "")

	admin, err := sql.Open("pgx", rawURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(ctx, fmt.Sprintf(`CREATE SCHEMA %q`, schema)); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), fmt.Sprintf(`DROP SCHEMA %q CASCADE`, schema))
		admin.Close()
	})
	u, _ := url.Parse(rawURL)
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	dsn := u.String()

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	st := postgres.New(db)
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	sk, err := pgsink.New(ctx, pgsink.Options{DSN: dsn})
	if err != nil {
		t.Fatalf("pg sink: %v", err)
	}
	t.Cleanup(func() { _ = sk.Close() })
	check, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { check.Close() })

	tn := &store.Tenant{Slug: "lim-" + uuid.NewString()[:8], Name: "limits"}
	if err := st.Tenants().Create(ctx, tn); err != nil {
		t.Fatalf("tenant: %v", err)
	}
	return &limitsEnv{db: check, store: st, sink: sk, tenant: tn}
}

func (e *limitsEnv) key(t *testing.T, l *store.Limits) *store.APIKey {
	t.Helper()
	k := &store.APIKey{TenantID: e.tenant.ID, Name: "k", Role: "agent", KeyHash: uuid.NewString(), KeyPrefix: "gk_test0000", Limits: l}
	if err := e.store.APIKeys().Create(context.Background(), k); err != nil {
		t.Fatalf("create key: %v", err)
	}
	return k
}

// seed writes priced llm_calls rows for key through the real sink.
func (e *limitsEnv) seed(t *testing.T, tenantID, keyID string, at time.Time, costs ...float64) {
	t.Helper()
	for _, c := range costs {
		c := c
		if err := e.sink.WriteBatch(context.Background(), []*pkgsink.LLMCall{{
			Timestamp: at, RequestID: uuid.NewString(), TenantID: tenantID, KeyID: keyID,
			Provider: "anthropic", Model: "claude-sonnet-4-5", StatusCode: 200, CostUSD: &c,
		}}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
}

// plane serves the LLM plane with limits against a stub upstream that
// answers a priced Anthropic message (claude-sonnet-4-5, 1000 in / 1000
// out ≈ $0.018), capturing through the real Postgres sink.
func (e *limitsEnv) plane(t *testing.T, keyID string, spend pkgsink.SpendReader) (*httptest.Server, *Limiter, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","model":"claude-sonnet-4-5","stop_reason":"end_turn","usage":{"input_tokens":1000,"output_tokens":1000}}`)
	}))
	t.Cleanup(up.Close)
	lim, err := NewLimiter(LimiterConfig{Keys: e.store.APIKeys(), Spend: spend})
	if err != nil {
		t.Fatal(err)
	}
	h, err := Handler(Config{
		UpstreamBaseURL: up.URL, MaxRequestBytes: 1 << 20, Authorizer: pkgauth.NewRoleAuthorizer(), Limiter: lim,
	}, capture.NewRecorder(e.sink, nil, nil, 0))
	if err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(withPrincipal(h, &pkgauth.Principal{
		Subject: keyID, TenantID: e.tenant.ID, KeyID: keyID, AuthMethod: "apikey", Roles: []string{"agent"},
	}))
	t.Cleanup(gw.Close)
	return gw, lim, &hits
}

func message(maxTokens int) string {
	return `{"model":"claude-sonnet-4-5","max_tokens":` + strconv.Itoa(maxTokens) + `,"messages":[{"role":"user","content":"hi"}]}`
}

func assertEnvelope(t *testing.T, body, wantType, wantMsg string) {
	t.Helper()
	var env struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("error body is not JSON: %s", body)
	}
	if env.Type != "error" || env.Error.Type != wantType || !strings.Contains(env.Error.Message, wantMsg) {
		t.Fatalf("error envelope = %s, want type %q message containing %q", body, wantType, wantMsg)
	}
}

// deniedRows counts the captured rows for key with status and a non-empty error.
func (e *limitsEnv) deniedRows(t *testing.T, keyID string, status int, errPrefix string) int {
	t.Helper()
	var n int
	if err := e.db.QueryRow(`SELECT count(*) FROM llm_calls WHERE key_id = $1 AND status_code = $2 AND error LIKE $3 AND cost_usd IS NULL AND fallback_index = -1`,
		keyID, status, errPrefix+"%").Scan(&n); err != nil {
		t.Fatalf("count denied rows: %v", err)
	}
	return n
}

// TestKeySpend_Postgres pins the spend query: day vs month windows, tenant
// and key scoping, NULL cost counted as 0.
func TestKeySpend_Postgres(t *testing.T) {
	e := newLimitsEnv(t)
	monthStart := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	dayStart := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)
	e.seed(t, e.tenant.ID, "key-a", dayStart.Add(10*time.Hour), 0.5)
	e.seed(t, e.tenant.ID, "key-a", dayStart, 0.125) // boundary: inclusive
	e.seed(t, e.tenant.ID, "key-a", time.Date(2026, 9, 10, 8, 0, 0, 0, time.UTC), 0.25)
	e.seed(t, e.tenant.ID, "key-a", monthStart.Add(-time.Second), 1)       // last month
	e.seed(t, e.tenant.ID, "key-b", dayStart.Add(time.Hour), 9)            // other key
	e.seed(t, "other-tenant", "key-a", dayStart.Add(time.Hour), 9)         // other tenant
	if err := e.sink.WriteBatch(context.Background(), []*pkgsink.LLMCall{{ // unknown cost
		Timestamp: dayStart.Add(time.Hour), RequestID: uuid.NewString(), TenantID: e.tenant.ID, KeyID: "key-a", StatusCode: 200,
	}}); err != nil {
		t.Fatal(err)
	}

	day, month, err := e.sink.KeySpend(context.Background(), e.tenant.ID, "key-a", dayStart, monthStart)
	if err != nil {
		t.Fatalf("KeySpend: %v", err)
	}
	if !near(day, 0.625) || !near(month, 0.875) {
		t.Fatalf("KeySpend = day %v month %v, want 0.625 / 0.875", day, month)
	}
	if day, month, err := e.sink.KeySpend(context.Background(), e.tenant.ID, "nobody", dayStart, monthStart); err != nil || day != 0 || month != 0 {
		t.Fatalf("KeySpend(no rows) = %v %v %v, want 0 0 nil", day, month, err)
	}
}

// TestLimits_DailyBudget429 is the budget path end to end: two priced rows
// already captured put the key over its daily budget, so the next call is
// refused with the Anthropic 429 envelope and a Retry-After to the next UTC
// midnight, never reaches the upstream, is itself captured as a 429 row,
// and counts in Status.
func TestLimits_DailyBudget429(t *testing.T) {
	e := newLimitsEnv(t)
	daily := 0.01
	k := e.key(t, &store.Limits{DailyUSD: &daily})
	e.seed(t, e.tenant.ID, k.ID, time.Now(), 0.006, 0.006)
	gw, lim, hits := e.plane(t, k.ID, e.sink)

	resp, body := do(t, http.MethodPost, gw.URL+"/v1/messages", map[string]string{"x-api-key": "sk"}, message(16))
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d (%s), want 429", resp.StatusCode, body)
	}
	assertEnvelope(t, body, "rate_limit_error", "daily budget exceeded for this key")
	now := time.Now().UTC()
	want := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC).Sub(now).Seconds()
	ra, err := strconv.Atoi(resp.Header.Get("Retry-After"))
	if err != nil || float64(ra) < want-2 || float64(ra) > want+2 {
		t.Fatalf("Retry-After = %q, want ≈ %.0f (seconds to next UTC midnight)", resp.Header.Get("Retry-After"), want)
	}
	if hits.Load() != 0 {
		t.Fatal("a denied call reached the upstream")
	}
	if n := e.deniedRows(t, k.ID, 429, "budget_exceeded: daily budget exceeded"); n != 1 {
		t.Fatalf("captured 429 rows = %d, want 1", n)
	}
	if got := lim.Status()["budget_denials"]; got != uint64(1) || lim.BudgetDenials() != 1 {
		t.Fatalf("budget_denials = %v (getter %d), want 1", got, lim.BudgetDenials())
	}
}

// TestLimits_MonthlyBudgetAndOwnSpend: a call under budget goes through; its
// captured cost is added to the cached spend at once (observe), so the next
// call is refused without waiting for the 10 s refresh. Monthly wins with
// Retry-After to the 1st of next month.
func TestLimits_MonthlyBudgetAndOwnSpend(t *testing.T) {
	e := newLimitsEnv(t)
	monthly := 0.01
	k := e.key(t, &store.Limits{MonthlyUSD: &monthly})
	gw, _, hits := e.plane(t, k.ID, e.sink)

	if resp, body := do(t, http.MethodPost, gw.URL+"/v1/messages", nil, message(16)); resp.StatusCode != http.StatusOK {
		t.Fatalf("first call status = %d (%s), want 200", resp.StatusCode, body)
	}
	resp, body := do(t, http.MethodPost, gw.URL+"/v1/messages", nil, message(16))
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second call status = %d (%s), want 429 (first call's ~$0.018 already over $0.01)", resp.StatusCode, body)
	}
	assertEnvelope(t, body, "rate_limit_error", "monthly budget exceeded for this key")
	now := time.Now().UTC()
	want := time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC).Sub(now).Seconds()
	if ra, _ := strconv.Atoi(resp.Header.Get("Retry-After")); float64(ra) < want-2 || float64(ra) > want+2 {
		t.Fatalf("Retry-After = %q, want ≈ %.0f", resp.Header.Get("Retry-After"), want)
	}
	if hits.Load() != 1 {
		t.Fatalf("upstream hits = %d, want 1", hits.Load())
	}
}

// TestLimits_RPM: rpm 2 admits two calls and refuses the third within the
// minute with Retry-After: 1; the denial is captured and counted.
func TestLimits_RPM(t *testing.T) {
	e := newLimitsEnv(t)
	rpm := 2
	k := e.key(t, &store.Limits{RPM: &rpm})
	gw, lim, hits := e.plane(t, k.ID, e.sink)
	for i := 0; i < 2; i++ {
		if resp, body := do(t, http.MethodPost, gw.URL+"/v1/messages", nil, message(16)); resp.StatusCode != http.StatusOK {
			t.Fatalf("call %d status = %d (%s)", i+1, resp.StatusCode, body)
		}
	}
	resp, body := do(t, http.MethodPost, gw.URL+"/v1/messages", nil, message(16))
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "1" {
		t.Fatalf("third call = %d Retry-After %q (%s), want 429 with Retry-After 1", resp.StatusCode, resp.Header.Get("Retry-After"), body)
	}
	assertEnvelope(t, body, "rate_limit_error", "requests per minute")
	if hits.Load() != 2 || lim.Status()["rpm_denials"] != uint64(1) {
		t.Fatalf("hits %d rpm_denials %v, want 2 / 1", hits.Load(), lim.Status()["rpm_denials"])
	}
	if n := e.deniedRows(t, k.ID, 429, "rpm_exceeded"); n != 1 {
		t.Fatalf("captured rpm denials = %d, want 1", n)
	}
}

// TestLimits_MaxTokens: over the cap → 400 (never clamped, never forwarded,
// captured); at the cap → forwarded unchanged.
func TestLimits_MaxTokens(t *testing.T) {
	e := newLimitsEnv(t)
	maxTok := 100
	k := e.key(t, &store.Limits{MaxTokens: &maxTok})
	gw, lim, hits := e.plane(t, k.ID, e.sink)

	resp, body := do(t, http.MethodPost, gw.URL+"/v1/messages", nil, message(101))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d (%s), want 400", resp.StatusCode, body)
	}
	assertEnvelope(t, body, "invalid_request_error", "max_tokens 101 exceeds this key's limit of 100")
	if hits.Load() != 0 || e.deniedRows(t, k.ID, 400, "max_tokens_exceeded") != 1 {
		t.Fatal("over-cap request was forwarded or not captured")
	}
	if resp, body := do(t, http.MethodPost, gw.URL+"/v1/messages", nil, message(100)); resp.StatusCode != http.StatusOK {
		t.Fatalf("at-cap status = %d (%s), want 200", resp.StatusCode, body)
	}
	if st := lim.Status(); st["budget_denials"] != uint64(0) || st["rpm_denials"] != uint64(0) {
		t.Fatalf("a max_tokens 400 moved the 429 counters: %v", st)
	}
}

// TestLimits_NoLimitsAndNoKey: a key without limits, and a Principal with
// no key id (OIDC/dev), both pass untouched.
func TestLimits_NoLimitsAndNoKey(t *testing.T) {
	e := newLimitsEnv(t)
	k := e.key(t, nil)
	gw, _, _ := e.plane(t, k.ID, e.sink)
	if resp, body := do(t, http.MethodPost, gw.URL+"/v1/messages", nil, message(1<<20)); resp.StatusCode != http.StatusOK {
		t.Fatalf("unlimited key: status = %d (%s)", resp.StatusCode, body)
	}
	gw2, _, _ := e.plane(t, "", e.sink)
	if resp, body := do(t, http.MethodPost, gw2.URL+"/v1/messages", nil, message(16)); resp.StatusCode != http.StatusOK {
		t.Fatalf("no key id: status = %d (%s)", resp.StatusCode, body)
	}
}

// TestLimits_USDBudgetWithoutSpendReaderFailsClosed: a USD budget with no
// Postgres capture store cannot be checked, so the key is refused (503)
// rather than served unbudgeted; its non-USD limits still apply normally.
func TestLimits_USDBudgetWithoutSpendReaderFailsClosed(t *testing.T) {
	e := newLimitsEnv(t)
	daily := 100.0
	k := e.key(t, &store.Limits{DailyUSD: &daily})
	gw, _, hits := e.plane(t, k.ID, nil)
	resp, body := do(t, http.MethodPost, gw.URL+"/v1/messages", nil, message(16))
	if resp.StatusCode != http.StatusServiceUnavailable || hits.Load() != 0 {
		t.Fatalf("status = %d (%s) hits %d, want 503 and no upstream call", resp.StatusCode, body, hits.Load())
	}
	rpm := 5
	k2 := e.key(t, &store.Limits{RPM: &rpm})
	gw2, _, _ := e.plane(t, k2.ID, nil)
	if resp, body := do(t, http.MethodPost, gw2.URL+"/v1/messages", nil, message(16)); resp.StatusCode != http.StatusOK {
		t.Fatalf("rpm-only key without spend reader: status = %d (%s)", resp.StatusCode, body)
	}
}

// TestLimits_CacheRefreshAndStaleOnError drives the Limiter's clock: a
// PATCHed limit is picked up after the TTL, a failed refresh keeps serving
// the cached values, and a key with nothing cached is refused (503) when
// the database is unreachable.
func TestLimits_CacheRefreshAndStaleOnError(t *testing.T) {
	e := newLimitsEnv(t)
	ctx := context.Background()
	rpm := 1000
	k := e.key(t, &store.Limits{RPM: &rpm})
	cold := e.key(t, &store.Limits{RPM: &rpm})

	clock := time.Now()
	lim, err := NewLimiter(LimiterConfig{Keys: e.store.APIKeys(), Spend: e.sink, Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatal(err)
	}
	if d := lim.check(ctx, e.tenant.ID, k.ID, []byte(message(1<<20))); d != nil {
		t.Fatalf("no max_tokens limit yet: denied %+v", d)
	}

	maxTok := 10
	if err := e.store.APIKeys().SetLimits(ctx, e.tenant.ID, k.ID, &store.Limits{MaxTokens: &maxTok}); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(5 * time.Second) // inside the TTL: still the cached limits
	if d := lim.check(ctx, e.tenant.ID, k.ID, []byte(message(1<<20))); d != nil {
		t.Fatalf("limits changed inside the 10 s TTL: %+v", d)
	}
	clock = clock.Add(6 * time.Second) // past the TTL: re-read
	if d := lim.check(ctx, e.tenant.ID, k.ID, []byte(message(11))); d == nil || d.status != http.StatusBadRequest {
		t.Fatalf("after the TTL the new max_tokens limit must apply, got %+v", d)
	}

	// Database gone: the warm key keeps its cached limits, the cold one is refused.
	_ = e.store.Close()
	clock = clock.Add(11 * time.Second)
	if d := lim.check(ctx, e.tenant.ID, k.ID, []byte(message(11))); d == nil || d.status != http.StatusBadRequest {
		t.Fatalf("stale fallback: want the cached max_tokens 400, got %+v", d)
	}
	if d := lim.check(ctx, e.tenant.ID, cold.ID, []byte(message(1))); d == nil || d.status != http.StatusServiceUnavailable {
		t.Fatalf("cold key with the database down: want 503, got %+v", d)
	}
}

// ---------------------------------------------------------------------------
// Model-level limits (models.limits), mirroring the key-level tests above.
// ---------------------------------------------------------------------------

// modelPlane serves the LLM plane with the limiter AND a model registry
// over the real store, holding one registered model "team-sonnet" with the
// given limits whose single target is the stub upstream (priced as
// claude-sonnet-4-5, ~$0.018 per call). The caller is an agent key of the
// tenant without limits of its own.
func (e *limitsEnv) modelPlane(t *testing.T, l *store.Limits, spend pkgsink.SpendReader) (*httptest.Server, *Limiter, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","model":"claude-sonnet-4-5","stop_reason":"end_turn","usage":{"input_tokens":1000,"output_tokens":1000}}`)
	}))
	t.Cleanup(up.Close)
	if err := e.store.Models().Create(context.Background(), &store.Model{
		TenantID: e.tenant.ID, Name: "team-sonnet", Enabled: true, Limits: l,
		Targets: []store.ModelTarget{{Vendor: "anthropic", Model: "claude-sonnet-4-5", BaseURL: up.URL, AllowCallerKey: true}},
	}); err != nil {
		t.Fatalf("create model: %v", err)
	}
	k := e.key(t, nil)
	lim, err := NewLimiter(LimiterConfig{Keys: e.store.APIKeys(), Spend: spend})
	if err != nil {
		t.Fatal(err)
	}
	h, err := Handler(Config{
		UpstreamBaseURL: "http://127.0.0.1:1", MaxRequestBytes: 1 << 20, Authorizer: pkgauth.NewRoleAuthorizer(), Limiter: lim,
		Registry: NewRegistry(e.store.Models(), nil, RegistryOptions{}),
	}, capture.NewRecorder(e.sink, nil, nil, 0))
	if err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(withPrincipal(h, &pkgauth.Principal{
		Subject: k.ID, TenantID: e.tenant.ID, KeyID: k.ID, AuthMethod: "apikey", Roles: []string{"agent"},
	}))
	t.Cleanup(gw.Close)
	return gw, lim, &hits
}

func modelMessage(maxTokens int) string {
	return `{"model":"team-sonnet","max_tokens":` + strconv.Itoa(maxTokens) + `,"messages":[{"role":"user","content":"hi"}]}`
}

// seedModel writes priced llm_calls rows that asked for requestedModel.
func (e *limitsEnv) seedModel(t *testing.T, tenantID, requestedModel string, at time.Time, costs ...float64) {
	t.Helper()
	for _, c := range costs {
		c := c
		if err := e.sink.WriteBatch(context.Background(), []*pkgsink.LLMCall{{
			Timestamp: at, RequestID: uuid.NewString(), TenantID: tenantID, KeyID: "any-key",
			Provider: "anthropic", Model: "claude-sonnet-4-5", RequestedModel: requestedModel, ResolvedModel: "claude-sonnet-4-5",
			StatusCode: 200, CostUSD: &c,
		}}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
}

// deniedModelRows counts the captured denials for a requested model.
func (e *limitsEnv) deniedModelRows(t *testing.T, requestedModel string, status int, errPrefix string) int {
	t.Helper()
	var n int
	if err := e.db.QueryRow(`SELECT count(*) FROM llm_calls WHERE requested_model = $1 AND status_code = $2 AND error LIKE $3
		AND cost_usd IS NULL AND fallback_index = -1`, requestedModel, status, errPrefix+"%").Scan(&n); err != nil {
		t.Fatalf("count denied rows: %v", err)
	}
	return n
}

// TestModelSpend_Postgres pins the spend query: day vs month windows,
// tenant and requested-model scoping (not the resolved one), NULL cost
// counted as 0.
func TestModelSpend_Postgres(t *testing.T) {
	e := newLimitsEnv(t)
	monthStart := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	dayStart := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)
	e.seedModel(t, e.tenant.ID, "team-sonnet", dayStart.Add(10*time.Hour), 0.5)
	e.seedModel(t, e.tenant.ID, "team-sonnet", dayStart, 0.125) // boundary: inclusive
	e.seedModel(t, e.tenant.ID, "team-sonnet", time.Date(2026, 9, 10, 8, 0, 0, 0, time.UTC), 0.25)
	e.seedModel(t, e.tenant.ID, "team-sonnet", monthStart.Add(-time.Second), 1) // last month
	e.seedModel(t, e.tenant.ID, "other-name", dayStart.Add(time.Hour), 9)       // same resolved model, other name
	e.seedModel(t, "other-tenant", "team-sonnet", dayStart.Add(time.Hour), 9)   // other tenant
	if err := e.sink.WriteBatch(context.Background(), []*pkgsink.LLMCall{{      // unknown cost
		Timestamp: dayStart.Add(time.Hour), RequestID: uuid.NewString(), TenantID: e.tenant.ID, RequestedModel: "team-sonnet", StatusCode: 200,
	}}); err != nil {
		t.Fatal(err)
	}

	day, month, err := e.sink.ModelSpend(context.Background(), e.tenant.ID, "team-sonnet", dayStart, monthStart)
	if err != nil {
		t.Fatalf("ModelSpend: %v", err)
	}
	if !near(day, 0.625) || !near(month, 0.875) {
		t.Fatalf("ModelSpend = day %v month %v, want 0.625 / 0.875", day, month)
	}
	if day, month, err := e.sink.ModelSpend(context.Background(), e.tenant.ID, "nobody", dayStart, monthStart); err != nil || day != 0 || month != 0 {
		t.Fatalf("ModelSpend(no rows) = %v %v %v, want 0 0 nil", day, month, err)
	}
}

// TestModelLimits_DailyBudget429: spend already captured for the model
// name puts it over its daily budget, so the next call -- from a key with
// no limits of its own -- is refused with the 429 envelope and a
// Retry-After to the next UTC midnight, never reaches the upstream, and is
// captured; another model name is unaffected.
func TestModelLimits_DailyBudget429(t *testing.T) {
	e := newLimitsEnv(t)
	daily := 0.01
	e.seedModel(t, e.tenant.ID, "team-sonnet", time.Now(), 0.006, 0.006)
	gw, lim, hits := e.modelPlane(t, &store.Limits{DailyUSD: &daily}, e.sink)

	resp, body := do(t, http.MethodPost, gw.URL+"/v1/messages", map[string]string{"x-api-key": "sk"}, modelMessage(16))
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d (%s), want 429", resp.StatusCode, body)
	}
	assertEnvelope(t, body, "rate_limit_error", "daily budget exceeded for model team-sonnet")
	now := time.Now().UTC()
	want := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC).Sub(now).Seconds()
	ra, err := strconv.Atoi(resp.Header.Get("Retry-After"))
	if err != nil || float64(ra) < want-2 || float64(ra) > want+2 {
		t.Fatalf("Retry-After = %q, want ≈ %.0f (seconds to next UTC midnight)", resp.Header.Get("Retry-After"), want)
	}
	if hits.Load() != 0 {
		t.Fatal("a denied call reached the upstream")
	}
	if n := e.deniedModelRows(t, "team-sonnet", 429, "budget_exceeded: daily budget exceeded for model team-sonnet"); n != 1 {
		t.Fatalf("captured 429 rows = %d, want 1", n)
	}
	if got := lim.Status()["budget_denials"]; got != uint64(1) || lim.BudgetDenials() != 1 {
		t.Fatalf("budget_denials = %v (getter %d), want 1", got, lim.BudgetDenials())
	}
}

// TestModelLimits_MonthlyBudgetAndOwnSpend: a call under budget goes
// through; its captured cost is added to the model's cached spend at once,
// so the next call is refused without waiting for the refresh.
func TestModelLimits_MonthlyBudgetAndOwnSpend(t *testing.T) {
	e := newLimitsEnv(t)
	monthly := 0.01
	gw, _, hits := e.modelPlane(t, &store.Limits{MonthlyUSD: &monthly}, e.sink)

	if resp, body := do(t, http.MethodPost, gw.URL+"/v1/messages", map[string]string{"x-api-key": "sk"}, modelMessage(16)); resp.StatusCode != http.StatusOK {
		t.Fatalf("first call status = %d (%s), want 200", resp.StatusCode, body)
	}
	resp, body := do(t, http.MethodPost, gw.URL+"/v1/messages", map[string]string{"x-api-key": "sk"}, modelMessage(16))
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second call status = %d (%s), want 429 (first call's ~$0.018 already over $0.01)", resp.StatusCode, body)
	}
	assertEnvelope(t, body, "rate_limit_error", "monthly budget exceeded for model team-sonnet")
	now := time.Now().UTC()
	want := time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC).Sub(now).Seconds()
	if ra, _ := strconv.Atoi(resp.Header.Get("Retry-After")); float64(ra) < want-2 || float64(ra) > want+2 {
		t.Fatalf("Retry-After = %q, want ≈ %.0f", resp.Header.Get("Retry-After"), want)
	}
	if hits.Load() != 1 {
		t.Fatalf("upstream hits = %d, want 1", hits.Load())
	}
}

// TestModelLimits_RPM: rpm 2 on the model admits two calls and refuses the
// third within the minute with Retry-After: 1; an unregistered model name
// on the same plane is not limited.
func TestModelLimits_RPM(t *testing.T) {
	e := newLimitsEnv(t)
	rpm := 2
	gw, lim, hits := e.modelPlane(t, &store.Limits{RPM: &rpm}, e.sink)
	for i := 0; i < 2; i++ {
		if resp, body := do(t, http.MethodPost, gw.URL+"/v1/messages", map[string]string{"x-api-key": "sk"}, modelMessage(16)); resp.StatusCode != http.StatusOK {
			t.Fatalf("call %d status = %d (%s)", i+1, resp.StatusCode, body)
		}
	}
	resp, body := do(t, http.MethodPost, gw.URL+"/v1/messages", map[string]string{"x-api-key": "sk"}, modelMessage(16))
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "1" {
		t.Fatalf("third call = %d Retry-After %q (%s), want 429 with Retry-After 1", resp.StatusCode, resp.Header.Get("Retry-After"), body)
	}
	assertEnvelope(t, body, "rate_limit_error", "rate limit exceeded for model team-sonnet (requests per minute)")
	if hits.Load() != 2 || lim.Status()["rpm_denials"] != uint64(1) {
		t.Fatalf("hits %d rpm_denials %v, want 2 / 1", hits.Load(), lim.Status()["rpm_denials"])
	}
	if n := e.deniedModelRows(t, "team-sonnet", 429, "rpm_exceeded"); n != 1 {
		t.Fatalf("captured rpm denials = %d, want 1", n)
	}
}

// TestModelLimits_MaxTokens: over the model's cap → 400 (never clamped,
// never forwarded, captured); at the cap → forwarded.
func TestModelLimits_MaxTokens(t *testing.T) {
	e := newLimitsEnv(t)
	maxTok := 100
	gw, lim, hits := e.modelPlane(t, &store.Limits{MaxTokens: &maxTok}, e.sink)

	resp, body := do(t, http.MethodPost, gw.URL+"/v1/messages", map[string]string{"x-api-key": "sk"}, modelMessage(101))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d (%s), want 400", resp.StatusCode, body)
	}
	assertEnvelope(t, body, "invalid_request_error", "max_tokens 101 exceeds model team-sonnet's limit of 100")
	if hits.Load() != 0 || e.deniedModelRows(t, "team-sonnet", 400, "max_tokens_exceeded") != 1 {
		t.Fatal("over-cap request was forwarded or not captured")
	}
	if resp, body := do(t, http.MethodPost, gw.URL+"/v1/messages", map[string]string{"x-api-key": "sk"}, modelMessage(100)); resp.StatusCode != http.StatusOK {
		t.Fatalf("at-cap status = %d (%s), want 200", resp.StatusCode, body)
	}
	if st := lim.Status(); st["budget_denials"] != uint64(0) || st["rpm_denials"] != uint64(0) {
		t.Fatalf("a max_tokens 400 moved the 429 counters: %v", st)
	}
}

// TestModelLimits_USDBudgetWithoutSpendReaderFailsClosed: a model with a
// USD budget and no Postgres capture store is refused (503), not served
// unbudgeted.
func TestModelLimits_USDBudgetWithoutSpendReaderFailsClosed(t *testing.T) {
	e := newLimitsEnv(t)
	daily := 100.0
	gw, _, hits := e.modelPlane(t, &store.Limits{DailyUSD: &daily}, nil)
	resp, body := do(t, http.MethodPost, gw.URL+"/v1/messages", map[string]string{"x-api-key": "sk"}, modelMessage(16))
	if resp.StatusCode != http.StatusServiceUnavailable || hits.Load() != 0 {
		t.Fatalf("status = %d (%s) hits %d, want 503 and no upstream call", resp.StatusCode, body, hits.Load())
	}
}

// TestModelLimits_ChangedLimitsApplyAtOnce drives checkModel directly: the
// limits are the registry row's, so a row with new limits is enforced on
// the very next call (no 10 s lag), and its spend is re-read for it.
func TestModelLimits_ChangedLimitsApplyAtOnce(t *testing.T) {
	e := newLimitsEnv(t)
	ctx := context.Background()
	e.seedModel(t, e.tenant.ID, "team-sonnet", time.Now(), 0.5)
	clock := time.Now()
	lim, err := NewLimiter(LimiterConfig{Keys: e.store.APIKeys(), Spend: e.sink, Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatal(err)
	}
	rpm := 1000
	row := &store.Model{TenantID: e.tenant.ID, Name: "team-sonnet", Limits: &store.Limits{RPM: &rpm}}
	if d := lim.checkModel(ctx, e.tenant.ID, row, []byte(modelMessage(16))); d != nil {
		t.Fatalf("rpm 1000: denied %+v", d)
	}
	// No limits at all: never denied, nothing cached.
	if d := lim.checkModel(ctx, e.tenant.ID, &store.Model{TenantID: e.tenant.ID, Name: "free"}, []byte(modelMessage(1<<20))); d != nil {
		t.Fatalf("model without limits: denied %+v", d)
	}
	daily := 0.4
	edited := &store.Model{TenantID: e.tenant.ID, Name: "team-sonnet", Limits: &store.Limits{DailyUSD: &daily}}
	clock = clock.Add(time.Second) // well inside the TTL
	d := lim.checkModel(ctx, e.tenant.ID, edited, []byte(modelMessage(16)))
	if d == nil || d.status != http.StatusTooManyRequests || !strings.Contains(d.message, "daily budget exceeded for model team-sonnet") {
		t.Fatalf("edited row: want the daily budget 429 ($0.5 spent, $0.4 budget), got %+v", d)
	}
	// Another tenant asking for the same name has its own spend.
	if d := lim.checkModel(ctx, "other-tenant", edited, []byte(modelMessage(16))); d != nil {
		t.Fatalf("other tenant: denied %+v", d)
	}
}
