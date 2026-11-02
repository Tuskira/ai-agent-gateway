package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/detection/turn"
	"github.com/Tuskira/tusk-ai-secured-gateway/detection/wire"
)

// Events and observations: the gateway's events (tool-call outcomes,
// refused keys; POST /v1/events) are forwarded to the engine in batches
// with each monitored call's Arguments redacted, and, when the tenant's
// engine policy asks (PolicyResponse.Observe), each generation turn is
// sent to the engine as an Observation with its bodies
// redacted. Both are off the request path, bounded, and drop (counted)
// rather than wait: neither is a judgment.

const (
	eventQueueSize   = 4096
	eventFlush       = 200 * time.Millisecond
	eventTimeout     = 5 * time.Second
	redactTimeout    = 2 * time.Second
	observeQueueSize = 256
	observeTimeout   = 10 * time.Second
	maxEventsBody    = 8 << 20
)

// sideband holds the event and observation queues.
type sideband struct {
	events chan wire.Event
	obs    chan wire.Observation
	// obsBytes is the raw body bytes the queued observations hold.
	obsBytes atomic.Int64
	wg       sync.WaitGroup
	stop     sync.Once

	eventsSent, eventsDropped   atomic.Int64
	observeSent, observeDropped atomic.Int64
	argumentsRedactFailed       atomic.Int64
}

func (a *Agent) startSideband() {
	a.side = &sideband{events: make(chan wire.Event, eventQueueSize), obs: make(chan wire.Observation, observeQueueSize)}
	a.side.wg.Go(a.runEvents)
	a.side.wg.Go(a.runObservations)
}

func (a *Agent) stopSideband() {
	a.side.stop.Do(func() {
		close(a.side.events)
		close(a.side.obs)
	})
	a.side.wg.Wait()
}

// events takes a batch of the gateway's events: 202 at once; a full
// queue drops what does not fit.
func (a *Agent) eventsHandler(w http.ResponseWriter, r *http.Request) {
	var b wire.EventBatch
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxEventsBody)).Decode(&b); err != nil {
		http.Error(w, "agent: bad events: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(b.Events) > wire.MaxEventBatch {
		http.Error(w, fmt.Sprintf("agent: bad events: more than %d", wire.MaxEventBatch), http.StatusBadRequest)
		return
	}
	for _, ev := range b.Events {
		if ev.TenantID == "" || ev.Kind == "" {
			a.side.eventsDropped.Add(1)
			continue
		}
		select {
		case a.side.events <- ev:
		default:
			a.side.eventsDropped.Add(1)
		}
	}
	answer(w, http.StatusAccepted, nil)
}

func (a *Agent) runEvents() {
	var buf []wire.Event
	tick := time.NewTicker(eventFlush)
	defer tick.Stop()
	for {
		select {
		case ev, ok := <-a.side.events:
			if !ok {
				a.postEvents(buf)
				return
			}
			buf = append(buf, a.redactEvent(ev))
			if len(buf) >= wire.MaxEventBatch {
				a.postEvents(buf)
				buf = nil
			}
		case <-tick.C:
			if len(buf) > 0 {
				a.postEvents(buf)
				buf = nil
			}
		}
	}
}

// redactEvent removes every secret from a monitored call's Arguments; when
// the scan cannot finish, the Arguments are not sent.
func (a *Agent) redactEvent(ev wire.Event) wire.Event {
	if ev.Arguments == "" {
		return ev
	}
	ctx, cancel := context.WithTimeout(context.Background(), redactTimeout)
	defer cancel()
	out, err := turn.RedactRaw(ctx, []byte(ev.Arguments))
	if err != nil {
		a.side.argumentsRedactFailed.Add(1)
		ev.Arguments = ""
		return ev
	}
	ev.Arguments = string(out)
	return ev
}

func (a *Agent) postEvents(evs []wire.Event) {
	if len(evs) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), eventTimeout)
	defer cancel()
	if err := a.send(ctx, wire.PathEvents, wire.EventBatch{Events: evs}); err != nil {
		a.side.eventsDropped.Add(int64(len(evs)))
		a.warn("agent: engine did not take events; dropped", err, "events", len(evs))
		return
	}
	a.side.eventsSent.Add(int64(len(evs)))
}

// observe queues an Observation of a generation turn, to be sent when the
// tenant's policy asks for one. A full queue (count, or a quarter of
// QueueBytes) drops it.
func (a *Agent) observe(t wire.Turn) {
	o := wire.Observation{V: 1, Meta: wire.MetaOfTurn(t), Headers: t.Headers, Request: t.Request,
		Response: t.Response, ResponseType: t.ResponseType}
	n := int64(len(t.Request) + len(t.Response))
	if a.side.obsBytes.Add(n) > int64(a.cfg.QueueBytes/4) {
		a.side.obsBytes.Add(-n)
		a.side.observeDropped.Add(1)
		return
	}
	select {
	case a.side.obs <- o:
	default:
		a.side.obsBytes.Add(-n)
		a.side.observeDropped.Add(1)
	}
}

func (a *Agent) runObservations() {
	for o := range a.side.obs {
		n := int64(len(o.Request) + len(o.Response))
		a.sendObservation(o)
		a.side.obsBytes.Add(-n)
	}
}

func (a *Agent) sendObservation(o wire.Observation) {
	ctx, cancel := context.WithTimeout(context.Background(), observeTimeout)
	defer cancel()
	if !a.policy(ctx, o.Meta.TenantID).Observe {
		return
	}
	rctx, rcancel := context.WithTimeout(ctx, a.cfg.PrepareTimeout)
	req, err := turn.RedactRaw(rctx, o.Request)
	var resp []byte
	if err == nil && len(o.Response) > 0 {
		resp, err = turn.RedactRaw(rctx, o.Response)
	}
	rcancel()
	if err != nil {
		// A scan that did not finish may have missed a secret: send nothing.
		a.side.observeDropped.Add(1)
		return
	}
	o.Request, o.Response = req, resp
	if err := a.send(ctx, wire.PathObserve, o); err != nil {
		a.side.observeDropped.Add(1)
		a.warn("agent: engine did not take an observation; dropped", err, "request_id", o.Meta.RequestID)
		return
	}
	a.side.observeSent.Add(1)
}

// send posts one JSON body to the engine and expects 200 or 202.
func (a *Agent) send(ctx context.Context, path string, in any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("agent: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.EngineURL+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.cfg.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("agent: POST %s: %w: %w", path, errEngineUnreachable, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &engineStatusError{code: resp.StatusCode,
			msg: fmt.Sprintf("agent: POST %s: %s: %s", path, resp.Status, bytes.TrimSpace(msg))}
	}
	return nil
}

// policyHandler serves GET /v1/policy?tenant_id= to the gateway: the
// tenant's engine policy, from the agent's cache.
func (a *Agent) policyHandler(w http.ResponseWriter, r *http.Request) {
	tenantID := r.URL.Query().Get("tenant_id")
	if tenantID == "" {
		http.Error(w, "agent: tenant_id is required", http.StatusBadRequest)
		return
	}
	answerJSON(w, a.policy(r.Context(), tenantID))
}
