// Package redact scrubs credential-shaped strings out of captured payloads
// before they are stored or exported.
//
// The gateway never changes what it forwards upstream or returns to a
// client; this package only rewrites the copy that body capture keeps (the
// LLM capture store, the body store, the analytics sinks, ingested records).
// Each match is replaced with "[REDACTED:<kind>]". The marker holds no quote
// or backslash, so a replacement made inside a JSON string keeps the
// document valid JSON.
//
// Every pattern is a precompiled Go (RE2) regexp, so a scrub is linear in
// the body size. Patterns are deliberately narrow (fixed prefixes, labelled
// key names, minimum lengths): a missed secret is possible, but ordinary
// text such as UUIDs, hashes, request ids or base64 images is left alone.
package redact

import (
	"bytes"
	"regexp"
	"sync/atomic"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink"
)

// Plane says which plane produced a captured body; it is the "plane" label
// on the redaction counter.
type Plane uint8

const (
	// LLM is the LLM proxy plane (and LLM records pushed to the ingest API).
	LLM Plane = iota
	// MCP is the MCP proxy plane (and access-log records pushed to the
	// ingest API).
	MCP
	numPlanes
)

var planeNames = [numPlanes]string{LLM: "llm", MCP: "mcp"}

// String is the plane's label value.
func (p Plane) String() string {
	if p < numPlanes {
		return planeNames[p]
	}
	return "unknown"
}

// Kinds of secret, the "kind" label on the redaction counter and the
// suffix of the marker that replaces a match.
const (
	KindAWSKeyID   = "aws_key_id"
	KindAWSSecret  = "aws_secret"
	KindJWT        = "jwt"
	KindBearer     = "bearer"
	KindAPIKey     = "api_key"
	KindPrivateKey = "private_key"
	KindPassword   = "password"
	KindSecret     = "secret"
)

// kinds is the closed set of kind label values, in counter order.
var kinds = []string{
	KindAWSKeyID, KindAWSSecret, KindJWT, KindBearer,
	KindAPIKey, KindPrivateKey, KindPassword, KindSecret,
}

var kindIndex = func() map[string]int {
	m := make(map[string]int, len(kinds))
	for i, k := range kinds {
		m[k] = i
	}
	return m
}()

// markerPrefix starts every replacement. A value that already starts with
// it is not redacted again, so scrubbing is idempotent and is not counted
// twice.
var markerPrefix = []byte("[REDACTED:")

// rule is one pattern. When the regexp has a group named "v", only that
// group is replaced and the rest of the match (a key name, "Bearer ", a
// delimiter) is kept; otherwise the whole match is replaced.
//
// needles is a cheap prefilter: the regexp only runs on a body that
// contains one of them (compared against an ASCII-lowercased copy when
// fold is set). Most bodies hold none, and a substring search is far faster
// than a regexp scan.
type rule struct {
	kind    string
	re      *regexp.Regexp
	v       int // index of the "v" group, or -1
	needles [][]byte
	fold    bool
}

func newRule(kind, expr string, fold bool, needles ...string) rule {
	re := regexp.MustCompile(expr)
	r := rule{kind: kind, re: re, v: re.SubexpIndex("v"), fold: fold}
	for _, n := range needles {
		r.needles = append(r.needles, []byte(n))
	}
	return r
}

// Key names whose value is redacted. JSON keys may be escaped (a JSON
// document carried inside a JSON string, as MCP tool results and tool_use
// inputs often are), hence the optional backslashes. A value stops at the
// first quote or backslash, so it never swallows the closing quote.
const (
	passwordKey = `(?i:password|passwd)`
	secretKey   = `(?i:secret)`
	jsonPair    = `\\?"\s*:\s*\\?"(?P<v>[^"\\]+)`
	kvPair      = `\s*[=:]\s*(?P<v>[^\s"'&,;\\]+)`
)

