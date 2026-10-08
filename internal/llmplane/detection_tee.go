package llmplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm"
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
	// held is the bytes of turns waiting (their raw bodies) or being posted
	// (the turn as posted: raw bodies and the canonical conversation).
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

// teeBatchItems bounds teeTurn.Items: the requests of a larger batch past
// it are not read (NormalizeError says so); the raw body is sent whole.
const teeBatchItems = 1000

// teeTurn is one complete call. The id is the gateway request id, which also
// keys the call's LLM log. Response is the first teeResponseBytes the client
// received, present only when the upstream answered 2xx and the relay to the
// client completed.
//
// Op and Dialect are the route's (RouteInfo). Conversation and Answer are
// the bodies read through the Dialect's llm.Reader, filled by the worker
// (normalize), off the request path; NormalizeError says why they are
// missing. The raw bodies are sent either way. A batch (Op routeBatch) has
// Items instead, each request of the batch read through the Dialect's
// llm.BatchReader, and never an Answer: its response is the batch object,
// which holds no generation.
type teeTurn struct {
	V              int              `json:"v"`
	ID             string           `json:"id"`
	TenantID       string           `json:"tenant_id"`
	SessionID      string           `json:"session_id,omitempty"`
	KeyID          string           `json:"key_id,omitempty"`
	Principal      string           `json:"principal,omitempty"`
	Model          string           `json:"model,omitempty"`
	Path           string           `json:"path"`
	At             time.Time        `json:"at"`
	StatusCode     int              `json:"status_code"`
	Request        []byte           `json:"request"`
	Response       []byte           `json:"response,omitempty"`
	Dialect        string           `json:"dialect,omitempty"`
	Op             string           `json:"op,omitempty"`
	Conversation   *teeConversation `json:"conversation,omitempty"`
	Answer         *teeAnswer       `json:"answer,omitempty"`
	NormalizeError string           `json:"normalize_error,omitempty"`
	Items          []teeTurnItem    `json:"items,omitempty"`

	// respType is the Content-Type the client got, and respCut whether the
	// response copy stopped at teeResponseBytes: how to read Response.
	respType string
	respCut  bool
	// held is what the turn counts in DetectionTee.held.
	held int64
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

// teed reports whether a call on route goes to the tee: one that runs a
// generation, alone or in a batch. Token counting, listing and management
// endpoints, and endpoints the provider does not describe, are not sent.
func teed(route RouteInfo) bool {
	return route.Op == routeGenerate || route.Op == routeBatch
}

// teeTurnOf is the turn for a call, without its response.
func teeTurnOf(r *http.Request, info callInfo, route RouteInfo, start time.Time, status int, reqBody []byte) *teeTurn {
	ctx := r.Context()
	return &teeTurn{
		V: teeVersion, ID: requestIDOf(ctx), TenantID: tenantOf(ctx), SessionID: sessionOf(ctx), KeyID: keyIDOf(ctx),
		Principal: principalOf(ctx), Model: info.requestedModel, Path: r.URL.Path, At: start, StatusCode: status,
		Request: reqBody, Dialect: route.Reader, Op: route.Op,
	}
}

// normalize reads the turn's bodies through its Dialect's Reader into
// Conversation and Answer. A body it cannot read leaves its field empty
// and says why in NormalizeError. A streamed response cut at
// teeResponseBytes (or before the model finished) is read up to the cut,
// with Answer.Truncated set.
func (t *teeTurn) normalize() {
	if t.Op == routeBatch {
		t.normalizeBatch()
		return
	}
	var rd llm.Reader
	if t.Op == routeGenerate && t.Dialect != "" {
		rd, _ = llm.ReaderByName(t.Dialect)
	}
	if rd == nil {
		name := t.Dialect
		if name == "" {
			name = "this route"
		}
		t.NormalizeError = "no reader for " + name
		return
	}
	var errs []string
	if req, err := rd.DecodeRequest(t.Request); err != nil {
		errs = append(errs, "decode request: "+err.Error())
	} else {
		c := toConversation(req, t.Dialect)
		t.Conversation = &c
	}
	if len(t.Response) > 0 {
		a, err := t.readAnswer(rd)
		if err != nil {
			errs = append(errs, "decode response: "+err.Error())
		}
		t.Answer = a
	}
	t.NormalizeError = strings.Join(errs, "; ")
}

// normalizeBatch reads a batch's requests through its Dialect's
// BatchReader into Items, the first teeBatchItems of them. Its response,
// the batch object, holds no generation: no Answer.
func (t *teeTurn) normalizeBatch() {
	var br llm.BatchReader
	if t.Dialect != "" {
		br, _ = llm.BatchReaderByName(t.Dialect)
	}
	if br == nil {
		name := t.Dialect
		if name == "" {
			name = "this route"
		}
		t.NormalizeError = "no reader for " + name
		return
	}
	items, err := br.DecodeBatch(t.Request)
	switch {
	case errors.Is(err, llm.ErrBatchFile):
		t.NormalizeError = llm.ErrBatchFile.Error()
		return
	case err != nil:
		t.NormalizeError = "decode request: " + err.Error()
		return
	}
	if len(items) > teeBatchItems {
		t.NormalizeError = fmt.Sprintf("batch of %d requests: only the first %d are read", len(items), teeBatchItems)
		items = items[:teeBatchItems]
	}
	t.Items = make([]teeTurnItem, 0, len(items))
	for _, it := range items {
		t.Items = append(t.Items, teeTurnItem{CustomID: it.CustomID, Conversation: toConversation(it.Request, t.Dialect)})
	}
}

// readAnswer reads Response: as a stream when the client got one (SSE; an
// AWS event stream, which the Bedrock Readers read as framed and which
// carries the Messages events of an Anthropic model on Bedrock's invoke
// route; a Gemini stream sent as a JSON array), else as one body.
func (t *teeTurn) readAnswer(rd llm.Reader) (*teeAnswer, error) {
	mt, _, _ := mime.ParseMediaType(t.respType)
	var src io.Reader
	switch {
	case mt == "text/event-stream":
		src = bytes.NewReader(t.Response)
	case mt == "application/vnd.amazon.eventstream" && t.Dialect == readerAnthropic:
		src = newEventStreamToSSE(bytes.NewReader(t.Response))
	case mt == "application/vnd.amazon.eventstream":
		src = bytes.NewReader(t.Response)
	case t.Dialect == readerGemini && bytes.HasPrefix(bytes.TrimLeft(t.Response, " \t\r\n"), []byte("[")):
		src = bytes.NewReader(t.Response)
	default:
		resp, err := rd.DecodeResponse(t.Response)
		if err != nil {
			if t.respCut {
				return &teeAnswer{Content: []teeBlock{}, Truncated: true}, err
			}
			return nil, err
		}
		a := toAnswer(resp, false)
		return &a, nil
	}
	dec := rd.NewResponseDecoder(src)
	var evs []llm.Event
	for {
		ev, err := dec.Next()
		if err == nil {
			evs = append(evs, ev)
			continue
		}
		cut := errors.Is(err, io.ErrUnexpectedEOF)
		if !cut && !errors.Is(err, io.EOF) {
			if !t.respCut {
				return nil, err
			}
			cut = true // the copy ended mid-frame
		}
		a := toAnswer(foldEvents(evs), cut)
		return &a, nil
	}
}

// offer queues t without blocking; a full queue (by count or bytes) or a
// closed tee drops it.
func (d *DetectionTee) offer(t *teeTurn) {
	n := t.size()
	t.held = n
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
		d.held.Add(-t.held)
	}
}

func (d *DetectionTee) post(t *teeTurn) {
	t.normalize()
	b, err := json.Marshal(t)
	if err != nil {
		d.fail(t.ID, fmt.Errorf("marshal turn: %w", err))
		return
	}
	// queue_bytes counts what is posted: the raw bodies (base64) and the
	// canonical conversation. A turn that no longer fits is dropped.
	if grow := int64(len(b)) - t.held; grow > 0 {
		if d.held.Add(grow) > d.maxBytes {
			d.held.Add(-grow)
			d.drop(t.ID, "queue_bytes")
			return
		}
		t.held += grow
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

// Sent is how many turns were posted to the detection agent and accepted,
// since process start.
func (d *DetectionTee) Sent() uint64 { return d.sent.Load() }

// Dropped is how many turns were discarded (queue full, byte budget,
// shutdown) since process start.
func (d *DetectionTee) Dropped() uint64 { return d.dropped.Load() }

// Failed is how many turns the agent refused or could not be reached for
// (error or non-202) since process start.
func (d *DetectionTee) Failed() uint64 { return d.failed.Load() }

// QueueDepth is how many turns are waiting to be posted right now.
func (d *DetectionTee) QueueDepth() int { return len(d.queue) }

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
