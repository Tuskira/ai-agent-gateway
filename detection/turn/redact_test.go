package turn

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// leakWindow is the shortest part of a secret the leak checks look for.
const leakWindow = 8

// strs is every string of a prepared turn as it crosses the wire.
func strs(t *testing.T, pt PreparedTurn) []string {
	t.Helper()
	b, err := json.Marshal(pt)
	if err != nil {
		t.Fatal(err)
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	var out []string
	var walk func(any)
	walk = func(v any) {
		switch v := v.(type) {
		case string:
			out = append(out, v)
		case []any:
			for _, x := range v {
				walk(x)
			}
		case map[string]any:
			for _, x := range v {
				walk(x)
			}
		}
	}
	walk(v)
	return out
}

// leak reports the first part of secret (leakWindow bytes or more) found in
// any string of pt.
func leak(t *testing.T, pt PreparedTurn, secret string) string {
	t.Helper()
	for _, s := range strs(t, pt) {
		for i := 0; i+leakWindow <= len(secret); i++ {
			if w := secret[i : i+leakWindow]; strings.Contains(s, w) {
				return fmt.Sprintf("%q (at %d of the secret) in %.120q", w, i, s)
			}
		}
	}
	return ""
}

// filler is n bytes (none when n < 0) of text that holds no secret and no
// part of one.
func filler(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat("lorem ipsum dolor sit amet. ", n/28+1)[:n]
}

// cutSecret is a secret as one rule finds it: text is what is placed in the
// field, secret the value the scanner finds in it.
type cutSecret struct{ name, text, secret string }

func cutSecrets() []cutSecret {
	key := join("AKIA", "Z3MFKR7QW2LXB5TN")
	pw := join("S3cr3t", "Passw0rd")
	// A multi-KB key block: longer than a tool input's whole cap.
	pem := join("-----BEGIN OPENSSH ", "PRIVATE KEY-----\n") +
		strings.Repeat(join("b3BlbnNzaC1rZXktdjEAAAA", "ABG5vbmUAAAAEbm9uZQAAAAAAAAAB\n"), 80) +
		join("-----END OPENSSH ", "PRIVATE KEY-----")
	return []cutSecret{
		{"aws key", key, key},
		{"connection string", "postgres://app:" + pw + "@db.internal:5432/prod", pw},
		{"private key block", pem, pem},
	}
}

// cutField builds a body whose field (named by the case) is n bytes and
// holds text at offset at; it returns the body and the field's form of a
// string (tool inputs are raw JSON, so a newline is escaped).
type cutField struct {
	name  string
	stage Stage
	cap   int
	// build returns the request and response bodies (response nil for the
	// request stage) with field = the field's exact content.
	build func(field string) (req, resp []byte)
	// json: the field is raw JSON (a tool input), so text in it is escaped.
	json bool
}

func esc(s string) string {
	b, _ := json.Marshal(s)
	return string(b[1 : len(b)-1])
}

func cutFields() []cutField {
	userMsg := func(text string) string {
		return `{"messages":[{"role":"user","content":` + jsonStr(text) + `}]}`
	}
	toolTurn := func(input, result string) string {
		return `{"messages":[{"role":"user","content":"run it"},` +
			`{"role":"assistant","content":[{"type":"tool_use","id":"tu_1","name":"Bash","input":` + input + `}]},` +
			`{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":` + jsonStr(result) + `}]}]}`
	}
	return []cutField{
		{"user_text", StageRequest, capText, func(f string) ([]byte, []byte) { return []byte(userMsg(f)), nil }, false},
		{"tool_results", StageRequest, capToolResult, func(f string) ([]byte, []byte) { return []byte(toolTurn(`{"q":1}`, f)), nil }, false},
		{"prior_tool_calls", StageRequest, capToolInput, func(f string) ([]byte, []byte) { return []byte(toolTurn(f, "ok")), nil }, true},
		{"unreadable", StageRequest, capText, func(f string) ([]byte, []byte) { return []byte(f), nil }, false},
		{"response_text", StageResponse, capText, func(f string) ([]byte, []byte) {
			return []byte(userMsg("hi")), []byte(`{"content":[{"type":"text","text":` + jsonStr(f) + `}]}`)
		}, false},
		{"response_tool_calls", StageResponse, capToolInput, func(f string) ([]byte, []byte) {
			return []byte(userMsg("hi")), []byte(`{"content":[{"type":"tool_use","id":"tu_r","name":"Bash","input":` + f + `}]}`)
		}, true},
	}
}

func jsonStr(s string) string { b, _ := json.Marshal(s); return string(b) }

// field is field content holding text with pre bytes of filler before it
// and post after: a JSON object for a tool input, plain text otherwise.
// Spaces keep text apart from the filler (a rule may need a word boundary).
func (f cutField) field(text string, pre, post int) string {
	text = " " + text + " "
	if f.json {
		return `{"q":"` + filler(max(0, pre-6)) + esc(text) + filler(max(0, post-2)) + `"}`
	}
	return filler(pre) + text + filler(post)
}

func (f cutField) prepare(field string) PreparedTurn {
	req, resp := f.build(field)
	if resp == nil {
		return PrepareRequest(req)
	}
	pt, _ := PrepareResponse(req, resp)
	return pt
}

// No part of a secret the scanner finds in the unclipped text leaves the
// host, wherever clipping cuts it: across the head cut and the tail cut of
// every clipped field, in both stages, for a field just over its cap and
// for one long enough that only its ends are scanned.
func TestNoSecretSurvivesClipping(t *testing.T) {
	for _, f := range cutFields() {
		for _, sc := range cutSecrets() {
			secret := sc.secret
			if f.json {
				secret = esc(secret)
			}
			text := sc.text
			if f.json {
				text = esc(text)
			}
			// Fields just over the cap (scanned whole) and long enough that
			// only their ends are scanned.
			for _, n := range []int{3 * f.cap, f.cap + 2*scanOverlap + 20000} {
				k, l := f.cap*2/3, f.cap/3 // kept head, kept tail
				w := len(text) + 2
				var fields []string
				for _, d := range []int{2, w / 2, w - 2} { // straddling each cut
					fields = append(fields, f.field(sc.text, k-w+d, n-k-d), f.field(sc.text, n-l-w+d, l-d))
				}
				for _, field := range fields {
					head, tail := clipBounds(field, f.cap)
					at := strings.Index(field, text)
					splits := func(cut int) bool { return at < cut && cut < at+len(text) }
					if !splits(head) && !splits(tail) {
						t.Fatalf("%s/%s: secret at %d does not straddle a cut (%d, %d)", f.name, sc.name, at, head, tail)
					}
					pt := f.prepare(field)
					if pt.Stage != f.stage {
						t.Fatalf("%s/%s: stage %q", f.name, sc.name, pt.Stage)
					}
					if l := leak(t, pt, secret); l != "" {
						t.Errorf("%s/%s, field %d bytes, secret at %d (cuts at %d, %d): leaked %s",
							f.name, sc.name, len(field), at, head, tail, l)
					}
					if len(pt.Secrets) == 0 && f.name != "prior_tool_calls" { // not a reported field
						t.Errorf("%s/%s, secret at %d: no secret hit", f.name, sc.name, at)
					}
				}
			}
		}
	}
}

// A value found in one string (here by generic-api-key, which needs the
// "aws_secret_access_key =" context) is removed from every string that
// leaves, where it sits bare: the goal, a prior tool call's input and its
// ref, the response text and a proposed call's input.
func TestSecretRemovedFromEveryString(t *testing.T) {
	v := join("wJalrXUtnFEMI/K7MDENG", "/bPxRfiCYzq8Tq3Rk2Z")
	req := `{"messages":[{"role":"user","content":"the old value was ` + v + `, rotate it"},
	 {"role":"assistant","content":[{"type":"tool_use","id":"tu_1","name":"Read","input":{"path":"/etc/app.env","was":"` + v + `"}}]},
	 {"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":"aws_secret_access_key = ` + v + `\nregion = us-east-1"}]}]}`
	resp := `{"content":[{"type":"text","text":"I found ` + v + ` in the file."},
	 {"type":"tool_use","id":"tu_r","name":"Bash","input":{"cmd":"rotate --old ` + v + ` && aws_secret_access_key = ` + v + `"}}]}`
	pr := PrepareRequest([]byte(req))
	if l := leak(t, pr, v); l != "" {
		t.Errorf("request: leaked %s", l)
	}
	if len(pr.Secrets) != 1 || pr.Secrets[0] != (SecretHit{Kind: "generic-api-key", Field: "tool_results", Index: 0}) {
		t.Errorf("request hits = %+v", pr.Secrets)
	}
	pp, ok := PrepareResponse([]byte(req), []byte(resp))
	if !ok {
		t.Fatal("response not prepared")
	}
	if l := leak(t, pp, v); l != "" {
		t.Errorf("response: leaked %s", l)
	}
	if len(pp.Secrets) != 1 || pp.Secrets[0].Field != "response_tool_calls" {
		t.Errorf("response hits = %+v", pp.Secrets)
	}

	// Deterministic: the same body prepares to the same bytes.
	a, _ := json.Marshal(PrepareRequest([]byte(req)))
	b, _ := json.Marshal(pr)
	if string(a) != string(b) {
		t.Errorf("not deterministic:\n%s\n%s", a, b)
	}

	// A short value is only removed where it was found: blanking it in
	// unrelated text would cut common words.
	short := `{"messages":[{"role":"user","content":"postgres://app:abcd1@db:5432/x"},
	 {"role":"assistant","content":[{"type":"tool_use","id":"tu_1","name":"Read","input":{}}]},
	 {"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":"step abcd1 done"}]}]}`
	ps := PrepareRequest([]byte(short))
	if r := ps.State.ToolResults; len(r) != 1 || r[0].Content != "step abcd1 done" {
		t.Errorf("short value blanked elsewhere: %+v", r)
	}
}

// A value found anywhere in the body is removed from every string that
// leaves, though the scanner finds it only where its context is: in the
// history, a system prompt, a tool input or result of an earlier turn, the
// prefill. Here the context is only in the source; the trailing turn
// carries the value bare. Nothing is reported: a hit is where a secret was
// found in a string that leaves.
func TestSecretFoundAnywhereInBodyRemoved(t *testing.T) {
	v := join("Qm7Lp2Vx", "9Rt4Kw8Zn3")
	ctx := `api_key = \"` + v + `\"`
	trailing := `{"role":"assistant","content":[{"type":"tool_use","id":"t9","name":"Bash","input":{"argv":["deploy","` + v + `"]}}]},
	 {"role":"user","content":[{"type":"tool_result","tool_use_id":"t9","content":"ok ` + v + `"}]}`
	for name, body := range map[string]string{
		"earlier user message": `{"messages":[{"role":"user","content":"remember my ` + ctx + `"},
		 {"role":"assistant","content":"noted"},{"role":"user","content":"deploy now"},` + trailing + `]}`,
		"anthropic system field":  `{"system":"env: ` + ctx + `","messages":[{"role":"user","content":"deploy now"},` + trailing + `]}`,
		"anthropic system blocks": `{"system":[{"type":"text","text":"env: ` + ctx + `"}],"messages":[{"role":"user","content":"deploy now"},` + trailing + `]}`,
		"earlier system message": `{"messages":[{"role":"system","content":"env: ` + ctx + `"},{"role":"user","content":"hi"},
		 {"role":"assistant","content":"hello"},{"role":"user","content":"deploy now"},` + trailing + `]}`,
		"earlier developer message": `{"messages":[{"role":"developer","content":[{"type":"text","text":"env: ` + ctx + `"}]},
		 {"role":"user","content":"hi"},{"role":"assistant","content":"hello"},{"role":"user","content":"deploy now"},` + trailing + `]}`,
		"earlier tool input": `{"messages":[{"role":"user","content":"set it"},
		 {"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"api_key":"` + v + `"}}]},
		 {"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"done"}]},
		 {"role":"assistant","content":"set"},{"role":"user","content":"deploy now"},` + trailing + `]}`,
		"earlier openai tool call arguments": `{"messages":[{"role":"user","content":"set it"},
		 {"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"set","arguments":"{\"api_key\":\"` + v + `\"}"}}]},
		 {"role":"tool","tool_call_id":"c1","content":"done"},
		 {"role":"assistant","content":"set"},{"role":"user","content":"deploy now"},` + trailing + `]}`,
		"earlier tool result": `{"messages":[{"role":"user","content":"read it"},
		 {"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Read","input":{"path":".env"}}]},
		 {"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"` + ctx + `"}]}]},
		 {"role":"assistant","content":"read"},{"role":"user","content":"deploy now"},` + trailing + `]}`,
		"trailing prefill": `{"messages":[{"role":"user","content":"deploy now"},` + trailing + `,
		 {"role":"assistant","content":"Sure, with ` + ctx + `"}]}`,
	} {
		pt := PrepareRequest([]byte(body))
		if pt.Unreadable {
			t.Fatalf("%s: unreadable", name)
		}
		if l := leak(t, pt, v); l != "" {
			t.Errorf("%s: leaked %s", name, l)
		}
		if r := pt.State.ToolResults; len(r) != 1 || r[0].Content != "ok [REDACTED:generic-api-key]" {
			t.Errorf("%s: tool result = %+v", name, r)
		}
		if len(pt.Secrets) != 0 {
			t.Errorf("%s: hits = %+v, want none (the value is bare in what leaves)", name, pt.Secrets)
		}
	}
	// The prefill is still not judged: it is not the user's turn.
	pt := PrepareRequest([]byte(`{"messages":[{"role":"user","content":"deploy now"},{"role":"assistant","content":"Sure, with ` + ctx + `"}]}`))
	if st := stateJSON(t, pt); st["user_text"] != "deploy now" || len(st) != 2 {
		t.Errorf("prefill judged: %v", st)
	}
}

// A reply is cleaned of every value the request it answers holds, though
// the request stage never sends it (a system message's secret).
func TestSecretFromRequestRemovedFromReply(t *testing.T) {
	v := join("Qm7Lp2Vx", "9Rt4Kw8Zn3")
	req := `{"messages":[{"role":"system","content":"env: DB_PASSWORD=\"` + v + `\""},{"role":"user","content":"connect to the db"}]}`
	resp := `{"type":"message","role":"assistant","content":[{"type":"text","text":"Connecting with ` + v + ` now."},
	 {"type":"tool_use","id":"tu_r","name":"Bash","input":{"cmd":"psql --password ` + v + `"}}]}`
	pt, ok := PrepareResponse([]byte(req), []byte(resp))
	if !ok {
		t.Fatal("response not prepared")
	}
	if l := leak(t, pt, v); l != "" {
		t.Errorf("leaked %s", l)
	}
	if pt.State.ResponseText != "Connecting with [REDACTED:generic-api-key] now." {
		t.Errorf("response_text = %q", pt.State.ResponseText)
	}
}

// Two values that overlap in the text (the end of one is the start of the
// other) are one span, replaced once: replacing one, then the other, left
// the part of the second the first had covered.
func TestOverlappingSecretsRedactedWhole(t *testing.T) {
	s1, s2 := join("Zx9Qw7Er", "5Ty3Ui1Op"), join("Ui1OpMn8", "Bv6Cx4Za2")
	both := join("Zx9Qw7Er5Ty3", "Ui1OpMn8Bv6Cx4Za2")
	body, _ := json.Marshal(map[string]any{"messages": []map[string]any{{"role": "user",
		"content": `my api_key = "` + s1 + `" and token = "` + s2 + `"` + "\nconcat: " + both + " end"}}})
	pt := PrepareRequest(body)
	for _, v := range []string{s1, s2} {
		if l := leak(t, pt, v); l != "" {
			t.Errorf("leaked %s", l)
		}
	}
	if !strings.HasSuffix(pt.State.UserText, "concat: [REDACTED:generic-api-key] end") {
		t.Errorf("user_text = %q", pt.State.UserText)
	}
	// Spans: overlapping and touching occurrences merge, the longest value
	// names the marker, and text between spans is kept.
	ms := byLength([]secretMatch{{"a", "abcdefgh"}, {"b", "fghijklmnop"}, {"c", "qrstuvwx"}})
	if got := replaceSecrets("1 abcdefghijklmnopqrstuvwx 2 qrstuvwx 3", ms); got != "1 [REDACTED:b] 2 [REDACTED:c] 3" {
		t.Errorf("replaceSecrets = %q", got)
	}
}

// A tool input is raw JSON, where a value can be written escaped ("\/",
// "\"", "A"): it is matched and replaced in one canonical encoding,
// both ways (a value found in text removed from a JSON input; a value
// found inside a JSON string removed from text).
func TestEscapedSecretInToolInputRedacted(t *testing.T) {
	bs := `\`
	toolTurn := func(input, result string) []byte {
		return []byte(`{"messages":[{"role":"user","content":"run it"},
		 {"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":` + input + `}]},
		 {"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":` + jsonStr(result) + `}]}]}`)
	}
	slash := join("Tq8vRb3Nw/K7Mz2Gh", "/bPxRfiCYz9LmQs4Vd1Kc6Y")
	quote := join(`Pa"ss1234`, "5word")
	bare := join("Qm7Lp2Vx", "9Rt4Kw8Zn3")
	// bare with its first two characters written as \u escapes
	uesc := bs + "u0051" + bs + "u006d" + bare[2:]
	for _, c := range []struct {
		name, secret string
		body         []byte
	}{
		{`\/ in the input`, slash, toolTurn(`{"cmd":"aws configure set aws_secret_access_key `+strings.ReplaceAll(slash, "/", bs+"/")+`"}`,
			"aws_secret_access_key = "+slash)},
		{`\" in the input`, quote, toolTurn(`{"cmd":"psql '`+strings.ReplaceAll(quote, `"`, bs+`"`)+`'"}`,
			"DATABASE_URL=postgres://app:"+quote+"@db:5432/x")},
		{`\u escapes in the input`, bare, toolTurn(`{"cmd":"deploy `+uesc+`"}`, `api_key = "`+bare+`"`)},
		{`found inside the input's JSON`, quote, toolTurn(`{"url":"postgres://app:`+strings.ReplaceAll(quote, `"`, bs+`"`)+`@db:5432/x"}`,
			"the password is "+quote)},
	} {
		pt := PrepareRequest(c.body)
		if pt.Unreadable || len(pt.State.PriorToolCalls) != 1 {
			t.Fatalf("%s: not read: %+v", c.name, pt)
		}
		for _, v := range []string{c.secret, esc(c.secret)} {
			if l := leak(t, pt, v); l != "" {
				t.Errorf("%s: leaked %s", c.name, l)
			}
		}
		if in := pt.State.PriorToolCalls[0].Input; !strings.Contains(in, "[REDACTED:") || !json.Valid([]byte(in)) {
			t.Errorf("%s: input = %s", c.name, in)
		}
	}
}

