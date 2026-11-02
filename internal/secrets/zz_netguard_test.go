package secrets

import "github.com/Tuskira/tusk-ai-secured-gateway/internal/netguard/netguardtest"

// The external_service tests stand a credential service up on loopback.
func init() { netguardtest.AllowLoopback() }
