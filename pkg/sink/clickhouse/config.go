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
}

const (
	defaultPort          = 9000
	defaultBatchSize     = 100
	defaultFlushInterval = 5 * time.Second
	defaultBufferSize    = 1000
	defaultDatabase      = "default"
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
	}
}
