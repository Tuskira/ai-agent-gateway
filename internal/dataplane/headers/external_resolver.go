package headers

import (
	"context"
	"fmt"

	pkgheaders "github.com/Tuskira/tusk-ai-secured-gateway/pkg/headers"
)

// externalResolver is the "external" built-in HeaderResolver: it
// dispatches to whichever ExternalProvider cfg["provider"] names, handing
// it just cfg["config"] (the provider-specific sub-object -- see
// pkg/headers.ExternalProvider.ConfigSchema). Config:
// {"type": "external", "provider": "<id>", "config": {...}}.
type externalResolver struct {
	reg *Registry
}

func (*externalResolver) Type() string { return "external" }

func (e *externalResolver) Validate(cfg map[string]any) error {
	provider, providerCfg, err := e.lookup(cfg)
	if err != nil {
		return err
	}
	return provider.Validate(providerCfg)
}

func (e *externalResolver) Resolve(ctx context.Context, cfg map[string]any) (string, error) {
	provider, providerCfg, err := e.lookup(cfg)
	if err != nil {
		return "", err
	}
	return provider.Resolve(ctx, providerCfg)
}

func (e *externalResolver) lookup(cfg map[string]any) (pkgheaders.ExternalProvider, map[string]any, error) {
	id, _ := cfg["provider"].(string)
	if id == "" {
		return nil, nil, fmt.Errorf("external: \"provider\" is required")
	}

	provider, ok := e.reg.provider(id)
	if !ok {
		return nil, nil, fmt.Errorf("external: no provider registered for id %q", id)
	}

	providerCfg, _ := cfg["config"].(map[string]any)
	if providerCfg == nil {
		providerCfg = map[string]any{}
	}
	return provider, providerCfg, nil
}
