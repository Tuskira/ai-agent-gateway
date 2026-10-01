package anthropic

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm/llmtest"
)

func TestDialectConformance(t *testing.T) { llmtest.RunDialect(t, Dialect{}) }

// The Provider renders back what the Dialect parsed: parse(render(parse(x)))
// equals parse(x), unknown fields and blocks included, with the model swapped.
func TestProviderRoundTrip(t *testing.T) {
	body := `{"model":"alias","max_tokens":9,"stream":true,"top_k":3,"metadata":{"user_id":"u"},"container":"c",
		"output_config":{"effort":"low","format":{"type":"json_schema"}},"thinking":{"type":"enabled","budget_tokens":1024,"display":"x"},
		"system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral"}}],
		"tools":[{"name":"f","input_schema":{"type":"object"},"strict":true},{"type":"web_search_20250305","name":"web_search","max_uses":2}],
		"tool_choice":{"type":"tool","name":"f"},
		"messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"search_result","source":"s","title":"t","content":[]}]},
			{"role":"assistant","content":[{"type":"thinking","thinking":"t","signature":"g"},{"type":"tool_use","id":"t1","name":"f","input":{"a":1}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","is_error":true,"content":"no"}]}]}`
	first, err := Dialect{}.ParseRequest(httptest.NewRequest("POST", "/", bytes.NewReader([]byte(body))))
	if err != nil {
		t.Fatal(err)
	}
	wire, err := renderRequest(first, "upstream")
	if err != nil {
		t.Fatal(err)
	}
	second, err := parseRequest(wire)
	if err != nil {
		t.Fatal(err)
	}
	if second.Model != "upstream" {
		t.Errorf("model = %q", second.Model)
	}
	second.Model = first.Model
	a, _ := json.Marshal(first)
	b, _ := json.Marshal(second)
	var x, y any
	_ = json.Unmarshal(a, &x)
	_ = json.Unmarshal(b, &y)
	if !reflect.DeepEqual(x, y) {
		t.Errorf("round trip changed the request\nfirst:  %s\nsecond: %s", a, b)
	}
}

// Live: LLMTEST_ANTHROPIC_API_KEY (and optionally LLMTEST_ANTHROPIC_MODEL,
// LLMTEST_ANTHROPIC_BASE_URL) run the provider suite against Anthropic.
func TestProviderConformance(t *testing.T) {
	key := os.Getenv("LLMTEST_ANTHROPIC_API_KEY")
	if key == "" {
		t.Skip("LLMTEST_ANTHROPIC_API_KEY not set")
	}
	target := llm.Target{Vendor: "anthropic", BaseURL: envOr("LLMTEST_ANTHROPIC_BASE_URL", "https://api.anthropic.com"),
		Model: envOr("LLMTEST_ANTHROPIC_MODEL", "claude-haiku-4-5"), Auth: llm.Auth{APIKey: key}}
	llmtest.RunProvider(t, Provider{}, target, llmtest.WithThinking())
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
