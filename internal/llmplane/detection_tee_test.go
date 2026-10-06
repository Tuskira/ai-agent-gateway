package llmplane

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// agentStub is a detection agent on loopback: it records every turn (decoded,
// and raw so an absent field can be told from an empty one) and answers 202
// after hold returns.
type agentStub struct {
	mu    sync.Mutex
	turns []teeTurn
	raw   []map[string]any
	// hold runs before the answer; nil answers at once.
	hold     func(r *http.Request)
	inFlight atomic.Int32
	maxSeen  atomic.Int32
	url      string
}

func newAgentStub(t *testing.T, hold func(r *http.Request)) *agentStub {
	t.Helper()
	a := &agentStub{hold: hold}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := a.inFlight.Add(1)
		defer a.inFlight.Add(-1)
		for m := a.maxSeen.Load(); n > m && !a.maxSeen.CompareAndSwap(m, n); m = a.maxSeen.Load() {
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/turns" || r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
			return
		}
		b, _ := io.ReadAll(r.Body)
		var turn teeTurn
		var raw map[string]any
		if json.Unmarshal(b, &turn) != nil || json.Unmarshal(b, &raw) != nil {
			http.Error(w, "bad turn", http.StatusBadRequest)
			return
		}
		a.mu.Lock()
		a.turns = append(a.turns, turn)
		a.raw = append(a.raw, raw)
		a.mu.Unlock()
		if a.hold != nil {
			a.hold(r)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(srv.Close)
	a.url = srv.URL
	return a
}

func (a *agentStub) got() ([]teeTurn, []map[string]any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]teeTurn(nil), a.turns...), append([]map[string]any(nil), a.raw...)
}

// wait polls up to 2s for n turns.
func (a *agentStub) wait(t *testing.T, n int) ([]teeTurn, []map[string]any) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if turns, raw := a.got(); len(turns) >= n {
			return turns, raw
		}
		time.Sleep(10 * time.Millisecond)
	}
	turns, _ := a.got()
	t.Fatalf("agent got %d turns, want %d", len(turns), n)
	return nil, nil
}

// newTee is a tee closed (drained, 1s bound) when the test ends.
func newTee(t *testing.T, cfg DetectionTeeConfig) *DetectionTee {
	t.Helper()
	d := NewDetectionTee(cfg)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		d.Close(ctx)
	})
	return d
}

// teeUpstream answers every call with status and body.
func teeUpstream(t *testing.T, status int, body string) string {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(up.Close)
	return up.URL
}

const (
	teeReq  = `{"model":"claude-sonnet-5","max_tokens":9,"messages":[{"role":"user","content":"hi"}]}`
	teeResp = `{"type":"message","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`
)

// A 2xx call posts one complete turn with both bodies, under the call's
// request id, even with body storage off.
func TestDetectionTee_TurnPerCall(t *testing.T) {
	agent := newAgentStub(t, nil)
	rec := &lastRec{}
	gw := gateway(t, Config{UpstreamBaseURL: teeUpstream(t, http.StatusOK, teeResp), StoreBodies: false,
		DetectionTee: newTee(t, DetectionTeeConfig{AgentURL: agent.url})}, rec)
	resp, body := do(t, http.MethodPost, gw.URL+"/v1/messages", map[string]string{"x-api-key": "sk"}, teeReq)
	if resp.StatusCode != http.StatusOK || body != teeResp {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	turns, _ := agent.wait(t, 1)
	c := rec.wait(t)
	if c.RequestBody != nil || c.ResponseBody != nil {
		t.Fatalf("store_bodies is off but the record carries bodies")
	}
	got := turns[0]
	if got.V != 1 || got.ID == "" || got.ID != c.RequestID || got.TenantID != "t" || got.Path != "/v1/messages" ||
		got.Model != "claude-sonnet-5" || got.StatusCode != http.StatusOK || got.At.IsZero() ||
		string(got.Request) != teeReq || string(got.Response) != teeResp {
		t.Errorf("turn = %+v", got)
	}
}

// A non-2xx upstream answer posts the turn with no response field.
func TestDetectionTee_NonOKHasNoResponse(t *testing.T) {
	agent := newAgentStub(t, nil)
	gw := gateway(t, Config{UpstreamBaseURL: teeUpstream(t, http.StatusTooManyRequests, `{"type":"error"}`),
		DetectionTee: newTee(t, DetectionTeeConfig{AgentURL: agent.url})}, &lastRec{})
	if resp, _ := do(t, http.MethodPost, gw.URL+"/v1/messages", map[string]string{"x-api-key": "sk"}, teeReq); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status %d", resp.StatusCode)
	}
	turns, raw := agent.wait(t, 1)
	if _, ok := raw[0]["response"]; ok || turns[0].StatusCode != http.StatusTooManyRequests || string(turns[0].Request) != teeReq {
		t.Errorf("turn = %v; want status 429, the request, and no response field", raw[0])
	}
}

