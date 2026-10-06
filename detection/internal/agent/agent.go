// Package agent is the detection agent: the sidecar next to the gateway that
// prepares each call on the customer's host (new turn extracted, secrets
// found and redacted) and hands it to the detection engine.
//
// The gateway's tee posts each completed call to POST /v1/turns; that is
// detection only, always judged in the background: queued up to queue_size
// turns and queue_bytes of raw turn bytes, judged at most max_in_flight at a
// time. The older inline pair (/v1/turns/request, /v1/turns/response) is
// kept for a gateway that holds a call for a verdict: there the engine's
// /v1/policy can ask for a request to be judged before forwarding.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/detection/turn"
	"github.com/Tuskira/tusk-ai-secured-gateway/detection/wire"
)

// errJudgingBusy is the detail of a call left unjudged because the queue is
// full; the engine records the turn as an error, as the gateway did
// before.
const errJudgingBusy = "not judged: rule engine at max_in_flight"

const (
	// backgroundTimeout bounds one background judgment, as in the gateway.
	backgroundTimeout = 30 * time.Second
	// markerTimeout bounds the not-judged marker: it is sent on the handler's
	// goroutine (no goroutine the queue would not bound), so it must be short.
	markerTimeout = 2 * time.Second
	// policyTimeout bounds a policy lookup on the request path.
	policyTimeout = time.Second
	// maxTurnBytes bounds a gateway turn: a base64 request body plus the
	// 1 MiB response copy.
	maxTurnBytes = 64 << 20
)

// Config is the agent's configuration; zero values take the defaults.
type Config struct {
	EngineURL     string
	Token         string
	MaxInFlight   int           // background judgments at once; default 256
	QueueSize     int           // background turns waiting for a judgment; default 1024
	QueueBytes    int           // raw request+response bytes of the queued turns; default 256 MiB
	EngineTimeout time.Duration // inline judgment budget; default 9s
	PolicyTTL     time.Duration // per-tenant policy cache; default 15s
	// PrepareTimeout bounds preparing one turn (extraction and the secret
	// scan: linear in the body, the scan bounded by windows; only a hostile
	// body gets near it). Past it the turn is sent as not judged, with the
	// rest of the stage's budget left to send it. Default 4s.
	PrepareTimeout time.Duration
}

// Agent serves the gateway's turns and talks to the engine.
type Agent struct {
	cfg    Config
	client *http.Client
	// queue is the local buffer of background turns, drained by MaxInFlight
	// workers. It holds each turn as the gateway sent it (preparing is the
	// worker's job), so it is bounded by bytes (queued, QueueBytes) as well
	// as by count: 1024 large turns would not fit a sidecar's memory.
	queue   chan job
	queued  atomic.Int64
	workers sync.WaitGroup
	stop    sync.Once

	mu       sync.Mutex
	policies map[string]cachedPolicy
	now      func() time.Time
	// versionWarned is when (unix ns) the wire-version mismatch was last
	// logged: it fails every call until one side is upgraded, so it is
	// logged once a minute, not once per call.
	versionWarned atomic.Int64
}

// job is one background unit: the stages of one call (of a batch, one per
// request), judged in order by one worker.
type job struct {
	size   int64 // raw bytes held, counted in Agent.queued
	stages []stage
}

// stage is one judgment: prepare builds the turn to judge within ctx's
// deadline (false: nothing to judge), sent under the Meta meta returns.
type stage struct {
	prepare func(ctx context.Context) (turn.PreparedTurn, bool)
	meta    func() wire.Meta
}

// stageOf is a stage sent under m.
func stageOf(m wire.Meta, p func(context.Context) (turn.PreparedTurn, bool)) stage {
	return stage{prepare: p, meta: func() wire.Meta { return m }}
}

type cachedPolicy struct {
	p  wire.PolicyResponse
	at time.Time
}

// New returns an agent for cfg.
func New(cfg Config) *Agent {
	if cfg.MaxInFlight <= 0 {
		cfg.MaxInFlight = 256
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 1024
	}
	if cfg.QueueBytes <= 0 {
		cfg.QueueBytes = 256 << 20
	}
	if cfg.EngineTimeout <= 0 {
		cfg.EngineTimeout = 9 * time.Second
	}
	if cfg.PolicyTTL <= 0 {
		cfg.PolicyTTL = 15 * time.Second
	}
	if cfg.PrepareTimeout <= 0 {
		cfg.PrepareTimeout = 4 * time.Second
	}
	cfg.EngineURL = strings.TrimRight(cfg.EngineURL, "/")
	// Go's default transport keeps 2 idle connections per host, so under load
	// nearly every judgment would pay a new TCP+TLS handshake to the engine.
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConns = cfg.MaxInFlight
	t.MaxIdleConnsPerHost = cfg.MaxInFlight
	a := &Agent{
		cfg:      cfg,
		client:   &http.Client{Transport: t},
		queue:    make(chan job, cfg.QueueSize),
		policies: map[string]cachedPolicy{},
		now:      time.Now,
	}
	for range cfg.MaxInFlight {
		a.workers.Go(func() {
			for j := range a.queue {
				a.queued.Add(-j.size)
				a.judge(j)
			}
		})
	}
	return a
}

