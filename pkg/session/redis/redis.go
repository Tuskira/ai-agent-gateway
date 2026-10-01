// Package redis is the shared session.Store and session.Notifier over a
// single Redis node, registered as driver "redis". It is what lets the
// MCP plane run more than one replica: any replica can resolve any
// session -- backend handles included -- and a tools/list_changed, or a
// notification for one session, raised on one replica reaches the SSE
// streams held open on the others.
//
// Layout in Redis:
//
//   - gw:sess:<id>  -- one JSON-encoded session.Record per session, SET
//     with a TTL that ends at Record.ExpiresAt, so Redis itself enforces
//     the expiry contract and there is nothing to sweep.
//   - gw:tools_changed -- the pub/sub channel tools/list_changed is
//     relayed over.
//   - gw:session_msgs -- the pub/sub channel session messages are
//     relayed over: one channel for every session, each message carrying
//     its tenant and session id, which every replica matches against the
//     streams it holds.
//
// Each message on either channel names the publishing instance so a
// replica can skip its own echo; see session.Notifier for why.
//
// This package is public so a binary built outside this module (a
// private plugin's own main) can blank-import it just as cmd/gateway
// does, and so it can serve as the worked example for a third-party
// driver. github.com/redis/go-redis stays an implementation detail: no
// exported identifier here mentions a go-redis type.
//
// Redis Cluster is not supported yet; the gateway's config validation
// rejects redis.cluster: true before this package is reached.
package redis

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	goredis "github.com/redis/go-redis/v9"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/session"
)

const (
	// Driver is the name this package registers with pkg/session.
	Driver = "redis"

	// KeyPrefix namespaces session keys, so the gateway can share a
	// Redis with other users of it without colliding.
	KeyPrefix = "gw:sess:"

	// Channel is the pub/sub channel tools/list_changed is relayed on.
	Channel = "gw:tools_changed"
	// SessionChannel is the pub/sub channel session messages are
	// relayed on.
	SessionChannel = "gw:session_msgs"

	// pingTimeout bounds the startup Ping. A wrong address should fail
	// the boot in seconds, not after the dial's own retries have run out.
	pingTimeout = 5 * time.Second
)

func init() {
	session.Register(Driver, func(ctx context.Context, cfg session.Config) (session.Store, session.Notifier, error) {
		st, n, err := New(ctx, cfg)
		if err != nil {
			return nil, nil, err
		}
		return st, n, nil
	})
}

// New dials the Redis named by cfg (Addr, Password, DB, TLS; Options is
// ignored), pings it so a bad address, password or TLS setting fails now
// rather than on the first MCP request, and returns a Store and a
// Notifier sharing one connection pool. Closing either closes the pool
// once both have been closed; the gateway closes both at shutdown.
func New(ctx context.Context, cfg session.Config) (*Store, *Notifier, error) {
	if cfg.Addr == "" {
		return nil, nil, errors.New("redis: addr is required")
	}
	opts := &goredis.Options{
		Addr:     cfg.Addr,
		Password: cfg.Password,
		DB:       cfg.DB,
	}
	if cfg.TLS {
		opts.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	client := goredis.NewClient(opts)

	pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		_ = client.Close()
		return nil, nil, fmt.Errorf("redis: ping %s: %w", cfg.Addr, err)
	}

	shared := &conn{client: client, refs: 2}
	return &Store{conn: shared}, &Notifier{conn: shared, instance: uuid.NewString()}, nil
}

// conn is the pool shared by a Store/Notifier pair, closed when the last
// of the two is closed.
type conn struct {
	client *goredis.Client
	mu     sync.Mutex
	refs   int
}

func (c *conn) release() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.refs <= 0 {
		return nil
	}
	c.refs--
	if c.refs == 0 {
		return c.client.Close()
	}
	return nil
}

// ---------------------------------------------------------------------------
// Store
// ---------------------------------------------------------------------------

// Store is the Redis session.Store: one JSON document per session under
// KeyPrefix+id, written with a TTL that ends at Record.ExpiresAt.
//
// Every Get decodes a fresh copy, so two concurrent requests on one
// session each see their own Record and the later Save wins; see
// session.Store for why that is acceptable.
type Store struct {
	conn *conn
}

var _ session.Store = (*Store)(nil)

// Get implements session.Store.
func (s *Store) Get(ctx context.Context, id string) (*session.Record, error) {
	if id == "" {
		return nil, session.ErrNotFound
	}
	raw, err := s.conn.client.Get(ctx, KeyPrefix+id).Bytes()
	if errors.Is(err, goredis.Nil) {
		return nil, session.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("redis: get session: %w", err)
	}
	var r session.Record
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("redis: decode session: %w", err)
	}
	// The key TTL is measured by Redis's clock and rounds to the
	// millisecond; the contract is stated against the Record, so apply
	// it exactly rather than trust the rounding.
	if r.Expired(time.Now()) {
		return nil, session.ErrNotFound
	}
	return &r, nil
}

