package openaicompat

import (
	"os"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm/llmtest"
)

func TestOpenAIReasoningModel(t *testing.T) {
	for m, want := range map[string]bool{
		"gpt-5": true, "gpt-5-mini": true, "GPT-6-sol": true, "o1": true, "o3-mini": true, "o4-mini": true, "openai/gpt-5": true,
		"gpt-4o": false, "gpt-4.1": false, "openai/gpt-oss-120b": false, "grok-4": false, "glm-4.6": false, "o": false, "": false,
	} {
		if got := openaiReasoningModel(m); got != want {
			t.Errorf("openaiReasoningModel(%q) = %v, want %v", m, got, want)
		}
	}
}

// Earlier reasoning goes back only to the vendors that want it.
func TestWantsReasoningBack(t *testing.T) {
	for _, c := range []struct {
		vendor, url, model string
		want               bool
	}{
		{"openai", "https://api.openai.com/v1", "gpt-4o", false},
		{"groq", "https://api.groq.com/openai/v1", "llama-3.3-70b", false},
		{"xai", "https://api.x.ai/v1", "grok-4", false},
		{"openai", "https://api.deepseek.com", "deepseek-chat", true},
		{"openai", "https://api.moonshot.ai/v1", "kimi-k2", true},
		{"openai", "https://open.bigmodel.cn/api/paas/v4", "glm-4.6", true},
		{"zhipu", "https://proxy.internal/v1", "m", true},
		{"openai", "https://notz.ai.example/v1", "m", false},
		// Kimi and GLM want their reasoning back whichever host serves them.
		{"nebius", "https://api.tokenfactory.eu-west2.nebius.com/v1/", "moonshotai/Kimi-K3", true},
		{"together", "https://api.together.ai/v1", "zai-org/GLM-5.3", true},
		{"other", "https://llm.internal/v1", "kimi-k3", true},
		// Only those two families: a DeepSeek model on a strict host stays off.
		{"groq", "https://api.groq.com/openai/v1", "deepseek-r1-distill-llama-70b", false},
		// Groq rejects the field outright, whatever model it serves.
		{"groq", "https://api.groq.com/openai/v1", "moonshotai/kimi-k2-instruct", false},
		{"other", "https://api.groq.com/openai/v1", "moonshotai/kimi-k2-instruct", false},
		{"openai", "https://api.openai.com/v1", "gpt-6-astra", false},
		{"nebius", "https://api.tokenfactory.eu-west2.nebius.com/v1/", "meta-llama/Llama-4-glmish", false},
	} {
		if got := WantsReasoningBack(c.vendor, c.url, c.model); got != c.want {
			t.Errorf("%s %s %s: echo = %v, want %v", c.vendor, c.url, c.model, got, c.want)
		}
	}
}

// A tool call's arguments are never replaced: an object passes, none is {},
// anything else is kept raw under InvalidArgumentsKey.
func TestToolInput(t *testing.T) {
	for in, want := range map[string]string{
		`{"a":1}`: `{"a":1}`,
		``:        `{}`,
		`  `:      `{}`,
		`{"a":`:   `{"_gateway_invalid_arguments":"{\"a\":"}`,
		`[1]`:     `{"_gateway_invalid_arguments":"[1]"}`,
		`null`:    `{"_gateway_invalid_arguments":"null"}`,
	} {
		if got := string(toolInput(in)); got != want {
			t.Errorf("toolInput(%q) = %s, want %s", in, got, want)
		}
	}
}

// Live: LLMTEST_OPENAI_COMPAT_BASE_URL (+ LLMTEST_OPENAI_COMPAT_MODEL, and
// LLMTEST_OPENAI_COMPAT_API_KEY for a keyed endpoint; LLMTEST_OPENAI_COMPAT_THINKING=1
// for a reasoning model) runs the provider suite against a real
// OpenAI-compatible endpoint, e.g. a local Ollama:
//
//	LLMTEST_OPENAI_COMPAT_BASE_URL=http://127.0.0.1:11434/v1 LLMTEST_OPENAI_COMPAT_MODEL=qwen2.5:1.5b
func TestProviderConformance(t *testing.T) {
	base := os.Getenv("LLMTEST_OPENAI_COMPAT_BASE_URL")
	if base == "" {
		t.Skip("LLMTEST_OPENAI_COMPAT_BASE_URL not set")
	}
	target := llm.Target{Vendor: envOr("LLMTEST_OPENAI_COMPAT_VENDOR", "openai_compat"), BaseURL: base,
		Model: envOr("LLMTEST_OPENAI_COMPAT_MODEL", "gpt-4o-mini"), Auth: llm.Auth{APIKey: envOr("LLMTEST_OPENAI_COMPAT_API_KEY", "llmtest")}}
	var opts []llmtest.Option
	if os.Getenv("LLMTEST_OPENAI_COMPAT_THINKING") == "1" {
		opts = append(opts, llmtest.WithThinking())
	}
	llmtest.RunProvider(t, Provider{}, target, opts...)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
