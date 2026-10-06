package llmplane

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DetectionTee hands each relayed call to a detection agent running next to
// the gateway (llm_proxy.detection.agent_url): one complete turn, posted
// after the call completes, off the request path. Detection only: nothing
// on the request path waits for the agent and nothing is ever blocked. A
// turn that cannot be queued or posted is dropped, counted and logged.
//
// It sits beside the Recorder, not behind the pkg/sink.LogSink seam: a sink
// only sees the LLMCall record, whose bodies are cut to the capture cap (and
// absent when capture.store_bodies is off), while the agent needs the full
// request body the router already holds in memory.
//
// Wire contract (the agent mirrors teeTurn field for field):
//
//	POST {agent}/v1/turns   teeTurn -> 202
type DetectionTee struct {
	url      string
	timeout  time.Duration
	client   *http.Client
	queue    chan *teeTurn
	maxBytes int64
	// held is the request+response bytes of turns waiting or being posted.
	held atomic.Int64

	// mu guards closed, so offer never sends on the closed queue.
	mu     sync.RWMutex
	closed bool
	// ctx is cancelled when Close stops draining: posts in flight abort
	// and the workers drop what is left.
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	sent, dropped, failed atomic.Uint64
	dropLog, failLog      rateLog
}

// DetectionTeeConfig is the resolved llm_proxy.detection block. A field
// <= 0 takes the config default.
type DetectionTeeConfig struct {
	AgentURL    string
	Timeout     time.Duration // one post; default 5s
	QueueSize   int           // turns waiting; default 1024
	QueueBytes  int64         // bytes held, waiting or being posted; default 256 MiB
	MaxInFlight int           // concurrent posts; default 8
}

// teeVersion is the turn's "v": bumped on a breaking contract change.
const teeVersion = 1

// teeResponseBytes bounds the response copy sent to the agent.
const teeResponseBytes = 1 << 20

// teeTurn is one complete call. The id is the gateway request id, which also
// keys the call's LLM log. Response is the first teeResponseBytes the client
// received, present only when the upstream answered 2xx and the relay to the
// client completed.
type teeTurn struct {
	V          int       `json:"v"`
	ID         string    `json:"id"`
	TenantID   string    `json:"tenant_id"`
	SessionID  string    `json:"session_id,omitempty"`
	KeyID      string    `json:"key_id,omitempty"`
	Principal  string    `json:"principal,omitempty"`
	Model      string    `json:"model,omitempty"`
	Path       string    `json:"path"`
	At         time.Time `json:"at"`
	StatusCode int       `json:"status_code"`
	Request    []byte    `json:"request"`
	Response   []byte    `json:"response,omitempty"`
}

func (t *teeTurn) size() int64 { return int64(len(t.Request) + len(t.Response)) }

// NewDetectionTee starts a tee posting to the agent at cfg.AgentURL with
// cfg.MaxInFlight workers. Call Close to drain and stop it.
func NewDetectionTee(cfg DetectionTeeConfig) *DetectionTee {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Second
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 1024
	}
	if cfg.QueueBytes <= 0 {
		cfg.QueueBytes = 256 << 20
	}
	if cfg.MaxInFlight <= 0 {
		cfg.MaxInFlight = 8
	}
	// The agent is an operator-set local sidecar, so a plain client rather
	// than the SSRF-guarded upstream transport (which refuses loopback).
	// Proxy is nil so HTTP(S)_PROXY never carries the raw bodies off the
	// host, and a redirect is not followed: it would re-send them wherever
	// Location points.
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.MaxIdleConnsPerHost = cfg.MaxInFlight
	ctx, cancel := context.WithCancel(context.Background())
	d := &DetectionTee{
		url:     strings.TrimRight(cfg.AgentURL, "/"),
		timeout: cfg.Timeout,
		client: &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
		queue:    make(chan *teeTurn, cfg.QueueSize),
		maxBytes: cfg.QueueBytes,
		ctx:      ctx,
		cancel:   cancel,
	}
	d.wg.Add(cfg.MaxInFlight)
	for range cfg.MaxInFlight {
		go d.run()
	}
	return d
}

// teeable reports whether a call goes to the tee: every POST the plane
// relays except the utility endpoints (token counting, batch management),
// which run no generation. There is no per-route allow-list: the agent
// decides what it can read.
func teeable(r *http.Request) bool {
	return r.Method == http.MethodPost && !utilityPath(r.URL.Path)
}

// utilityPath reports the endpoints that run no generation: token counting
// on each provider and Anthropic's Message Batches management/results API.
// The batches match is on whole path segments, so a path merely prefixed by
// it (/v1/messages/batches-export) still counts as a call.
func utilityPath(path string) bool {
	return strings.HasSuffix(path, "/count_tokens") || // Anthropic
		strings.HasSuffix(path, "/count-tokens") || // Bedrock CountTokens
		strings.HasSuffix(path, ":countTokens") || // Gemini
		strings.HasSuffix(path, "/input_tokens") || // OpenAI Responses input-token count
		strings.HasSuffix(path, "/messages/batches") || // Anthropic Message Batches: the collection
		strings.Contains(path, "/messages/batches/") // ... and anything under one batch
}

