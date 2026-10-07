package turn

import (
	"cmp"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"io"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"

	"github.com/rs/zerolog"
	"github.com/spf13/viper"
	"github.com/zricethezav/gitleaks/v8/config"
	"github.com/zricethezav/gitleaks/v8/detect"
	"github.com/zricethezav/gitleaks/v8/logging"
)

// secretsConfig is gitleaks' default ruleset extended with the engine's
// rules (secrets.toml).
//
//go:embed secrets.toml
var secretsConfig string

var (
	secretsOnce sync.Once
	secretsDet  *detect.Detector
)

// secretDetector is the gitleaks detector, built on first use (about 25ms).
// The config is embedded and covered by tests, so a failure to build it is
// a programming error.
func secretDetector() *detect.Detector {
	secretsOnce.Do(func() {
		logging.Logger = zerolog.Nop() // gitleaks logs to stderr by default
		v := viper.New()
		v.SetConfigType("toml")
		if err := v.ReadConfig(strings.NewReader(secretsConfig)); err != nil {
			panic("turn: secrets.toml: " + err.Error())
		}
		var vc config.ViperConfig
		if err := v.Unmarshal(&vc); err != nil {
			panic("turn: secrets.toml: " + err.Error())
		}
		cfg, err := vc.Translate()
		if err != nil {
			panic("turn: secrets.toml: " + err.Error())
		}
		secretsDet = detect.NewDetector(cfg)
		// An inline allow comment marks a known false positive in a
		// repository; in a call it is text the sender wrote, and honouring
		// it would let anyone keep a secret on its line from being removed.
		secretsDet.IgnoreGitleaksAllow = true
	})
	return secretsDet
}

// secretMatch is one secret found in a text: the gitleaks rule id (the
// redaction kind) and the secret value itself.
type secretMatch struct {
	kind   string
	secret string
}

// scanSecrets finds the secrets in text. Only for text whose size is
// bounded (a capped field, a hit list, an id): a string of the body goes
// through scanText, which a deadline stops.
func scanSecrets(text string) []secretMatch {
	ms, _ := scanSecretsContext(context.Background(), text)
	return ms
}

// Long text is scanned in pieces of scanChunk bytes, each overlapping the
// next by scanOverlap. One rule's regex over a piece cannot be stopped, so
// the piece bounds how far past a deadline a scan runs (a keyword-dense
// piece costs about 0.1 s for the slowest rule); the overlap finds whole
// every secret (with the context its rule needs) up to scanOverlap long
// that a piece's edge cuts.
const (
	scanChunk   = 256 << 10
	scanOverlap = 64 << 10
)

// scanSecretsContext finds the secrets in all of text, every byte of it,
// piece by piece (scanChunk). A match that runs into a piece's cut edge
// (one that is not an end of text) may be a secret the cut truncated: it
// is dropped, and the overlapping piece finds it whole. It stops between
// two rules once ctx is done and returns ctx's error: what it found by then
// is not every secret, so the caller must send nothing.
func scanSecretsContext(ctx context.Context, text string) ([]secretMatch, error) {
	if len(text) <= scanChunk+scanOverlap {
		return scanPiece(ctx, text)
	}
	var out []secretMatch
	for lo := 0; ; lo += scanChunk {
		a, b := runeStart(text, lo, -1), runeStart(text, min(len(text), lo+scanChunk+scanOverlap), 1)
		ms, err := scanPiece(ctx, text[a:b])
		if err != nil {
			return nil, err
		}
		for _, m := range ms {
			if l, h := touches(text[a:b], m, a > 0, b < len(text)); !l && !h {
				out = append(out, m)
			}
		}
		if b == len(text) {
			break
		}
	}
	slices.SortFunc(out, func(a, b secretMatch) int {
		return cmp.Or(strings.Compare(a.kind, b.kind), strings.Compare(a.secret, b.secret))
	})
	return slices.Compact(out), nil
}

