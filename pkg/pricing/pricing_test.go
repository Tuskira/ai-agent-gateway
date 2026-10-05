package pricing

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

func wantCost(t *testing.T, got *float64, ok bool, want float64) {
	t.Helper()
	if !ok || got == nil {
		t.Fatalf("cost = (%v, %v), want %v", got, ok, want)
	}
	if math.Abs(*got-want) > 1e-12 {
		t.Fatalf("cost = %v, want %v", *got, want)
	}
}

// Anthropic reports cache tokens SEPARATELY from input — base input is the
// input count as-is, and cache_read/cache_write are added on top.
func TestCost_AnthropicSeparateCache(t *testing.T) {
	// haiku $1/$5, cache_read $0.10, cache_write $1.25 per Mtok.
	got, ok := Cost("anthropic", "claude-haiku-4-5", Usage{Input: 100, Output: 200, CacheRead: 10000, CacheCreation: 5000})
	wantCost(t, got, ok, (100*1.0+10000*0.10+5000*1.25+200*5.0)/1e6) // 0.008350
}

// A dated Anthropic snapshot id falls back to its undated alias.
func TestCost_AnthropicDatedFallback(t *testing.T) {
	got, ok := Cost("anthropic", "claude-haiku-4-5-20251001", Usage{Input: 15, Output: 6})
	wantCost(t, got, ok, (15*1.0+6*5.0)/1e6) // 0.000045
}

// OpenAI reports cached tokens as a SUBSET of input — non-cached input is
// input-cacheRead, so cached tokens are not billed twice.
func TestCost_OpenAISubsetCache(t *testing.T) {
	got, ok := Cost("openai", "gpt-4o-mini", Usage{Input: 1000, Output: 50, CacheRead: 800})
	wantCost(t, got, ok, (200*0.15+800*0.075+50*0.60)/1e6) // 0.000120
}

// Gemini is also subset-cache.
func TestCost_GeminiSubsetCache(t *testing.T) {
	got, ok := Cost("gemini", "gemini-2.5-flash", Usage{Input: 1000, Output: 100, CacheRead: 400})
	wantCost(t, got, ok, (600*0.30+400*0.03+100*2.50)/1e6)
}

// gemini-3.8-flash thinking: Output already folds in thinking tokens.
func TestCost_Gemini38FlashThinking(t *testing.T) {
	got, ok := Cost("gemini", "gemini-3.8-flash", Usage{Input: 13, Output: 83})
	wantCost(t, got, ok, (13*0.75+83*3.75)/1e6) // 0.00032100
}

// Bedrock inference-profile ids carry a region prefix that must be stripped;
// for Claude 4.5+ the prefix also selects the price: global./in-region bill the
// base rate, geographic profiles 1.1x, GovCloud 1.2x.
func TestCost_BedrockRegionNormalize(t *testing.T) {
	base := (10*1.0 + 4*5.0) / 1e6 // 0.000030
	for id, mult := range map[string]float64{
		"global.anthropic.claude-haiku-4-5-20251001-v1:0": 1,
		"anthropic.claude-haiku-4-5-20251001-v1:0":        1,
		"us.anthropic.claude-haiku-4-5-20251001-v1:0":     1.1,
		"eu.anthropic.claude-haiku-4-5-20251001-v1:0":     1.1,
		"jp.anthropic.claude-haiku-4-5-20251001-v1:0":     1.1,
		"us-gov.anthropic.claude-haiku-4-5-20251001-v1:0": 1.2,
	} {
		got, ok := Cost("bedrock", id, Usage{Input: 10, Output: 4})
		if !ok || math.Abs(*got-base*mult) > 1e-12 {
			t.Errorf("%s: cost = %v, want %v", id, got, base*mult)
		}
	}
	// A model without the premium (Opus 4.1) bills the same on every profile.
	got, ok := Cost("bedrock", "us.anthropic.claude-opus-4-1-20250805-v1:0", Usage{Input: 10, Output: 4})
	wantCost(t, got, ok, (10*15.0+4*75.0)/1e6)
	// The premium is Bedrock-only: first-party Anthropic is unaffected.
	got, ok = Cost("anthropic", "claude-haiku-4-5", Usage{Input: 10, Output: 4})
	wantCost(t, got, ok, base)
}

// An unknown model returns (nil, false) — never a guessed price.
func TestCost_UnknownModel(t *testing.T) {
	if got, ok := Cost("openai", "totally-made-up-model", Usage{Input: 10, Output: 10}); ok || got != nil {
		t.Fatalf("want (nil, false), got (%v, %v)", got, ok)
	}
}

