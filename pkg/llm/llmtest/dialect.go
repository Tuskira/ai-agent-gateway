package llmtest

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http/httptest"
	"path"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm"
)

// Wire samples, one directory per dialect, hand-written from the vendor's
// published API reference (request/response/error/stream examples). They are
// samples of the format, not simulations of a vendor's behaviour. Kinds:
//
//	request_*.json  {"wire": <client body>, "neutral": <llm.Request>}
//	response_*.json {"neutral": <llm.Response>, "wire": <client body>}
//	error_*.json    {"neutral": <llm.Error>, "wire": <client body>}
//	stream_*.json   {"events": [<llm.Event>...], "sse": "<client stream>"}
//
//go:embed testdata
var fixtures embed.FS

// RunDialect checks d against the wire samples in testdata/<d.Name()>/:
// ParseRequest must produce the neutral request, RenderResponse and
// RenderError the wire body, and the StreamEncoder the stream (one Write per
// event). It also checks a malformed body is a *llm.RequestError. With no
// samples for d, the sample subtests are skipped.
func RunDialect(t *testing.T, d llm.Dialect) {
	t.Run("malformed_request", func(t *testing.T) {
		_, err := d.ParseRequest(httptest.NewRequest("POST", "/", strings.NewReader("{not json")))
		if !errors.As(err, new(*llm.RequestError)) {
			t.Errorf("err = %v, want *llm.RequestError", err)
		}
	})
	dir := path.Join("testdata", d.Name())
	entries, err := fs.ReadDir(fixtures, dir)
	if err != nil {
		t.Skipf("no wire samples for dialect %q", d.Name())
	}
	for _, e := range entries {
		name := e.Name()
		raw, err := fixtures.ReadFile(path.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		var fx struct {
			Wire    json.RawMessage `json:"wire"`
			Neutral json.RawMessage `json:"neutral"`
			Events  []llm.Event     `json:"events"`
			SSE     string          `json:"sse"`
		}
		if err := json.Unmarshal(raw, &fx); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		t.Run(strings.TrimSuffix(name, ".json"), func(t *testing.T) {
			switch {
			case strings.HasPrefix(name, "request_"):
				req, err := d.ParseRequest(httptest.NewRequest("POST", "/", bytes.NewReader(fx.Wire)))
				if err != nil {
					t.Fatal(err)
				}
				got, _ := json.Marshal(req)
				sameJSON(t, got, fx.Neutral)
			case strings.HasPrefix(name, "response_"):
				var resp llm.Response
				if err := json.Unmarshal(fx.Neutral, &resp); err != nil {
					t.Fatal(err)
				}
				got, err := d.RenderResponse(&resp)
				if err != nil {
					t.Fatal(err)
				}
				sameJSON(t, got, fx.Wire)
			case strings.HasPrefix(name, "error_"):
				var e llm.Error
				if err := json.Unmarshal(fx.Neutral, &e); err != nil {
					t.Fatal(err)
				}
				sameJSON(t, d.RenderError(&e), fx.Wire)
			case strings.HasPrefix(name, "stream_"):
				var w writes
				enc := d.NewStreamEncoder(&w)
				for _, ev := range fx.Events {
					if err := enc.Write(ev); err != nil {
						t.Fatal(err)
					}
				}
				if err := enc.Close(); err != nil {
					t.Fatal(err)
				}
				want := splitSSE(t, fx.SSE)
				if len(w) != len(fx.Events) || len(want) != len(w) {
					t.Fatalf("%d events -> %d writes, want %d frames", len(fx.Events), len(w), len(want))
				}
				for i := range w {
					got := splitSSE(t, string(w[i]))
					if len(got) != 1 || got[0].event != want[i].event {
						t.Fatalf("write %d = %q, want one %q frame", i, w[i], want[i].event)
					}
					sameJSON(t, []byte(got[0].data), []byte(want[i].data))
				}
			default:
				t.Fatalf("unknown sample kind %s", name)
			}
		})
	}
}

type writes [][]byte

func (w *writes) Write(p []byte) (int, error) {
	*w = append(*w, append([]byte(nil), p...))
	return len(p), nil
}

type frame struct{ event, data string }

var reFrame = regexp.MustCompile(`(?m)^event: (\S+)\ndata: (.*)\n\n`)

func splitSSE(t *testing.T, s string) []frame {
	t.Helper()
	var out []frame
	for _, m := range reFrame.FindAllStringSubmatch(s, -1) {
		out = append(out, frame{m[1], m[2]})
	}
	if len(reFrame.ReplaceAllString(s, "")) != 0 {
		t.Fatalf("not SSE frames: %q", s)
	}
	return out
}

func sameJSON(t *testing.T, got, want []byte) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("got invalid JSON %s: %v", got, err)
	}
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("bad sample JSON %s: %v", want, err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Errorf("JSON mismatch\n got: %s\nwant: %s", got, want)
	}
}
