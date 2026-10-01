package mcp_test

import (
	"encoding/json"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
)

func TestRequestValidate(t *testing.T) {
	tests := []struct {
		name    string
		req     mcp.Request
		wantErr int // 0 means "valid"
	}{
		{"valid", mcp.Request{JSONRPC: "2.0", Method: "ping", ID: 1}, 0},
		{"valid notification", mcp.Request{JSONRPC: "2.0", Method: "notifications/initialized"}, 0},
		{"wrong version", mcp.Request{JSONRPC: "1.0", Method: "ping"}, mcp.ErrorCodeInvalidRequest},
		{"missing version", mcp.Request{Method: "ping"}, mcp.ErrorCodeInvalidRequest},
		{"missing method", mcp.Request{JSONRPC: "2.0"}, mcp.ErrorCodeInvalidRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.req.Validate()
			switch {
			case tt.wantErr == 0 && err != nil:
				t.Fatalf("Validate() = %v, want nil", err)
			case tt.wantErr != 0 && err == nil:
				t.Fatalf("Validate() = nil, want code %d", tt.wantErr)
			case tt.wantErr != 0 && err.Code != tt.wantErr:
				t.Fatalf("Validate() code = %d, want %d", err.Code, tt.wantErr)
			}
		})
	}
}

func TestIsNotification(t *testing.T) {
	if !(&mcp.Request{JSONRPC: "2.0", Method: "x"}).IsNotification() {
		t.Error("a request with no id must be a notification")
	}
	if (&mcp.Request{JSONRPC: "2.0", Method: "x", ID: 0}).IsNotification() {
		t.Error("id 0 is an id: a request carrying it is not a notification")
	}
	if (&mcp.Request{JSONRPC: "2.0", Method: "x", ID: ""}).IsNotification() {
		t.Error("the empty string is an id: a request carrying it is not a notification")
	}
}

func TestErrorCodesAreStable(t *testing.T) {
	// These three are part of the gateway's contract with its clients:
	// -32000 means re-initialize, -32001 means the credential is bad,
	// -32003 means the profile denied the tool. Renumbering any of them
	// silently changes how every client behaves.
	cases := map[string]struct{ got, want int }{
		"session not found": {mcp.ErrorCodeSessionNotFound, -32000},
		"unauthorized":      {mcp.ErrorCodeUnauthorized, -32001},
		"tool not allowed":  {mcp.ErrorCodeToolNotAllowed, -32003},
	}
	for name, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", name, c.got, c.want)
		}
	}
}

func TestNewSuccessResponse(t *testing.T) {
	resp := mcp.NewSuccessResponse("abc", mcp.PingResult{})
	if resp.JSONRPC != "2.0" || resp.ID != "abc" || resp.Error != nil {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if string(resp.Result) != "{}" {
		t.Fatalf("Result = %s, want {}", resp.Result)
	}
}

func TestNewSuccessResponseUnmarshalableResultBecomesAnError(t *testing.T) {
	// A response with neither result nor error is uninterpretable; an
	// internal error at least tells the caller what happened.
	resp := mcp.NewSuccessResponse(1, make(chan int))
	if resp.Error == nil || resp.Error.Code != mcp.ErrorCodeInternalError {
		t.Fatalf("want an internal error, got %+v", resp)
	}
}

func TestFormatID(t *testing.T) {
	// JSON numbers decode to float64, which must not render as "1.0".
	var decoded mcp.Request
	if err := json.Unmarshal([]byte(`{"jsonrpc":"2.0","id":7,"method":"ping"}`), &decoded); err != nil {
		t.Fatal(err)
	}
	if got := mcp.FormatID(decoded.ID); got != "7" {
		t.Errorf("FormatID(7) = %q, want %q", got, "7")
	}
	if got := mcp.FormatID("req-1"); got != "req-1" {
		t.Errorf("FormatID(\"req-1\") = %q", got)
	}
	if got := mcp.FormatID(nil); got != "" {
		t.Errorf("FormatID(nil) = %q, want empty", got)
	}
}

func TestToolsListResultEncodesEmptyAsArray(t *testing.T) {
	// A profile that grants nothing must serialize as [], not null:
	// clients treat a missing list differently from an empty one.
	raw, err := json.Marshal(mcp.ToolsListResult{Tools: []mcp.Tool{}})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"tools":[]}` {
		t.Fatalf("got %s, want {\"tools\":[]}", raw)
	}
}
