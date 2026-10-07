// Package agent is the detection agent: the sidecar next to the gateway that
// prepares each call on the customer's host (new turn extracted, secrets
// found and redacted) and hands it to the detection engine.
//
// The gateway's tee posts each completed call to POST /v1/turns; that is
// detection only, always judged in the background: queued up to queue_size
// turns and queue_bytes of what they hold (turnBytes), judged at most max_in_flight at a
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
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/Tuskira/tusk-ai-secured-gateway/detection/turn"
	"github.com/Tuskira/tusk-ai-secured-gateway/detection/wire"
)

// errJudgingBusy is the detail of a call left unjudged because the queue is
// full; the engine records the turn as an error, as the gateway did
// before.
const errJudgingBusy = "not judged: rule engine at max_in_flight"

// errPrepareFailed is the detail of a stage whose preparation panicked: a
// bug in the agent, logged at Error, never the panic's value (it may quote
// the turn).
const errPrepareFailed = "not judged: the agent failed preparing the turn"

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
	EngineURL   string
	Token       string
	MaxInFlight int // background judgments at once; default 256
	QueueSize   int // background turns waiting for a judgment; default 1024
	QueueBytes  int // bytes the queued turns hold (turnBytes); default 256 MiB
	// EngineTimeout is the inline judgment budget; default 8s, so the policy
	// lookup (1s) and it fit a gateway's 10s wait with a second to spare.
	EngineTimeout time.Duration
	PolicyTTL     time.Duration // per-tenant policy cache; default 15s
	// PrepareTimeout bounds preparing one turn (extraction and the secret
	// scan of every byte of every string: linear in the body; only a huge or
	// hostile body gets near it). Past it the turn is sent as not judged,
	// with the rest of the stage's budget left to send it. Default 4s.
	PrepareTimeout time.Duration
	// ScanCacheBytes is the budget of the secret-scan cache
	// (turn.SetScanCacheBytes). The cache is process-wide, so the command
	// applies it, not New; default turn.DefaultScanCacheBytes (64 MiB).
	ScanCacheBytes int
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

	// inline bounds the inline judgments (POST /v1/turns/request for an
	// inline tenant) running at once, MaxInFlight: each prepares on the
	// handler's goroutine, which no queue bounds.
	inline chan struct{}

	mu       sync.Mutex
	policies map[string]cachedPolicy
	now      func() time.Time
	// warned is when (unix ns) a failed engine call of each kind
	// (engineErrKind) was last logged: a down engine fails every call, so
	// each kind is logged once a minute, not once per call.
	warnMu sync.Mutex
	warned map[string]int64
	// stats are the counts /healthz reports.
	stats stats
	// prepareRequest prepares an inline request (a test replaces it).
	prepareRequest func(context.Context, []byte) turn.PreparedTurn
}

// stats are the agent's counts since it started, served on /healthz:
// stages the engine accepted judged, not-judged turns it accepted (a
// deadline, a panic, the queue-full marker, a batch past its cap), stages
// dropped because the engine could not be reached or failed, and turns
// shed because the queue was full.
type stats struct {
	judged            atomic.Int64
	notJudgedSent     atomic.Int64
	droppedEngineDown atomic.Int64
	droppedQueueFull  atomic.Int64
}

// Health is the JSON of GET /healthz.
type Health struct {
	Status            string `json:"status"`
	Judged            int64  `json:"judged"`
	DroppedEngineDown int64  `json:"dropped_engine_down"`
	DroppedQueueFull  int64  `json:"dropped_queue_full"`
	NotJudgedSent     int64  `json:"not_judged_sent"`
}

// Health is the agent's counts as /healthz serves them.
func (a *Agent) Health() Health {
	return Health{Status: "ok", Judged: a.stats.judged.Load(), DroppedEngineDown: a.stats.droppedEngineDown.Load(),
		DroppedQueueFull: a.stats.droppedQueueFull.Load(), NotJudgedSent: a.stats.notJudgedSent.Load()}
}

// sent counts a turn the engine accepted.
func (a *Agent) sent(pt turn.PreparedTurn) {
	if pt.NotJudged != "" {
		a.stats.notJudgedSent.Add(1)
	} else {
		a.stats.judged.Add(1)
	}
}

// job is one background unit: the stages of one call (of a batch, one per
// request), judged in order by one worker.
type job struct {
	size   int64 // bytes held (turnBytes), counted in Agent.queued
	stages []stage
}

// stage is one judgment of stage st: prepare builds the turn to judge
// within ctx's deadline (false: nothing to judge), sent under the Meta
// meta returns.
type stage struct {
	st      turn.Stage
	prepare func(ctx context.Context) (turn.PreparedTurn, bool)
	meta    func() wire.Meta
}