// Handler serves the gateway's turns and /healthz.
func (a *Agent) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+wire.PathTurns, a.turns)
	mux.HandleFunc("POST "+wire.PathTurnRequest, a.turnRequest)
	mux.HandleFunc("POST "+wire.PathTurnResponse, a.turnResponse)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		answer(w, http.StatusOK, []byte("ok\n"))
	})
	return mux
}

// Wait judges what is left in the queue, then stops the workers. Call it
// once the handler has stopped serving: a turn after it would panic.
func (a *Agent) Wait() {
	a.stop.Do(func() { close(a.queue) })
	a.workers.Wait()
}

// Queued is how many turns wait in the queue.
func (a *Agent) Queued() int { return len(a.queue) }

// turns takes one complete turn from the gateway's detection tee. It answers
// 202 at once and always works in the background, whatever the engine's
// /v1/policy says (the tee cannot act on a verdict): one queued job prepares
// and sends the request stage, then the response stage when the turn carries
// a response, so the engine sees them in that order. Each stage is read from
// the turn's canonical conversation and answer when the gateway sent them
// (wire.Turn.Call), else from the raw bodies. A batch is judged item by
// item (batchStages); it has no response stage.
func (a *Agent) turns(w http.ResponseWriter, r *http.Request) {
	var t wire.Turn
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxTurnBytes)).Decode(&t); err != nil {
		http.Error(w, "agent: bad turn: "+err.Error(), http.StatusBadRequest)
		return
	}
	if t.V > wire.TurnVersion {
		http.Error(w, fmt.Sprintf("%s %d: this agent speaks %d", wire.UnsupportedVersion, t.V, wire.TurnVersion), http.StatusBadRequest)
		return
	}
	switch {
	case t.ID == "":
		http.Error(w, "agent: bad turn: id is required", http.StatusBadRequest)
		return
	case t.TenantID == "":
		http.Error(w, "agent: bad turn: tenant_id is required", http.StatusBadRequest)
		return
	case t.At.IsZero():
		http.Error(w, "agent: bad turn: at is required", http.StatusBadRequest)
		return
	case len(t.Request) == 0:
		http.Error(w, "agent: bad turn: request is required", http.StatusBadRequest)
		return
	}
	m := wire.MetaOfTurn(t)
	var stages []stage
	if len(t.Items) > 0 {
		stages = batchStages(t, m)
	} else {
		// The canonical conversation and answer when the gateway read them,
		// else the raw bodies; secrets are searched in the raw bodies
		// either way.
		call := t.Call()
		if call.Conversation != nil {
			m.History = call.Conversation.History
		}
		stages = append(stages, stageOf(m, func(ctx context.Context) (turn.PreparedTurn, bool) {
			return turn.PrepareCallRequest(ctx, call), true
		}))
		if len(t.Response) > 0 && t.Op != wire.OpBatch { // a batch's response is the batch object
			stages = append(stages, stageOf(m, func(ctx context.Context) (turn.PreparedTurn, bool) {
				return turn.PrepareCallResponse(ctx, call)
			}))
		}
	}
	queued := a.background(len(t.Request)+len(t.Response), stages...)
	answer(w, http.StatusAccepted, nil)
	if !queued {
		a.notJudged(m, turn.StageRequest)
	}
}

// batchStages is one request stage per item of a batch, the first
// wire.MaxBatchItems, each sent under the batch call's id with the item's
// id in Meta.Item and prepared from the item's conversation, with every
// value found anywhere in the raw batch body removed (turn.Batch). Items
// past the cap, or a batch the gateway read only in part (NormalizeError),
// add one not-judged request stage for the whole call, with the reason.
func batchStages(t wire.Turn, m wire.Meta) []stage {
	b := turn.NewBatch(t.Request)
	items := t.Items[:min(len(t.Items), wire.MaxBatchItems)]
	stages := make([]stage, 0, len(items)+1)
	for _, it := range items {
		var id string // set by prepare, read by meta after it
		stages = append(stages, stage{
			prepare: func(ctx context.Context) (turn.PreparedTurn, bool) {
				pt := b.PrepareItem(ctx, &it.Conversation)
				id = b.ItemID(it.CustomID)
				return pt, true
			},
			meta: func() wire.Meta {
				im := m
				im.Item, im.History = id, it.Conversation.History
				return im
			},
		})
	}
	var reason string
	switch {
	case len(t.Items) > wire.MaxBatchItems:
		reason = fmt.Sprintf("not judged: %d batch requests past the first %d", len(t.Items)-wire.MaxBatchItems, wire.MaxBatchItems)
	case t.NormalizeError != "":
		reason = "not judged: " + t.NormalizeError
	}
	if reason != "" {
		stages = append(stages, stageOf(m, func(context.Context) (turn.PreparedTurn, bool) {
			return turn.NotJudgedTurn(turn.StageRequest, reason), true
		}))
	}
	return stages
}

