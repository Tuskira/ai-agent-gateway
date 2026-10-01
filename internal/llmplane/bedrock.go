package llmplane

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/config"
)

// Bedrock credential-header names (Flow B). Raw temp creds OR a role ARN.
const (
	hdrBedrockAccessKeyID = "X-Bedrock-Access-Key-Id"
	hdrBedrockSecretKey   = "X-Bedrock-Secret-Access-Key"
	hdrBedrockSessionTok  = "X-Bedrock-Session-Token"
	hdrBedrockRoleARN     = "X-Bedrock-Role-Arn"
	hdrBedrockExternalID  = "X-Bedrock-External-Id"
	hdrBedrockRegion      = "X-Bedrock-Region"
)

// bedrockMode is the per-request credential mode.
type bedrockMode int

const (
	modeGateway     bedrockMode = iota // C: gateway's own AWS identity (fallback)
	modePassthrough                    // A: request is already SigV4-signed → forward as-is
	modeClientKeys                     // B: client supplied creds/role → gateway signs with them
)

// bedrockProvider forwards /model/* to Amazon Bedrock in one of three credential
// modes (auto-detected per request, or forced by credentialMode):
//
//	A passthrough — the request is already signed; forward it untouched.
//	B client_keys — the client sent AWS creds/role; sign with those, strip them.
//	C gateway     — sign with the gateway's own identity (default fallback).
type bedrockProvider struct {
	defaultRegion  string
	credentialMode string          // auto | passthrough | client_keys | gateway
	roleAccounts   map[string]bool // "<partition>:<account>" keys whose roles may be assumed; "*" = any; empty = role mode off
	signer         *bedrockSigner
}

// newBedrockProvider builds the provider. roleAccounts is the comma-separated
// allowlist behind X-Bedrock-Role-Arn (see Config.BedrockRoleAccounts): each
// entry is normalized to "<partition>:<account>" so an account listed bare
// authorizes the default "aws" partition only — a GovCloud or China role must
// be listed as "aws-us-gov:<account>" / "aws-cn:<account>".
func newBedrockProvider(region, credentialMode, roleAccounts string) *bedrockProvider {
	if credentialMode == "" {
		credentialMode = "auto"
	}
	accounts := map[string]bool{}
	for _, a := range strings.Split(roleAccounts, ",") {
		if a = strings.TrimSpace(a); a != "" {
			accounts[qualifyRoleAccount(a)] = true
		}
	}
	return &bedrockProvider{defaultRegion: region, credentialMode: credentialMode, roleAccounts: accounts, signer: &bedrockSigner{}}
}

// qualifyRoleAccount normalizes one allowlist entry (or a role ARN's parsed
// partition+account) to the "<partition>:<account>" form the allowlist is keyed
// by. A bare account id means the default "aws" partition; "*" is the wildcard
// and is left alone, so it keeps matching every account in every partition.
func qualifyRoleAccount(entry string) string {
	if entry == "*" || strings.Contains(entry, ":") {
		return entry
	}
	return "aws:" + entry
}

// reRoleARN matches an IAM role ARN, capturing its partition and account ID.
// The partition is captured, not just tolerated: account 123456789012 in the
// commercial partition and the SAME number in aws-us-gov are different AWS
// accounts, so the allowlist must compare both.
var reRoleARN = regexp.MustCompile(`^arn:(aws(?:-[a-z]+)*):iam::(\d{12}):role/[\w+=,.@/-]{1,512}$`)

func (*bedrockProvider) Name() string { return "bedrock" }

func (*bedrockProvider) ID() string { return "bedrock" }

// Model parses the model/inference-profile id from the upstream path
// (/model/{id}/{action}).
func (*bedrockProvider) Model(_ []byte, upstreamPath string) string {
	return parseModelID(upstreamPath)
}

