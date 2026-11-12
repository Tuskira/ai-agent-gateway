package clickhouse

import (
	"crypto/tls"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// Config configures the ClickHouse sink. It is built from
// internal/config.Config.Sinks.ClickHouse by cmd/gateway/main.go.
type Config struct {
	Host     string
	Port     int
	Database string
	Username string
	Password string
	// Secure enables TLS on the connection to ClickHouse.
	Secure bool

	// BatchSize is the number of buffered records (access logs + LLM
	// calls combined) that triggers an immediate flush.
	BatchSize int
	// FlushInterval is the longest a record waits before being flushed,
	// even if BatchSize hasn't been reached.
	FlushInterval time.Duration
	// BufferSize is the bounded queue's capacity; a record written once
	// it is full is dropped (see Sink.Dropped) rather than blocking the
	// caller.
	BufferSize int

	// SyncInsert turns ClickHouse asynchronous inserts off: each flush is
	// then a plain synchronous INSERT carrying no async-insert settings
	// (whatever the server or user profile configures applies), as before
	// async inserts were introduced. The zero value (false) keeps async
	// inserts on; see asyncSettings.
	SyncInsert bool
	// AsyncInsertBusyTimeout is how long ClickHouse buffers asynchronous
	// inserts (from every gateway replica writing the same table) before
	// writing them out as one part. It is sent per insert as
	// async_insert_busy_timeout_ms, with the server's adaptive timeout
	// turned off, so it is the actual wait rather than an upper bound.
	// Ignored when SyncInsert is set.
	AsyncInsertBusyTimeout time.Duration

	// adaptiveBusyTimeoutKnown is set by New when the server has the
	// async_insert_use_adaptive_busy_timeout setting (see asyncSettings).
	adaptiveBusyTimeoutKnown bool

	// Version is the gateway version recorded alongside each schema apply
	// in the schema ledger (gateway_schema_migrations). Informational only:
	// whether the schema is up to date is decided by its fingerprint.
	Version string
}

const (
	defaultPort          = 9000
	defaultBatchSize     = 100
	defaultFlushInterval = 2 * time.Second
	defaultBufferSize    = 1000
	// defaultAsyncInsertBusyTimeout and defaultFlushInterval were chosen
	// by measurement (six sinks, ~0.7 records/s in total, one ClickHouse
	// 24): ClickHouse writes at most one part per busy timeout per table,
	// so 10s keeps a table at or under 360 new parts an hour (288 measured,
	// against ~1,450 with synchronous 5s flushes), and a 2s flush interval
	// keeps a record's worst-case delay before it is queryable at ~12s
	// (2s + 10s) for the same part count as 5s.
	defaultAsyncInsertBusyTimeout = 10 * time.Second
	defaultDatabase               = "default"
)

// withDefaults returns a copy of c with every unset field filled in from
// the documented defaults.
func (c Config) withDefaults() Config {
	if c.Port <= 0 {
		c.Port = defaultPort
	}
	if c.BatchSize <= 0 {
		c.BatchSize = defaultBatchSize
	}
	if c.FlushInterval <= 0 {
		c.FlushInterval = defaultFlushInterval
	}
	if c.BufferSize <= 0 {
		c.BufferSize = defaultBufferSize
	}
	if c.AsyncInsertBusyTimeout <= 0 {
		c.AsyncInsertBusyTimeout = defaultAsyncInsertBusyTimeout
	}
	if c.Database == "" {
		c.Database = defaultDatabase
	}
	return c
}

func (c Config) addr() string {
	return fmt.Sprintf("%s:%d", c.Host, c.Port)
}

// options renders c as clickhouse-go connection options.
func (c Config) options() *clickhouse.Options {
	var tlsCfg *tls.Config
	if c.Secure {
		tlsCfg = &tls.Config{} // system root pool, standard verification
	}
	return &clickhouse.Options{
		Addr: []string{c.addr()},
		Auth: clickhouse.Auth{
			Database: c.Database,
			Username: c.Username,
			Password: c.Password,
		},
		TLS: tlsCfg,
		// The driver's default pool (MaxIdleConns 5, MaxOpenConns 10) is
		// shared with the analytics read side. With async inserts each
		// in-flight flush holds up to two connections (one per table)
		// while ClickHouse buffers it, so the pool grows by that many to
		// leave the readers their usual ten.
		MaxIdleConns: 5,
		MaxOpenConns: 10 + 2*maxInflightFlushes,
	}
}
