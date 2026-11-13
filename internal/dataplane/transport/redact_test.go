package transport_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/profile"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
)

// fakeJWT is assembled at run time so the source holds no token literal.
func fakeJWT() string {
	e := base64.RawURLEncoding.EncodeToString
	return e([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + e([]byte(`{"sub":"tester"}`)) + "." + e([]byte("signature-bytes-here"))
}

// A tool call carrying a JWT: the backend gets it and the client gets the
// echo back unchanged, but the access log stores both payloads scrubbed.
func TestAccessLogRedactsSecretsInStoredPayloads(t *testing.T) {
	jwt := fakeJWT()
	for _, c := range []struct {
		name     string
		noRedact bool
	}{{"redaction on", false}, {"redaction off", true}} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, fixtureOptions{ProfileTools: []string{"echo"}, StoreBodies: true, NoRedact: c.noRedact})
			params, _ := json.Marshal(map[string]any{"name": "alpha__echo", "arguments": map[string]any{"token": jwt}})
			_, decoded := f.call(t, mcp.Request{JSONRPC: mcp.Version, ID: 9, Method: mcp.MethodToolsCall, Params: params},
				map[string]string{profile.Header: "Reader"})
			if decoded.Error != nil {
				t.Fatalf("tools/call failed: %+v", decoded.Error)
			}
			if !bytes.Contains(decoded.Result, []byte(jwt)) {
				t.Errorf("client result lost the token: %s", decoded.Result)
			}
			if args := f.backend.ToolArgs("echo"); args == nil || args["token"] != jwt {
				t.Errorf("backend got %v, want the token unchanged", args)
			}

			records := f.sink.all()
			rec := records[len(records)-1]
			for name, body := range map[string][]byte{"request": rec.RequestBody, "response": rec.ResponseBody} {
				leaked := bytes.Contains(body, []byte(jwt))
				marked := bytes.Contains(body, []byte("[REDACTED:jwt]"))
				if c.noRedact {
					if !leaked {
						t.Errorf("%s body with redaction off = %s", name, body)
					}
					continue
				}
				if leaked || !marked {
					t.Errorf("%s body stored %s", name, body)
				}
				if !json.Valid(body) {
					t.Errorf("%s body is not valid JSON: %s", name, body)
				}
			}
		})
	}
}
