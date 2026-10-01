package llmplane

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestWriteRateLimited_DialectShapedWithRetryAfter(t *testing.T) {
	tests := []struct {
		path    string
		topKeys []string // keys the dialect's envelope must carry
		check   func(t *testing.T, body map[string]any)
	}{
		{"/v1/messages", []string{"type", "error"}, func(t *testing.T, b map[string]any) {
			if b["type"] != "error" {
				t.Errorf("anthropic envelope = %v", b)
			}
		}},
		{"/anthropic/v1/messages", []string{"type", "error"}, nil},
		{"/openai/v1/chat/completions", []string{"error"}, func(t *testing.T, b map[string]any) {
			if e, _ := b["error"].(map[string]any); e == nil || e["message"] == nil {
				t.Errorf("openai envelope = %v", b)
			}
		}},
		{"/gemini/v1beta/models/x:generateContent", []string{"error"}, func(t *testing.T, b map[string]any) {
			e, _ := b["error"].(map[string]any)
			if e == nil || e["status"] != "RESOURCE_EXHAUSTED" || e["code"] != float64(429) {
				t.Errorf("gemini envelope = %v", b)
			}
		}},
		{"/model/anthropic.claude/invoke", []string{"message"}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			WriteRateLimited(rec, httptest.NewRequest(http.MethodPost, tc.path, nil), 90*time.Second)
			if rec.Code != http.StatusTooManyRequests {
				t.Fatalf("status = %d, want 429", rec.Code)
			}
			if got := rec.Header().Get("Retry-After"); got != "90" {
				t.Errorf("Retry-After = %q, want 90", got)
			}
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body %q: %v", rec.Body.String(), err)
			}
			for _, k := range tc.topKeys {
				if _, ok := body[k]; !ok {
					t.Errorf("body %v lacks key %q", body, k)
				}
			}
			if tc.check != nil {
				tc.check(t, body)
			}
		})
	}
}
