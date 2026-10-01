package dptest_test

import (
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/dptest"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store/storetest"
)

func TestMCPCatalogConformance(t *testing.T) {
	storetest.RunMCPCatalog(t, func(*testing.T) store.Store { return dptest.New() })
}
