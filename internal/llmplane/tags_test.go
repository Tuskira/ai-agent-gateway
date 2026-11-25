package llmplane

import (
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	pkgsink "github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink"
)

// headerUpstream is an Anthropic-shaped upstream that records the headers of
// the last request it received.
func headerUpstream(t *testing.T) (*httptest.Server, func() http.Header) {
	t.Helper()
	var mu sync.Mutex
	var got http.Header
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = r.Header.Clone()
		mu.Unlock()
		_, _ = io.WriteString(w, `{"id":"m","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	t.Cleanup(up.Close)
	return up, func() http.Header {
		mu.Lock()
		defer mu.Unlock()
		return got
	}
}

func TestRequestTags_RecordedAndNotForwarded(t *testing.T) {
	up, upstreamHeaders := headerUpstream(t)
	rec := &lastRec{}
	gw := gateway(t, Config{UpstreamBaseURL: up.URL, StoreBodies: true, MaxCaptureRequestBytes: 1 << 20, MaxCaptureResponseBytes: 1 << 20}, rec)

	before := pkgsink.TagsDropped()
	do(t, "POST", gw.URL+"/anthropic/v1/messages", map[string]string{
		"X-Gateway-User": "  alice@example.com ",
		"X-Gateway-Tags": "team=search,feature=chat%20bot,Bad Key=x,novalue,job=",
		"X-Session-Id":   "s1",
	}, `{"model":"claude-sonnet-4-5"}`)
	c := rec.wait(t)

	if c.User != "alice@example.com" {
		t.Errorf("User = %q", c.User)
	}
	want := map[string]string{"team": "search", "feature": "chat bot"}
	if !reflect.DeepEqual(c.Tags, want) {
		t.Errorf("Tags = %v, want %v", c.Tags, want)
	}
	if d := pkgsink.TagsDropped() - before; d != 3 {
		t.Errorf("dropped counter moved by %d, want 3 (bad key, no value, empty value)", d)
	}
	if c.SessionID != "s1" {
		t.Errorf("SessionID = %q", c.SessionID)
	}

	uh := upstreamHeaders()
	for _, h := range []string{"X-Gateway-User", "X-Gateway-Tags", "X-Session-Id"} {
		if v := uh.Get(h); v != "" {
			t.Errorf("%s reached the upstream: %q", h, v)
		}
	}
	for k := range c.Headers {
		if strings.HasPrefix(strings.ToLower(k), "x-gateway-") {
			t.Errorf("captured headers include %s", k)
		}
	}
}

func TestRequestTags_AbsentHeadersLeaveRowUntouched(t *testing.T) {
	up, _ := headerUpstream(t)
	rec := &lastRec{}
	gw := gateway(t, Config{UpstreamBaseURL: up.URL}, rec)
	before := pkgsink.TagsDropped()
	do(t, "POST", gw.URL+"/anthropic/v1/messages", nil, `{"model":"claude-sonnet-4-5"}`)
	c := rec.wait(t)
	if c.User != "" || len(c.Tags) != 0 || pkgsink.TagsDropped() != before {
		t.Errorf("User=%q Tags=%v dropped+%d", c.User, c.Tags, pkgsink.TagsDropped()-before)
	}
}

func TestRequestTags_RepeatedHeaderLinesAndPairCap(t *testing.T) {
	up, _ := headerUpstream(t)
	rec := &lastRec{}
	gw := gateway(t, Config{UpstreamBaseURL: up.URL, MaxTagPairs: 2}, rec)
	req, _ := http.NewRequest("POST", gw.URL+"/anthropic/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-5"}`))
	req.Header.Add("X-Gateway-Tags", "a=1")
	req.Header.Add("X-Gateway-Tags", "b=2,c=3")
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if got := rec.wait(t).Tags; !reflect.DeepEqual(got, map[string]string{"a": "1", "b": "2"}) {
		t.Errorf("Tags = %v, want a and b only (cap 2)", got)
	}
}

// With tags disabled the headers are still removed before the upstream call,
// but nothing is recorded.
func TestRequestTags_DisabledStripsButDoesNotRecord(t *testing.T) {
	up, upstreamHeaders := headerUpstream(t)
	rec := &lastRec{}
	gw := gateway(t, Config{UpstreamBaseURL: up.URL, TagsDisabled: true}, rec)
	do(t, "POST", gw.URL+"/anthropic/v1/messages", map[string]string{
		"X-Gateway-User": "alice", "X-Gateway-Tags": "team=search",
	}, `{"model":"claude-sonnet-4-5"}`)
	c := rec.wait(t)
	if c.User != "" || len(c.Tags) != 0 {
		t.Errorf("recorded User=%q Tags=%v with tags disabled", c.User, c.Tags)
	}
	uh := upstreamHeaders()
	if uh.Get("X-Gateway-User") != "" || uh.Get("X-Gateway-Tags") != "" {
		t.Errorf("headers reached the upstream: %v", uh)
	}
}

// A refused call (upstream unreachable) is captured with its tags too.
func TestRequestTags_OnErrorRow(t *testing.T) {
	rec := &lastRec{}
	gw := gateway(t, Config{UpstreamBaseURL: "http://127.0.0.1:1"}, rec)
	do(t, "POST", gw.URL+"/anthropic/v1/messages", map[string]string{"X-Gateway-User": "bob", "X-Gateway-Tags": "team=x"}, `{"model":"claude-sonnet-4-5"}`)
	c := rec.wait(t)
	if c.Error == "" || c.User != "bob" || c.Tags["team"] != "x" {
		t.Errorf("error row: err=%q user=%q tags=%v", c.Error, c.User, c.Tags)
	}
}
