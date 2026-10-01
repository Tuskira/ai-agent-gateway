// Package log builds the process-wide slog.Logger and lets it ride in a
// context.Context so call chains don't need to thread it explicitly.
package log

import (
	"context"
	"log/slog"
	"os"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/config"
)

type ctxKey struct{}

// New builds a logger from cfg: JSON to stdout normally, or a human-readable
// text handler when cfg.Development is set.
func New(cfg config.Logging) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level(cfg.Level)}

	var h slog.Handler
	if cfg.Development {
		h = slog.NewTextHandler(os.Stdout, opts)
	} else {
		h = slog.NewJSONHandler(os.Stdout, opts)
	}
	return slog.New(h)
}

func level(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// With returns a context carrying logger, retrievable via From.
func With(ctx context.Context, logger *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, logger)
}

// From returns the logger stashed in ctx by With, or slog.Default() if none.
func From(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.Default()
}