// A relay cut short -- the client leaving mid-stream, or the upstream
// failing mid-body -- posts the turn without a response: the agent gets the
// request (it reached the model) but never a partial response.
func TestDetectionTee_IncompleteRelayHasNoResponse(t *testing.T) {
	t.Run("client disconnect", func(t *testing.T) {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			for i := 0; i < 200; i++ {
				if _, err := w.Write([]byte("event: ping\ndata: {}\n\n")); err != nil {
					return
				}
				w.(http.Flusher).Flush()
				select {
				case <-r.Context().Done():
					return
				case <-time.After(10 * time.Millisecond):
				}
			}
		}))
		defer up.Close()
		agent := newAgentStub(t, nil)
		rec := &lastRec{}
		gw := gateway(t, Config{UpstreamBaseURL: up.URL, DetectionTee: newTee(t, DetectionTeeConfig{AgentURL: agent.url})}, rec)
		req, _ := http.NewRequest(http.MethodPost, gw.URL+"/v1/messages", strings.NewReader(teeReq))
		req.Header.Set("x-api-key", "sk")
		tr := &http.Transport{}
		resp, err := tr.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = resp.Body.Read(make([]byte, 16)) // the stream started
		_ = resp.Body.Close()
		tr.CloseIdleConnections()
		turns, raw := agent.wait(t, 1)
		if _, ok := raw[0]["response"]; ok || turns[0].StatusCode != http.StatusOK || string(turns[0].Request) != teeReq {
			t.Errorf("turn = %v; want the request and no response field", raw[0])
		}
		if c := rec.wait(t); c.Error != "client_closed" {
			t.Errorf("recorded error = %q, want client_closed", c.Error)
		}
	})
	t.Run("upstream cut", func(t *testing.T) {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "100")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("short")) // fewer bytes than promised: the connection is cut
		}))
		defer up.Close()
		agent := newAgentStub(t, nil)
		gw := gateway(t, Config{UpstreamBaseURL: up.URL, DetectionTee: newTee(t, DetectionTeeConfig{AgentURL: agent.url})}, &lastRec{})
		req, _ := http.NewRequest(http.MethodPost, gw.URL+"/v1/messages", strings.NewReader(teeReq))
		req.Header.Set("x-api-key", "sk")
		if resp, err := http.DefaultTransport.RoundTrip(req); err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		_, raw := agent.wait(t, 1)
		if _, ok := raw[0]["response"]; ok {
			t.Errorf("turn = %v; want no response field", raw[0])
		}
	})
}

// A response over the cap is sent as its first teeResponseBytes.
func TestDetectionTee_ResponseBounded(t *testing.T) {
	big := `{"text":"` + strings.Repeat("a", 1<<20) + `"}`
	agent := newAgentStub(t, nil)
	gw := gateway(t, Config{UpstreamBaseURL: teeUpstream(t, http.StatusOK, big),
		DetectionTee: newTee(t, DetectionTeeConfig{AgentURL: agent.url})}, &lastRec{})
	if _, body := do(t, http.MethodPost, gw.URL+"/v1/messages", map[string]string{"x-api-key": "sk"}, teeReq); body != big {
		t.Fatalf("client got %d bytes, want %d", len(body), len(big))
	}
	turns, _ := agent.wait(t, 1)
	if !bytes.Equal(turns[0].Response, []byte(big[:teeResponseBytes])) {
		t.Errorf("response = %d bytes; want the first %d", len(turns[0].Response), teeResponseBytes)
	}
}

// A slow agent never delays the call: the handler answers long before the
// agent does.
func TestDetectionTee_SlowAgentDoesNotDelay(t *testing.T) {
	agent := newAgentStub(t, func(r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	})
	gw := gateway(t, Config{UpstreamBaseURL: teeUpstream(t, http.StatusOK, teeResp),
		DetectionTee: newTee(t, DetectionTeeConfig{AgentURL: agent.url, Timeout: 3 * time.Second})}, &lastRec{})
	for i := 0; i < 3; i++ {
		begin := time.Now()
		resp, _ := do(t, http.MethodPost, gw.URL+"/v1/messages", map[string]string{"x-api-key": "sk"}, teeReq)
		if took := time.Since(begin); resp.StatusCode != http.StatusOK || took > 200*time.Millisecond {
			t.Errorf("call %d: status %d in %s; want 200 well under the agent's 2s", i, resp.StatusCode, took)
		}
	}
	agent.wait(t, 1) // the agent did get the turns, while still answering
}

