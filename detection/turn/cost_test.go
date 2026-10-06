package turn

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// slowdown scales the time bounds of these tests (race_test.go).
var slowdown time.Duration = 1

// A tool output shaped like one huge private key block (BEGIN, 20 MiB,
// END) is scanned at its windows only, not whole: a key block is widened to
// at most maxKeyBlock. The block's head is still found and cut out (a BEGIN
// line no END follows in the window: private-key-unterminated).
func TestHugeKeyBlockPreparedInBoundedTime(t *testing.T) {
	secretDetector()
	resetScanCache()
	body := keyBlockBody(20 << 20)
	start := time.Now()
	pt := PrepareRequest(body)
	if d := time.Since(start); d > slowdown*time.Second {
		t.Errorf("20 MiB key block prepared in %v", d)
	}
	if len(pt.Secrets) == 0 || pt.Secrets[0] != (SecretHit{Kind: "private-key-unterminated", Field: "tool_results"}) {
		t.Errorf("hits = %+v", pt.Secrets)
	}
	r := pt.State.ToolResults
	if len(r) != 1 || strings.Contains(r[0].Content, "-----BEGIN") || len(r[0].Content) > capToolResult+len(clipMark) {
		t.Errorf("tool result = %.200q", r)
	}
	if got := keyBlockEnd(strings.Repeat("x", 100)+"-----BEGIN"+strings.Repeat("y", 2*maxKeyBlock)+"-----END Z-----", 200); got != 200 {
		t.Errorf("keyBlockEnd widened %d bytes past maxKeyBlock", got-200)
	}
}

// A deadline stops preparing and the turn goes out as not judged, with the
// reason and nothing else: never a turn left half redacted.
func TestPrepareDeadlineSendsNotJudged(t *testing.T) {
	secretDetector()
	done, cancel := context.WithCancel(context.Background())
	cancel()
	key := join("AKIA", "Z3MFKR7QW2LXB5TN")
	body := []byte(`{"messages":[{"role":"user","content":"why does ` + key + ` fail?"}]}`)
	want := NotJudgedTurn(StageRequest, DeadlineReason)
	for name, pt := range map[string]PreparedTurn{
		"request":    PrepareRequestContext(done, body),
		"unreadable": PrepareRequestContext(done, []byte(`{"prompt":"`+key+`"}`)),
	} {
		if !reflect.DeepEqual(pt, want) {
			t.Errorf("%s: %+v", name, pt)
		}
	}
	pt, ok := PrepareResponseContext(done, body, []byte(`{"content":[{"type":"text","text":"use `+key+`"}]}`))
	if !ok || !reflect.DeepEqual(pt, NotJudgedTurn(StageResponse, DeadlineReason)) {
		t.Errorf("response: %+v", pt)
	}
	// A deadline that falls while it runs: not judged, or the whole turn.
	big := keyBlockBody(4 << 20)
	whole := PrepareRequest(big)
	for _, d := range []time.Duration{time.Microsecond, time.Millisecond, 10 * time.Millisecond, 50 * time.Millisecond} {
		resetScanCache()
		ctx, cancel := context.WithTimeout(context.Background(), d)
		pt := PrepareRequestContext(ctx, big)
		cancel()
		if !reflect.DeepEqual(pt, NotJudgedTurn(StageRequest, DeadlineReason)) && !reflect.DeepEqual(pt, whole) {
			t.Errorf("deadline %v: partial turn %.300v", d, pt)
		}
	}
}

// forgedFiller is n bytes of text that trips every keyword rule's
// prefilter, so each scanned byte costs the most.
func forgedFiller(n int) string {
	return strings.Repeat("api_key password token secret aaaa ", n/35+1)[:n]
}

// Normalized cuts what a buggy or hostile agent sent before it scans: a
// 15 MiB not-judged reason, a 15 MiB user_text inside one key block, 16
// strings of 1 MiB each a key block, 400k hits each normalize in a few
// tens of milliseconds, and to the caps.
func TestNormalizedBoundsForgedTurns(t *testing.T) {
	secretDetector()
	block := func(n int) string {
		return join("-----BEGIN RSA ", "PRIVATE KEY-----\n") + forgedFiller(n) + join("\n-----END RSA ", "PRIVATE KEY-----")
	}
	hits := PreparedTurn{Stage: StageRequest, State: State{UserText: "x"}}
	for i := range 400_000 {
		hits.Secrets = append(hits.Secrets, SecretHit{Kind: fmt.Sprintf("k%d", i), Field: "user_text"})
	}
	lists := PreparedTurn{Stage: StageRequest}
	for i := range maxToolItems {
		lists.State.ToolResults = append(lists.State.ToolResults, ToolResult{Tool: "t", Content: fmt.Sprint(i) + block(1<<20)})
		lists.Refs.ToolResults = append(lists.Refs.ToolResults, ToolResultRef{CallInput: fmt.Sprint(i, "c") + block(1<<20)})
	}
	for name, pt := range map[string]PreparedTurn{
		"400k hits":                hits,
		"15 MiB not_judged":        NotJudgedTurn(StageRequest, forgedFiller(15<<20)),
		"15 MiB key block":         {Stage: StageRequest, State: State{UserText: block(15 << 20)}},
		"16 x 1 MiB key blocks":    lists,
		"15 MiB response, 8 calls": {Stage: StageResponse, State: State{ResponseText: forgedFiller(15 << 20), ResponseToolCalls: slices.Repeat([]ToolCall{{Name: forgedFiller(1 << 20), Input: forgedFiller(1 << 20)}}, 20)}},
	} {
		resetScanCache()
		start := time.Now()
		n := pt.Normalized()
		if d := time.Since(start); d > slowdown*200*time.Millisecond {
			t.Errorf("%s: normalized in %v", name, d)
		}
		if len(n.NotJudged) > capNotJudged+len(clipMark) || len(n.Secrets) > maxSecretHits {
			t.Errorf("%s: reason %d bytes, %d hits", name, len(n.NotJudged), len(n.Secrets))
		}
		s, _ := n.Restore()
		for _, r := range s.texts(n.Stage) {
			if !fits(len(*r.p), r.n) {
				t.Errorf("%s: a string of %d bytes, cap %d", name, len(*r.p), r.n)
			}
		}
		if len(s.ToolResults) > maxToolItems || len(s.ResponseToolCalls) > maxToolItems {
			t.Errorf("%s: lists not cut", name)
		}
	}
}

