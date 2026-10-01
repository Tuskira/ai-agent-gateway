package llmplane

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/config"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// SeedModels upserts cfg's llm_proxy.models entries into the registry as
// PLATFORM rows (tenant ""), by name: an existing platform row of the same
// name is rewritten to match the file, a missing one is created. Rows the
// file does not mention are left alone -- the file is a seed, not the
// source of truth, so an operator can still add platform rows another way
// without the next restart deleting them. Idempotent; every replica may
// run it at boot. Returns how many rows were created and updated.
func SeedModels(ctx context.Context, models store.ModelStore, seeds []config.ModelSeed, logger *slog.Logger) (created, updated int, err error) {
	for _, seed := range seeds {
		m := &store.Model{
			Name:        seed.Name,
			Description: seed.Description,
			Enabled:     true,
			Targets:     make([]store.ModelTarget, 0, len(seed.Targets)),
			Metadata:    map[string]any{"seed": "config"},
		}
		for _, t := range seed.Targets {
			m.Targets = append(m.Targets, store.ModelTarget{
				Vendor: t.Vendor, Model: t.Model, BaseURL: t.BaseURL, Credential: t.Credential, Region: t.Region,
				AllowCallerKey: t.AllowCallerKey, Label: t.Label,
			})
		}
		if seed.Price != nil {
			m.Price = &store.ModelPrice{Input: seed.Price.Input, Output: seed.Price.Output, CacheRead: seed.Price.CacheRead, CacheWrite: seed.Price.CacheWrite}
		}

		existing, err := models.GetByName(ctx, "", seed.Name)
		switch {
		case err == nil:
			m.ID, m.TenantID = existing.ID, ""
			m.Limits = existing.Limits // not part of the seed; keep what the row has
			if existing.Metadata != nil {
				m.Metadata = existing.Metadata
				m.Metadata["seed"] = "config"
			}
			if err := models.Update(ctx, m); err != nil {
				return created, updated, fmt.Errorf("seed model %q: update: %w", seed.Name, err)
			}
			updated++
		case errors.Is(err, store.ErrNotFound):
			if err := models.Create(ctx, m); err != nil {
				return created, updated, fmt.Errorf("seed model %q: create: %w", seed.Name, err)
			}
			created++
		default:
			return created, updated, fmt.Errorf("seed model %q: look up: %w", seed.Name, err)
		}
	}
	if logger != nil && len(seeds) > 0 {
		logger.Info("model registry seeded from config", "created", created, "updated", updated)
	}
	return created, updated, nil
}
