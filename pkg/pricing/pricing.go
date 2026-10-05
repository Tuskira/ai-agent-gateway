// Package pricing computes the USD cost of a captured LLM call from a
// per-model rate card and the call's token counts. It is deliberately
// provider-aware because cache-token accounting differs between providers:
// Anthropic/Bedrock report cache tokens SEPARATELY from input, while
// OpenAI/Gemini fold cached tokens INTO the input count. Getting that wrong
// double-bills cache reads — the classic bug in gateways that translate every
// provider to one shape. Because this plane does native passthrough, it keys on
// the ACTUAL upstream model and applies the right formula per provider.
//
// The rate card is an instance (Card), not a package-level global: an
// operator can start from the embedded prices.json and merge an override
// file over it (see Card.MergeFile), and the LLM plane is handed the
// resulting instance via its Config. Default is a package-level Card built
// from the embedded JSON alone, kept for callers that don't need an
// override, and Cost is a convenience wrapping Default.Cost.
//
// Prices are estimates: the embedded rate card is a static, approximate
// list-price table (see prices.json's "_note"), and even an
// operator-supplied override file is still a manually maintained price
// list, not a provider-reported invoice amount. An unknown model yields
// (nil, false) — the caller stores NULL, never a guessed price.
package pricing

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"sync"
)

//go:embed prices.json
var pricesJSON []byte

// rate is a model's price card, in USD per MILLION tokens. Above200k, when set,
// is the long-context override applied to the WHOLE request once the prompt
// exceeds 200K tokens (Claude Sonnet 4.5, Gemini 2.5/3.x Pro bill every token at
// the higher tier past that threshold). RegionalPremium marks models whose
// Bedrock geo inference profiles bill above the global rate (Claude 4.5+; see
// bedrockPremium). Priority/Flex replace the rate for a call served on that
// provider service tier (OpenAI service_tier, Gemini serviceTier).
// FastMultiplier and USGeoMultiplier scale every token cost of an Anthropic
// call reported as speed "fast" / inference_geo "us".
type rate struct {
	Input           float64 `json:"input"`
	Output          float64 `json:"output"`
	CacheRead       float64 `json:"cache_read"`
	CacheWrite      float64 `json:"cache_write"`
	Above200k       *rate   `json:"above_200k,omitempty"`
	RegionalPremium bool    `json:"regional_premium,omitempty"`
	Priority        *rate   `json:"priority,omitempty"`
	Flex            *rate   `json:"flex,omitempty"`
	FastMultiplier  float64 `json:"fast_multiplier,omitempty"`
	USGeoMultiplier float64 `json:"us_geo_multiplier,omitempty"`
}

// Usage is the token accounting for one call that Cost prices. Fields mirror the
// provider-normalized counts captured by the LLM plane. CacheCreation1h is the
// subset of CacheCreation written with a 1-hour TTL (billed at 2x base input vs
// 1.25x for the 5-minute default); WebSearchRequests is a per-request server-tool
// count billed as a flat fee, not per token.
//
// The remaining fields are the call's pricing context: Region is the Bedrock
// upstream region (GovCloud and per-region rates); Tier the provider service
// tier the call was served on ("priority", "flex"; "" = standard); Fast and
// GeoUS Anthropic's speed "fast" and inference_geo "us".
type Usage struct {
	Input             int64
	Output            int64
	CacheRead         int64
	CacheCreation     int64
	CacheCreation1h   int64
	WebSearchRequests int64
	Region            string
	Tier              string
	Fast, GeoUS       bool
}

// webSearchUSD is the Anthropic server-tool web-search fee: $10 per 1,000
// requests = $0.01 per request, billed on top of tokens.
const webSearchUSD = 0.01

// cardFile is prices.json's on-disk shape. Both the embedded card and any
// operator-supplied override file (llm_proxy.pricing_file) parse into this.
type cardFile struct {
	Note   string          `json:"_note"`
	Models map[string]rate `json:"models"`
}

