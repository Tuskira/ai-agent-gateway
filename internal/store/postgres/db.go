// Package postgres is the Postgres implementation of pkg/store.Store. It
// registers itself under driver "postgres" (see store.go's init), and
// db.go here just opens the underlying connection pool.
package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/config"
)

const pingTimeout = 5 * time.Second

// Open opens a *sql.DB against cfg using the pgx stdlib driver, applies
// baseline pool settings, and verifies connectivity with a bounded ping.
// Callers are responsible for closing the returned DB.
func Open(ctx context.Context, cfg config.Database) (*sql.DB, error) {
	db, err := sql.Open("pgx", cfg.DSN())
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)
	db.SetConnMaxIdleTime(5 * time.Minute)

	pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()

	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping db: %w", err)
	}

	return db, nil
}