// touches reports whether m, found in w, runs into w's start or end where
// that is a cut (lo, hi). An unterminated private key block runs to the end
// of what is scanned by design: it is kept (the piece that holds its END
// finds the block whole, and both values are removed).
func touches(w string, m secretMatch, lo, hi bool) (bool, bool) {
	if m.kind == "private-key-unterminated" {
		return false, false
	}
	return lo && strings.HasPrefix(w, m.secret), hi && strings.HasSuffix(w, m.secret)
}

// runeStart moves i to the nearest rune start in direction dir (1 or -1).
func runeStart(text string, i, dir int) int {
	for i > 0 && i < len(text) && !utf8Start(text[i]) {
		i += dir
	}
	return i
}

// scanPiece is the gitleaks scan of text, stopped between two rules once
// ctx is done.
func scanPiece(ctx context.Context, text string) ([]secretMatch, error) {
	if strings.TrimSpace(text) == "" {
		return nil, ctx.Err()
	}
	var out []secretMatch
	seen := map[secretMatch]bool{}
	// DetectContext is the only way to stop a scan between rules; in
	// gitleaks v8 it takes the deprecated detect.Fragment.
	frag := detect.Fragment{Raw: text} //nolint:staticcheck // SA1019: no other type until v9
	for _, f := range secretDetector().DetectContext(ctx, frag) {
		s := f.Secret
		if s == "" {
			s = f.Match
		}
		if f.RuleID == "private-key-unterminated" && strings.Contains(f.Match, "-----END") {
			// A complete block: the default private-key rule has it. The
			// greedy match runs on to the end of the text, so an open block
			// after a complete one is inside it: keep that one.
			s = s[strings.LastIndex(s, "-----BEGIN"):]
			if strings.Contains(s, "-----END") {
				continue
			}
		}
		m := secretMatch{kind: f.RuleID, secret: s}
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	// gitleaks reports findings in no fixed order; sort so a decision's
	// detail and evidence are the same on every run.
	slices.SortFunc(out, func(a, b secretMatch) int {
		return cmp.Or(strings.Compare(a.kind, b.kind), strings.Compare(a.secret, b.secret))
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// redactSecrets replaces every secret in text with [REDACTED:<rule id>].
func redactSecrets(text string) string { return replaceSecrets(text, byLength(scanSecrets(text))) }

// replaceSecrets replaces every occurrence of each value of ms (byLength
// order) in text. Occurrences that overlap or touch are one span, replaced
// once with the kind of the longest value in it: replacing one value after
// another would leave the part of an overlapping second value that the
// first did not cover.
func replaceSecrets(text string, ms []secretMatch) string {
	type span struct{ lo, hi, rank int }
	var spans []span
	for rank, m := range ms {
		for i := 0; i < len(text); {
			j := strings.Index(text[i:], m.secret)
			if j < 0 {
				break
			}
			spans = append(spans, span{i + j, i + j + len(m.secret), rank})
			i += j + 1 // occurrences of one value can overlap too
		}
	}
	if len(spans) == 0 {
		return text
	}
	slices.SortFunc(spans, func(a, b span) int { return cmp.Or(a.lo-b.lo, a.rank-b.rank) })
	var b strings.Builder
	at := 0
	for i := 0; i < len(spans); {
		cur := spans[i]
		for i++; i < len(spans) && spans[i].lo <= cur.hi; i++ {
			cur.hi, cur.rank = max(cur.hi, spans[i].hi), min(cur.rank, spans[i].rank)
		}
		b.WriteString(text[at:cur.lo])
		b.WriteString("[REDACTED:" + ms[cur.rank].kind + "]")
		at = cur.hi
	}
	b.WriteString(text[at:])
	return b.String()
}

// withEncodings adds to ms each value's other form: as it is written inside
// a JSON string (a tool input is JSON, canonicalJSON's form: a quote or a
// backslash escaped), and, for a value found inside such a string, as it
// reads decoded. A value then matches whichever kind of field it sits in.
func withEncodings(ms []secretMatch) []secretMatch {
	out := slices.Clone(ms)
	for _, m := range ms {
		if q, err := jsontext.AppendQuote(nil, m.secret); err == nil {
			if e := string(q[1 : len(q)-1]); e != m.secret {
				out = append(out, secretMatch{m.kind, e})
			}
		}
		if strings.Contains(m.secret, `\`) {
			var d string
			if jsonv2.Unmarshal([]byte(`"`+m.secret+`"`), &d) == nil && d != m.secret && d != "" {
				out = append(out, secretMatch{m.kind, d})
			}
		}
	}
	return out
}

// byLength orders ms longest first, so a secret containing another is
// replaced whole, then by kind and value so the output is the same on every
// run. A value matched by two rules keeps the first kind.
func byLength(ms []secretMatch) []secretMatch {
	out := slices.Clone(ms)
	slices.SortFunc(out, func(a, b secretMatch) int {
		return cmp.Or(len(b.secret)-len(a.secret), strings.Compare(a.kind, b.kind), strings.Compare(a.secret, b.secret))
	})
	return slices.CompactFunc(out, func(a, b secretMatch) bool { return a.secret == b.secret })
}

// ── what leaves the host ────────────────────────────────────────────────────

// minSharedSecret is the shortest value found in one string that is also
// removed from every other string. A shorter one (a 4-character password
// in a connection string) is still removed where it was found, but blanking
// it everywhere would cut common words out of unrelated text.
const minSharedSecret = 8

// textRef is one string of a State that leaves the host: its cap, and the
// field its secrets are reported under ("" = scanned and redacted, never
// reported).
type textRef struct {
	p     *string
	n     int
	field string
	index int
	json  bool // raw JSON (a tool input): kept in canonicalJSON's form
}

// texts lists every string of s that leaves the host. The stage's reported
// fields come first, in the order that decides where a kind of secret was
// first found: user_text, tool_results then harness_text for a request,
// response_text then response_tool_calls for a response.
func (s *State) texts(st Stage) []textRef {
	var out []textRef
	add := func(p *string, n int, field string, i int) { out = append(out, textRef{p, n, field, i, false}) }
	addJSON := func(p *string, n int, field string, i int) { out = append(out, textRef{p, n, field, i, true}) }
	request := func(report bool) {
		f, g, h := "", "", ""
		if report {
			f, g, h = "user_text", "tool_results", "harness_text"
		}
		add(&s.UserText, capText, f, 0)
		for i := range s.ToolResults {
			add(&s.ToolResults[i].Content, capToolResult, g, i)
		}
		add(&s.HarnessText, capText, h, 0)
	}
	response := func(report bool) {
		f, g := "", ""
		if report {
			f, g = "response_text", "response_tool_calls"
		}
		add(&s.ResponseText, capText, f, 0)
		for i := range s.ResponseToolCalls {
			addJSON(&s.ResponseToolCalls[i].Input, capToolInput, g, i)
		}
	}
	if st == StageResponse {
		response(true)
		request(false)
	} else {
		request(true)
		response(false)
	}
	add(&s.UserGoal, capText, "", 0)
	for i := range s.ToolResults {
		r := &s.ToolResults[i]
		addJSON(&r.CallInput, capToolInput, "", i)
		add(&r.Tool, capName, "", i)
		add(&r.CallID, capName, "", i)
	}
	for i := range s.PriorToolCalls {
		c := &s.PriorToolCalls[i]
		addJSON(&c.Input, capToolInput, "", i)
		add(&c.Name, capName, "", i)
		add(&c.ID, capName, "", i)
	}
	for i := range s.ResponseToolCalls {
		c := &s.ResponseToolCalls[i]
		add(&c.Name, capName, "", i)
		add(&c.ID, capName, "", i)
	}
	return out
}

// scrub is what makes a State fit to leave the host. It finds the secrets
// in every string, raw, and in every text of extra (the rest of the bodies:
// the history, the system prompt, the request behind a reply), then
// removes every value found from every string (a rule that needs context,
// such as generic-api-key's "api_key=", matches a value in one field only,
// while the same value can sit bare in another, or in a later turn), and
// only then clips each string to its cap, never through a secret (a secret
// cut in two no longer matches, so redacting after clipping would let most
// of it through). It returns where each kind of secret was first found,
// one hit per gitleaks rule, in the strings that leave only: extra is
// searched for values to remove, never reported.
//
// The result is a fixed point: scrubbing it again changes nothing, which is
// what lets the engine re-apply it (PreparedTurn.Normalized) to a turn an
// honest agent sent without changing what is judged.
//
// Every byte of every string is scanned, however long: a value stated in
// the middle of a long history string is removed from a bare copy of it
// elsewhere. That is linear in the body but not cheap (about 0.2 s per MiB
// of keyword-dense text), so it is bounded by ctx instead: it stops with
// ctx's error once ctx is done (checked between two rules of each scan),
// leaving s half scrubbed, and the caller must then send nothing of it.
//
// cache scans through scanCache, which keeps the raw values found: only the
// agent's Prepare* may pass it, never the engine's Normalized.
func (s *State) scrub(ctx context.Context, st Stage, extra []string, cache bool) ([]SecretHit, error) {
	return s.scrubWith(ctx, st, extra, nil, cache)
}

// scrubWith is scrub with pre, values already found elsewhere (a batch's
// body, Batch.values), removed as the values found in extra are.
func (s *State) scrubWith(ctx context.Context, st Stage, extra []string, pre []secretMatch, cache bool) ([]SecretHit, error) {
	refs := s.texts(st)
	raw := make([]string, len(refs))
	found := make([][]secretMatch, len(refs))
	// Each distinct string is scanned once: user_goal is often user_text,
	// and every string of the turn is in the body again.
	memo := map[string][]secretMatch{}
	var scanErr error
	scan := func(text string) []secretMatch {
		ms, ok := memo[text]
		if !ok && scanErr == nil {
			ms, scanErr = scanText(ctx, text, cache)
			memo[text] = ms
		}
		return ms
	}
	for i, r := range refs {
		// What is sent is what the engine re-derives: JSON would turn each
		// invalid byte into U+FFFD on the wire, so do it first, one way.
		raw[i] = validUTF8(*r.p)
		if r.json {
			// One encoding per value: "\/" or "\u0041" would hide a
			// value from the scan and from its removal.
			if c, ok := canonicalJSON([]byte(raw[i])); ok {
				raw[i] = c
			}
		}
		found[i] = scan(raw[i])
	}
	var shared []secretMatch
	addShared := func(ms []secretMatch) {
		for _, m := range ms {
			if len(m.secret) >= minSharedSecret {
				shared = append(shared, m)
			}
		}
	}
	for _, ms := range found {
		addShared(ms)
	}
	for _, x := range extra {
		addShared(scan(validUTF8(x)))
	}
	if scanErr != nil {
		return nil, scanErr // a string not scanned to its end: its values are not all known
	}
	addShared(pre)
	shared = withEncodings(shared)
	for i, r := range refs {
		if err := ctx.Err(); err != nil {
			return nil, err // checked per string: with many values, removing them is the long part
		}
		*r.p = clipRedacted(raw[i], r.n, byLength(append(withEncodings(found[i]), shared...)))
	}
	// What is left should hold no secret. Clipping joins head and tail and
	// a marker replaces a value, so a new match is possible in principle:
	// redact it in place, a few rounds at most.
	for range 3 {
		clean := true
		for i, r := range refs {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			ms := scanSecrets(*r.p)
			if len(ms) == 0 {
				continue
			}
			clean = false
			found[i] = append(found[i], ms...)
			*r.p = clipRedacted(*r.p, r.n, byLength(ms))
		}
		if clean {
			break
		}
	}

	var hits []SecretHit
	seen := map[string]bool{}
	for i, r := range refs {
		if r.field == "" {
			continue
		}
		for _, m := range found[i] {
			if !seen[m.kind] {
				seen[m.kind] = true
				hits = append(hits, SecretHit{Kind: m.kind, Field: r.field, Index: r.index})
			}
		}
	}
	return hits, nil
}

// fits reports whether a string of length l is within cap n. clipString's own
// output is n plus its marker, so that is within the cap too.
func fits(l, n int) bool { return l <= n+len(clipMark) }

// clipRedacted is text with every value of ms (byLength order) replaced,
// clipped to n. The cuts move off any value they would split, and the
// budget shrinks until the redacted result fits (a marker can be longer
// than the value it replaces).
func clipRedacted(text string, n int, ms []secretMatch) string {
	for budget := n; ; {
		out := redactedCut(text, budget, ms)
		if fits(len(out), n) || budget <= 0 {
			return out
		}
		budget -= len(out) - n - len(clipMark)
	}
}

func redactedCut(text string, budget int, ms []secretMatch) string {
	if fits(len(text), budget) {
		return replaceSecrets(text, ms)
	}
	head, tail := clipBounds(text, max(budget, 0))
	for moved := true; moved; {
		moved = false
		for _, m := range ms {
			if at, ok := splitAt(text, m.secret, head); ok {
				head, moved = at, true
			}
			if at, ok := splitAt(text, m.secret, tail); ok {
				tail, moved = at+len(m.secret), true
			}
		}
	}
	return replaceSecrets(text[:head], ms) + clipMark + replaceSecrets(text[tail:], ms)
}

// splitAt finds an occurrence of v in text that a cut at i would split, and
// returns where it starts.
func splitAt(text, v string, i int) (int, bool) {
	lo, hi := max(0, i-len(v)+1), min(len(text), i+len(v)-1)
	for lo < hi {
		j := strings.Index(text[lo:hi], v)
		if j < 0 {
			return 0, false
		}
		if at := lo + j; at < i && i < at+len(v) {
			return at, true
		}
		lo += j + 1
	}
	return 0, false
}

// scanCache keeps the scan of long strings across calls: an agent resends
// its whole history on every call, so the same tool outputs and messages
// come back turn after turn, and scanning them (every byte of each) is the
// costly part of preparing a turn. Keyed by the text's SHA-256, bounded by
// entries; when full, half is dropped. Only a scan that ran to the end is
// kept: one a deadline stopped is not every secret of its text.
//
// It holds raw secret values (the matches), so it is opt-in: off unless the
// agent, which runs on the customer's host, calls EnableScanCache. The
// engine never reads or writes it: PreparedTurn.Normalized scans uncached,
// so values from a forged or un-redacted turn are not retained.
var scanCacheOn atomic.Bool

// EnableScanCache turns on the scan cache for the Prepare* functions
// in this process. Call it once at start-up, in the agent
// only: it keeps raw secret values in memory (see scanCache).
func EnableScanCache() { scanCacheOn.Store(true) }

var scanCache = struct {
	sync.Mutex
	m map[[sha256.Size]byte][]secretMatch
}{m: map[[sha256.Size]byte][]secretMatch{}}

const (
	cacheMinText = 1 << 10 // shorter: hashing costs about what scanning does
	cacheEntries = 8192
)

// scanText is the scan of one string of a body, every byte of it, through
// scanCache when cache is set. Past ctx it returns ctx's error. The slice
// it returns may be shared: append to it only through a copy.
func scanText(ctx context.Context, text string, cache bool) ([]secretMatch, error) {
	if !cache || len(text) < cacheMinText {
		return scanSecretsContext(ctx, text)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	h := sha256.New()
	_, _ = io.WriteString(h, text)
	var k [sha256.Size]byte
	h.Sum(k[:0])
	scanCache.Lock()
	ms, ok := scanCache.m[k]
	scanCache.Unlock()
	if ok {
		return ms, nil
	}
	ms, err := scanSecretsContext(ctx, text)
	if err != nil {
		return nil, err
	}
	ms = slices.Clip(ms)
	scanCache.Lock()
	if len(scanCache.m) >= cacheEntries {
		i := 0
		for key := range scanCache.m { // map order is random: drop about half
			if i++; i%2 == 0 {
				delete(scanCache.m, key)
			}
		}
	}
	scanCache.m[k] = ms
	scanCache.Unlock()
	return ms, nil
}

// validUTF8 is s with each run of invalid UTF-8 replaced by U+FFFD.
func validUTF8(s string) string { return strings.ToValidUTF8(s, string(utf8.RuneError)) }