// rules run in order; earlier rules win (a JWT sent as a bearer token is
// reported as jwt, and the later bearer rule skips the marker).
var rules = []rule{
	newRule(KindPrivateKey, `-----BEGIN[A-Z0-9 ]*PRIVATE KEY-----[\s\S]*?-----END[A-Z0-9 ]*PRIVATE KEY-----`, false, "PRIVATE KEY-----"),
	newRule(KindJWT, `\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`, false, "eyJ"),
	newRule(KindAWSKeyID, `\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`, false, "AKIA", "ASIA"),
	// Only when labelled: a bare 40-character base64 string is far more
	// often a hash or an id than an AWS secret.
	newRule(KindAWSSecret, `(?i:secret_?access_?key)\\?"?\s*[:=]\s*\\?"?(?P<v>[A-Za-z0-9/+]{40})(?:[^A-Za-z0-9/+=]|\z)`, true, "accesskey", "access_key"),
	// OpenAI / Anthropic style keys, and the gateway's own gk_ keys.
	newRule(KindAPIKey, `\bsk-[A-Za-z0-9_-]{20,}`, false, "sk-"),
	newRule(KindAPIKey, `\bgk_[A-Za-z0-9]{20,}`, false, "gk_"),
	newRule(KindBearer, `(?i:\bbearer)\s+(?P<v>[A-Za-z0-9._~+/-]{16,}=*)`, true, "bearer"),
	newRule(KindSecret, secretKey+jsonPair, true, "secret"),
	newRule(KindPassword, passwordKey+jsonPair, true, "password", "passwd"),
	newRule(KindSecret, secretKey+kvPair, true, "secret"),
	newRule(KindPassword, passwordKey+kvPair, true, "password", "passwd"),
}

// tailRules apply only to a body that was cut short before it reached the
// scrubber (a bounded stream capture, or a record its sender already
// truncated). They redact a secret whose end fell past the cut, which the
// full rules cannot recognise: an unterminated private key block, or a
// known prefix/label running into the end of the text.
var tailRules = []rule{
	newRule(KindPrivateKey, `-----BEGIN[A-Z0-9 ]*PRIVATE KEY-----[\s\S]*\z`, false, "PRIVATE KEY-----"),
	newRule(KindJWT, `\beyJ[A-Za-z0-9_.-]*\z`, false, "eyJ"),
	newRule(KindAWSKeyID, `\b(?:AKIA|ASIA)[A-Z0-9]{1,16}\z`, false, "AKIA", "ASIA"),
	newRule(KindAWSSecret, `(?i:secret_?access_?key)\\?"?\s*[:=]\s*\\?"?(?P<v>[A-Za-z0-9/+=]+)\z`, true, "accesskey", "access_key"),
	newRule(KindAPIKey, `\b(?:sk-[A-Za-z0-9_-]*|gk_[A-Za-z0-9]*)\z`, false, "sk-", "gk_"),
	newRule(KindBearer, `(?i:\bbearer)\s+(?P<v>[A-Za-z0-9._~+/=-]+)\z`, true, "bearer"),
	newRule(KindSecret, secretKey+`\\?"\s*:\s*\\?"(?P<v>[^"\\]+)\z`, true, "secret"),
	newRule(KindPassword, passwordKey+`\\?"\s*:\s*\\?"(?P<v>[^"\\]+)\z`, true, "password", "passwd"),
	newRule(KindSecret, secretKey+`\s*[=:]\s*(?P<v>[^\s"'&,;\\]+)\z`, true, "secret"),
	newRule(KindPassword, passwordKey+`\s*[=:]\s*(?P<v>[^\s"'&,;\\]+)\z`, true, "password", "passwd"),
}

// counts[plane][kind] is the number of redactions since process start.
var counts [numPlanes][]atomic.Uint64

func init() {
	for p := range counts {
		counts[p] = make([]atomic.Uint64, len(kinds))
	}
}

// Range calls fn once per (plane, kind) with the number of redactions made
// since process start, zeros included, in a stable order. It is what the
// capture_redactions metric reads.
func Range(fn func(plane, kind string, n uint64)) {
	for p := Plane(0); p < numPlanes; p++ {
		for i, k := range kinds {
			fn(p.String(), k, counts[p][i].Load())
		}
	}
}

