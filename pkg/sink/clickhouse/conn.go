package clickhouse

import (
	"context"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// conn is the subset of clickhouse-go/v2's driver.Conn this package uses.
// driver.Conn (returned by clickhouse.Open) satisfies it structurally;
// declaring our own narrow interface here -- rather than depending on
// driver.Conn everywhere -- is what lets the Reader-side unit tests
// (reader_test.go, gated behind the "integration" build tag like the
// insert-path tests) substitute a fake without implementing driver.Conn's
// full surface (Select, AsyncInsert, Stats, ...).
type conn interface {
	Ping(ctx context.Context) error
	Exec(ctx context.Context, query string, args ...any) error
	PrepareBatch(ctx context.Context, query string, opts ...driver.PrepareBatchOption) (driver.Batch, error)
	Query(ctx context.Context, query string, args ...any) (driver.Rows, error)
	QueryRow(ctx context.Context, query string, args ...any) driver.Row
	Close() error
}