// Current Claude 5-family flagships are priced (were NULL before the card refresh).
func TestCost_CurrentClaudeLineup(t *testing.T) {
	got, ok := Cost("anthropic", "claude-sonnet-5", Usage{Input: 1000, Output: 500})
	wantCost(t, got, ok, (1000*2.0+500*10.0)/1e6) // 0.007
}

// >200K prompt bills the WHOLE request at the long-context tier (Sonnet 4.5).
func TestCost_LongContextTier_Sonnet(t *testing.T) {
	// Below threshold: flat 3/15.
	lo, ok := Cost("anthropic", "claude-sonnet-4-5", Usage{Input: 100000, Output: 1000})
	wantCost(t, lo, ok, (100000*3.0+1000*15.0)/1e6)
	// Above threshold: 6/22.50, and cache_read scales to 0.60.
	hi, ok := Cost("anthropic", "claude-sonnet-4-5", Usage{Input: 250000, Output: 20000, CacheRead: 10000})
	wantCost(t, hi, ok, (250000*6.0+10000*0.60+20000*22.50)/1e6) // 1.956
}

// Gemini Pro >200K tier keys off the raw prompt (input already includes cache).
func TestCost_LongContextTier_Gemini(t *testing.T) {
	got, ok := Cost("gemini", "gemini-2.5-pro", Usage{Input: 250000, Output: 5000})
	wantCost(t, got, ok, (250000*2.50+5000*15.0)/1e6) // 0.70
}

// 1-hour cache writes bill at 2x base input; 5-minute at 1.25x.
func TestCost_Anthropic1hCacheSplit(t *testing.T) {
	// haiku: 1000 creation, 400 of it 1h -> 600@1.25x + 400@2x.
	got, ok := Cost("anthropic", "claude-haiku-4-5", Usage{CacheCreation: 1000, CacheCreation1h: 400})
	wantCost(t, got, ok, (600*1.25+400*(2*1.0))/1e6) // 0.00155
}

// Web-search server-tool fee: $10 / 1,000 requests, flat, on top of tokens.
func TestCost_WebSearchFee(t *testing.T) {
	got, ok := Cost("anthropic", "claude-haiku-4-5", Usage{Input: 10, Output: 5, WebSearchRequests: 3})
	wantCost(t, got, ok, (10*1.0+5*5.0)/1e6+3*0.01) // 0.030035
}

// OpenAI dashed-date snapshot falls back to the base model.
func TestCost_OpenAIDatedSnapshot(t *testing.T) {
	got, ok := Cost("openai", "gpt-4o-2024-08-06", Usage{Input: 1000, Output: 100})
	wantCost(t, got, ok, (1000*2.50+100*10.0)/1e6) // 0.0035
}

// A new dated Bedrock snapshot of a priced Claude model falls back to its alias.
func TestCost_BedrockDatedAliasFallback(t *testing.T) {
	got, ok := Cost("bedrock", "us.anthropic.claude-sonnet-5-20260514-v1:0", Usage{Input: 100, Output: 50})
	wantCost(t, got, ok, 1.1*(100*2.0+50*10.0)/1e6) // 0.00077 via claude-sonnet-5 alias, us. profile premium
}

func writeJSON(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "prices-override.json")
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write override file: %v", err)
	}
	return path
}

// MergeFile: a file overriding one model replaces just that model's rate;
// every other embedded model (and its behavior, e.g. Bedrock normalization)
// is untouched.
func TestMergeFile_OverridesOneModelKeepsRest(t *testing.T) {
	path := writeJSON(t, `{
		"models": {
			"claude-sonnet-4-5": {"input": 999.0, "output": 999.0, "cache_read": 99.0, "cache_write": 99.0}
		}
	}`)

	merged, n, err := Default.MergeFile(path)
	if err != nil {
		t.Fatalf("MergeFile: %v", err)
	}
	if n != 1 {
		t.Errorf("models loaded = %d, want 1", n)
	}

	// Overridden model uses the file's price.
	got, ok := merged.Cost("anthropic", "claude-sonnet-4-5", Usage{Input: 1_000_000})
	wantCost(t, got, ok, 999.0)

	// Untouched model still uses the embedded price.
	got, ok = merged.Cost("anthropic", "claude-haiku-4-5", Usage{Input: 1_000_000})
	wantCost(t, got, ok, 1.0)

	// Untouched Bedrock model, still region-normalized correctly.
	got, ok = merged.Cost("bedrock", "global.anthropic.claude-haiku-4-5-20251001-v1:0", Usage{Input: 10, Output: 4})
	wantCost(t, got, ok, (10*1.0+4*5.0)/1e6)

	// The source card (Default) is untouched by the merge — still the embedded
	// Sonnet 4.5, whose >200K long-context tier ($6/Mtok) applies at 1M tokens
	// (the override replaced the entry and dropped that tier, hence 999 above).
	got, ok = Default.Cost("anthropic", "claude-sonnet-4-5", Usage{Input: 1_000_000})
	wantCost(t, got, ok, 6.0)
}