func teeTurnOf(r *http.Request, info callInfo, start time.Time, status int, reqBody, respBody []byte) *teeTurn {
	ctx := r.Context()
	return &teeTurn{
		V: teeVersion, ID: requestIDOf(ctx), TenantID: tenantOf(ctx), SessionID: sessionOf(ctx), KeyID: keyIDOf(ctx),
		Principal: principalOf(ctx), Model: info.requestedModel, Path: r.URL.Path, At: start, StatusCode: status,
		Request: reqBody, Response: respBody,
	}
}

// offer queues t without blocking; a full queue (by count or bytes) or a
// closed tee drops it.
func (d *DetectionTee) offer(t *teeTurn) {
	n := t.size()
	if d.held.Add(n) > d.maxBytes {
		d.held.Add(-n)
		d.drop(t.ID, "queue_bytes")
		return
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		d.held.Add(-n)
		d.drop(t.ID, "shutting down")
		return
	}
	select {
	case d.queue <- t:
	default:
		d.held.Add(-n)
		d.drop(t.ID, "queue_size")
	}
}

// run is one worker: it posts queued turns until the queue is closed and
// empty. Once Close has given up draining, what is left is dropped.
func (d *DetectionTee) run() {
	defer d.wg.Done()
	for t := range d.queue {
		if d.ctx.Err() != nil {
			d.dropped.Add(1)
		} else {
			d.post(t)
		}
		d.held.Add(-t.size())
	}
}

func (d *DetectionTee) post(t *teeTurn) {
	b, err := json.Marshal(t)
	if err != nil {
		d.fail(t.ID, fmt.Errorf("marshal turn: %w", err))
		return
	}
	ctx, cancel := context.WithTimeout(d.ctx, d.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.url+"/v1/turns", bytes.NewReader(b))
	if err != nil {
		d.fail(t.ID, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.client.Do(req)
	if err != nil {
		if d.ctx.Err() != nil {
			d.dropped.Add(1) // aborted by Close: counted in its one log line
			return
		}
		d.fail(t.ID, err)
		return
	}
	drainClose(resp.Body)
	if resp.StatusCode != http.StatusAccepted {
		d.fail(t.ID, fmt.Errorf("status %d", resp.StatusCode))
		return
	}
	d.sent.Add(1)
}

func (d *DetectionTee) drop(id, reason string) {
	n := d.dropped.Add(1)
	if d.dropLog.allow(time.Now()) {
		slog.Warn("llmplane: detection tee dropped a turn (logged at most once a minute)",
			"request_id", id, "reason", reason, "dropped_total", n)
	}
}

func (d *DetectionTee) fail(id string, err error) {
	n := d.failed.Add(1)
	if d.failLog.allow(time.Now()) {
		slog.Warn("llmplane: detection agent did not take a turn (logged at most once a minute)",
			"request_id", id, "error", err, "failed_total", n)
	}
}

// Close stops taking turns and posts what is queued until ctx is done; then
// it aborts posts in flight and drops the rest, with one log line. Safe to
// call more than once.
func (d *DetectionTee) Close(ctx context.Context) {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return
	}
	d.closed = true
	close(d.queue)
	d.mu.Unlock()

	done := make(chan struct{})
	go func() { d.wg.Wait(); close(done) }()
	before := d.dropped.Load()
	select {
	case <-done:
	case <-ctx.Done():
		d.cancel()
		<-done
		slog.Warn("llmplane: detection tee stopped before its queue drained; turns dropped",
			"dropped", d.dropped.Load()-before)
	}
	d.cancel()
}

// Status is the tee's /health entry: turns posted, dropped (queue full,
// shutdown) and failed (agent error or non-202), and what is waiting now.
func (d *DetectionTee) Status() map[string]any {
	return map[string]any{
		"enabled": true, "sent": d.sent.Load(), "dropped": d.dropped.Load(), "failed": d.failed.Load(),
		"queued": len(d.queue), "held_bytes": d.held.Load(),
	}
}

// rateLog lets one log line through per minute.
type rateLog struct{ last atomic.Int64 }

func (l *rateLog) allow(now time.Time) bool {
	prev := l.last.Load()
	if prev != 0 && now.UnixNano()-prev < int64(time.Minute) {
		return false
	}
	return l.last.CompareAndSwap(prev, now.UnixNano())
}

// drainClose reads what is left of a body so its connection is reused.
func drainClose(b io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(b, 64<<10))
	_ = b.Close()
}
