package llmplane

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// The AWS key id is the documented example value; the JWT is assembled at
// run time so the source holds no token literal.
const redactAWSKey = "AKIAIOSFODNN7EXAMPLE"

func redactJWT() string {
	e := base64.RawURLEncoding.EncodeToString
	return e([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + e([]byte(`{"sub":"tester"}`)) + "." + e([]byte("signature-bytes-here"))
}

// upstreamEcho records the request body it got and answers with reply.
func upstreamEcho(t *testing.T, contentType, reply string) (*httptest.Server, func() string) {
	t.Helper()
	var mu sync.Mutex
	var got string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = string(b)
		mu.Unlock()
		w.Header().Set("Content-Type", contentType)
		for _, ev := range strings.SplitAfter(reply, "\n\n") {
			_, _ = io.WriteString(w, ev)
			w.(http.Flusher).Flush()
		}
	}))
	t.Cleanup(up.Close)
	return up, func() string { mu.Lock(); defer mu.Unlock(); return got }
}

// A non-streamed call: the stored request, messages, system and response
// are scrubbed, while the upstream got the request and the client got the
// response byte for byte.
func TestRouter_StoredBodiesAreRedacted(t *testing.T) {
	jwt := redactJWT()
	reply := `{"id":"msg_r","type":"message","content":[{"type":"text","text":"your token is ` + jwt + `"}],"usage":{"input_tokens":3,"output_tokens":4}}`
	up, upGot := upstreamEcho(t, "application/json", reply)
	reqBody := `{"model":"claude-test-1","system":"db password=hunter2x","messages":[{"role":"user","content":"my key is ` + redactAWSKey + `"}]}`

	rec := &lastRec{}
	gw := gateway(t, Config{UpstreamBaseURL: up.URL, StoreBodies: true, RedactSecrets: true,
		MaxCaptureRequestBytes: 1 << 20, MaxCaptureResponseBytes: 1 << 20}, rec)
	resp, got := do(t, "POST", gw.URL+"/anthropic/v1/messages", nil, reqBody)
	if resp.StatusCode != http.StatusOK || got != reply {
		t.Fatalf("client got %d %q, want the upstream reply as is", resp.StatusCode, got)
	}
	if upGot() != reqBody {
		t.Errorf("upstream got %q, want the request as sent", upGot())
	}
	call := rec.wait(t)
	for name, v := range map[string][]byte{"request_body": call.RequestBody, "messages": call.Messages, "system": call.System, "response_body": call.ResponseBody} {
		for _, secret := range []string{redactAWSKey, jwt, "hunter2x"} {
			if strings.Contains(string(v), secret) {
				t.Errorf("%s stored a secret: %s", name, v)
			}
		}
	}
	if !strings.Contains(string(call.Messages), "[REDACTED:aws_key_id]") ||
		!strings.Contains(string(call.System), "[REDACTED:password]") ||
		!strings.Contains(string(call.ResponseBody), "[REDACTED:jwt]") {
		t.Errorf("markers missing: messages %s system %s response %s", call.Messages, call.System, call.ResponseBody)
	}
	if !json.Valid(call.RequestBody) || !json.Valid(call.ResponseBody) {
		t.Errorf("stored bodies are not valid JSON: %s / %s", call.RequestBody, call.ResponseBody)
	}
	if call.Truncated {
		t.Error("truncated set on a body that fit")
	}
}

// A streamed call is scrubbed after assembly: a key split across two text
// deltas is redacted in the stored message, and the client still gets the
// raw stream.
func TestRouter_StreamSecretSplitAcrossEventsIsRedacted(t *testing.T) {
	events := []string{
		`{"type":"message_start","message":{"id":"msg_s","type":"message","role":"assistant","model":"claude-test-1","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":2,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"key ` + redactAWSKey[:9] + `"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"` + redactAWSKey[9:] + ` done"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":5}}`,
		`{"type":"message_stop"}`,
	}
	stream := sseStream(events)
	up, _ := upstreamEcho(t, "text/event-stream", stream)
	rec := &lastRec{}
	gw := gateway(t, Config{UpstreamBaseURL: up.URL, StoreBodies: true, RedactSecrets: true, AssembleStreams: true,
		MaxCaptureRequestBytes: 1 << 20, MaxCaptureResponseBytes: 1 << 20}, rec)
	_, got := do(t, "POST", gw.URL+"/anthropic/v1/messages", nil, `{"model":"claude-test-1","stream":true}`)
	if got != stream {
		t.Fatalf("client stream differs from the upstream's")
	}
	call := rec.wait(t)
	if strings.Contains(string(call.ResponseBody), redactAWSKey) || !strings.Contains(string(call.ResponseBody), "key [REDACTED:aws_key_id] done") {
		t.Errorf("stored %s", call.ResponseBody)
	}
}

// A raw stream capture cut by the cap in the middle of a secret stores
// neither half of it, and stays flagged truncated.
func TestRouter_CutStreamCaptureTailIsRedacted(t *testing.T) {
	stream := "data: {\"text\":\"Bearer abcdefghijklmnopqrstuvwxyz0123456789\"}\n\n"
	up, _ := upstreamEcho(t, "text/event-stream", stream)
	rec := &lastRec{}
	cut := strings.Index(stream, "Bearer ") + len("Bearer abcdefgh")
	gw := gateway(t, Config{UpstreamBaseURL: up.URL, StoreBodies: true, RedactSecrets: true, AssembleStreams: false,
		MaxCaptureRequestBytes: 1 << 20, MaxCaptureResponseBytes: cut}, rec)
	_, got := do(t, "POST", gw.URL+"/anthropic/v1/messages", nil, `{"model":"claude-test-1","stream":true}`)
	if got != stream {
		t.Fatalf("client stream differs from the upstream's")
	}
	call := rec.wait(t)
	if strings.Contains(string(call.ResponseBody), "abcdefgh") || !call.Truncated {
		t.Errorf("stored (truncated=%v) %q", call.Truncated, call.ResponseBody)
	}
	if len(call.ResponseBody) > cut {
		t.Errorf("stored %d bytes, over the %d cap", len(call.ResponseBody), cut)
	}
}

// RedactSecrets off stores bodies as they were.
func TestRouter_RedactionOff(t *testing.T) {
	up, _ := upstreamEcho(t, "application/json", `{"id":"m","type":"message","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
	rec := &lastRec{}
	gw := gateway(t, Config{UpstreamBaseURL: up.URL, StoreBodies: true, RedactSecrets: false,
		MaxCaptureRequestBytes: 1 << 20, MaxCaptureResponseBytes: 1 << 20}, rec)
	body := `{"model":"claude-test-1","messages":[{"role":"user","content":"` + redactAWSKey + `"}]}`
	do(t, "POST", gw.URL+"/anthropic/v1/messages", nil, body)
	if call := rec.wait(t); string(call.RequestBody) != body {
		t.Errorf("stored %s", call.RequestBody)
	}
}