// A long field is scanned at its kept ends plus scanOverlap. A secret the
// window's edge cuts was found as its prefix, and removing that prefix from
// another string left the rest: the cut match is dropped and the text
// across the edge scanned instead, so the secret is found whole.
func TestSecretAtScanWindowEdgeFoundWhole(t *testing.T) {
	sec := join("Qm7Lp2Vx", "9Rt4Kw8Zn3Hy6Jd")
	head, _ := clipBounds(strings.Repeat("a", 300000), capText)
	pre := `api_key = "`
	// The window's end falls this far into the secret: far enough that the
	// scan matches the part before it (a shorter part is not found at all:
	// what no window reaches is the windowed scan's limit).
	for _, into := range []int{12, 16, len(sec) - 1} {
		start := head + scanOverlap - into - len(pre)
		a := filler(start) + pre + sec + `" ` + filler(200000)
		if got := scanWindows(a, capText); len(got) != 1 || got[0].secret != sec {
			t.Errorf("into %d: window finds %v", into, got)
		}
		st := State{UserText: a, ToolResults: []ToolResult{{Tool: "Bash", Content: "value is " + sec}}}
		pt := prepareState(&st, StageRequest)
		if l := leak(t, pt, sec); l != "" {
			t.Errorf("into %d: leaked %s", into, l)
		}
	}
}
