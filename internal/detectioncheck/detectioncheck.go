// Package detectioncheck is the MCP plane's inline tool-call check
// (llm_proxy.detection.inline_tool_calls): before a granted tools/call is
// forwarded to its connector, the gateway asks the detection agent next to
// it (llm_proxy.detection.agent_url) whether the tenant's tool calls are
// checked -- an engine-side policy, cached here per tenant -- and, when
// they are, hands the agent the call and waits for a verdict. A verdict
// that blocks refuses the call; nothing is forwarded. Every failure (agent
// down, timeout, a non-200, an answer that is not a verdict) fails open: the
// call goes through. There is no fail-closed option.
//
// Wire contract (the agent mirrors these field for field, in its wire
// package; pinned by testdata/tool_call_check.json, which the agent's tests
// read a byte-identical copy of):
//
//	GET  {agent}/v1/policy?tenant_id=   -> {"inline_tool_calls": bool, ...}
//	POST {agent}/v1/tool-calls/check    ToolCallCheck -> Verdict
package detectioncheck

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
	"unicode/utf8"
)

// version is a check's "v".
const version = 1

// MaxArguments bounds ToolCallCheck.Arguments (64 KiB); a longer value is
// cut, on a UTF-8 boundary, and ArgumentsTruncated is set.
const MaxArguments = 64 << 10

// maxAnswer bounds an agent answer; a longer one is a failure.
const maxAnswer = 1 << 20

// policyTTL is how long a tenant's policy is cached; a failed lookup is
// cached for policyFailTTL (as "off"), so an agent that is down costs one
// probe per tenant, not one per call. The LLM plane's inline prevention
// uses the same two values.
const (
	policyTTL     = 15 * time.Second
	policyFailTTL = 5 * time.Second
)

// Tool-call outcomes a check is sent for.
const (
	OutcomeAllowed   = "allowed"
	OutcomeMonitored = "monitored"
)

// ToolCallCheck is one granted tools/call, before it is forwarded.
type ToolCallCheck struct {
	V                  int       `json:"v,omitempty"`
	ID                 string    `json:"id"`
	TenantID           string    `json:"tenant_id"`
	SessionID          string    `json:"session_id,omitempty"`
	KeyID              string    `json:"key_id,omitempty"`
	Principal          string    `json:"principal,omitempty"`
	ClientIP           string    `json:"client_ip,omitempty"`
	At                 time.Time `json:"at"`
	Profile            string    `json:"profile,omitempty"`
	ConnectorID        string    `json:"connector_id"`
	Server             string    `json:"server"`
	Tool               string    `json:"tool"`
	Outcome            string    `json:"outcome"`
	Arguments          string    `json:"arguments,omitempty"`
	ArgumentsTruncated bool      `json:"arguments_truncated,omitempty"`
}

// Verdict is the agent's answer to a ToolCallCheck.
type Verdict struct {
	Block *Block `json:"block,omitempty"`
}

// Block names the rule that refuses a call.
type Block struct {
	RuleID      string   `json:"rule_id"`
	OWASP       string   `json:"owasp"`
	Title       string   `json:"title"`
	Probability *float64 `json:"probability,omitempty"`
	Threshold   float64  `json:"threshold"`
	Detail      string   `json:"detail,omitempty"`
}

// policy is the part of the agent's GET /v1/policy answer the check reads.
type policy struct {
	InlineToolCalls bool `json:"inline_tool_calls,omitempty"`
}

// CutArguments is args cut to MaxArguments on a UTF-8 boundary, and
// whether it was cut.
func CutArguments(args string) (string, bool) {
	if len(args) <= MaxArguments {
		return args, false
	}
	cut := MaxArguments
	for cut > 0 && !utf8.RuneStart(args[cut]) {
		cut--
	}
	return args[:cut], true
}

// Config tunes a Checker.
type Config struct {
	AgentURL   string
	AgentToken string        // sent as "Authorization: Bearer <token>" when set
	Timeout    time.Duration // one check: policy lookup + verdict; default 2s
}

