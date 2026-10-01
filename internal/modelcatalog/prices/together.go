package prices

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

func init() { Register("together", togetherSource{}) }

const (
	togetherTimeout = 10 * time.Second
	togetherMaxBody = 5 << 20 // 5 MiB
)

// togetherPricing is the "pricing" object on each model in Together's own
// GET /v1/models response (docs.together.ai/reference/models), already in
// USD per million tokens -- no unit conversion needed. Fields besides
// Input/Output/CachedInput (base, finetune, hourly, request, ...) exist on
// the wire but aren't modeled here.
type togetherPricing struct {
	Input       float64 `json:"input"`
	Output      float64 `json:"output"`
	CachedInput float64 `json:"cached_input"`
}

type togetherModel struct {
	ID      string          `json:"id"`
	Pricing togetherPricing `json:"pricing"`
}

type togetherSource struct{}

func (togetherSource) RequiresAPIKey() bool { return true }

// Fetch calls GET {provider.BaseURL}/models with apiKey as a bearer token
// -- Together's docs don't distinguish a read-only "list models" scope
// from a full key, so any tenant's own key works for this (hence "tenant-
// independent" at the call site: the handler never persists it).
func (togetherSource) Fetch(ctx context.Context, provider *store.ModelCatalogProvider, apiKey string) (*Result, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("prices: together: an api key is required")
	}
	url := strings.TrimRight(provider.BaseURL, "/") + "/models"

	client := &http.Client{
		Timeout:       togetherTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	ctx, cancel := context.WithTimeout(ctx, togetherTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("prices: together: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("prices: together: request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("prices: together: provider responded with HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, togetherMaxBody))
	if err != nil {
		return nil, fmt.Errorf("prices: together: read response: %w", err)
	}
	var models []togetherModel
	if err := json.Unmarshal(body, &models); err != nil {
		return nil, fmt.Errorf("prices: together: unexpected response shape: %w", err)
	}

	out := make(map[string]store.ModelPrice)
	for _, m := range models {
		if m.ID == "" {
			continue
		}
		out[m.ID] = store.ModelPrice{
			Input:     m.Pricing.Input,
			Output:    m.Pricing.Output,
			CacheRead: m.Pricing.CachedInput,
		}
	}
	return &Result{SourceURL: url, Prices: out}, nil
}
