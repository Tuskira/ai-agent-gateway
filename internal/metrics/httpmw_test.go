package metrics

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
)

// scrape returns the exposition text the Prometheus handler serves now.
func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape = %d", rec.Code)
	}
	return rec.Body.String()
}

// sample returns the value of the series line starting with prefix, or
// fails the test when it is absent.
func sample(t *testing.T, body, prefix string) float64 {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, prefix) {
			f := strings.Fields(line)
			v, err := strconv.ParseFloat(f[len(f)-1], 64)
			if err != nil {
				t.Fatalf("parse %q: %v", line, err)
			}
			return v
		}
	}
	t.Fatalf("series %q not found in:\n%s", prefix, body)
	return 0
}

func promMetrics(t *testing.T) *Metrics {
	t.Helper()
	return newMetrics(t, promCfg("127.0.0.1:0"))
}

func serve(h http.Handler, method, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec
}

func TestInstrumentRecordsStatusRouteAndDuration(t *testing.T) {
	m := promMetrics(t)
	h := m.Instrument("mcp", MCPRoute)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		time.Sleep(15 * time.Millisecond)
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, "x")
	}))

	serve(h, http.MethodPost, "/mcp")
	serve(h, http.MethodPost, "/mcp")
	serve(h, http.MethodDelete, "/mcp")
	serve(h, http.MethodGet, "/nope")

	body := scrape(t, m)
	for prefix, want := range map[string]float64{
		`gateway_http_requests_total{method="POST",plane="mcp",route="/mcp",status="202"}`:    2,
		`gateway_http_requests_total{method="DELETE",plane="mcp",route="/mcp",status="204"}`:  1,
		`gateway_http_requests_total{method="GET",plane="mcp",route="other",status="202"}`:    1,
		`gateway_http_request_duration_seconds_count{method="POST",plane="mcp",route="/mcp"}`: 2,
	} {
		if got := sample(t, body, prefix); got != want {
			t.Errorf("%s = %v, want %v", prefix, got, want)
		}
	}
	// Two sleeps of 15ms: the sum must reflect real elapsed time, and the
	// buckets must be the explicit ones.
	if sum := sample(t, body, `gateway_http_request_duration_seconds_sum{method="POST",plane="mcp",route="/mcp"}`); sum < 0.03 || sum > 5 {
		t.Errorf("duration sum = %v, want about 0.03", sum)
	}
	if !strings.Contains(body, `gateway_http_request_duration_seconds_bucket{method="POST",plane="mcp",route="/mcp",le="300"}`) {
		t.Errorf("explicit 300s bucket missing:\n%s", body)
	}
	if !strings.Contains(body, `le="0.005"`) {
		t.Errorf("explicit 5ms bucket missing")
	}
}

func TestInstrumentStatusSemantics(t *testing.T) {
	m := promMetrics(t)
	mw := m.Instrument("llm", func(*http.Request) string { return "/x" })

	handlers := []http.HandlerFunc{
		// Wrote nothing: net/http answers 200.
		func(http.ResponseWriter, *http.Request) {},
		// Body before header: net/http sent 200 and ignores the later 201.
		func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "b"); w.WriteHeader(201) },
		// The first status wins.
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(404); w.WriteHeader(500) },
		// An informational 1xx is not the final status.
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusEarlyHints); w.WriteHeader(503) },
	}
	for _, fn := range handlers {
		mw(fn).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	}
	body := scrape(t, m)
	// 200 twice (nothing written, body-then-header), 404, 503.
	if got := sample(t, body, `gateway_http_requests_total{method="GET",plane="llm",route="/x",status="200"}`); got != 2 {
		t.Errorf("status 200 = %v, want 2", got)
	}
	for _, code := range []string{"404", "503"} {
		if got := sample(t, body, `gateway_http_requests_total{method="GET",plane="llm",route="/x",status="`+code+`"}`); got != 1 {
			t.Errorf("status %s = %v, want 1", code, got)
		}
	}
	if strings.Contains(body, `status="500"`) || strings.Contains(body, `status="103"`) {
		t.Errorf("a non-final status leaked into the labels:\n%s", body)
	}
}

