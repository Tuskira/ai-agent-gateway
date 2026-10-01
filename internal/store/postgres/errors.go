package postgres

import (
	"database/sql"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// pgUniqueViolation is the SQLSTATE for a unique_violation.
// https://www.postgresql.org/docs/current/errcodes-appendix.html
const pgUniqueViolation = "23505"

// pgInvalidTextRepresentation is the SQLSTATE Postgres raises when a bind
// parameter can't be parsed as the column's type -- most commonly a
// caller-supplied id that isn't even a well-formed UUID, against a uuid
// column. Mapped onto store.ErrNotFound by mapReadErr: a malformed id can
// never match an existing row either, so the caller sees the same "not
// found" it would for a well-formed id that simply isn't there, not a
// raw driver error.
// https://www.postgresql.org/docs/current/errcodes-appendix.html
const pgInvalidTextRepresentation = "22P02"

// mapWriteErr maps a driver error from an INSERT/UPDATE into the store
// package's sentinel errors: a unique constraint violation becomes
// store.ErrConflict, everything else passes through wrapped.
func mapWriteErr(err error) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
		return store.ErrConflict
	}
	return err
}

// mapReadErr maps sql.ErrNoRows (from QueryRowContext.Scan, or a zero-row
// result the caller has already detected) and a malformed-id type error
// (see pgInvalidTextRepresentation) onto store.ErrNotFound.
func mapReadErr(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return store.ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgInvalidTextRepresentation {
		return store.ErrNotFound
	}
	return err
}