// detectMode picks the credential mode. An explicit config mode wins; otherwise
// auto: a pre-signed request → passthrough; client cred headers → client_keys;
// else the gateway's own identity.
func (p *bedrockProvider) detectMode(r *http.Request) bedrockMode {
	switch p.credentialMode {
	case "passthrough":
		return modePassthrough
	case "client_keys":
		return modeClientKeys
	case "gateway":
		return modeGateway
	}
	if strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256") {
		return modePassthrough
	}
	if r.Header.Get(hdrBedrockAccessKeyID) != "" || r.Header.Get(hdrBedrockRoleARN) != "" {
		return modeClientKeys
	}
	return modeGateway
}

// reAuthScope pulls the region out of a SigV4 credential scope:
// Credential=<ak>/<yyyymmdd>/<region>/bedrock/aws4_request
var reAuthScope = regexp.MustCompile(`Credential=[^/]+/[^/]+/([^/]+)/bedrock/aws4_request`)

// parseAuthScopeRegion returns the region embedded in a pre-signed Authorization
// header (Flow A), or "" if it can't be parsed.
func parseAuthScopeRegion(authorization string) string {
	if m := reAuthScope.FindStringSubmatch(authorization); len(m) == 2 {
		return m[1]
	}
	return ""
}

// parseModelID extracts the model/inference-profile id from /model/{id}/{action}.
// The id may carry dots and a :N version suffix (e.g.
// us.anthropic.claude-haiku-4-5-20251001-v1:0); it never contains a slash for the
// inference-profile forms we support. ponytail: ARN ids (slashes) are Phase 2.
func parseModelID(path string) string {
	rest := strings.TrimPrefix(path, "/model/")
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return rest[:i]
	}
	return rest
}

// reAWSRegion matches an AWS region name (us-east-1, us-gov-west-1, ...). The
// region is caller-controlled and part of the upstream host, so anything else
// ("x@evil.host/#") could redirect the gateway, and its signature, elsewhere.
// It is config.ReAWSRegion itself, not a copy: the same rule must decide the
// per-request check here and the startup check on llm_proxy.bedrock.region,
// or one of them drifts into accepting what the other rejects.
var reAWSRegion = config.ReAWSRegion

// bedrockHost returns the bedrock-runtime host for region, or a clientError if
// region is not a well-formed AWS region name.
func bedrockHost(region string) (string, error) {
	if !reAWSRegion.MatchString(region) {
		return "", clientError{fmt.Sprintf("invalid AWS region %q", region)}
	}
	return "bedrock-runtime." + region + ".amazonaws.com", nil
}

// regionFor returns the request region for the signed modes (B/C): X-Bedrock-Region
// header (a legitimate selector when the client brings its own creds), else config.
func (p *bedrockProvider) regionFor(r *http.Request) string {
	if h := r.Header.Get(hdrBedrockRegion); h != "" {
		return h
	}
	return p.defaultRegion
}

func (p *bedrockProvider) BuildUpstream(ctx context.Context, r *http.Request, body []byte, upstreamPath string) (*http.Request, error) {
	switch p.detectMode(r) {
	case modePassthrough:
		return p.buildPassthrough(ctx, r, body, upstreamPath)
	case modeClientKeys:
		return p.buildSigned(ctx, r, body, upstreamPath, true)
	default:
		return p.buildSigned(ctx, r, body, upstreamPath, false)
	}
}

