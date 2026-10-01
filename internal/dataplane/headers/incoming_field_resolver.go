package headers

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"golang.org/x/net/http/httpguts"
)

// incomingHeaderDenylist is the set of inbound header names the
// incoming_field resolver refuses to forward onto an outbound request,
// canonicalized (http.CanonicalHeaderKey) for case-insensitive lookup. See
// Validate for the "mcp-" prefix, which is checked separately.
//
// Categories:
//   - Caller identity/auth: Authorization is how the caller authenticates
//     to THIS gateway, never something to hand a downstream connector
//     verbatim. X-Tenant-Id and Tenantid are gateway-owned identity,
//     always sourced from the authenticated Principal -- forwarding a
//     caller-supplied value would be a tenant-impersonation vector.
//   - X-Gateway-Key: this gateway's own API-key header; same reasoning.
//   - HTTP hop-by-hop (RFC 7230 6.1): meaningful only between the caller
//     and this gateway, never safe to copy onto a request to a different
//     server.
var incomingHeaderDenylist = map[string]struct{}{
	http.CanonicalHeaderKey("authorization"):       {},
	http.CanonicalHeaderKey("x-tenant-id"):         {},
	http.CanonicalHeaderKey("tenantid"):            {},
	http.CanonicalHeaderKey("x-gateway-key"):       {},
	http.CanonicalHeaderKey("connection"):          {},
	http.CanonicalHeaderKey("keep-alive"):          {},
	http.CanonicalHeaderKey("proxy-authenticate"):  {},
	http.CanonicalHeaderKey("proxy-authorization"): {},
	http.CanonicalHeaderKey("te"):                  {},
	http.CanonicalHeaderKey("trailer"):             {},
	http.CanonicalHeaderKey("transfer-encoding"):   {},
	http.CanonicalHeaderKey("upgrade"):             {},
}

// incomingFieldResolver is the "incoming_field" built-in HeaderResolver: it
// copies one header from the inbound request being proxied onto the
// outbound one. Config: {"type": "incoming_field", "header": "X-Foo"}.
//
// Validate refuses any header on incomingHeaderDenylist, or whose name
// starts with "mcp-" (the gateway's own MCP protocol/session headers), so
// a connector config can never be used to forge the caller's identity or
// tamper with MCP session state.
type incomingFieldResolver struct{}

func (incomingFieldResolver) Type() string { return "incoming_field" }

func (incomingFieldResolver) Validate(cfg map[string]any) error {
	name, _ := cfg["header"].(string)
	if name == "" {
		return fmt.Errorf("incoming_field: \"header\" is required")
	}
	if !httpguts.ValidHeaderFieldName(name) {
		return fmt.Errorf("incoming_field: %q is not a valid header name", name)
	}
	if isDenylistedIncomingHeader(name) {
		return fmt.Errorf("incoming_field: header %q is not forwardable (denylisted)", name)
	}
	return nil
}

func (incomingFieldResolver) Resolve(ctx context.Context, cfg map[string]any) (string, error) {
	name, _ := cfg["header"].(string)

	req, ok := incomingRequestFrom(ctx)
	if !ok || req == nil {
		return "", fmt.Errorf("incoming_field: no inbound request available")
	}

	// http.Header.Values canonicalizes the lookup key itself.
	values := req.Header.Values(name)
	if len(values) == 0 {
		return "", fmt.Errorf("incoming_field: header %q not present on the inbound request", name)
	}
	return strings.Join(values, ", "), nil
}

func isDenylistedIncomingHeader(name string) bool {
	if strings.HasPrefix(strings.ToLower(name), "mcp-") {
		return true
	}
	_, denied := incomingHeaderDenylist[http.CanonicalHeaderKey(name)]
	return denied
}
