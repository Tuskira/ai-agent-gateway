package pricing

import (
	"encoding/json"
	"testing"
)

// *Card is the built-in Pricer, and Default works through the interface.
func TestCard_IsPricer(t *testing.T) {
	var p Pricer = Default
	got, ok := p.Cost("anthropic", "claude-haiku-4-5", Usage{Input: 100, Output: 200})
	wantCost(t, got, ok, (100*1.0+200*5.0)/1e6)
}

// A card built from rates prices with the same formula as the embedded one:
// the same Rate as the embedded entry gives the same cost, cache and
// long-context tiers included, and keys are normalized like MergeFile's.
func TestNewCardFromRates_MatchesEmbeddedFormula(t *testing.T) {
	var f cardFile
	if err := json.Unmarshal(pricesJSON, &f); err != nil {
		t.Fatal(err)
	}
	sonnet, ok := f.Models["claude-sonnet-4-5"]
	if !ok || sonnet.Above200k == nil {
		t.Fatal("embedded claude-sonnet-4-5 with an above_200k tier is the fixture this test needs")
	}
	c := NewCardFromRates(map[string]Rate{"  Claude-Sonnet-4-5 ": sonnet})

	for _, u := range []Usage{
		{Input: 100, Output: 200, CacheRead: 10_000, CacheCreation: 5_000, CacheCreation1h: 1_000},
		{Input: 250_000, Output: 10}, // past the 200K long-context threshold
	} {
		want, _ := Default.Cost("anthropic", "claude-sonnet-4-5", u)
		got, ok := c.Cost("anthropic", "claude-sonnet-4-5-20250929", u) // dated alias falls back too
		wantCost(t, got, ok, *want)
	}

	// The card holds only what it was given: no embedded models.
	if got, ok := c.Cost("anthropic", "claude-haiku-4-5", Usage{Input: 1}); ok || got != nil {
		t.Errorf("model not in rates priced at %v, want (nil, false)", got)
	}
}

// Bedrock region rows and the regional premium work on a rates-built card.
func TestNewCardFromRates_Bedrock(t *testing.T) {
	c := NewCardFromRates(map[string]Rate{
		"claude-x":                  {Input: 2, Output: 10, RegionalPremium: true},
		"us-gov-west-1/amazon.nova": {Input: 7, Output: 7},
	})
	got, ok := c.Cost("bedrock", "us.anthropic.claude-x-20260101-v1:0", Usage{Input: 1000, Output: 100})
	wantCost(t, got, ok, 1.1*(1000*2.0+100*10.0)/1e6)
	got, ok = c.Cost("bedrock", "amazon.nova", Usage{Input: 1_000_000, Region: "us-gov-west-1"})
	wantCost(t, got, ok, 7)
}

// The card keeps its own copy: changing the caller's map or a tier row
// afterwards does not change prices.
func TestNewCardFromRates_CopiesInput(t *testing.T) {
	flex := &Rate{Input: 1, Output: 1}
	rates := map[string]Rate{"m": {Input: 4, Output: 8, Flex: flex}}
	c := NewCardFromRates(rates)

	rates["m"] = Rate{Input: 1000}
	rates["other"] = Rate{Input: 1}
	flex.Input = 1000

	got, ok := c.Cost("openai", "m", Usage{Input: 1_000_000})
	wantCost(t, got, ok, 4)
	got, ok = c.Cost("openai", "m", Usage{Input: 1_000_000, Tier: "flex"})
	wantCost(t, got, ok, 1)
	if _, ok := c.Cost("openai", "other", Usage{Input: 1}); ok {
		t.Error("model added to the map after NewCardFromRates was priced")
	}
}

// An empty card prices nothing and never guesses.
func TestNewCardFromRates_Empty(t *testing.T) {
	for _, rates := range []map[string]Rate{nil, {}} {
		if got, ok := NewCardFromRates(rates).Cost("anthropic", "claude-haiku-4-5", Usage{Input: 1}); ok || got != nil {
			t.Errorf("empty card priced at %v, want (nil, false)", got)
		}
	}
}

// MergeFile still layers a file over a rates-built card.
func TestNewCardFromRates_MergeFile(t *testing.T) {
	c := NewCardFromRates(map[string]Rate{"a": {Input: 1}, "b": {Input: 2}})
	merged, n, err := c.MergeFile(writeJSON(t, `{"models":{"b":{"input":3}}}`))
	if err != nil || n != 1 {
		t.Fatalf("MergeFile = %d, %v", n, err)
	}
	got, ok := merged.Cost("openai", "a", Usage{Input: 1_000_000})
	wantCost(t, got, ok, 1)
	got, ok = merged.Cost("openai", "b", Usage{Input: 1_000_000})
	wantCost(t, got, ok, 3)
}