// Normalized is idempotent on whatever is received, and leaves no secret
// the scanner finds: random forged turns of secrets, near-misses, markers,
// clip marks, key block lines and invalid UTF-8, over the caps or not.
func TestNormalizedIdempotentOnForgedTurns(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 7))
	tok := func(n int) string {
		const alnum = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
		b := make([]byte, n)
		for i := range b {
			b[i] = alnum[r.IntN(len(alnum))]
		}
		return string(b)
	}
	iterations := 60
	if slowdown > 1 {
		iterations = 5 // the race detector: fewer turns, same shapes
	}
	for it := range iterations {
		toks := []string{tok(18), tok(24), tok(12) + "AB12", tok(30)}
		toks = append(toks, toks[0][6:]+tok(8)) // overlaps the first
		pick := func() string { return toks[r.IntN(len(toks))] }
		text := func() string {
			var b strings.Builder
			for range r.IntN(30) {
				switch r.IntN(13) {
				case 0:
					b.WriteString(`api_key = "` + pick() + `" `)
				case 1:
					b.WriteString(pick())
				case 2:
					b.WriteString("postgres://u:" + pick() + "@db:5432/x ")
				case 3:
					b.WriteString(join("AK", "IA") + strings.ToUpper(tok(16)) + " ")
				case 4:
					b.WriteString(join("-----BEGIN RSA ", "PRIVATE KEY-----\n") + strings.Repeat(tok(64)+"\n", 1+r.IntN(40)))
				case 5:
					b.WriteString(join("-----END RSA ", "PRIVATE KEY-----\n"))
				case 6:
					b.WriteString("token: ")
				case 7:
					b.WriteString(strings.Repeat("lorem ipsum ", r.IntN(800)))
				case 8:
					b.WriteString("[REDACTED:generic-api-key]")
				case 9:
					b.WriteString(clipMark)
				case 10:
					b.WriteString(join("gh", "p_") + tok(36) + " ")
				case 11:
					b.WriteString("\xff\xfe \xc3 ") // invalid UTF-8
				default:
					b.WriteString(" é漢 ")
				}
			}
			if r.IntN(4) == 0 {
				b.WriteString(strings.Repeat("filler text ", r.IntN(2000)))
			}
			return b.String()
		}
		var st State
		st.UserText, st.UserGoal, st.HarnessText, st.ResponseText = text(), text(), text(), text()
		for range r.IntN(10) {
			st.ToolResults = append(st.ToolResults, ToolResult{Tool: "t", Content: text(), CallInput: text(), CallID: tok(5)})
			st.PriorToolCalls = append(st.PriorToolCalls, ToolCall{Name: "n", Input: text(), ID: tok(4)})
			st.ResponseToolCalls = append(st.ResponseToolCalls, ToolCall{Name: "m", Input: text(), ID: tok(4)})
		}
		stage := []Stage{StageRequest, StageResponse}[r.IntN(2)]
		pt := newTurn(stage, &st, []SecretHit{{Kind: "k", Field: "user_text"}})
		pt.Unreadable = r.IntN(6) == 0
		n1 := overWire(t, pt.Normalized())
		n2 := overWire(t, n1.Normalized())
		if !reflect.DeepEqual(n1, n2) {
			a, b := mustJSON(t, n1), mustJSON(t, n2)
			i := 0
			for i < len(a) && i < len(b) && a[i] == b[i] {
				i++
			}
			t.Errorf("%d (%s, unreadable %v): not idempotent at byte %d:\n%q\n%q", it, stage, pt.Unreadable, i,
				a[max(0, i-120):min(len(a), i+80)], b[max(0, i-120):min(len(b), i+80)])
		}
		for _, s := range strs(t, n1) {
			if ms := scanSecrets(s); len(ms) > 0 || !utf8.ValidString(s) {
				t.Errorf("%d: left %v in %.120q", it, ms, s)
				break
			}
		}
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