// Checker checks tool calls with the detection agent. A nil *Checker is
// off: Check reports nothing checked.
type Checker struct {
	url     string
	token   string
	timeout time.Duration
	client  *http.Client
	now     func() time.Time

	mu       sync.Mutex
	policies map[string]cachedPolicy

	checked, blocked, failed atomic.Uint64
	lastLog                  atomic.Int64
}

type cachedPolicy struct {
	on    bool
	until time.Time
}

// New returns a Checker posting to cfg.AgentURL.
func New(cfg Config) *Checker {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 2 * time.Second
	}
	// A local sidecar: no proxy (the arguments never leave the host
	// through HTTP(S)_PROXY) and no redirects (the token is not forwarded).
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	return &Checker{
		url:     strings.TrimRight(cfg.AgentURL, "/"),
		token:   cfg.AgentToken,
		timeout: cfg.Timeout,
		client: &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
		now:      time.Now,
		policies: map[string]cachedPolicy{},
	}
}

// Check asks the agent about c. checked is true when a verdict came back
// (the tenant's policy turned the check on and the agent answered); block
// is the refusing rule, if any. Any failure is (nil, false): fail open.
// The whole check, policy lookup included, is bounded by Config.Timeout,
// and it is not cut short by the caller going away (a client that
// disconnects does not count as a failed check).
func (c *Checker) Check(ctx context.Context, tc ToolCallCheck) (block *Block, checked bool) {
	if c == nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.timeout)
	defer cancel()
	if !c.tenantOn(ctx, tc.TenantID) {
		return nil, false
	}
	tc.V = version
	if tc.At.IsZero() {
		tc.At = c.now()
	}
	b, err := json.Marshal(tc)
	if err != nil {
		c.fail(tc.ID, err)
		return nil, false
	}
	var v Verdict
	if err := c.call(ctx, http.MethodPost, "/v1/tool-calls/check", b, &v); err != nil {
		c.fail(tc.ID, err)
		return nil, false
	}
	c.checked.Add(1)
	if v.Block != nil {
		c.blocked.Add(1)
	}
	return v.Block, true
}

// tenantOn is the tenant's cached policy, looked up on a miss.
func (c *Checker) tenantOn(ctx context.Context, tenantID string) bool {
	now := c.now()
	c.mu.Lock()
	if p, ok := c.policies[tenantID]; ok && now.Before(p.until) {
		c.mu.Unlock()
		return p.on
	}
	c.mu.Unlock()

	var p policy
	err := c.call(ctx, http.MethodGet, "/v1/policy?tenant_id="+url.QueryEscape(tenantID), nil, &p)
	entry := cachedPolicy{on: p.InlineToolCalls, until: now.Add(policyTTL)}
	if err != nil {
		c.fail("", fmt.Errorf("policy for tenant %s: %w", tenantID, err))
		entry = cachedPolicy{until: now.Add(policyFailTTL)}
	}
	c.mu.Lock()
	c.policies[tenantID] = entry
	c.mu.Unlock()
	return entry.on
}

// errTooLarge is an answer over maxAnswer bytes.
var errTooLarge = errors.New("answer over 1 MiB")

func (c *Checker) call(ctx context.Context, method, path string, body []byte, out any) error {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.url+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxAnswer))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer+1))
	if err != nil {
		return err
	}
	if len(raw) > maxAnswer {
		return errTooLarge
	}
	return json.Unmarshal(raw, out)
}

// fail counts a failed check (or policy lookup) and logs it at most once a
// minute.
func (c *Checker) fail(id string, err error) {
	n := c.failed.Add(1)
	now := c.now().Unix()
	last := c.lastLog.Load()
	if now-last < 60 || !c.lastLog.CompareAndSwap(last, now) {
		return
	}
	slog.Warn("detection: tool-call check failed; failing open (logged at most once a minute)",
		"request_id", id, "error", err, "failed_total", n)
}

// Status is the check's /health entry: checks the agent answered, how many
// blocked, and failures (each failed open).
func (c *Checker) Status() map[string]any {
	if c == nil {
		return map[string]any{"enabled": false}
	}
	return map[string]any{"enabled": true, "checked": c.checked.Load(), "blocked": c.blocked.Load(), "failed": c.failed.Load()}
}
