// Package detectionevents posts the gateway's events -- what it did that
// is not a model call -- to the detection agent next to it
// (llm_proxy.detection.agent_url): every MCP tools/call with the caller's
// profile outcome, and every request refused because the caller's key is
// suspended, revoked or expired. The agent redacts and forwards them to its
// engine. Like the LLM plane's detection tee, nothing on the request path
// waits for the agent: Emit only queues, a full queue drops (counted), and
// a batch the agent does not take is dropped and logged.
//
// Wire contract (the agent mirrors Event field for field):
//
//	POST {agent}/v1/events   {"events":[Event...]} -> 202
package detectionevents

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Event kinds and tool-call outcomes.
const (
	KindToolCall = "tool_call"
	KindRefused  = "refused"

	OutcomeAllowed    = "allowed"
	OutcomeMonitored  = "monitored"  // granted with permission "monitor": runs, and is reported
	OutcomeDenied     = "denied"     // the profile does not grant the tool
	OutcomeRestricted = "restricted" // the key is restricted: every tool call is refused
	OutcomeRefused    = "refused"    // the key is suspended, revoked or expired
)

// version is an event's "v".
const version = 1

// maxBatch bounds one post (the agent refuses a larger batch).
const maxBatch = 500

// MaxArguments bounds Event.Arguments; a longer value is cut.
const MaxArguments = 16 << 10

// Event is one thing the gateway did.
type Event struct {
	V         int       `json:"v,omitempty"`
	Kind      string    `json:"kind"`
	ID        string    `json:"id,omitempty"`
	TenantID  string    `json:"tenant_id"`
	SessionID string    `json:"session_id,omitempty"`
	KeyID     string    `json:"key_id,omitempty"`
	Principal string    `json:"principal,omitempty"`
	At        time.Time `json:"at"`
	Plane     string    `json:"plane"`

	Profile     string `json:"profile,omitempty"`
	ConnectorID string `json:"connector_id,omitempty"`
	Server      string `json:"server,omitempty"`
	Tool        string `json:"tool,omitempty"`
	Outcome     string `json:"outcome,omitempty"`
	Arguments   string `json:"arguments,omitempty"`

	Reason string `json:"reason,omitempty"`
	Detail string `json:"detail,omitempty"`
}

type batch struct {
	Events []Event `json:"events"`
}

// Config tunes an Emitter; a field <= 0 takes its default.
type Config struct {
	AgentURL  string
	Timeout   time.Duration // one post; default 5s
	QueueSize int           // events waiting; default 4096
	// FlushInterval is how long a partial batch waits for more events;
	// default 200ms.
	FlushInterval time.Duration
}

// Emitter queues events and posts them in batches. A nil *Emitter is a
// valid no-op, so callers need no nil checks.
type Emitter struct {
	url     string
	timeout time.Duration
	flush   time.Duration
	client  *http.Client
	queue   chan Event

	mu     sync.RWMutex
	closed bool
	done   chan struct{}
	ctx    context.Context
	cancel context.CancelFunc

	sent, dropped, failed atomic.Uint64
	lastLog               atomic.Int64
}

// New starts an Emitter posting to cfg.AgentURL. Call Close to drain it.
func New(cfg Config) *Emitter {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Second
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 4096
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = 200 * time.Millisecond
	}
	// A local sidecar: no proxy (the events never leave the host through
	// HTTP(S)_PROXY) and no redirects.
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	ctx, cancel := context.WithCancel(context.Background())
	e := &Emitter{
		url:     strings.TrimRight(cfg.AgentURL, "/"),
		timeout: cfg.Timeout,
		flush:   cfg.FlushInterval,
		client: &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
		queue:  make(chan Event, cfg.QueueSize),
		done:   make(chan struct{}),
		ctx:    ctx,
		cancel: cancel,
	}
	go e.run()
	return e
}

// Emit queues ev without blocking; a full queue or a closed Emitter drops
// it. Safe on a nil Emitter.
func (e *Emitter) Emit(ev Event) {
	if e == nil {
		return
	}
	ev.V = version
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	if len(ev.Arguments) > MaxArguments {
		ev.Arguments = ev.Arguments[:MaxArguments]
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.closed {
		e.drop("shutting down")
		return
	}
	select {
	case e.queue <- ev:
	default:
		e.drop("queue full")
	}
}

func (e *Emitter) run() {
	defer close(e.done)
	var buf []Event
	tick := time.NewTicker(e.flush)
	defer tick.Stop()
	for {
		select {
		case ev, ok := <-e.queue:
			if !ok {
				e.post(buf)
				return
			}
			buf = append(buf, ev)
			if len(buf) >= maxBatch {
				e.post(buf)
				buf = nil
			}
		case <-tick.C:
			if len(buf) > 0 {
				e.post(buf)
				buf = nil
			}
		}
	}
}

func (e *Emitter) post(evs []Event) {
	if len(evs) == 0 {
		return
	}
	if e.ctx.Err() != nil {
		e.dropped.Add(uint64(len(evs)))
		return
	}
	b, err := json.Marshal(batch{Events: evs})
	if err != nil {
		e.fail(len(evs), err)
		return
	}
	ctx, cancel := context.WithTimeout(e.ctx, e.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.url+"/v1/events", bytes.NewReader(b))
	if err != nil {
		e.fail(len(evs), err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.client.Do(req)
	if err != nil {
		e.fail(len(evs), err)
		return
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		e.fail(len(evs), fmt.Errorf("status %d", resp.StatusCode))
		return
	}
	e.sent.Add(uint64(len(evs)))
}

func (e *Emitter) drop(reason string) {
	n := e.dropped.Add(1)
	e.logOnce("detection events: event dropped", "reason", reason, "dropped_total", n)
}

func (e *Emitter) fail(n int, err error) {
	t := e.failed.Add(uint64(n))
	e.logOnce("detection events: agent did not take a batch", "events", n, "error", err, "failed_total", t)
}

// logOnce logs at most once a minute.
func (e *Emitter) logOnce(msg string, args ...any) {
	now := time.Now().Unix()
	last := e.lastLog.Load()
	if now-last < 60 || !e.lastLog.CompareAndSwap(last, now) {
		return
	}
	slog.Warn(msg+" (logged at most once a minute)", args...)
}

// Close stops taking events and posts what is queued until ctx is done;
// then it drops the rest. Safe on a nil Emitter and to call twice.
func (e *Emitter) Close(ctx context.Context) {
	if e == nil {
		return
	}
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	e.closed = true
	close(e.queue)
	e.mu.Unlock()
	select {
	case <-e.done:
	case <-ctx.Done():
		e.cancel()
		<-e.done
	}
	e.cancel()
}

// Status is the emitter's /health entry.
func (e *Emitter) Status() map[string]any {
	if e == nil {
		return map[string]any{"enabled": false}
	}
	return map[string]any{"enabled": true, "sent": e.sent.Load(), "dropped": e.dropped.Load(),
		"failed": e.failed.Load(), "queued": len(e.queue)}
}
