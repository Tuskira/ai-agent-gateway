package translate

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
)

// ProviderKeyHeader is the header a caller sends its own vendor key in for a
// translated target: "X-Provider-Key", or "X-Provider-Key-<label>" for a
// target that carries a label (one header per vendor, so a session that
// uses several sends them all). Read here, never forwarded anywhere.
const ProviderKeyHeader = "X-Provider-Key"

// CallerKey picks the caller's own vendor key for a translated target that
// accepts one (allow_caller_key). In order:
//
//  1. X-Provider-Key-<label> when the target has a label, else
//     X-Provider-Key: the key the caller addressed to this vendor.
//  2. The credential header of the client's own dialect (x-api-key, or a
//     Bearer Authorization) -- unless it is the credential that
//     authenticated the caller to the gateway (gk_..., or whatever the
//     Authenticator kept as RawCredential, e.g. an OIDC token) or an
//     Anthropic credential (sk-ant-..., API key or OAuth token): Claude
//     Code sends one by default, and it must not reach another vendor.
//
// "" means the caller sent none: the call goes out without a credential,
// which is what a keyless local server (Ollama, vLLM) expects.
func CallerKey(r *http.Request, label string) string {
	name := ProviderKeyHeader
	if label != "" {
		name += "-" + label
	}
	if k := r.Header.Get(name); k != "" {
		return k
	}
	var gateway string
	if pr, ok := pkgauth.PrincipalFrom(r.Context()); ok {
		gateway = pr.RawCredential
	}
	bearer, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if bearer == r.Header.Get("Authorization") {
		bearer = "" // not a Bearer credential (e.g. SigV4): nothing to forward
	}
	for _, k := range []string{r.Header.Get("X-Api-Key"), bearer} {
		if k != "" && k != gateway && !strings.HasPrefix(k, "gk_") && !strings.HasPrefix(k, "sk-ant-") {
			return k
		}
	}
	return ""
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func rawObject(raw json.RawMessage) map[string]json.RawMessage {
	var m map[string]json.RawMessage
	_ = json.Unmarshal(raw, &m)
	return m
}
