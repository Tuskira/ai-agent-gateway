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

// scanSecrets finds the secrets in text.
func scanSecrets(text string) []secretMatch {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	var out []secretMatch
	seen := map[secretMatch]bool{}
	for _, f := range secretDetector().DetectString(text) {
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
	return out
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

// scanOverlap is how far past each kept end of a field longer than its cap
// the secret scan reaches. Scanning a whole 10 MiB field costs about 2 s
// (every keyword rule runs its regex over all of it); scanning what is kept
// plus this much on each side finds, whole, every secret up to 64 KiB long
// that the cut would split. Private key blocks have no length bound, so
// scanWindows also widens a window to a block it opens or closes, up to
// maxKeyBlock.
const scanOverlap = 64 << 10

// maxKeyBlock bounds how far a window is widened to take a key block whole.
// A real private key block is a few KB (RSA 16384 is under 13 KB); a wider
// BEGIN … END span is not a key to take whole, and widening to it made one
// 20 MiB tool output a whole-text scan (seconds). What a window cuts of
// such a span is still found: private-key-unterminated matches a BEGIN
// line with no END after it.
const maxKeyBlock = 64 << 10

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
// It stops with ctx's error once ctx is done (checked before each scan,
// each bounded by its window), leaving s half scrubbed: the caller must
// then send nothing of it.
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
	scan := func(text string, n int) []secretMatch {
		ms, ok := memo[text]
		if !ok && ctx.Err() == nil {
			if cache {
				ms = cachedScan(text, n)
			} else {
				ms = scanWindows(text, n)
			}
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
		found[i] = scan(raw[i], r.n)
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
		addShared(scan(validUTF8(x), capText))
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

// fits reports whether a string of length l is within cap n. Clip's own
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

// scanCache keeps the windowed scan of long strings across calls: an agent
// resends its whole history on every call, so the same tool outputs and
// messages come back turn after turn, and scanning them is the costly part
// of preparing a turn. Keyed by the text's SHA-256 and the cap, bounded by
// entries; when full, half is dropped.
//
// It holds raw secret values (the matches), so it is opt-in: off unless the
// agent, which runs on the customer's host, calls EnableScanCache. The
// engine never reads or writes it: PreparedTurn.Normalized scans uncached,
// so values from a forged or un-redacted turn are not retained.
var scanCacheOn atomic.Bool

// EnableScanCache turns on the scan cache for PrepareRequest and
// PrepareResponse in this process. Call it once at start-up, in the agent
// only: it keeps raw secret values in memory (see scanCache).
func EnableScanCache() { scanCacheOn.Store(true) }

var scanCache = struct {
	sync.Mutex
	m map[scanKey][]secretMatch
}{m: map[scanKey][]secretMatch{}}

type scanKey struct {
	sum [sha256.Size]byte
	n   int
}

const (
	cacheMinText = 1 << 10 // shorter: hashing costs about what scanning does
	cacheEntries = 8192
)

// cachedScan is scanWindows through scanCache. The slice it returns is
// shared: append to it only through a copy.
func cachedScan(text string, n int) []secretMatch {
	if len(text) < cacheMinText {
		return scanWindows(text, n)
	}
	h := sha256.New()
	_, _ = io.WriteString(h, text)
	k := scanKey{n: n}
	h.Sum(k.sum[:0])
	scanCache.Lock()
	ms, ok := scanCache.m[k]
	scanCache.Unlock()
	if ok {
		return ms
	}
	ms = slices.Clip(scanWindows(text, n))
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
	return ms
}

// scanWindows finds the secrets in text that clipping it to n would keep
// any part of: all of text when it is short enough, else its kept head and
// tail, each widened by scanOverlap and to a private key block it cuts into.
func scanWindows(text string, n int) []secretMatch {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	if len(text) <= n+2*scanOverlap+len(clipMark) {
		return scanSecrets(text)
	}
	head, tail := clipBounds(text, n)
	end, start := keyBlockEnd(text, head+scanOverlap), keyBlockStart(text, tail-scanOverlap)
	if end >= start {
		return scanSecrets(text)
	}
	end, start = runeStart(text, end, 1), runeStart(text, start, -1)
	ms := append(scanWindow(text, 0, end), scanWindow(text, start, len(text))...)
	slices.SortFunc(ms, func(a, b secretMatch) int {
		return cmp.Or(strings.Compare(a.kind, b.kind), strings.Compare(a.secret, b.secret))
	})
	return slices.Compact(ms)
}

// scanWindow finds the secrets in text[lo:hi]. A match that runs into a
// cut edge of the window (one that is not an end of text) may be a secret
// the cut truncated: its prefix or suffix, which would leave the rest when
// removed elsewhere. It is dropped, and the text across that edge (the
// edge plus and minus scanOverlap) is scanned instead, keeping what that
// scan finds clear of its own edges: every secret up to scanOverlap long
// that crosses the edge, whole.
func scanWindow(text string, lo, hi int) []secretMatch {
	var out []secretMatch
	cutLo, cutHi := false, false
	for _, m := range scanSecrets(text[lo:hi]) {
		l, h := touches(text[lo:hi], m, lo > 0, hi < len(text))
		cutLo, cutHi = cutLo || l, cutHi || h
		if !l && !h {
			out = append(out, m)
		}
	}
	for _, at := range []int{lo, hi} {
		if (at == lo && !cutLo) || (at == hi && !cutHi) {
			continue
		}
		a, b := runeStart(text, max(0, at-scanOverlap), -1), runeStart(text, min(len(text), at+scanOverlap), 1)
		for _, m := range scanSecrets(text[a:b]) {
			if l, h := touches(text[a:b], m, a > 0, b < len(text)); !l && !h {
				out = append(out, m)
			}
		}
	}
	return out
}

// touches reports whether m, found in w, runs into w's start or end where
// that is a cut (lo, hi). An unterminated private key block runs to the end
// of what is scanned by design: it is kept.
func touches(w string, m secretMatch, lo, hi bool) (bool, bool) {
	if m.kind == "private-key-unterminated" {
		return false, false
	}
	return lo && strings.HasPrefix(w, m.secret), hi && strings.HasSuffix(w, m.secret)
}

// validUTF8 is s with each run of invalid UTF-8 replaced by U+FFFD.
func validUTF8(s string) string { return strings.ToValidUTF8(s, string(utf8.RuneError)) }

// runeStart moves i to the nearest rune start in direction dir (1 or -1).
func runeStart(text string, i, dir int) int {
	for i > 0 && i < len(text) && !UTF8Start(text[i]) {
		i += dir
	}
	return i
}

// keyBlockEnd moves a window's end past the END line of a key block that
// opens before it and closes after it, within maxKeyBlock.
func keyBlockEnd(text string, end int) int {
	lo := max(0, end-maxKeyBlock)
	b := strings.LastIndex(text[lo:end], "-----BEGIN")
	if b < 0 || strings.Contains(text[lo+b:end], "-----END") {
		return end
	}
	hi := min(len(text), lo+b+maxKeyBlock)
	e := strings.Index(text[end:max(end, hi)], "-----END")
	if e < 0 {
		return end // not closed within maxKeyBlock: private-key-unterminated matches it
	}
	end += e + len("-----END")
	if k := strings.Index(text[end:min(len(text), end+128)], "-----"); k >= 0 {
		end += k + len("-----")
	}
	return end
}

// keyBlockStart moves a window's start back to the BEGIN line of a key
// block that is still open where the window starts, within maxKeyBlock.
func keyBlockStart(text string, start int) int {
	lo := max(0, start-maxKeyBlock)
	b := strings.LastIndex(text[lo:start], "-----BEGIN")
	if b < 0 || strings.Contains(text[lo+b:start], "-----END") {
		return start
	}
	return lo + b
}
