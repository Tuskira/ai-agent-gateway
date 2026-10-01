package llmplane

import "net/http"

// SetTransportOverrideForTest swaps the upstream transport (nil restores the guarded one).
func SetTransportOverrideForTest(rt http.RoundTripper) { transportOverride = rt }