// Card is a rate card: a set of per-model USD/Mtok rates plus the cost
// formula in Cost. Build one from the embedded prices.json (Default, or
// NewCard) and optionally layer an operator override file over it with
// MergeFile. A Card is safe for concurrent use.
type Card struct {
	table    map[string]rate
	missOnce sync.Map // dedupes the "no rate-card entry" warning per provider/model
}

var (
	reDate    = regexp.MustCompile(`-\d{8}$`)                             // trailing -YYYYMMDD (Anthropic snapshot alias)
	reDateISO = regexp.MustCompile(`-\d{4}-\d{2}-\d{2}$`)                 // trailing -YYYY-MM-DD (OpenAI snapshot alias)
	reBrDate  = regexp.MustCompile(`-\d{8}-v\d+:\d+$`)                    // Bedrock snapshot suffix -YYYYMMDD-vN:M
	reRegion  = regexp.MustCompile(`^(us|eu|apac|au|jp|global|us-gov)\.`) // Bedrock inference-profile region prefix
)

// Default is the package-level rate card built from the embedded
// prices.json. Kept for backward compatibility with callers that don't need
// an operator-overridden card (see Cost).
var Default *Card

func init() {
	c, err := newCardFromJSON(pricesJSON)
	if err != nil {
		panic("pricing: invalid embedded prices.json: " + err.Error())
	}
	Default = c
}

// NewCard builds a fresh Card from the embedded prices.json — the same
// rate card Default holds, but as an independent instance a caller can then
// merge an override file over without touching the package-level Default.
func NewCard() *Card {
	c, err := newCardFromJSON(pricesJSON)
	if err != nil {
		// Unreachable: the embedded file is validated once in init() above,
		// and pricesJSON is compiled-in, immutable content.
		panic("pricing: invalid embedded prices.json: " + err.Error())
	}
	return c
}

func newCardFromJSON(data []byte) (*Card, error) {
	var f cardFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, err
	}
	return &Card{table: f.Models}, nil
}

// Cost computes provider/model's cost using the package-level Default rate
// card. See Card.Cost for the formula and return contract.
func Cost(provider, model string, u Usage) (*float64, bool) {
	return Default.Cost(provider, model, u)
}

// MergeFile reads a JSON file in prices.json's shape and returns a NEW Card
// combining c's models with the file's: a model present in the file
// REPLACES c's entry for it; a model absent from the file keeps c's value.
// It also returns how many models the file itself defined. c is untouched.
//
// A malformed file — unreadable, invalid JSON, or well-formed JSON with no
// models — is an error. The caller (cmd/gateway) treats that as fatal at
// startup: an operator override that can't be parsed must never be silently
// ignored in favor of the embedded card.
func (c *Card) MergeFile(path string) (*Card, int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, fmt.Errorf("read pricing file %q: %w", path, err)
	}
	var f cardFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, 0, fmt.Errorf("parse pricing file %q: %w", path, err)
	}
	if len(f.Models) == 0 {
		return nil, 0, fmt.Errorf("pricing file %q: no models defined", path)
	}

	merged := make(map[string]rate, len(c.table)+len(f.Models))
	for k, v := range c.table {
		merged[k] = v
	}
	for k, v := range f.Models {
		merged[strings.ToLower(strings.TrimSpace(k))] = v
	}
	return &Card{table: merged}, len(f.Models), nil
}

// normalize maps a captured provider+model onto a rate-card key: lowercased,
// and for Bedrock the inference-profile region prefix (us./eu./…) is stripped.
func normalize(provider, model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	if provider == "bedrock" {
		m = reRegion.ReplaceAllString(m, "")
	}
	return m
}