// Policy is the redaction setting of one capture path.
type Policy struct {
	// On turns scrubbing on. Off, Body only applies the byte cap.
	On    bool
	Plane Plane
}

// Scrub returns b with every secret replaced. It never modifies b: when
// something is redacted the result is a new slice, otherwise it is b.
func (p Policy) Scrub(b []byte) []byte {
	if !p.On {
		return b
	}
	return apply(rules, b, p.Plane)
}

// Body is the stored form of a captured body: scrubbed (when On), then
// capped to max bytes (max <= 0 is unbounded), reporting whether the
// stored copy is incomplete. cut says b was already cut short before it
// got here; the tail of such a body is also checked for a partial secret.
// Scrubbing runs before the cap, so a secret straddling the cap is
// redacted whole rather than stored half-visible.
func (p Policy) Body(b []byte, max int, cut bool) ([]byte, bool) {
	if p.On {
		b = apply(rules, b, p.Plane)
		if cut {
			b = scrubTail(b, p.Plane)
		}
	}
	out, capped := sink.TruncateBody(b, max)
	return out, cut || capped
}

// tailWindow bounds how much of a cut body the tail rules look at: each of
// them matches only at the very end, and no secret they know is longer
// than this, so the rest of the body need not be scanned again.
const tailWindow = 64 << 10

// scrubTail applies tailRules to the last tailWindow bytes of b.
func scrubTail(b []byte, plane Plane) []byte {
	if len(b) <= tailWindow {
		return apply(tailRules, b, plane)
	}
	head, tail := b[:len(b)-tailWindow], b[len(b)-tailWindow:]
	scrubbed := apply(tailRules, tail, plane)
	if len(scrubbed) == len(tail) && &scrubbed[0] == &tail[0] {
		return b
	}
	out := make([]byte, 0, len(head)+len(scrubbed))
	return append(append(out, head...), scrubbed...)
}

// apply runs rs over b in order, counting each replacement against plane.
func apply(rs []rule, b []byte, plane Plane) []byte {
	if len(b) == 0 {
		return b
	}
	// The lowercased copy is made once, only if a fold rule needs it. It is
	// only used for the prefilter, so it may go stale as b is rewritten:
	// replacements only remove text or add markers, which never needs a
	// rule that the original body did not already trigger.
	var lower []byte
	for _, r := range rs {
		hay := b
		if r.fold {
			if lower == nil {
				lower = asciiLower(b)
			}
			hay = lower
		}
		if !containsAny(hay, r.needles) {
			continue
		}
		b = r.replace(b, plane)
	}
	return b
}

// replace rewrites every match of r in b.
func (r rule) replace(b []byte, plane Plane) []byte {
	idx := r.re.FindAllSubmatchIndex(b, -1)
	if idx == nil {
		return b
	}
	var out []byte
	last, n := 0, 0
	for _, m := range idx {
		start, end := m[0], m[1]
		if r.v >= 0 {
			start, end = m[2*r.v], m[2*r.v+1]
		}
		if start < 0 || bytes.HasPrefix(b[start:end], markerPrefix) {
			continue
		}
		if out == nil {
			out = make([]byte, 0, len(b))
		}
		out = append(out, b[last:start]...)
		out = append(out, markerPrefix...)
		out = append(out, r.kind...)
		out = append(out, ']')
		last = end
		n++
	}
	if n == 0 {
		return b
	}
	out = append(out, b[last:]...)
	counts[plane][kindIndex[r.kind]].Add(uint64(n))
	return out
}

func containsAny(b []byte, needles [][]byte) bool {
	for _, n := range needles {
		if bytes.Contains(b, n) {
			return true
		}
	}
	return false
}

// asciiLower is b with A-Z lowered and every other byte unchanged (same
// length, unlike bytes.ToLower on non-ASCII input).
func asciiLower(b []byte) []byte {
	out := make([]byte, len(b))
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		out[i] = c
	}
	return out
}