func (a *Agent) turnRequest(w http.ResponseWriter, r *http.Request) {
	t, ok := decodeTurn(w, r)
	if !ok {
		return
	}
	// The policy lookup (a cache miss) gets its own short budget so the whole
	// EngineTimeout is left for the judgment: together they stay inside the
	// gateway's agent_timeout (policyTimeout + EngineTimeout = 10s).
	p := a.policy(r.Context(), t.TenantID)
	v := wire.Verdict{WantResponse: p.WantResponse}
	m := wire.MetaOf(t)
	if p.Inline {
		ctx, cancel := context.WithTimeout(r.Context(), a.cfg.EngineTimeout)
		defer cancel()
		v.Block = a.judgeInline(ctx, m, t.Request)
		answerJSON(w, v)
		return
	}
	queued := a.background(len(t.Request), stageOf(m, func(ctx context.Context) (turn.PreparedTurn, bool) {
		return turn.PrepareRequestContext(ctx, t.Request), true
	}))
	answerJSON(w, v)
	if !queued {
		a.notJudged(m, turn.StageRequest)
	}
}

func (a *Agent) turnResponse(w http.ResponseWriter, r *http.Request) {
	t, ok := decodeTurn(w, r)
	if !ok {
		return
	}
	m := wire.MetaOf(t)
	queued := a.background(len(t.Request)+len(t.Response), stageOf(m, func(ctx context.Context) (turn.PreparedTurn, bool) {
		return turn.PrepareResponseContext(ctx, t.Request, t.Response)
	}))
	answer(w, http.StatusAccepted, nil)
	if !queued {
		a.notJudged(m, turn.StageResponse)
	}
}

func decodeTurn(w http.ResponseWriter, r *http.Request) (wire.TurnRequest, bool) {
	var t wire.TurnRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxTurnBytes)).Decode(&t); err != nil {
		http.Error(w, "agent: bad turn: "+err.Error(), http.StatusBadRequest)
		return t, false
	}
	return t, true
}

// judgeInline judges a request before it is forwarded. Any engine failure
// fails open: the call goes through unjudged rather than being refused.
func (a *Agent) judgeInline(ctx context.Context, m wire.Meta, body []byte) *wire.Block {
	pt, _ := a.prepare(ctx, m, func(ctx context.Context) (turn.PreparedTurn, bool) {
		return turn.PrepareRequestContext(ctx, body), true
	})
	res, err := a.detect(ctx, wire.DetectRequest{Sync: true, Meta: m, Turn: pt})
	if err != nil {
		a.warn("agent: inline judgment failed; failing open", err, "request_id", m.RequestID, "tenant", m.TenantID)
		return nil
	}
	if res == nil {
		return nil
	}
	return res.Blocking
}

// background queues the stages of one call, as one job of size raw bytes,
// to be judged off the request path. When the queue is full (QueueSize
// jobs, or QueueBytes of them) it reports false and the caller sends the
// not-judged marker once it has answered. Preparing runs in the worker: it
// scans for secrets, which is the costly part.
func (a *Agent) background(size int, stages ...stage) bool {
	n := int64(size)
	if a.queued.Add(n) > int64(a.cfg.QueueBytes) {
		a.queued.Add(-n)
		return false
	}
	select {
	case a.queue <- job{size: n, stages: stages}:
		return true
	default:
		a.queued.Add(-n)
		return false
	}
}

// judge sends the job's stages one after the other; a stage that fails does
// not stop the next.
func (a *Agent) judge(j job) {
	for _, st := range j.stages {
		func() {
			ctx, cancel := context.WithTimeout(context.Background(), backgroundTimeout)
			defer cancel()
			pt, ok := a.prepare(ctx, st.meta(), st.prepare)
			if !ok {
				return
			}
			m := st.meta()
			if _, err := a.detect(ctx, wire.DetectRequest{Meta: m, Turn: pt}); err != nil {
				a.warn("agent: background judgment failed", err, "request_id", m.RequestID, "tenant", m.TenantID, "stage", pt.Stage)
			}
		}()
	}
}