// A down agent never delays or fails the call; the turn counts as failed.
func TestDetectionTee_AgentDown(t *testing.T) {
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close()
	tee := newTee(t, DetectionTeeConfig{AgentURL: down.URL})
	gw := gateway(t, Config{UpstreamBaseURL: teeUpstream(t, http.StatusOK, teeResp), DetectionTee: tee}, &lastRec{})
	if resp, body := do(t, http.MethodPost, gw.URL+"/v1/messages", map[string]string{"x-api-key": "sk"}, teeReq); resp.StatusCode != http.StatusOK || body != teeResp {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	for i := 0; i < 200 && tee.failed.Load() == 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if s := tee.Status(); s["failed"] != uint64(1) || s["sent"] != uint64(0) {
		t.Errorf("status = %v; want 1 failed, 0 sent", s)
	}
}

// A full queue drops the turn and counts it; the call is not delayed.
func TestDetectionTee_QueueFullDrops(t *testing.T) {
	release := make(chan struct{})
	agent := newAgentStub(t, func(*http.Request) { <-release })
	tee := newTee(t, DetectionTeeConfig{AgentURL: agent.url, QueueSize: 1, MaxInFlight: 1})
	t.Cleanup(func() { close(release) }) // runs before the tee's Close
	gw := gateway(t, Config{UpstreamBaseURL: teeUpstream(t, http.StatusOK, teeResp), DetectionTee: tee}, &lastRec{})
	call := func() {
		t.Helper()
		begin := time.Now()
		resp, _ := do(t, http.MethodPost, gw.URL+"/v1/messages", map[string]string{"x-api-key": "sk"}, teeReq)
		if took := time.Since(begin); resp.StatusCode != http.StatusOK || took > 200*time.Millisecond {
			t.Errorf("status %d in %s", resp.StatusCode, took)
		}
	}
	call()
	agent.wait(t, 1) // the one worker is busy with it
	call()           // waits in the queue
	call()           // queue full: dropped
	if s := tee.Status(); s["dropped"] != uint64(1) || s["queued"] != 1 {
		t.Errorf("status = %v; want 1 dropped, 1 queued", s)
	}
}

// queue_bytes bounds the bytes held: a turn that does not fit is dropped.
func TestDetectionTee_QueueBytesDrops(t *testing.T) {
	agent := newAgentStub(t, nil)
	tee := newTee(t, DetectionTeeConfig{AgentURL: agent.url, QueueBytes: 16})
	tee.offer(&teeTurn{ID: "big", Request: []byte(teeReq)})
	tee.offer(&teeTurn{ID: "small", Request: []byte("{}")})
	agent.wait(t, 1)
	turns, _ := agent.got()
	if s := tee.Status(); s["dropped"] != uint64(1) || len(turns) != 1 || turns[0].ID != "small" {
		t.Errorf("status = %v, turns = %d; want the 16-byte cap to drop only the big turn", s, len(turns))
	}
}

// No more than max_in_flight posts run at once.
func TestDetectionTee_MaxInFlight(t *testing.T) {
	release := make(chan struct{})
	agent := newAgentStub(t, func(*http.Request) { <-release })
	tee := newTee(t, DetectionTeeConfig{AgentURL: agent.url, MaxInFlight: 2})
	for i := 0; i < 6; i++ {
		tee.offer(&teeTurn{ID: "t", Request: []byte("{}")})
	}
	agent.wait(t, 2)
	time.Sleep(50 * time.Millisecond) // room for a third post, if one could start
	if n := agent.inFlight.Load(); n != 2 {
		t.Errorf("%d posts in flight, want 2", n)
	}
	close(release)
	agent.wait(t, 6)
	if m := agent.maxSeen.Load(); m != 2 {
		t.Errorf("max posts in flight = %d, want 2", m)
	}
}

// Close posts what is queued before it returns.
func TestDetectionTee_CloseDrains(t *testing.T) {
	agent := newAgentStub(t, func(*http.Request) { time.Sleep(20 * time.Millisecond) })
	tee := NewDetectionTee(DetectionTeeConfig{AgentURL: agent.url, MaxInFlight: 1})
	for i := 0; i < 5; i++ {
		tee.offer(&teeTurn{ID: "t", Request: []byte("{}")})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tee.Close(ctx)
	if turns, _ := agent.got(); len(turns) != 5 || tee.sent.Load() != 5 {
		t.Errorf("after Close: agent got %d turns, sent %d; want 5", len(turns), tee.sent.Load())
	}
	tee.offer(&teeTurn{ID: "late"}) // after Close: dropped, never a panic
	if s := tee.Status(); s["dropped"] != uint64(1) {
		t.Errorf("status = %v; want the late turn dropped", s)
	}
}

// Close is bounded: past its context it aborts the post in flight and drops
// the queue.
func TestDetectionTee_CloseBounded(t *testing.T) {
	agent := newAgentStub(t, func(r *http.Request) { <-r.Context().Done() })
	tee := NewDetectionTee(DetectionTeeConfig{AgentURL: agent.url, MaxInFlight: 1, Timeout: time.Minute})
	for i := 0; i < 3; i++ {
		tee.offer(&teeTurn{ID: "t", Request: []byte("{}")})
	}
	agent.wait(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	begin := time.Now()
	tee.Close(ctx)
	if took := time.Since(begin); took > time.Second {
		t.Errorf("Close took %s past a 100ms bound", took)
	}
	if s := tee.Status(); s["dropped"] != uint64(3) || s["sent"] != uint64(0) || s["failed"] != uint64(0) || s["held_bytes"] != int64(0) {
		t.Errorf("status = %v; want all 3 dropped, nothing held", s)
	}
}

// redirectTransport sends every upstream call to target, keeping the path.
type redirectTransport struct{ target *url.URL }

func (f redirectTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.URL.Scheme, r.URL.Host = f.target.Scheme, f.target.Host
	return (&http.Transport{}).RoundTrip(r)
}

// Every generation route of every provider goes through the tee; model
// listing and token counting do not.
func TestDetectionTee_Routes(t *testing.T) {
	up := teeUpstream(t, http.StatusOK, teeResp)
	u, _ := url.Parse(up)
	transportOverride = redirectTransport{target: u} // Bedrock signs for its AWS host
	defer func() { transportOverride = nil }()
	agent := newAgentStub(t, nil)
	gw := gateway(t, Config{
		UpstreamBaseURL: up, OpenAIEnabled: true, OpenAIBaseURL: up, GeminiEnabled: true, GeminiBaseURL: up,
		BedrockEnabled: true, BedrockRegion: "us-east-1", BedrockCredentialMode: "passthrough",
		DetectionTee: newTee(t, DetectionTeeConfig{AgentURL: agent.url, MaxInFlight: 1}), // one worker: posted in order
	}, &lastRec{})
	sigv4 := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20260101/us-east-1/bedrock/aws4_request, SignedHeaders=host, Signature=00"
	for _, c := range []struct {
		method, path, auth string
		sent               bool
	}{
		{http.MethodGet, "/openai/v1/models", "Bearer sk-x", false},
		{http.MethodPost, "/v1/messages/count_tokens", "", false},
		{http.MethodPost, "/v1/messages", "", true},
		{http.MethodPost, "/openai/v1/chat/completions", "Bearer sk-x", true},
		{http.MethodPost, "/model/x/invoke", sigv4, true},
		{http.MethodPost, "/gemini/v1beta/models/g:generateContent", "", true},
	} {
		hdr := map[string]string{"x-api-key": "sk", "Content-Type": "application/json"}
		if c.auth != "" {
			hdr["Authorization"] = c.auth
		}
		body := teeReq
		if c.method == http.MethodGet {
			body = ""
		}
		if resp, b := do(t, c.method, gw.URL+c.path, hdr, body); resp.StatusCode != http.StatusOK {
			t.Fatalf("%s %s: status %d %s", c.method, c.path, resp.StatusCode, b)
		}
	}
	want := []string{"/v1/messages", "/openai/v1/chat/completions", "/model/x/invoke", "/gemini/v1beta/models/g:generateContent"}
	turns, _ := agent.wait(t, len(want))
	var got []string
	for _, tt := range turns {
		got = append(got, tt.Path)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("turns for %v; want %v", got, want)
	}
}

// The wire contract is pinned by fixtures in testdata/detection, shared with
// the agent's own tests: a turn built from fixed inputs marshals to them.
func TestDetectionTee_ContractFixtures(t *testing.T) {
	turn := teeTurn{
		V: teeVersion, ID: "req_0123456789abcdef", TenantID: "tenant-1", SessionID: "session-1", KeyID: "key-1",
		Principal: "user-1", Model: "example-model", Path: "/v1/messages",
		At:         time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		StatusCode: http.StatusOK,
		Request:    []byte(`{"model":"example-model","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`),
		Response:   []byte(`{"type":"message","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":3,"output_tokens":1}}`),
	}
	sameJSON := func(name string, got teeTurn) {
		t.Helper()
		want, err := os.ReadFile(filepath.Join("testdata", "detection", name))
		if err != nil {
			t.Fatal(err)
		}
		gb, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		var g, w any
		if err := json.Unmarshal(gb, &g); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(want, &w); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !reflect.DeepEqual(g, w) {
			t.Errorf("%s: marshaled turn = %s\nwant %s", name, gb, want)
		}
	}
	sameJSON("turn.json", turn)
	turn.StatusCode, turn.Response = http.StatusTooManyRequests, nil
	sameJSON("turn_no_response.json", turn)
}