// MergeFile: malformed JSON is a clear error, not a silent fallback.
func TestMergeFile_MalformedJSONIsError(t *testing.T) {
	path := writeJSON(t, `{not valid json`)
	if _, _, err := Default.MergeFile(path); err == nil {
		t.Fatal("want error for malformed JSON, got nil")
	}
}

// MergeFile: well-formed JSON with no models defined is also an error, not
// treated as "no-op override".
func TestMergeFile_NoModelsIsError(t *testing.T) {
	path := writeJSON(t, `{"models": {}}`)
	if _, _, err := Default.MergeFile(path); err == nil {
		t.Fatal("want error for a file with no models, got nil")
	}
}

// MergeFile: a missing file is an error.
func TestMergeFile_MissingFileIsError(t *testing.T) {
	if _, _, err := Default.MergeFile(filepath.Join(t.TempDir(), "does-not-exist.json")); err == nil {
		t.Fatal("want error for a missing file, got nil")
	}
}

// Pricing context: service tiers, Anthropic fast mode and US-only inference,
// GovCloud by region, and region-specific Bedrock rows.
func TestCost_PricingContext(t *testing.T) {
	for _, c := range []struct {
		name, provider, model string
		u                     Usage
		want                  float64
	}{
		{"openai priority", "openai", "gpt-4o", Usage{Input: 1000, CacheRead: 200, Output: 100, Tier: "priority"}, (800*4.25 + 200*2.125 + 100*17.0) / 1e6},
		{"openai flex falls back to standard", "openai", "gpt-4o", Usage{Input: 1000, Output: 100, Tier: "flex"}, (1000*2.50 + 100*10.0) / 1e6},
		{"gemini flex", "gemini", "gemini-3.8-flash", Usage{Input: 1000, Output: 100, Tier: "flex"}, (1000*0.375 + 100*1.875) / 1e6},
		{"gemini priority", "gemini", "gemini-2.5-flash", Usage{Input: 1000, Output: 100, Tier: "priority"}, (1000*0.54 + 100*4.50) / 1e6},
		{"anthropic fast", "anthropic", "claude-opus-5-5", Usage{Input: 1000, CacheRead: 1000, Output: 100, Fast: true}, 2 * (1000*4.0 + 1000*0.20 + 100*20.0) / 1e6},
		{"anthropic fast+us", "anthropic", "claude-opus-5-5", Usage{Input: 1000, Output: 100, Fast: true, GeoUS: true}, 2.2 * (1000*4.0 + 100*20.0) / 1e6},
		{"anthropic us geo", "anthropic", "claude-sonnet-5", Usage{Input: 1000, Output: 100, GeoUS: true}, 1.1 * (1000*2.0 + 100*10.0) / 1e6},
		{"us geo n/a on haiku", "anthropic", "claude-haiku-4-5", Usage{Input: 1000, Output: 100, GeoUS: true}, (1000*1.0 + 100*5.0) / 1e6},
		{"fast n/a on sonnet", "anthropic", "claude-sonnet-5", Usage{Input: 1000, Output: 100, Fast: true}, (1000*2.0 + 100*10.0) / 1e6},
		{"govcloud bare id", "bedrock", "anthropic.claude-sonnet-5", Usage{Input: 1000, Output: 100, Region: "us-gov-west-1"}, 1.2 * (1000*2.0 + 100*10.0) / 1e6},
		{"commercial bare id", "bedrock", "anthropic.claude-sonnet-5", Usage{Input: 1000, Output: 100, Region: "us-east-1"}, (1000*2.0 + 100*10.0) / 1e6},
		{"nova eu profile row", "bedrock", "eu.amazon.nova-lite-v1:0", Usage{Input: 1000, Output: 100, Region: "eu-west-1"}, (1000*0.078 + 100*0.312) / 1e6},
		{"nova govcloud region row", "bedrock", "amazon.nova-micro-v1:0", Usage{Input: 1000, Output: 100, Region: "us-gov-west-1"}, (1000*0.042 + 100*0.168) / 1e6},
		{"nova us profile = base", "bedrock", "us.amazon.nova-lite-v1:0", Usage{Input: 1000, Output: 100, Region: "us-east-1"}, (1000*0.06 + 100*0.24) / 1e6},
	} {
		got, ok := Cost(c.provider, c.model, c.u)
		if !ok || math.Abs(*got-c.want) > 1e-12 {
			t.Errorf("%s: cost = %v, want %v", c.name, got, c.want)
		}
	}
}