// prepare runs p within PrepareTimeout of ctx. A turn the deadline cut
// short comes back as not judged (turn.DeadlineReason), never half
// redacted; it is sent like that and logged here.
func (a *Agent) prepare(ctx context.Context, m wire.Meta, p func(context.Context) (turn.PreparedTurn, bool)) (turn.PreparedTurn, bool) {
	pctx, cancel := context.WithTimeout(ctx, a.cfg.PrepareTimeout)
	defer cancel()
	pt, ok := p(pctx)
	if ok && pt.NotJudged == turn.DeadlineReason {
		slog.Warn("agent: preparing a turn ran past its deadline; sending it as not judged",
			"request_id", m.RequestID, "tenant", m.TenantID, "stage", pt.Stage)
	}
	return pt, ok
}

func (a *Agent) notJudged(m wire.Meta, st turn.Stage) {
	ctx, cancel := context.WithTimeout(context.Background(), markerTimeout)
	defer cancel()
	if _, err := a.detect(ctx, wire.DetectRequest{Meta: m, Turn: turn.NotJudgedTurn(st, errJudgingBusy)}); err != nil {
		a.warn("agent: not-judged marker not sent", err, "request_id", m.RequestID, "tenant", m.TenantID, "stage", st)
	}
}

// policy is the tenant's routing, cached for PolicyTTL. On an engine error it
// keeps the last value (or, never fetched, judges in the background and asks
// for responses) and holds it for a TTL too.
// Caching the error means a down engine costs one lookup per tenant per TTL,
// not one per call on the request path.
func (a *Agent) policy(ctx context.Context, tenantID string) wire.PolicyResponse {
	a.mu.Lock()
	hit, ok := a.policies[tenantID]
	a.mu.Unlock()
	if ok && a.now().Sub(hit.at) < a.cfg.PolicyTTL {
		return hit.p
	}
	ctx, cancel := context.WithTimeout(ctx, policyTimeout)
	defer cancel()
	var p wire.PolicyResponse
	if err := a.call(ctx, http.MethodGet, wire.PathPolicy+"?tenant_id="+url.QueryEscape(tenantID), nil, &p); err != nil {
		slog.Warn("agent: tenant policy lookup failed; using last known", "tenant", tenantID, "error", err)
		p = wire.PolicyResponse{WantResponse: true}
		if ok {
			p = hit.p
		}
	}
	a.mu.Lock()
	a.policies[tenantID] = cachedPolicy{p: p, at: a.now()}
	a.mu.Unlock()
	return p
}

// warn logs a failed call to the engine. The wire-version mismatch is
// logged at most once a minute.
func (a *Agent) warn(msg string, err error, args ...any) {
	if errors.Is(err, errEngineVersion) {
		now, last := a.now().UnixNano(), a.versionWarned.Load()
		if last != 0 && now-last < int64(time.Minute) || !a.versionWarned.CompareAndSwap(last, now) {
			return
		}
	}
	slog.Warn(msg, append(args, "error", err)...)
}

// errEngineVersion is a 400 from an engine that does not speak this
// agent's wire version.
var errEngineVersion = errors.New("the engine does not accept wire version")

func (a *Agent) detect(ctx context.Context, req wire.DetectRequest) (*wire.Judgment, error) {
	req.V = wire.Version
	var out wire.DetectResponse
	err := a.call(ctx, http.MethodPost, wire.PathDetect, req, &out)
	return out.Result, err
}

// call sends one JSON request to the engine and decodes a 200 answer.
func (a *Agent) call(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("agent: marshal: %w", err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.cfg.EngineURL+path, body)
	if err != nil {
		return fmt.Errorf("agent: %s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+a.cfg.Token)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("agent: %s %s: %w", method, path, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body) // drain, so the connection is reused
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		if resp.StatusCode == http.StatusBadRequest && bytes.HasPrefix(msg, []byte(wire.UnsupportedVersion)) {
			// Every call fails (open) until one side is upgraded: say so.
			return fmt.Errorf("agent: %s %s: %w %d; upgrade the engine (calls fail open until then): %s",
				method, path, errEngineVersion, wire.Version, bytes.TrimSpace(msg))
		}
		return fmt.Errorf("agent: %s %s: %s: %s", method, path, resp.Status, bytes.TrimSpace(msg))
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("agent: %s %s: decode: %w", method, path, err)
	}
	return nil
}

func answerJSON(w http.ResponseWriter, v any) {
	b, _ := json.Marshal(v) // a Verdict always marshals
	w.Header().Set("Content-Type", "application/json")
	answer(w, http.StatusOK, b)
}

// answer writes a complete response and flushes it, so the gateway has its
// answer even while the handler goes on to send a not-judged marker.
func answer(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
	_ = http.NewResponseController(w).Flush()
}
