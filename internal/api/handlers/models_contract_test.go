package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/analytics"
)

// consoleFixture is the file the console's tests share with this one:
// the requests the console sends and the responses these handlers give.
// TestModels_ConsoleContract replays the requests through the real
// handlers and compares what they answer with the file;
// web/src/test/models.contract.test.tsx checks the same file against the
// console's types and against what its dialog actually sends. A change on
// either side that the other does not follow fails one of the two.
//
// Regenerate the responses after an intended API change with
//
//	UPDATE_CONSOLE_FIXTURES=1 go test ./internal/api/handlers -run TestModels_ConsoleContract
const consoleFixture = "../../../web/src/test/fixtures/models-api.json"

type contractExchange struct {
	Request  json.RawMessage `json:"request,omitempty"`
	Status   int             `json:"status"`
	Response json.RawMessage `json:"response"`
}

type contractFixture struct {
	Comment string `json:"_comment"`
	// Create is what the Add dialog sends for a new model.
	Create contractExchange `json:"create"`
	// Seed registers the model the Edit dialog is opened on.
	Seed contractExchange `json:"seed"`
	// List is GET /models after both (plus one platform row).
	List contractExchange `json:"list"`
	// Update is what the Edit dialog sends for the seeded model.
	Update contractExchange `json:"update"`
	// Invalid is a request the API refuses (400 envelope).
	Invalid contractExchange `json:"invalid"`
	// AnalyticsModels is GET /analytics/models.
	AnalyticsModels contractExchange `json:"analytics_models"`
}

// fixedTime replaces every timestamp the handlers stamp, so the file is
// stable.
const fixedTime = "2026-01-01T00:00:00Z"

func normalizeTimes(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if (k == "created_at" || k == "updated_at") && e != nil {
				x[k] = fixedTime
				continue
			}
			x[k] = normalizeTimes(e)
		}
	case []any:
		for i := range x {
			x[i] = normalizeTimes(x[i])
		}
	}
	return v
}

func TestModels_ConsoleContract(t *testing.T) {
	raw, err := os.ReadFile(filepath.FromSlash(consoleFixture))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var fx contractFixture
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&fx); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	update := os.Getenv("UPDATE_CONSOLE_FIXTURES") != ""

	deps, _, _ := newModelDeps(t) // platform row "haiku", credential "anthropic-prod" in tenant-a
	h := Models{Deps: deps}

	check := func(name string, ex *contractExchange, w *httptest.ResponseRecorder) map[string]any {
		t.Helper()
		var got any
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("%s: response is not JSON: %s", name, w.Body.String())
		}
		got = normalizeTimes(got)
		if update {
			b, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			ex.Status, ex.Response = w.Code, b
		} else {
			var want any
			if err := json.Unmarshal(ex.Response, &want); err != nil {
				t.Fatalf("%s: fixture response is not JSON: %v", name, err)
			}
			if w.Code != ex.Status || !reflect.DeepEqual(got, want) {
				gotJSON, _ := json.MarshalIndent(got, "", "  ")
				t.Errorf("%s: the handler answers %d\n%s\nbut the console fixture has %d\n%s", name, w.Code, gotJSON, ex.Status, ex.Response)
			}
		}
		m, _ := got.(map[string]any)
		return m
	}
	post := func(body json.RawMessage) *httptest.ResponseRecorder {
		return postModel(t, h, "tenant-a", string(body))
	}

	check("create", &fx.Create, post(fx.Create.Request))
	seeded := check("seed", &fx.Seed, post(fx.Seed.Request))
	id, _ := seeded["id"].(string)
	if id == "" {
		t.Fatalf("seed: no id in %v", seeded)
	}

	list := serve(http.MethodGet, "/models", h.List, withPrincipal(httptest.NewRequest(http.MethodGet, "/models", nil), "tenant-a", "admin"))
	check("list", &fx.List, list)

	put := withPrincipal(httptest.NewRequest(http.MethodPut, "/models/"+id, strings.NewReader(string(fx.Update.Request))), "tenant-a", "admin")
	check("update", &fx.Update, serve(http.MethodPut, "/models/{id}", h.Update, put))

	check("invalid", &fx.Invalid, post(fx.Invalid.Request))

	// GET /analytics/models: the real handler over a reader that returns
	// one priced row, one unpriced row.
	cost := 0.0421
	seen := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	reader := &fakeAnalyticsReader{modelsSummary: &analytics.ModelsSummary{
		Range: analytics.Range7d,
		Models: []analytics.ModelSummaryRow{
			{Name: "local-coder", Provider: "openai_compat", Calls: 12, Tokens: 3400, CostUSD: &cost, UsedBy: 2, LastSeen: seen, Status: "active"},
			{Name: "claude-sonnet-4-5", Provider: "anthropic", Calls: 1, Tokens: 20, UsedBy: 1, LastSeen: seen, Status: "active"},
		},
		TotalModels:    2,
		HighestTraffic: &analytics.ModelHighestTraffic{Name: "local-coder", Tokens: 3400},
	}}
	ah := Analytics{Deps: newAnalyticsTestDeps(reader)}
	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/analytics/models?range=7d", nil), "tenant-a", "admin")
	check("analytics_models", &fx.AnalyticsModels, serve(http.MethodGet, "/analytics/models", ah.Models, req))

	if update {
		out, err := json.MarshalIndent(fx, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.FromSlash(consoleFixture), append(out, '\n'), 0o644); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
		t.Logf("rewrote %s", consoleFixture)
	}
}