func TestInstrumentEmptyRouteIsUnmatchedAndMethodIsBounded(t *testing.T) {
	m := promMetrics(t)
	h := m.Instrument("api", func(*http.Request) string { return "" })(http.NotFoundHandler())
	serve(h, "BREW", "/a")
	serve(h, "BREW2", "/b")
	serve(h, http.MethodGet, "/c")

	body := scrape(t, m)
	if got := sample(t, body, `gateway_http_requests_total{method="OTHER",plane="api",route="unmatched",status="404"}`); got != 2 {
		t.Errorf("OTHER = %v, want 2", got)
	}
	if got := sample(t, body, `gateway_http_requests_total{method="GET",plane="api",route="unmatched",status="404"}`); got != 1 {
		t.Errorf("GET = %v, want 1", got)
	}
	if strings.Contains(body, "BREW") {
		t.Error("client-controlled method leaked into a label")
	}
}

func TestInstrumentNilRouteFuncIsUnmatched(t *testing.T) {
	m := promMetrics(t)
	h := m.Instrument("api", nil)(http.NotFoundHandler())
	serve(h, http.MethodGet, "/a")
	if got := sample(t, scrape(t, m), `gateway_http_requests_total{method="GET",plane="api",route="unmatched",status="404"}`); got != 1 {
		t.Errorf("= %v, want 1", got)
	}
}

func TestInstrumentInFlightReturnsToZero(t *testing.T) {
	m := promMetrics(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	h := m.Instrument("mcp", MCPRoute)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		entered <- struct{}{}
		<-release
	}))

	var wg sync.WaitGroup
	const n = 3
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			serve(h, http.MethodPost, "/mcp")
		}()
	}
	for range n {
		<-entered
	}
	if got := sample(t, scrape(t, m), `gateway_http_requests_in_flight{plane="mcp"}`); got != n {
		t.Errorf("in flight while blocked = %v, want %d", got, n)
	}
	close(release)
	wg.Wait()
	if got := sample(t, scrape(t, m), `gateway_http_requests_in_flight{plane="mcp"}`); got != 0 {
		t.Errorf("in flight after = %v, want 0", got)
	}
}

func TestInstrumentPanicDecrementsAndCountsAs500(t *testing.T) {
	m := promMetrics(t)
	h := m.Instrument("mcp", MCPRoute)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))
	func() {
		defer func() { _ = recover() }()
		serve(h, http.MethodPost, "/mcp")
	}()
	body := scrape(t, m)
	if got := sample(t, body, `gateway_http_requests_in_flight{plane="mcp"}`); got != 0 {
		t.Errorf("in flight after panic = %v, want 0", got)
	}
	if got := sample(t, body, `gateway_http_requests_total{method="POST",plane="mcp",route="/mcp",status="500"}`); got != 1 {
		t.Errorf("panic request = %v, want 1 with status 500", got)
	}
}

func TestInstrumentPassesFlushThrough(t *testing.T) {
	m := promMetrics(t)
	var sawFlusher bool
	h := m.Instrument("mcp", MCPRoute)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f, ok := w.(http.Flusher)
		sawFlusher = ok
		if !ok {
			return
		}
		_, _ = io.WriteString(w, "data: 1\n\n")
		f.Flush()
	}))
	rec := serve(h, http.MethodGet, "/mcp/stream")
	if !sawFlusher {
		t.Fatal("wrapped ResponseWriter is not an http.Flusher")
	}
	if !rec.Flushed {
		t.Error("Flush did not reach the underlying writer")
	}
	if got := sample(t, scrape(t, m), `gateway_http_requests_total{method="GET",plane="mcp",route="/mcp/stream",status="200"}`); got != 1 {
		t.Errorf("stream request = %v, want 1", got)
	}
}

func TestInstrumentFlushBeforeHeaderIs200(t *testing.T) {
	m := promMetrics(t)
	h := m.Instrument("mcp", MCPRoute)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.(http.Flusher).Flush()
	}))
	serve(h, http.MethodGet, "/mcp/stream")
	if got := sample(t, scrape(t, m), `gateway_http_requests_total{method="GET",plane="mcp",route="/mcp/stream",status="200"}`); got != 1 {
		t.Errorf("= %v, want 1", got)
	}
}

