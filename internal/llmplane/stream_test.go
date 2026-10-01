package llmplane

import (
	"net/http"
	"testing"
)

func TestStreamFromRequest(t *testing.T) {
	resp := func(ct string) *http.Response {
		return &http.Response{Header: http.Header{"Content-Type": {ct}}}
	}
	cases := []struct {
		name string
		body string
		path string
		ct   string
		want bool
	}{
		{"anthropic body flag", `{"stream":true}`, "/v1/messages", "application/json", true},
		{"anthropic sse content-type", `{}`, "/v1/messages", "text/event-stream", true},
		{"bedrock eventstream content-type", `{}`, "/model/x/converse-stream", "application/vnd.amazon.eventstream", true},
		{"gemini array stream", `{}`, "/gemini/v1beta/models/g:streamGenerateContent", "application/json", true},
		{"bedrock stream path, error body", `{}`, "/bedrock/model/m/invoke-with-response-stream", "application/json", true},
		{"non-stream json", `{"stream":false}`, "/v1/messages", "application/json", false},
		{"empty", ``, "", "", false},
	}
	for _, c := range cases {
		if got := streamFromRequest([]byte(c.body), c.path, resp(c.ct)); got != c.want {
			t.Errorf("%s: streamFromRequest = %v, want %v", c.name, got, c.want)
		}
	}
}