// stageOf is a stage st sent under m.
func stageOf(m wire.Meta, st turn.Stage, p func(context.Context) (turn.PreparedTurn, bool)) stage {
	return stage{st: st, prepare: p, meta: func() wire.Meta { return m }}
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
		cfg.EngineTimeout = 8 * time.Second
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
		inline:   make(chan struct{}, cfg.MaxInFlight),
		policies: map[string]cachedPolicy{},
		warned:   map[string]int64{},
		now:      time.Now,

		prepareRequest: turn.PrepareRequestContext,
	}
	for range cfg.MaxInFlight {
		a.workers.Go(func() {
			for j := range a.queue {
				a.judge(j)
			}
		})
	}
	return a
}

// Handler serves the gateway's turns and /healthz (Health, as JSON).
func (a *Agent) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+wire.PathTurns, a.turns)
	mux.HandleFunc("POST "+wire.PathTurnRequest, a.turnRequest)
	mux.HandleFunc("POST "+wire.PathTurnResponse, a.turnResponse)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		answerJSON(w, a.Health())
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
			m.History = history(call.Conversation.History)
		}
		stages = append(stages, stageOf(m, turn.StageRequest, func(ctx context.Context) (turn.PreparedTurn, bool) {
			return turn.PrepareCallRequest(ctx, call), true
		}))
		if len(t.Response) > 0 && t.Op != wire.OpBatch { // a batch's response is the batch object
			stages = append(stages, stageOf(m, turn.StageResponse, func(ctx context.Context) (turn.PreparedTurn, bool) {
				return turn.PrepareCallResponse(ctx, call)
			}))
		}
	}
	queued := a.background(turnBytes(t), stages...)
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
			st: turn.StageRequest,
			prepare: func(ctx context.Context) (turn.PreparedTurn, bool) {
				pt := b.PrepareItem(ctx, &it.Conversation)
				id = b.ItemID(it.CustomID)
				return pt, true
			},
			meta: func() wire.Meta {
				im := m
				im.Item, im.History = id, history(it.Conversation.History)
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
		stages = append(stages, stageOf(m, turn.StageRequest, func(context.Context) (turn.PreparedTurn, bool) {
			return turn.NotJudgedTurn(turn.StageRequest, reason), true
		}))
	}
	return stages
}

// turnBytes is what a queued gateway turn holds until its judgment is
// done, counted against QueueBytes: the decoded raw request and response,
// and the canonical forms its stages read (the conversation and answer,
// or the batch items judged), which hold the text again. The JSON the
// handler decoded them from is not held: it is garbage once the handler
// returns.
func turnBytes(t wire.Turn) int {
	n := int64(len(t.Request) + len(t.Response))
	if len(t.Items) > 0 {
		for _, it := range t.Items[:min(len(t.Items), wire.MaxBatchItems)] {
			n += int64(len(it.CustomID)) + conversationBytes(&it.Conversation)
		}
		return int(n)
	}
	c := t.Call()
	n += conversationBytes(c.Conversation)
	if c.Answer != nil {
		n += int64(unsafe.Sizeof(*c.Answer)) + int64(len(c.Answer.StopReason)) + blocksBytes(c.Answer.Content)
	}
	return int(n)
}

// conversationBytes is the memory c holds, about: its strings and blocks.
func conversationBytes(c *wire.Conversation) int64 {
	if c == nil {
		return 0
	}
	n := int64(unsafe.Sizeof(*c)) + int64(len(c.History)) + blocksBytes(c.System)
	n += int64(cap(c.Messages)) * int64(unsafe.Sizeof(wire.Message{}))
	for _, m := range c.Messages {
		n += int64(len(m.Role)) + blocksBytes(m.Content)
	}
	for _, t := range c.Tools {
		n += int64(unsafe.Sizeof(t)) + int64(len(t))
	}
	return n
}

// blocksBytes is the memory bs holds, about, nested blocks included.
func blocksBytes(bs []wire.ContentBlock) int64 {
	n := int64(cap(bs)) * int64(unsafe.Sizeof(wire.ContentBlock{}))
	for _, b := range bs {
		n += int64(len(b.Type) + len(b.Text) + len(b.ID) + len(b.Name) + len(b.Input) +
			len(b.ToolUseID) + len(b.MediaType) + len(b.Raw))
		n += blocksBytes(b.Content)
	}
	return n
}