// unwrapWriter has no Flush of its own but exposes the real writer through
// Unwrap, as an outer middleware's wrapper does.
type unwrapWriter struct {
	http.ResponseWriter
	inner http.ResponseWriter
}

func (u unwrapWriter) Unwrap() http.ResponseWriter { return u.inner }

func TestStatusWriterUnwrapAndController(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := newStatusWriter(unwrapWriter{ResponseWriter: struct{ http.ResponseWriter }{rec}, inner: rec})
	if sw.Unwrap() == nil {
		t.Fatal("Unwrap returned nil")
	}
	// ResponseController reaches the Flusher through the Unwrap chain even
	// though the immediate inner writer has none.
	sw.Flush()
	if !rec.Flushed {
		t.Error("Flush did not follow the Unwrap chain")
	}
	if sw.statusCode() != http.StatusOK {
		t.Errorf("statusCode = %d, want 200", sw.statusCode())
	}
}

func TestInstrumentFlushOnNonFlusherDoesNotPanic(t *testing.T) {
	m := promMetrics(t)
	h := m.Instrument("mcp", MCPRoute)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.(http.Flusher).Flush()
	}))
	h.ServeHTTP(struct{ http.ResponseWriter }{httptest.NewRecorder()}, httptest.NewRequest(http.MethodGet, "/", nil))
}

