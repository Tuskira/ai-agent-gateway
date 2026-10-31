package llmplane

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// Inline prevention (llm_proxy.detection.inline): before a generation call
// is forwarded, the gateway asks the detection agent whether the tenant's
// requests are judged inline -- an engine-side policy, cached here per
// tenant -- and, when they are, hands the agent the request and waits for a
// verdict. A verdict that blocks refuses the call with 400 and nothing is
// forwarded. Every failure (agent down, timeout, bad answer) fails open:
// the call goes through and the tee still sends the whole turn.
//
// Wire contract (the agent mirrors these field for field):
//
//	GET  {agent}/v1/policy?tenant_id=   -> teePolicy
//	POST {agent}/v1/turns/request       teeTurnRequest -> teeVerdict

// ruleBlockStatus is a refused call's status. 400 invalid_request_error,
// not 403: agent clients read a 403 as their own credential failing
// (Claude Code reports "Failed to authenticate" and retries), while a 400
// surfaces the message as-is and is never retried.
const ruleBlockStatus = http.StatusBadRequest

// inlinePolicyTTL is how long a tenant's policy is cached; a failed lookup
// is cached for inlinePolicyFailTTL (as "not inline") so an agent that is
// down costs one probe per tenant, not one per call.
const (
	inlinePolicyTTL     = 15 * time.Second
	inlinePolicyFailTTL = 5 * time.Second
	inlineRequestBytes  = 64 << 20
)

// teePolicy is the agent's answer to GET /v1/policy.
type teePolicy struct {
	Inline       bool `json:"inline"`
	WantResponse bool `json:"want_response"`
	Observe      bool `json:"observe,omitempty"`
}

// teeTurnRequest is the request stage of one call, handed over before it is
// forwarded.
type teeTurnRequest struct {
	ID        string    `json:"id"`
	TenantID  string    `json:"tenant_id"`
	SessionID string    `json:"session_id,omitempty"`
	KeyID     string    `json:"key_id,omitempty"`
	Principal string    `json:"principal,omitempty"`
	Model     string    `json:"model,omitempty"`
	Path      string    `json:"path"`
	At        time.Time `json:"at"`
	Request   []byte    `json:"request"`
}

// teeVerdict is the agent's answer to a teeTurnRequest.
type teeVerdict struct {
	Block        *teeRuleBlock `json:"block,omitempty"`
	WantResponse bool          `json:"want_response"`
}

// teeRuleBlock names the rule that refuses a call.
type teeRuleBlock struct {
	RuleID      string   `json:"rule_id"`
	OWASP       string   `json:"owasp"`
	Title       string   `json:"title"`
	Probability *float64 `json:"probability,omitempty"`
	Threshold   float64  `json:"threshold"`
	Detail      string   `json:"detail,omitempty"`
}

// blockedMessage is the client-facing refusal: which rule, which OWASP
// category, and that nothing reached the model.
func blockedMessage(b *teeRuleBlock) string {
	msg := fmt.Sprintf("Blocked by gateway rule %s (OWASP %s: %s)", b.RuleID, b.OWASP, b.Title)
	if b.Probability != nil {
		msg += fmt.Sprintf(", probability %.2f >= %.2f", *b.Probability, b.Threshold)
	} else if b.Detail != "" {
		msg += ", " + b.Detail
	}
	return msg + ". The request was not sent to the model."
}

// inlineJudge holds the per-tenant policy cache. Zero value is unusable;
// built by NewDetectionTee when Inline is set.
type inlineJudge struct {
	timeout time.Duration
	now     func() time.Time

	mu       sync.Mutex
	policies map[string]cachedPolicy

	failLog rateLog
}

type cachedPolicy struct {
	inline bool
	until  time.Time
}

// Inline reports whether the tee judges requests before forwarding.
func (d *DetectionTee) Inline() bool { return d != nil && d.inline != nil }

// judgeRequest asks the agent for a verdict on a call's request stage.
// judged is true when the agent judged (or queued) the request stage, so
// the tee turn must not carry it again; block is the refusing rule, if any.
// Any failure returns (false, nil): fail open.
func (d *DetectionTee) judgeRequest(ctx context.Context, tr teeTurnRequest) (judged bool, block *teeRuleBlock) {
	ij := d.inline
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ij.timeout)
	defer cancel()
	if !d.tenantInline(ctx, tr.TenantID) {
		return false, nil
	}
	b, err := json.Marshal(tr)
	if err != nil {
		d.inlineFail(tr.ID, err)
		return false, nil
	}
	var v teeVerdict
	if err := d.call(ctx, http.MethodPost, "/v1/turns/request", b, &v); err != nil {
		d.inlineFail(tr.ID, err)
		return false, nil
	}
	return true, v.Block
}

// tenantInline is the tenant's cached policy, looked up on a miss.
func (d *DetectionTee) tenantInline(ctx context.Context, tenantID string) bool {
	ij := d.inline
	now := ij.now()
	ij.mu.Lock()
	if p, ok := ij.policies[tenantID]; ok && now.Before(p.until) {
		ij.mu.Unlock()
		return p.inline
	}
	ij.mu.Unlock()

	var p teePolicy
	err := d.call(ctx, http.MethodGet, "/v1/policy?tenant_id="+url.QueryEscape(tenantID), nil, &p)
	entry := cachedPolicy{inline: p.Inline, until: now.Add(inlinePolicyTTL)}
	if err != nil {
		d.inlineFail("", fmt.Errorf("policy for tenant %s: %w", tenantID, err))
		entry = cachedPolicy{until: now.Add(inlinePolicyFailTTL)}
	}
	ij.mu.Lock()
	ij.policies[tenantID] = entry
	ij.mu.Unlock()
	return entry.inline
}

func (d *DetectionTee) call(ctx context.Context, method, path string, body []byte, out any) error {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, d.url+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer drainClose(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

func (d *DetectionTee) inlineFail(id string, err error) {
	d.inlineFailed.Add(1)
	if d.inline.failLog.allow(time.Now()) {
		slog.Warn("llmplane: inline judgment failed; failing open (logged at most once a minute)",
			"request_id", id, "error", err, "failed_total", d.inlineFailed.Load())
	}
}

// inlineCheck runs inline prevention for a call about to be forwarded: it
// returns judged (the request stage was handed to the agent) and, when a
// rule refuses the call, the refusal. Nothing happens -- and no round trip
// is made -- unless the tee is inline and the route is a generation.
func (rt *router) inlineCheck(r *http.Request, route RouteInfo, info callInfo, start time.Time, body []byte) (bool, *teeRuleBlock) {
	if !rt.tee.Inline() || route.Op != routeGenerate || len(body) > inlineRequestBytes {
		return false, nil
	}
	ctx := r.Context()
	return rt.tee.judgeRequest(ctx, teeTurnRequest{
		ID: requestIDOf(ctx), TenantID: tenantOf(ctx), SessionID: sessionOf(ctx), KeyID: keyIDOf(ctx),
		Principal: principalOf(ctx), Model: info.requestedModel, Path: r.URL.Path, At: start, Request: body,
	})
}
