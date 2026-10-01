package orchestrator

import "github.com/Tuskira/tusk-ai-secured-gateway/internal/netguard/netguardtest"

func init() { netguardtest.AllowLoopback() }