// buildPassthrough (Flow A) forwards a request that is ALREADY SigV4-signed by the
// client, untouched — the client's Authorization + X-Amz-* headers ride along and we
// do NOT re-sign. The region comes from the signed credential scope so the upstream
// host matches what was signed; nothing signed is mutated.
func (p *bedrockProvider) buildPassthrough(ctx context.Context, r *http.Request, body []byte, upstreamPath string) (*http.Request, error) {
	region := parseAuthScopeRegion(r.Header.Get("Authorization"))
	if region == "" {
		region = p.regionFor(r)
	}
	host, err := bedrockHost(region)
	if err != nil {
		return nil, err
	}
	url := "https://" + host + upstreamPath
	if r.URL.RawQuery != "" {
		url += "?" + r.URL.RawQuery
	}
	up, err := http.NewRequestWithContext(ctx, r.Method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	// Keep the client's signed headers (Authorization, X-Amz-*, Content-Type); drop
	// only hop-by-hop + gateway/session/cred headers (skipForward). No re-signing.
	copyHeaders(up.Header, r.Header)
	up.Host = host
	return up, nil
}

// copyBedrockNativeHeaders forwards the client's X-Amzn-Bedrock-* request headers
// (GuardrailIdentifier/Version, Trace, PerformanceConfig-Latency, ...) onto a
// re-signed request so those native InvokeModel features survive Flows B/C; the
// SigV4 signer then folds them into SignedHeaders. Only this prefix is copied —
// a blanket copy would drag stray client X-Amz-Date/Authorization into the
// canonical request and break the signature. (Flow A forwards them untouched.)
func copyBedrockNativeHeaders(dst, src http.Header) {
	for name, vals := range src {
		if strings.HasPrefix(strings.ToLower(name), "x-amzn-bedrock-") {
			for _, v := range vals {
				dst.Add(name, v)
			}
		}
	}
}

// buildSigned signs the request. client=true (Flow B) uses the caller's AWS creds
// from the X-Bedrock-* headers; client=false (Flow C) uses the gateway's own identity.
// Client auth/cred headers are never forwarded — a fresh, minimal header set is signed.
func (p *bedrockProvider) buildSigned(ctx context.Context, r *http.Request, body []byte, upstreamPath string, client bool) (*http.Request, error) {
	region := p.regionFor(r)
	host, err := bedrockHost(region)
	if err != nil {
		return nil, err
	}
	url := "https://" + host + upstreamPath
	if r.URL.RawQuery != "" {
		url += "?" + r.URL.RawQuery
	}
	up, err := http.NewRequestWithContext(ctx, r.Method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if ct := r.Header.Get("Content-Type"); ct != "" {
		up.Header.Set("Content-Type", ct)
	} else {
		up.Header.Set("Content-Type", "application/json")
	}
	if acc := r.Header.Get("Accept"); acc != "" {
		up.Header.Set("Accept", acc)
	}
	// Preserve native Bedrock feature headers (guardrails/trace/latency) through
	// the re-sign; without this they are dropped in signed modes.
	copyBedrockNativeHeaders(up.Header, r.Header)
	up.Host = host // the signed Host must match the URL host

	var provider aws.CredentialsProvider
	if client {
		provider, err = p.credsFromHeaders(ctx, r, region)
	} else {
		provider, err = p.signer.gatewayProvider(ctx)
	}
	var ce clientError
	if errors.As(err, &ce) {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("bedrock: credentials: %w", err)
	}
	if err := signRequest(ctx, up, body, region, provider); err != nil {
		if arn := r.Header.Get(hdrBedrockRoleARN); client && arn != "" {
			p.signer.forgetRole(arn, tenantOf(ctx), region)
		}
		return nil, fmt.Errorf("bedrock: sign: %w", err)
	}
	return up, nil
}

// credsFromHeaders builds a credentials provider from the client's X-Bedrock-* headers:
// raw temp creds (access key + secret [+ session token]) or a role ARN to assume.
//
// Role assumption runs as the gateway, so it needs an operator allowlist of AWS
// accounts, and the ExternalID is always the caller's tenant ID (never the
// caller's choice) to stop one tenant assuming another's role.
func (p *bedrockProvider) credsFromHeaders(ctx context.Context, r *http.Request, region string) (aws.CredentialsProvider, error) {
	if ak := r.Header.Get(hdrBedrockAccessKeyID); ak != "" {
		sk := r.Header.Get(hdrBedrockSecretKey)
		if sk == "" {
			return nil, clientError{hdrBedrockAccessKeyID + " without " + hdrBedrockSecretKey}
		}
		return staticProvider(ak, sk, r.Header.Get(hdrBedrockSessionTok)), nil
	}
	if arn := r.Header.Get(hdrBedrockRoleARN); arn != "" {
		m := reRoleARN.FindStringSubmatch(arn)
		switch {
		case len(p.roleAccounts) == 0:
			return nil, clientError{hdrBedrockRoleARN + " is not enabled on this gateway (llm_proxy.bedrock.allowed_role_accounts)"}
		case m == nil:
			return nil, clientError{hdrBedrockRoleARN + " is not a valid IAM role ARN"}
		case !p.roleAccounts["*"] && !p.roleAccounts[m[1]+":"+m[2]]:
			return nil, clientError{"AWS account " + m[1] + ":" + m[2] + " is not in llm_proxy.bedrock.allowed_role_accounts (a bare account id allows the \"aws\" partition only)"}
		}
		tenant := tenantOf(ctx)
		if tenant == "" {
			return nil, clientError{hdrBedrockRoleARN + " requires a tenant-scoped gateway key"}
		}
		if ext := r.Header.Get(hdrBedrockExternalID); ext != "" && ext != tenant {
			return nil, clientError{hdrBedrockExternalID + " is set by the gateway (the tenant ID) and cannot be overridden"}
		}
		return p.signer.roleProvider(ctx, arn, tenant, region)
	}
	return nil, clientError{"client_keys mode but no " + hdrBedrockAccessKeyID + " / " + hdrBedrockRoleARN + " header"}
}

// ParseUsage handles converse (camelCase), invoke (snake_case), and event-stream
// (usage JSON inside binary frames, plus base64 model chunks). It scans rather
// than fully decodes the vnd.amazon.eventstream framing — enough to recover tokens.
func (*bedrockProvider) ParseUsage(body []byte) Usage {
	// Fast path: a clean top-level converse response.
	var conv struct {
		StopReason string `json:"stopReason"`
		Usage      struct {
			InputTokens           int64 `json:"inputTokens"`
			OutputTokens          int64 `json:"outputTokens"`
			CacheReadInputTokens  int64 `json:"cacheReadInputTokens"`
			CacheWriteInputTokens int64 `json:"cacheWriteInputTokens"`
			// Nova InvokeModel spells the cache counts *TokenCount.
			CacheReadInputTokenCount  int64 `json:"cacheReadInputTokenCount"`
			CacheWriteInputTokenCount int64 `json:"cacheWriteInputTokenCount"`
			// CacheDetails splits cache writes by TTL; the 1h slice bills 2x input.
			CacheDetails []struct {
				TTL         string `json:"ttl"`
				InputTokens int64  `json:"inputTokens"`
			} `json:"cacheDetails"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &conv) == nil && (conv.Usage.InputTokens > 0 || conv.Usage.OutputTokens > 0) {
		u := conv.Usage
		out := Usage{
			InputTokens: u.InputTokens, OutputTokens: u.OutputTokens,
			CacheReadTokens:     max(u.CacheReadInputTokens, u.CacheReadInputTokenCount),
			CacheCreationTokens: max(u.CacheWriteInputTokens, u.CacheWriteInputTokenCount),
			StopReason:          conv.StopReason,
		}
		for _, d := range u.CacheDetails {
			if d.TTL == "1h" {
				out.CacheCreation1hTokens += d.InputTokens
			}
		}
		return out
	}
	return scanBedrockUsage(body)
}

var (
	reBase64Bytes = regexp.MustCompile(`"bytes"\s*:\s*"([A-Za-z0-9+/=]+)"`)
	reStopReason  = regexp.MustCompile(`"stop(?:_r|R)eason"\s*:\s*"([^"]*)"`)
	reInputCamel  = regexp.MustCompile(`"inputTokens"\s*:\s*(\d+)`)
	reOutputCamel = regexp.MustCompile(`"outputTokens"\s*:\s*(\d+)`)
	reCacheRCamel = regexp.MustCompile(`"cacheReadInputTokens"\s*:\s*(\d+)`)
	reCacheWCamel = regexp.MustCompile(`"cacheWriteInputTokens"\s*:\s*(\d+)`)
	reInputSnake  = regexp.MustCompile(`"input_tokens"\s*:\s*(\d+)`)
	reOutputSnake = regexp.MustCompile(`"output_tokens"\s*:\s*(\d+)`)
	reCacheRSnake = regexp.MustCompile(`"cache_read_input_tokens"\s*:\s*(\d+)`)
	reCacheWSnake = regexp.MustCompile(`"cache_creation_input_tokens"\s*:\s*(\d+)`)
	reCacheW1h    = regexp.MustCompile(`"ephemeral_1h_input_tokens"\s*:\s*(\d+)`)
	// Converse cacheDetails entry for the 1-hour TTL, either key order.
	reCacheDetail1h = regexp.MustCompile(`\{[^{}]*"ttl"\s*:\s*"1h"[^{}]*\}`)
	reDetailTokens  = regexp.MustCompile(`"inputTokens"\s*:\s*(\d+)`)
	// InvokeModel native shapes for non-Claude/Nova models — without these the
	// token counts for Llama/Titan/Mistral/etc. are recorded as 0.
	reInputLlama   = regexp.MustCompile(`"prompt_token_count"\s*:\s*(\d+)`)        // Meta Llama
	reOutputLlama  = regexp.MustCompile(`"generation_token_count"\s*:\s*(\d+)`)    // Meta Llama
	reInputTitan   = regexp.MustCompile(`"inputTextTokenCount"\s*:\s*(\d+)`)       // Amazon Titan
	reOutputTitan  = regexp.MustCompile(`"totalOutputTextTokenCount"\s*:\s*(\d+)`) // Amazon Titan
	reInputOpenAI  = regexp.MustCompile(`"prompt_tokens"\s*:\s*(\d+)`)             // OpenAI-shaped (gpt-oss, DeepSeek, ...)
	reOutputOpenAI = regexp.MustCompile(`"completion_tokens"\s*:\s*(\d+)`)
	// amazon-bedrock-invocationMetrics: the model-agnostic counts Bedrock appends
	// to the last chunk of every InvokeModel stream (Nova's non-stream body uses
	// the same cache field names). Input/output are a fallback only (see below).
	reInputMetric  = regexp.MustCompile(`"inputTokenCount"\s*:\s*(\d+)`)
	reOutputMetric = regexp.MustCompile(`"outputTokenCount"\s*:\s*(\d+)`)
	reCacheRMetric = regexp.MustCompile(`"cacheReadInputTokenCount"\s*:\s*(\d+)`)
	reCacheWMetric = regexp.MustCompile(`"cacheWriteInputTokenCount"\s*:\s*(\d+)`)
)

// scanBedrockUsage tokenizes a possibly-binary event stream: it decodes any
// base64 model chunks, concatenates them with the raw bytes, then takes the max
// of each token field seen (stream metadata repeats cumulative totals).
func scanBedrockUsage(body []byte) Usage {
	combined := append([]byte(nil), body...) // own copy: appended to below
	for _, m := range reBase64Bytes.FindAllSubmatch(body, -1) {
		if dec, err := base64.StdEncoding.DecodeString(string(m[1])); err == nil {
			combined = append(combined, ' ')
			combined = append(combined, dec...)
		}
	}
	maxInt := func(re *regexp.Regexp) int64 {
		var best int64
		for _, m := range re.FindAllSubmatch(combined, -1) {
			var v int64
			_, _ = fmt.Sscanf(string(m[1]), "%d", &v)
			if v > best {
				best = v
			}
		}
		return best
	}
	u := Usage{
		InputTokens:           max(maxInt(reInputCamel), maxInt(reInputSnake), maxInt(reInputLlama), maxInt(reInputTitan), maxInt(reInputOpenAI)),
		OutputTokens:          max(maxInt(reOutputCamel), maxInt(reOutputSnake), maxInt(reOutputLlama), maxInt(reOutputTitan), maxInt(reOutputOpenAI)),
		CacheReadTokens:       max(maxInt(reCacheRCamel), maxInt(reCacheRSnake), maxInt(reCacheRMetric)),
		CacheCreationTokens:   max(maxInt(reCacheWCamel), maxInt(reCacheWSnake), maxInt(reCacheWMetric)),
		CacheCreation1hTokens: maxInt(reCacheW1h),
	}
	// converse-stream metadata: cacheDetails[{ttl:"1h", inputTokens:N}]. Max, not
	// sum, like every other field here: the metadata event repeats the same
	// cumulative total, so adding the matches double-counts a buffer that holds
	// the event twice (a retried frame, or head and tail overlapping).
	var detail1h int64
	for _, m := range reCacheDetail1h.FindAll(combined, -1) {
		if t := reDetailTokens.FindSubmatch(m); t != nil {
			v, _ := strconv.ParseInt(string(t[1]), 10, 64)
			detail1h = max(detail1h, v)
		}
	}
	u.CacheCreation1hTokens = max(u.CacheCreation1hTokens, detail1h)
	// Model-specific shapes found no input/output (e.g. Mistral's native invoke
	// stream): use Bedrock's own invocation metrics.
	if u.InputTokens == 0 && u.OutputTokens == 0 {
		u.InputTokens, u.OutputTokens = maxInt(reInputMetric), maxInt(reOutputMetric)
	}
	if m := reStopReason.FindSubmatch(combined); m != nil {
		u.StopReason = string(m[1])
	}
	return u
}

// HeaderUsage reads the token counts Bedrock returns as InvokeModel response
// headers for every model. The router uses it only when the body parse found
// nothing (models whose native body carries no counts, e.g. Mistral).
func (*bedrockProvider) HeaderUsage(h http.Header) Usage {
	n := func(k string) int64 {
		v, _ := strconv.ParseInt(h.Get(k), 10, 64)
		return max(v, 0)
	}
	return Usage{
		InputTokens:         n("X-Amzn-Bedrock-Input-Token-Count"),
		OutputTokens:        n("X-Amzn-Bedrock-Output-Token-Count"),
		CacheReadTokens:     n("X-Amzn-Bedrock-Cache-Read-Input-Token-Count"),
		CacheCreationTokens: n("X-Amzn-Bedrock-Cache-Write-Input-Token-Count"),
	}
}

// ---------------------------------------------------------------------------
// Model-registry targets
// ---------------------------------------------------------------------------

// bedrockAnthropicVersion is the anthropic_version Bedrock requires in an
// InvokeModel body for Anthropic models (the Messages API shape).
const bedrockAnthropicVersion = "bedrock-2023-05-31"

// buildBedrockTarget builds a signed request for a registry target when
// the client already speaks the Bedrock dialect: the model id in the path
// is swapped for the target's, the request is signed for the target's
// region with the target's credential. Body untouched.
func (p *bedrockProvider) buildBedrockTarget(ctx context.Context, r *http.Request, body []byte, upstreamPath string, t resolvedTarget, creds map[string]string) (*upstreamAttempt, error) {
	rest := strings.TrimPrefix(upstreamPath, "/model/")
	action := ""
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		action = rest[i:]
	}
	path := "/model/" + t.Model + action
	up, region, err := p.buildTargetSigned(ctx, r, body, path, t, creds)
	if err != nil {
		return nil, err
	}
	return &upstreamAttempt{req: up, pricingProvider: "bedrock", region: region}, nil
}

// buildAnthropicOnBedrock builds a signed Bedrock InvokeModel request from
// an Anthropic-dialect /v1/messages request. Bedrock's invoke body for an
// Anthropic model IS the Messages body, minus "model" (it is in the path)
// and "stream" (the action selects streaming), plus the mandatory
// anthropic_version. A streaming call is re-framed on the way back
// (eventstream -> SSE, see eventstream.go); the events themselves are
// Anthropic's own.
func (p *bedrockProvider) buildAnthropicOnBedrock(ctx context.Context, r *http.Request, body []byte, t resolvedTarget, creds map[string]string) (*upstreamAttempt, error) {
	stream := streamFlagOf(body)
	set := map[string]any{}
	if !hasTopLevel(body, "anthropic_version") {
		set["anthropic_version"] = bedrockAnthropicVersion
	}
	rewritten, err := rewriteTopLevel(body, set, []string{"model", "stream"})
	if err != nil {
		return nil, clientError{"request body is not a JSON object"}
	}
	action := "/invoke"
	if stream {
		action = "/invoke-with-response-stream"
	}
	up, region, err := p.buildTargetSigned(ctx, r, rewritten, "/model/"+t.Model+action, t, creds)
	if err != nil {
		return nil, err
	}
	att := &upstreamAttempt{req: up, pricingProvider: "bedrock", region: region}
	if stream {
		att.adapt = func(resp *http.Response) {
			if resp.StatusCode/100 != 2 {
				return // a JSON error body is relayed as-is
			}
			resp.Header.Set("Content-Type", "text/event-stream; charset=utf-8")
			resp.Header.Del("Content-Length")
			resp.ContentLength = -1
			resp.Body = struct {
				io.Reader
				io.Closer
			}{newEventStreamToSSE(resp.Body), resp.Body}
		}
	}
	return att, nil
}

// buildTargetSigned signs a request to path for a registry target. Region:
// the caller's X-Bedrock-Region if set, else the target's. Credentials, in
// precedence order: the caller's own X-Bedrock-* headers (BYOK, exactly as
// on the passthrough path), the target's credential (fields
// access_key_id / secret_access_key [/ session_token]), the gateway's own
// identity. A request the caller pre-signed cannot be re-targeted -- its
// signature covers the path and body being rewritten -- and is refused.
func (p *bedrockProvider) buildTargetSigned(ctx context.Context, r *http.Request, body []byte, path string, t resolvedTarget, creds map[string]string) (*http.Request, string, error) {
	if strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256") {
		return nil, "", clientError{"a pre-signed Bedrock request cannot be routed through the model registry (the signature covers the path being rewritten)"}
	}
	region := r.Header.Get(hdrBedrockRegion)
	if region == "" {
		region = t.Region
	}
	host, err := bedrockHost(region)
	if err != nil {
		return nil, "", err
	}
	u := "https://" + host + path
	if r.URL.RawQuery != "" {
		u += "?" + r.URL.RawQuery
	}
	up, err := http.NewRequestWithContext(ctx, r.Method, u, bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	if ct := r.Header.Get("Content-Type"); ct != "" {
		up.Header.Set("Content-Type", ct)
	} else {
		up.Header.Set("Content-Type", "application/json")
	}
	if acc := r.Header.Get("Accept"); acc != "" {
		up.Header.Set("Accept", acc)
	}
	copyBedrockNativeHeaders(up.Header, r.Header)
	up.Host = host

	var provider aws.CredentialsProvider
	switch {
	case r.Header.Get(hdrBedrockAccessKeyID) != "" || r.Header.Get(hdrBedrockRoleARN) != "":
		provider, err = p.credsFromHeaders(ctx, r, region)
	case creds != nil:
		provider, err = bedrockCredsFromFields(creds)
	default:
		provider, err = p.signer.gatewayProvider(ctx)
	}
	var ce clientError
	if errors.As(err, &ce) {
		return nil, "", err
	}
	if err != nil {
		return nil, "", fmt.Errorf("bedrock: credentials: %w", err)
	}
	if err := signRequest(ctx, up, body, region, provider); err != nil {
		return nil, "", fmt.Errorf("bedrock: sign: %w", err)
	}
	return up, region, nil
}

// bedrockCredsFromFields builds a static provider from a registry
// credential's decrypted fields.
func bedrockCredsFromFields(fields map[string]string) (aws.CredentialsProvider, error) {
	ak, sk := fields["access_key_id"], fields["secret_access_key"]
	if ak == "" || sk == "" {
		return nil, clientError{"model registry: bedrock credential needs access_key_id and secret_access_key fields"}
	}
	return staticProvider(ak, sk, fields["session_token"]), nil
}

// streamFlagOf reads the top-level "stream" flag from a JSON body.
func streamFlagOf(body []byte) bool {
	var m struct {
		Stream bool `json:"stream"`
	}
	return json.Unmarshal(body, &m) == nil && m.Stream
}

// hasTopLevel reports whether key is a top-level member of the JSON body.
func hasTopLevel(body []byte, key string) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return false
	}
	_, ok := m[key]
	return ok
}
