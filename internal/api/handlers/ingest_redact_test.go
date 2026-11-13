package handlers

import (
	"strings"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/config"
)

// Ingested records are scrubbed like proxied ones, before the cap, and a
// body the sender already cut short has its tail checked too.
func TestIngest_RedactsSecretsBeforeCap(t *testing.T) {
	const key = "AKIAIOSFODNN7EXAMPLE"
	sinkFake := &ingestFakeSink{}
	d := newIngestDeps(sinkFake, true)
	d.LLMCapture = config.LLMCapture{StoreBodies: true, RedactSecrets: true, MaxRequestBytes: 1 << 20, MaxResponseBytes: 1 << 20}
	d.Capture = config.Capture{StoreBodies: true, RedactSecrets: true, MaxRequestBytes: 12, MaxResponseBytes: 1 << 20}

	body := `{"schema_version":1,
		"llm_calls":[{"timestamp":"2026-01-15T12:00:00Z","request_id":"r1","status_code":200,
			"request_body":"{\"k\":\"` + key + `\"}","messages":"[\"password=hunter2x\"]","response_body":"ok"}],
		"access_logs":[{"timestamp":"2026-01-15T12:00:00Z","request_id":"r2","status_code":200,
			"request_body":"key ` + key + ` end","response_body":"tail Bearer abcd","truncated":true}]}`
	doIngest(t, d, ingestRequestWithPrincipal(body, "tenant-a", false))

	if len(sinkFake.gotLLM) != 1 || len(sinkFake.gotAccess) != 1 {
		t.Fatalf("got %d llm / %d access records", len(sinkFake.gotLLM), len(sinkFake.gotAccess))
	}
	llm := sinkFake.gotLLM[0]
	if string(llm.RequestBody) != `{"k":"[REDACTED:aws_key_id]"}` || strings.Contains(string(llm.Messages), "hunter2x") || llm.Truncated {
		t.Errorf("llm record: request %s messages %s truncated %v", llm.RequestBody, llm.Messages, llm.Truncated)
	}
	a := sinkFake.gotAccess[0]
	// Capped at 12 bytes after scrubbing: no part of the key survives.
	if string(a.RequestBody) != "key [REDACTE" || !a.Truncated {
		t.Errorf("access request %q truncated %v", a.RequestBody, a.Truncated)
	}
	if string(a.ResponseBody) != "tail Bearer [REDACTED:bearer]" {
		t.Errorf("access response %q", a.ResponseBody)
	}
}