func (a *Agent) turnRequest(w http.ResponseWriter, r *http.Request) {
	t, ok := decodeTurn(w, r)
	if !ok {
		return
	}
	// The policy lookup (a cache miss) gets its own short budget so the whole
	// EngineTimeout is left for the judgment: together (1s + 8s by default)
	// they stay inside a gateway's 10s wait, with a second to spare.
	p := a.policy(r.Context(), t.TenantID)
	v := wire.Verdict{WantResponse: p.WantResponse}
	m := wire.MetaOf(t)
	// An inline tenant's request is judged here, MaxInFlight at most at
	// once; past that it goes to the background queue unjudged inline
	// (fails open, as an engine failure does).
	if p.Inline && a.acquireInline() {
		defer func() { <-a.inline }()
		ctx, cancel := context.WithTimeout(r.Context(), a.cfg.EngineTimeout)
		defer cancel()
		v.Block = a.judgeInline(ctx, m, t.Request)
		answerJSON(w, v)
		return
	}
	queued := a.background(len(t.Request), stageOf(m, turn.StageRequest, func(ctx context.Context) (turn.PreparedTurn, bool) {
		return a.prepareRequest(ctx, t.Request), true
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
	queued := a.background(len(t.Request)+len(t.Response), stageOf(m, turn.StageResponse, func(ctx context.Context) (turn.PreparedTurn, bool) {
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

// acquireInline takes an inline slot if one is free.
func (a *Agent) acquireInline() bool {
	select {
	case a.inline <- struct{}{}:
		return true
	default:
		return false
	}
}

// judgeInline judges a request before it is forwarded. Any engine failure
// fails open: the call goes through unjudged rather than being refused.
func (a *Agent) judgeInline(ctx context.Context, m wire.Meta, body []byte) *wire.Block {
	start := time.Now()
	pt, _ := a.prepare(ctx, m, stageOf(m, turn.StageRequest, func(ctx context.Context) (turn.PreparedTurn, bool) {
		return a.prepareRequest(ctx, body), true
	}))
	prepared := time.Since(start)
	res, err := a.detect(ctx, wire.DetectRequest{Sync: true, Meta: m, Turn: pt})
	returned := 1
	if err != nil {
		a.stats.droppedEngineDown.Add(1)
		a.warn("agent: inline judgment failed; failing open", err, "request_id", m.RequestID, "tenant", m.TenantID)
		returned = 0
	} else {
		a.sent(pt)
	}
	debugTurn(m, string(turn.StageRequest), returned, 1-returned, len(body), prepared)
	if res == nil {
		return nil
	}
	return res.Blocking
}

// background queues the stages of one call, as one job of size bytes,
// to be judged off the request path. When the queue is full (QueueSize
// jobs, or QueueBytes of them) it reports false and the caller sends the
// not-judged marker once it has answered. Preparing runs in the worker: it
// scans for secrets, which is the costly part.
func (a *Agent) background(size int, stages ...stage) bool {
	n := int64(size)
	if a.queued.Add(n) > int64(a.cfg.QueueBytes) {
		a.queued.Add(-n)
		a.stats.droppedQueueFull.Add(1)
		return false
	}
	select {
	case a.queue <- job{size: n, stages: stages}:
		return true
	default:
		a.queued.Add(-n)
		a.stats.droppedQueueFull.Add(1)
		return false
	}
}

// judge sends the job's stages one after the other; a stage that fails (or
// panics) does not stop the next. The job's bytes count against QueueBytes
// until it is done: the worker holds the raw turn until then.
func (a *Agent) judge(j job) {
	defer a.queued.Add(-j.size)
	var stages []string
	returned, dropped := 0, 0
	var prepared time.Duration
	var m wire.Meta
	for _, st := range j.stages {
		func() {
			defer func() {
				if v := recover(); v != nil { // past prepare's own recovery: drop the stage, keep the worker
					logPanic("agent: judging a turn panicked; the stage is dropped", v, st.st)
					dropped++
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), backgroundTimeout)
			defer cancel()
			start := time.Now()
			pt, ok := a.prepare(ctx, st.meta(), st)
			prepared += time.Since(start)
			if !ok {
				return
			}
			m = st.meta()
			stages = append(stages, string(pt.Stage))
			if _, err := a.detect(ctx, wire.DetectRequest{Meta: m, Turn: pt}); err != nil {
				a.stats.droppedEngineDown.Add(1)
				a.warn("agent: background judgment failed", err, "request_id", m.RequestID, "tenant", m.TenantID, "stage", pt.Stage)
				dropped++
				return
			}
			a.sent(pt)
			returned++
		}()
	}
	debugTurn(m, strings.Join(stages, ","), returned, dropped, int(j.size), prepared)
}

// debugTurn is the debug line of one judged turn: no text of it, only its
// ids, stages, how many judgments the engine returned and how many were
// dropped, its raw bytes and the time spent preparing it.
func debugTurn(m wire.Meta, stages string, returned, dropped, bytes int, prepared time.Duration) {
	if !slog.Default().Enabled(context.Background(), slog.LevelDebug) {
		return
	}
	slog.Debug("agent: turn judged", "tenant", m.TenantID, "request_id", m.RequestID, "stages", stages,
		"returned", returned, "dropped", dropped, "bytes", bytes, "prepare_ms", prepared.Milliseconds())
}

// prepare runs st's prepare within PrepareTimeout of ctx. A turn the
// deadline cut short comes back as not judged (turn.DeadlineReason), never
// half redacted; it is sent like that and logged here. A panic (a bug) is
// recovered, logged at Error, and the stage is sent as not judged
// (errPrepareFailed), so neither a worker nor the process dies with it.
func (a *Agent) prepare(ctx context.Context, m wire.Meta, st stage) (pt turn.PreparedTurn, ok bool) {
	pctx, cancel := context.WithTimeout(ctx, a.cfg.PrepareTimeout)
	defer cancel()
	defer func() {
		if v := recover(); v != nil {
			logPanic("agent: preparing a turn panicked; sending it as not judged", v, st.st,
				"request_id", m.RequestID, "tenant", m.TenantID)
			pt, ok = turn.NotJudgedTurn(st.st, errPrepareFailed), true
		}
	}()
	pt, ok = st.prepare(pctx)
	if ok && pt.NotJudged == turn.DeadlineReason {
		slog.Warn("agent: preparing a turn ran past its deadline; sending it as not judged",
			"request_id", m.RequestID, "tenant", m.TenantID, "stage", pt.Stage)
	}
	return pt, ok
}

// logPanic logs a recovered panic at Error with its stack. The value is
// logged only when the runtime raised it (an index out of range, a nil
// dereference): a panic the code raised may quote the turn.
func logPanic(msg string, v any, st turn.Stage, args ...any) {
	what := fmt.Sprintf("%T", v)
	if re, ok := v.(runtime.Error); ok {
		what = re.Error()
	}
	slog.Error(msg, append(args, "stage", st, "panic", what, "stack", string(debug.Stack()))...)
}

func (a *Agent) notJudged(m wire.Meta, st turn.Stage) {
	ctx, cancel := context.WithTimeout(context.Background(), markerTimeout)
	defer cancel()
	pt := turn.NotJudgedTurn(st, errJudgingBusy)
	if _, err := a.detect(ctx, wire.DetectRequest{Meta: m, Turn: pt}); err != nil {
		a.stats.droppedEngineDown.Add(1)
		a.warn("agent: not-judged marker not sent", err, "request_id", m.RequestID, "tenant", m.TenantID, "stage", st)
		return
	}
	a.sent(pt)
}

// history is a conversation's history as the engine gets it: lower case
// ("Full" is full). A value this agent does not know is passed on as it
// is, lower-cased.
func history(h string) string { return strings.ToLower(h) }

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

// warn logs a failed call to the engine, at most once a minute for each
// kind of failure (engineErrKind): a down or failing engine fails every
// call, and one line per call would flood the log. The count of what was
// dropped is on /healthz.
func (a *Agent) warn(msg string, err error, args ...any) {
	kind, now := engineErrKind(err), a.now().UnixNano()
	a.warnMu.Lock()
	last, ok := a.warned[kind]
	if ok && now-last < int64(time.Minute) {
		a.warnMu.Unlock()
		return
	}
	a.warned[kind] = now
	a.warnMu.Unlock()
	slog.Warn(msg, append(args, "error", err, "error_kind", kind, "note", "logged once a minute per error_kind")...)
}

// engineErrKind is the kind of a failed engine call: "version" (the
// engine does not speak this wire version), "unreachable" (no answer:
// refused, timed out, reset), "status <code>" (another non-200 answer),
// "decode" (a 200 that is not the answer), else "other".
func engineErrKind(err error) string {
	var se *engineStatusError
	switch {
	case errors.Is(err, errEngineVersion):
		return "version"
	case errors.Is(err, errEngineUnreachable), errors.Is(err, context.DeadlineExceeded):
		return "unreachable"
	case errors.As(err, &se):
		return fmt.Sprintf("status %d", se.code)
	case errors.Is(err, errEngineDecode):
		return "decode"
	}
	return "other"
}

// errEngineVersion is a 400 from an engine that does not speak this
// agent's wire version.
var errEngineVersion = errors.New("the engine does not accept wire version")

// errEngineUnreachable is a call the engine did not answer; errEngineDecode
// a 200 whose body is not the answer.
var (
	errEngineUnreachable = errors.New("engine unreachable")
	errEngineDecode      = errors.New("decode")
)

// engineStatusError is a non-200 answer from the engine.
type engineStatusError struct {
	code int
	msg  string
}

func (e *engineStatusError) Error() string { return e.msg }

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
		return fmt.Errorf("agent: %s %s: %w: %w", method, path, errEngineUnreachable, err)
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
		return &engineStatusError{code: resp.StatusCode,
			msg: fmt.Sprintf("agent: %s %s: %s: %s", method, path, resp.Status, bytes.TrimSpace(msg))}
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("agent: %s %s: %w: %w", method, path, errEngineDecode, err)
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
