//go:build e2e

package e2e

import "github.com/Tuskira/tusk-ai-secured-gateway/internal/netguard/netguardtest"

// This suite drives the gateway over a real socket against backends it
// starts on loopback (httptest servers, a local MCP stub, etc. -- see the
// package doc in mcp_test.go). internal/netguard (added to block SSRF on
// outbound connections) blocks loopback by default, which is exactly what
// production wants, but it would also block every e2e backend here. This
// init -- unlike the unit tests under internal/..., which stand up their
// own httptest servers but exercise the gateway's Go APIs directly rather
// than dialing out to them -- installs the loopback allowance once for the
// whole e2e binary, so Policy.control (netguard's dial-time Control hook)
// lets these requests through. It has no effect on a production binary:
// netguardtest is a test-only package and this file only builds under the
// e2e tag.
func init() {
	netguardtest.AllowLoopback()
}