// Gemini Pro priority and flex tiers.
func TestCost_GeminiProTiers(t *testing.T) {
	for _, c := range []struct {
		model, tier string
		want        float64
	}{
		{"gemini-2.5-pro", "priority", (1000*2.25 + 100*18.0) / 1e6},
		{"gemini-2.5-pro", "flex", (1000*0.625 + 100*5.0) / 1e6},
		{"gemini-3.1-pro-preview", "priority", (1000*3.60 + 100*21.6) / 1e6},
		{"gemini-3.1-pro-preview", "flex", (1000*1.00 + 100*6.0) / 1e6},
	} {
		got, ok := Cost("gemini", c.model, Usage{Input: 1000, Output: 100, Tier: c.tier})
		if !ok || math.Abs(*got-c.want) > 1e-12 {
			t.Errorf("%s %s: cost = %v, want %v", c.model, c.tier, got, c.want)
		}
	}
	// Priority above the 200K threshold uses the priority.above_200k row.
	got, ok := Cost("gemini", "gemini-2.5-pro", Usage{Input: 250_000, Output: 100, Tier: "priority"})
	if !ok || math.Abs(*got-(250_000*4.50+100*27.0)/1e6) > 1e-12 {
		t.Errorf("pro priority >200K: cost = %v", got)
	}
}

// Every flex row in the embedded card follows one convention, so a new model's
// tier row can be checked against it instead of eyeballed: the flex tier halves
// input and output and leaves cache_read at the standard rate (the published
// Gemini batch/flex discount applies to the tokens a call generates, not to
// reading an already-written cache). gemini-3.8-flash once carried a halved
// cache_read, under-billing every cached flex call by 50%.
func TestFlexRowsFollowTheHalfRateConvention(t *testing.T) {
	seen := 0
	for model, r := range Default.table {
		if r.Flex == nil {
			continue
		}
		seen++
		if math.Abs(r.Flex.Input-0.5*r.Input) > 1e-12 {
			t.Errorf("%s: flex.input = %v, want %v (half of input)", model, r.Flex.Input, 0.5*r.Input)
		}
		if math.Abs(r.Flex.Output-0.5*r.Output) > 1e-12 {
			t.Errorf("%s: flex.output = %v, want %v (half of output)", model, r.Flex.Output, 0.5*r.Output)
		}
		if math.Abs(r.Flex.CacheRead-r.CacheRead) > 1e-12 {
			t.Errorf("%s: flex.cache_read = %v, want %v (the standard rate)", model, r.Flex.CacheRead, r.CacheRead)
		}
	}
	if seen == 0 {
		t.Fatal("no model in the embedded card has a flex row; the invariant checked nothing")
	}
}

func TestUnpricedPath(t *testing.T) {
	for path, want := range map[string]bool{
		"/anthropic/v1/messages/count_tokens":                          true,
		"/bedrock/model/anthropic.claude-sonnet-4-5/count-tokens":      true,
		"/gemini/v1beta/models/gemini-2.5-flash:countTokens":           true,
		"/openai/v1/responses/input_tokens":                            true,
		"/anthropic/v1/messages/batches/msgbatch_1/results":            true,
		"/v1/messages/batches":                                         true,
		"/v1/messages/batches-export":                                  false, // a substring match priced this at $0
		"/v1/messages/batchesque":                                      false,
		"/anthropic/v1/messages":                                       false,
		"/bedrock/model/anthropic.claude-sonnet-4-5/invoke":            false,
		"/gemini/v1beta/models/gemini-2.5-flash:generateContent":       false,
		"/openai/v1/chat/completions":                                  false,
		"/openai/v1/responses":                                         false,
		"/gemini/v1beta/models/gemini-2.5-flash:streamGenerateContent": false,
	} {
		if got := UnpricedPath(path); got != want {
			t.Errorf("UnpricedPath(%q) = %v, want %v", path, got, want)
		}
	}
}
