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
// {"type": "external", "provider": "<id>", "config": {...}}, plus
// "source": "<name>" for a provider that reads a credential source (see
// pkg/headers.Source); Registry.Resolve loads that source and passes it
// to the provider in ctx (pkg/headers.ResolveInfoFrom).
type externalResolver struct {
	reg *Registry
}

func (*externalResolver) Type() string { return "external" }

func (e *externalResolver) Validate(cfg map[string]any) error {
	provider, providerCfg, err := e.lookup(cfg)
	if err != nil {
		return err
	}
	if err := checkSourceField(provider, cfg); err != nil {
		return err
	}
	return provider.Validate(providerCfg)
}

// checkSourceField enforces the shape of cfg["source"]: a provider that
// implements SourceSchemaProvider needs one (it has no settings without
// it), and any other provider must not be given one.
func checkSourceField(provider pkgheaders.ExternalProvider, cfg map[string]any) error {
	raw, present := cfg["source"]
	name, isString := raw.(string)
	if present && raw != nil && !isString {
		return fmt.Errorf("external: \"source\" must be a string")
	}
	_, wantsSource := provider.(pkgheaders.SourceSchemaProvider)
	switch {
	case wantsSource && name == "":
		return fmt.Errorf("external: provider %q needs a credential source (\"source\")", provider.ProviderID())
	case !wantsSource && name != "":
		return fmt.Errorf("external: provider %q does not use credential sources", provider.ProviderID())
	}
	return nil
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