// Save implements session.Store. The key's TTL is the time left until
// r.ExpiresAt; a Record that has already expired is deleted instead,
// since SET refuses a non-positive expiry and the contract says an
// expired Record must not be returned.
func (s *Store) Save(ctx context.Context, r *session.Record) error {
	if err := r.Validate(); err != nil {
		return err
	}
	ttl := time.Until(r.ExpiresAt)
	if ttl < time.Millisecond {
		return s.Delete(ctx, r.ID)
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("redis: encode session: %w", err)
	}
	// go-redis sends a TTL with sub-second precision as PX, so the key
	// lapses with the session rather than up to a second either side.
	if err := s.conn.client.Set(ctx, KeyPrefix+r.ID, raw, ttl).Err(); err != nil {
		return fmt.Errorf("redis: set session: %w", err)
	}
	return nil
}

// Delete implements session.Store.
func (s *Store) Delete(ctx context.Context, id string) error {
	if id == "" {
		return nil
	}
	if err := s.conn.client.Del(ctx, KeyPrefix+id).Err(); err != nil {
		return fmt.Errorf("redis: del session: %w", err)
	}
	return nil
}

// Close implements session.Store.
func (s *Store) Close() error { return s.conn.release() }

// ---------------------------------------------------------------------------
// Notifier
// ---------------------------------------------------------------------------

// Notifier relays tools/list_changed and session messages over Redis
// pub/sub. Each instance draws a random id at construction and stamps it
// on what it publishes, which is how a subscriber recognizes -- and
// skips -- the instance's own messages (session.Notifier's "never
// itself" rule).
type Notifier struct {
	conn     *conn
	instance string
}

var _ session.Notifier = (*Notifier)(nil)

// message is the payload on Channel.
type message struct {
	// Origin is the publishing Notifier's instance id.
	Origin string `json:"origin"`
	Tenant string `json:"tenant"`
}

// sessionEnvelope is the payload on SessionChannel.
type sessionEnvelope struct {
	Origin string `json:"origin"`
	session.SessionMessage
}

// Publish implements session.Notifier.
func (n *Notifier) Publish(ctx context.Context, tenantID string) error {
	if tenantID == "" {
		return errors.New("redis: Publish called with an empty tenant id")
	}
	return n.publish(ctx, Channel, message{Origin: n.instance, Tenant: tenantID})
}

// Subscribe implements session.Notifier. It returns nil once ctx is
// done. go-redis re-subscribes on its own after a dropped connection; a
// message published during the gap is lost, which the Notifier contract
// allows.
func (n *Notifier) Subscribe(ctx context.Context, fn func(tenantID string)) error {
	return n.subscribe(ctx, Channel, func(payload []byte) {
		var m message
		if err := json.Unmarshal(payload, &m); err != nil {
			return // not ours; ignore rather than fail the subscriber
		}
		if m.Origin == n.instance || m.Tenant == "" {
			return
		}
		fn(m.Tenant)
	})
}

// PublishSession implements session.Notifier.
func (n *Notifier) PublishSession(ctx context.Context, msg session.SessionMessage) error {
	if err := msg.Validate(); err != nil {
		return fmt.Errorf("redis: PublishSession: %w", err)
	}
	return n.publish(ctx, SessionChannel, sessionEnvelope{Origin: n.instance, SessionMessage: msg})
}

// SubscribeSessions implements session.Notifier, on the same terms as
// Subscribe.
func (n *Notifier) SubscribeSessions(ctx context.Context, fn func(session.SessionMessage)) error {
	return n.subscribe(ctx, SessionChannel, func(payload []byte) {
		var m sessionEnvelope
		if err := json.Unmarshal(payload, &m); err != nil {
			return
		}
		if m.Origin == n.instance || m.Validate() != nil {
			return
		}
		fn(m.SessionMessage)
	})
}

func (n *Notifier) publish(ctx context.Context, channel string, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("redis: encode %s message: %w", channel, err)
	}
	if err := n.conn.client.Publish(ctx, channel, payload).Err(); err != nil {
		return fmt.Errorf("redis: publish %s: %w", channel, err)
	}
	return nil
}

// subscribe holds a subscription to channel, handing every payload to
// fn, until ctx is done.
func (n *Notifier) subscribe(ctx context.Context, channel string, fn func(payload []byte)) error {
	ps := n.conn.client.Subscribe(ctx, channel)
	defer func() { _ = ps.Close() }()

	// Wait for Redis to confirm the subscription, so a publish that
	// follows a successful return from here is not missed.
	if _, err := ps.Receive(ctx); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("redis: subscribe %s: %w", channel, err)
	}

	msgs := ps.Channel()
	for {
		select {
		case <-ctx.Done():
			return nil
		case msg, ok := <-msgs:
			if !ok {
				if ctx.Err() != nil {
					return nil
				}
				return fmt.Errorf("redis: subscription to %s closed", channel)
			}
			fn([]byte(msg.Payload))
		}
	}
}

// Close implements session.Notifier.
func (n *Notifier) Close() error { return n.conn.release() }