// lookup finds model's rate. For Bedrock a region-specific row wins: the exact
// inference-profile id (eu.amazon.nova-lite-v1:0), then "<region>/<id>"
// (us-gov-west-1/amazon.nova-lite-v1:0), then the id with the profile prefix
// stripped. regional reports whether a region-specific row matched (its price
// already includes any regional difference).
func (c *Card) lookup(provider, model, region string) (r rate, regional, ok bool) {
	key := normalize(provider, model)
	if provider == "bedrock" {
		for _, k := range []string{strings.ToLower(strings.TrimSpace(model)), region + "/" + key} {
			if r, ok := c.table[k]; ok && k != key {
				return r, true, true
			}
		}
	}
	if r, ok := c.table[key]; ok {
		return r, false, true
	}
	// Dated-snapshot fallbacks -> undated alias, so a new snapshot of an
	// already-priced model is never silently NULL:
	//   Anthropic direct: claude-haiku-4-5-20251001   -> claude-haiku-4-5
	//   OpenAI:           gpt-4o-2024-08-06           -> gpt-4o
	//   Bedrock:          anthropic.claude-sonnet-5-20260514-v1:0 -> claude-sonnet-5
	for _, base := range []string{
		reDate.ReplaceAllString(key, ""),
		reDateISO.ReplaceAllString(key, ""),
		strings.TrimPrefix(reBrDate.ReplaceAllString(key, ""), "anthropic."),
	} {
		if base != key {
			if r, ok := c.table[base]; ok {
				return r, false, true
			}
		}
	}
	return rate{}, false, false
}

// Cost returns the USD cost of one call, or (nil, false) if the model is not
// in the rate card. Cache accounting is provider-correct: Anthropic/Bedrock
// report cache tokens separately from input; OpenAI/Gemini fold cached
// tokens into it.
func (c *Card) Cost(provider, model string, u Usage) (*float64, bool) {
	r, regional, ok := c.lookup(provider, model, u.Region)
	if !ok {
		c.logMiss(provider, model)
		return nil, false
	}

	// Multipliers come from the model's base row; a tier or long-context row
	// replaces the rates but not these flags.
	mult := 1.0
	if provider == "bedrock" && r.RegionalPremium && !regional {
		mult = bedrockPremium(model, u.Region)
	}
	if u.Fast && r.FastMultiplier > 0 {
		mult *= r.FastMultiplier
	}
	if u.GeoUS && r.USGeoMultiplier > 0 {
		mult *= r.USGeoMultiplier
	}
	switch {
	case u.Tier == "priority" && r.Priority != nil:
		r = *r.Priority
	case u.Tier == "flex" && r.Flex != nil:
		r = *r.Flex
	}

	// Long-context tier: some models (Claude Sonnet 4.5, Gemini Pro) bill the
	// WHOLE request at a higher rate once the prompt passes 200K tokens. The
	// threshold is the true prompt size: anthropic/bedrock report input WITHOUT
	// cache (add it back); openai/gemini fold cache INTO input already.
	promptSize := u.Input
	if provider == "anthropic" || provider == "bedrock" {
		promptSize = u.Input + u.CacheRead + u.CacheCreation
	}
	if r.Above200k != nil && promptSize > 200_000 {
		r = *r.Above200k
	}

	cost := tokenCost(provider, r, mult, u)
	return &cost, true
}

// tokenCost applies the per-token formula for rate r scaled by mult, with
// provider's cache accounting, plus flat server-tool fees. Shared by
// Card.Cost and CostOverride so both price a call identically.
func tokenCost(provider string, r rate, mult float64, u Usage) float64 {
	baseIn := u.Input
	switch provider {
	case "openai", "gemini":
		// cached tokens are a subset of input; don't double-count them.
		if baseIn = u.Input - u.CacheRead; baseIn < 0 {
			baseIn = 0
		}
	}
	crRate, cwRate := r.CacheRead, r.CacheWrite
	if crRate == 0 { // unset → no cache discount (only reached if a non-caching model reports cache tokens)
		crRate = r.Input
	}
	if cwRate == 0 {
		cwRate = r.Input
	}

	// Cache-creation TTL split: 1-hour writes bill at 2x base input, the 5-minute
	// default at cwRate (1.25x). CacheCreation1h is a subset of CacheCreation.
	cache1h := u.CacheCreation1h
	if cache1h > u.CacheCreation {
		cache1h = u.CacheCreation
	}
	cache5m := u.CacheCreation - cache1h

	cost := mult * (float64(baseIn)*r.Input +
		float64(u.CacheRead)*crRate +
		float64(cache5m)*cwRate +
		float64(cache1h)*(2*r.Input) +
		float64(u.Output)*r.Output) / 1_000_000
	// Per-request server-tool fees are flat USD, not per token.
	cost += float64(u.WebSearchRequests) * webSearchUSD
	return cost
}

