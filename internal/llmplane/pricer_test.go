package llmplane

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/pricing"
)

// Config.Pricing takes any pricing.Pricer: a card built from caller rates
// (no prices.json) prices the captured call, and a model it does not know is
// stored with no cost rather than falling back to the embedded card.
func TestRouter_ConfigPricerPricesCall(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"usage":{"input_tokens":1000,"output_tokens":100}}`)
	}))
	defer up.Close()
	card := pricing.NewCardFromRates(map[string]pricing.Rate{"house-model": {Input: 7, Output: 70}})
	rec := &lastRec{}
	gw := gateway(t, Config{UpstreamBaseURL: up.URL, Pricing: card}, rec)

	do(t, "POST", gw.URL+"/anthropic/v1/messages", nil, `{"model":"house-model"}`)
	if c := rec.wait(t); c.CostUSD == nil || !near(*c.CostUSD, (1000*7.0+100*70.0)/1e6) {
		t.Errorf("house-model cost = %v, want the card's rate", c.CostUSD)
	}

	rec.last = nil
	do(t, "POST", gw.URL+"/anthropic/v1/messages", nil, `{"model":"claude-haiku-4-5"}`)
	if c := rec.wait(t); c.CostUSD != nil {
		t.Errorf("model missing from the card cost %v, want nil (no fallback to the embedded card)", *c.CostUSD)
	}
}