func TestInstrumentHijackerOnlyWhenInnerSupportsIt(t *testing.T) {
	m := promMetrics(t)
	mw := m.Instrument("mcp", MCPRoute)

	var plainIsHijacker bool
	mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, plainIsHijacker = w.(http.Hijacker)
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if plainIsHijacker {
		t.Error("wrapper claims http.Hijacker over a writer that has none")
	}

	// A real server connection can be hijacked through the wrapper, and the
	// request is recorded as 101 when the handler never wrote a status.
	srv := httptest.NewServer(mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("wrapper hides http.Hijacker from a real connection")
			return
		}
		conn, rw, err := hj.Hijack()
		if err != nil {
			t.Errorf("Hijack: %v", err)
			return
		}
		_, _ = rw.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n")
		_ = rw.Flush()
		_ = conn.Close()
	})))
	defer srv.Close()

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_, _ = io.WriteString(conn, "GET /mcp/stream HTTP/1.1\r\nHost: x\r\n\r\n")
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if line, err := bufio.NewReader(conn).ReadString('\n'); err != nil || !strings.HasPrefix(line, "HTTP/1.1 200") {
		t.Fatalf("hijacked response = %q, %v", line, err)
	}

	re := regexp.MustCompile(`gateway_http_requests_total\{method="GET",plane="mcp",route="/mcp/stream",status="101"\} 1`)
	deadline := time.Now().Add(5 * time.Second)
	for !re.MatchString(scrape(t, m)) {
		if time.Now().After(deadline) {
			t.Fatalf("hijacked request not recorded as 101:\n%s", scrape(t, m))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestInstrumentAPIRouteInsideChi(t *testing.T) {
	m := promMetrics(t)
	r := chi.NewRouter()
	r.Use(m.Instrument("api", APIRoute))
	r.Get("/api/v1/connectors/{id}", func(w http.ResponseWriter, _ *http.Request) {})
	r.Mount("/", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ui") }))
	r.Route("/api/v1", func(r chi.Router) {
		r.NotFound(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	})

	serve(r, http.MethodGet, "/api/v1/connectors/11111111-1111-1111-1111-111111111111")
	serve(r, http.MethodGet, "/api/v1/connectors/22222222-2222-2222-2222-222222222222")
	serve(r, http.MethodGet, "/some/ui/page")

	body := scrape(t, m)
	if got := sample(t, body, `gateway_http_requests_total{method="GET",plane="api",route="/api/v1/connectors/{id}",status="200"}`); got != 2 {
		t.Errorf("pattern route = %v, want 2 (ids must not reach the label)", got)
	}
	if got := sample(t, body, `gateway_http_requests_total{method="GET",plane="api",route="/*",status="200"}`); got != 1 {
		t.Errorf("UI mount route = %v, want 1", got)
	}
	if strings.Contains(body, "1111") {
		t.Error("a path id leaked into a label")
	}
}

func TestInstrumentSharesInstrumentsAcrossPlanes(t *testing.T) {
	m := promMetrics(t)
	a := m.Instrument("mcp", MCPRoute)
	b := m.Instrument("llm", LLMRoute)
	ok := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	serve(a(ok), http.MethodGet, "/health")
	serve(b(ok), http.MethodGet, "/health")
	body := scrape(t, m)
	for _, plane := range []string{"mcp", "llm"} {
		if got := sample(t, body, `gateway_http_requests_total{method="GET",plane="`+plane+`",route="/health",status="200"}`); got != 1 {
			t.Errorf("plane %s = %v, want 1", plane, got)
		}
	}
	if strings.Count(body, "# TYPE gateway_http_requests_total") != 1 {
		t.Error("instrument family registered more than once")
	}
}

func TestInstrumentDisabledAndNilAreNoOps(t *testing.T) {
	for name, m := range map[string]*Metrics{"none": newMetrics(t, promCfgNone()), "nil": nil} {
		t.Run(name, func(t *testing.T) {
			var sawFlusher bool
			h := m.Instrument("mcp", MCPRoute)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, sawFlusher = w.(http.Flusher)
				w.WriteHeader(http.StatusTeapot)
			}))
			rec := serve(h, http.MethodPost, "/mcp")
			if rec.Code != http.StatusTeapot || !sawFlusher {
				t.Errorf("code=%d flusher=%v", rec.Code, sawFlusher)
			}
		})
	}
}

func TestMCPRoute(t *testing.T) {
	for _, c := range []struct{ method, path, want string }{
		{"GET", "/health", "/health"},
		{"HEAD", "/health", "/health"},
		{"POST", "/health", "other"},
		{"POST", "/mcp", "/mcp"},
		{"DELETE", "/mcp", "/mcp"},
		{"GET", "/mcp", "other"},
		{"GET", "/mcp/stream", "/mcp/stream"},
		{"POST", "/mcp/stream", "other"},
		{"GET", "/mcp/stream/extra", "other"},
		{"GET", "/", "other"},
		{"POST", "/anything/abc123", "other"},
	} {
		r := httptest.NewRequest(c.method, c.path, nil)
		if got := MCPRoute(r); got != c.want {
			t.Errorf("MCPRoute(%s %s) = %q, want %q", c.method, c.path, got, c.want)
		}
	}
}

func TestLLMRoute(t *testing.T) {
	for path, want := range map[string]string{
		"/health":                                   "/health",
		"/anthropic/v1/messages":                    "anthropic",
		"/openai/v1/chat/completions":               "openai",
		"/gemini/v1beta/models/gemini-2.5:generate": "gemini",
		"/bedrock/model/anthropic.claude/invoke":    "bedrock",
		"/openai":                                   "openai",
		"/v1/messages":                              "anthropic",
		"/v1/messages/count_tokens":                 "anthropic",
		"/v1/complete":                              "anthropic",
		"/model/anthropic.claude-3/invoke":          "bedrock",
		"/v1/chat/completions":                      "other",
		"/models/some-model-id":                     "other",
		"/random-segment/x":                         "other",
		"/":                                         "other",
		"/modelx/y":                                 "other",
	} {
		r := httptest.NewRequest(http.MethodPost, path, nil)
		if got := LLMRoute(r); got != want {
			t.Errorf("LLMRoute(%s) = %q, want %q", path, got, want)
		}
	}
}

func TestAPIRouteOutsideChiIsUnmatched(t *testing.T) {
	if got := APIRoute(httptest.NewRequest(http.MethodGet, "/api/v1/x", nil)); got != "unmatched" {
		t.Errorf("APIRoute without chi = %q", got)
	}
}