// Override is a caller-supplied flat rate for one model, in USD per million
// tokens -- what a model-registry row's price field carries. It replaces
// the rate card entirely for that call: no snapshot fallback, no tier or
// long-context rows, no regional multiplier.
type Override struct {
	Input, Output, CacheRead, CacheWrite float64
}

// CostOverride prices u at o with provider's cache accounting (the same
// rules as Card.Cost: Anthropic/Bedrock report cache tokens separately
// from input, OpenAI/Gemini fold them into it). A zero CacheRead/CacheWrite
// falls back to the input rate, as on the card. Always returns a value:
// the caller configured the price, so there is no "unknown" case.
func CostOverride(provider string, o Override, u Usage) *float64 {
	r := rate{Input: o.Input, Output: o.Output, CacheRead: o.CacheRead, CacheWrite: o.CacheWrite}
	cost := tokenCost(provider, r, 1, u)
	return &cost
}

// bedrockPremium is the Bedrock price multiplier for a RegionalPremium model:
// GovCloud (a us-gov. profile, or any call served from a us-gov-* region) bills
// 20% over the global rate, geographic profiles (us./eu./apac./au./jp.) 10%;
// global. and a bare in-region id in a commercial region bill the base rate.
func bedrockPremium(model, region string) float64 {
	m := reRegion.FindStringSubmatch(strings.ToLower(strings.TrimSpace(model)))
	switch {
	case strings.HasPrefix(region, "us-gov-") || m != nil && m[1] == "us-gov":
		return 1.2
	case m == nil || m[1] == "global":
		return 1
	default:
		return 1.1
	}
}

// logMiss warns once per unknown provider/model so a missing rate is visible in
// logs without spamming a line per call.
func (c *Card) logMiss(provider, model string) {
	key := provider + "/" + model
	if _, seen := c.missOnce.LoadOrStore(key, struct{}{}); !seen {
		slog.Warn("pricing: no rate-card entry; cost stored as NULL", "provider", provider, "model", model)
	}
}

// UnpricedPathSuffixes and UnpricedPathSegment name the free utility
// endpoints: token counting on each provider and Anthropic's Message Batches
// management/results API (batch work is billed by the batch, not by these
// calls; results JSONL would otherwise price its last line as a full-rate
// call). The LLM plane stores no cost for them and the ClickHouse usage view
// (pkg/sink/clickhouse llm_usage_canonical) leaves them out of token usage;
// both read this one list.
var UnpricedPathSuffixes = []string{
	"/count_tokens",     // Anthropic
	"/count-tokens",     // Bedrock CountTokens
	":countTokens",      // Gemini
	"/input_tokens",     // OpenAI Responses input-token count
	"/messages/batches", // Anthropic Message Batches: the collection
}

// UnpricedPathSegment matches anything under one batch. It is a whole path
// segment: a bare "/messages/batches" substring test also swallowed anything
// merely PREFIXED by it (/v1/messages/batches-export), silently zeroing a
// billable call.
const UnpricedPathSegment = "/messages/batches/"

// UnpricedPath reports whether path is a free utility endpoint.
func UnpricedPath(path string) bool {
	for _, s := range UnpricedPathSuffixes {
		if strings.HasSuffix(path, s) {
			return true
		}
	}
	return strings.Contains(path, UnpricedPathSegment)
}
