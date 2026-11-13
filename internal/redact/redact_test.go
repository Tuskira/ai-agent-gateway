package redact

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Test vectors are assembled at run time from harmless pieces so the
// source holds no literal credential for a secret scanner to flag. The AWS
// key id is the documented example value.

const awsKeyID = "AKIAIOSFODNN7EXAMPLE"

func b64url(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

// fakeJWT is header.payload.signature with a JSON header, as a real token.
func fakeJWT() string {
	return b64url(`{"alg":"HS256","typ":"JWT"}`) + "." + b64url(`{"sub":"user-1","name":"test"}`) + "." + b64url("not-a-real-signature-bytes")
}

func fakeAWSSecret() string { return strings.Repeat("wJalrXUtnFEMIK7MDENGbPxRfiCYEXAMPLEKEY", 2)[:40] }

func fakePrivateKey(sep string) string {
	return "-----BEGIN " + "RSA PRIVATE KEY-----" + sep +
		strings.Repeat("MIIEowIBAAKCAQEA", 4) + sep +
		strings.Repeat("ZmFrZWtleWJ5dGVz", 4) + sep +
		"-----END " + "RSA PRIVATE KEY-----"
}

func fakeSKKey(prefix string) string { return prefix + strings.Repeat("Ab3dEf6hIj", 4) }

var on = Policy{On: true, Plane: LLM}

func scrub(s string) string { return string(on.Scrub([]byte(s))) }

func TestScrub_EachKind(t *testing.T) {
	jwt := fakeJWT()
	cases := []struct {
		name, in, want string
		gone           string // must not survive
	}{
		{"aws key id", "creds: " + awsKeyID + " in output", "creds: [REDACTED:aws_key_id] in output", awsKeyID},
		{"aws sts key id", "ASIA" + "ABCDEFGHIJKLMNOP", "[REDACTED:aws_key_id]", "ABCDEFGHIJKLMNOP"},
		{"aws secret labelled env", "aws_secret_access_key = " + fakeAWSSecret() + "\n", "aws_secret_access_key = [REDACTED:aws_secret]\n", fakeAWSSecret()},
		{"aws secret labelled json", `{"SecretAccessKey": "` + fakeAWSSecret() + `"}`, `{"SecretAccessKey": "[REDACTED:aws_secret]"}`, fakeAWSSecret()},
		{"jwt", "token " + jwt + " end", "token [REDACTED:jwt] end", jwt},
		{"jwt as bearer", "Authorization: Bearer " + jwt, "Authorization: Bearer [REDACTED:jwt]", jwt},
		{"bearer", "authorization: bearer abcdefghijklmnop0123456789", "authorization: bearer [REDACTED:bearer]", "abcdefghijklmnop0123456789"},
		{"anthropic style key", "key=" + fakeSKKey("sk-ant-api03-"), "key=[REDACTED:api_key]", fakeSKKey("sk-ant-api03-")},
		{"openai style key", `"api_key": "` + fakeSKKey("sk-proj-") + `"`, `"api_key": "[REDACTED:api_key]"`, fakeSKKey("sk-proj-")},
		{"gateway key", "x-gateway-key: " + fakeSKKey("gk_"), "x-gateway-key: [REDACTED:api_key]", fakeSKKey("gk_")},
		{"private key", "pem:\n" + fakePrivateKey("\n") + "\nafter", "pem:\n[REDACTED:private_key]\nafter", "MIIEowIBAAKCAQEA"},
		{"private key escaped in json", `{"pem":"` + fakePrivateKey(`\n`) + `"}`, `{"pem":"[REDACTED:private_key]"}`, "MIIEowIBAAKCAQEA"},
		{"json password", `{"user":"bob","password":"hunter2!"}`, `{"user":"bob","password":"[REDACTED:password]"}`, "hunter2!"},
		{"json db_password", `{"db_password" : "s3cr3t-value"}`, `{"db_password" : "[REDACTED:password]"}`, "s3cr3t-value"},
		{"json client_secret", `{"client_secret":"abc123xyz"}`, `{"client_secret":"[REDACTED:secret]"}`, "abc123xyz"},
		{"escaped json password", `{"text":"{\"password\": \"hunter2\"}"}`, `{"text":"{\"password\": \"[REDACTED:password]\"}"}`, "hunter2"},
		{"kv password", "login password=hunter2&next=1", "login password=[REDACTED:password]&next=1", "hunter2"},
		{"kv secret yaml", "client_secret: abc123xyz\nother: 1", "client_secret: [REDACTED:secret]\nother: 1", "abc123xyz"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := scrub(c.in)
			if got != c.want {
				t.Errorf("scrub(%q)\n got %q\nwant %q", c.in, got, c.want)
			}
			if strings.Contains(got, c.gone) {
				t.Errorf("secret survived: %q", got)
			}
		})
	}
}

// Ordinary text must come through unchanged.
func TestScrub_LeavesOrdinaryTextAlone(t *testing.T) {
	img := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x89, 'P', 'N', 'G', 0, 1, 2, 3, 250, 251}, 400))
	cases := []string{
		"Click the password reset link to choose a new password.",
		"Your password must be at least 12 characters; secrets are stored in the vault.",
		"The bearer of this letter is a friend.",
		"Bearer tokens are short-lived.",
		"request id 550e8400-e29b-41d4-a716-446655440000 failed",
		"sha256 e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		"trace 4bf92f3577b34da6a3ce929d0e0e4736 span 00f067aa0ba902b7 req_011CSHtK9uPLSMMxUj9sdi4n",
		"commit da39a3ee5e6b4b0d3255bfef95601890afd80709 and AKIA alone",
		"risk-assessment-framework-for-the-whole-team and task-0123456789abcdefghij",
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + img + `"}}`,
		`{"password_required": true, "secret_count": 3, "password": null, "password": ""}`,
		"eyJhbGciOi is only a prefix",
		"Travelling in ASIA and AKIAs",
	}
	for _, in := range cases {
		if got := scrub(in); got != in {
			t.Errorf("over-redacted:\n in %q\ngot %q", in, got)
		}
	}
}

func TestScrub_KeepsJSONValid(t *testing.T) {
	doc := map[string]any{
		"model": "m",
		"messages": []any{map[string]any{"role": "user", "content": "use " + awsKeyID +
			" and Bearer abcdefghijklmnop0123456789 and " + fakeJWT() + "\n" + fakePrivateKey("\n")}},
		"tool_result": `{"password": "hunter2", "SecretAccessKey": "` + fakeAWSSecret() + `"}`,
		"password":    `p"a\ss`,
		"api_key":     fakeSKKey("sk-"),
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	got := on.Scrub(raw)
	if bytes.Equal(got, raw) {
		t.Fatal("nothing was redacted")
	}
	var back map[string]any
	if err := json.Unmarshal(got, &back); err != nil {
		t.Fatalf("scrubbed body is not valid JSON: %v\n%s", err, got)
	}
	for _, kind := range []string{KindAWSKeyID, KindBearer, KindJWT, KindPrivateKey, KindPassword, KindAWSSecret, KindAPIKey} {
		if !bytes.Contains(got, []byte("[REDACTED:"+kind+"]")) {
			t.Errorf("no %s marker in %s", kind, got)
		}
	}
	for _, secret := range []string{awsKeyID, fakeJWT(), "hunter2", fakeAWSSecret(), fakeSKKey("sk-"), "MIIEowIBAAKCAQEA"} {
		if bytes.Contains(got, []byte(secret)) {
			t.Errorf("secret %q survived in %s", secret, got)
		}
	}
}

func TestScrub_Idempotent(t *testing.T) {
	in := "a " + awsKeyID + ` {"password":"x1"} password=y2 Bearer abcdefghijklmnop0123456789 ` + fakeJWT() + " " + fakeSKKey("sk-ant-")
	once := scrub(in)
	before := counter(LLM, KindPassword)
	twice := scrub(once)
	if once != twice {
		t.Errorf("not idempotent:\nonce  %q\ntwice %q", once, twice)
	}
	if after := counter(LLM, KindPassword); after != before {
		t.Errorf("second pass counted %d redactions", after-before)
	}
}

func TestScrub_DoesNotModifyInput(t *testing.T) {
	in := []byte("key " + awsKeyID)
	orig := append([]byte(nil), in...)
	out := on.Scrub(in)
	if !bytes.Equal(in, orig) {
		t.Fatalf("input modified: %q", in)
	}
	if bytes.Equal(out, in) {
		t.Fatal("nothing redacted")
	}
	clean := []byte("nothing here")
	if got := on.Scrub(clean); &got[0] != &clean[0] {
		t.Error("a clean body should be returned as is, without a copy")
	}
}

func TestPolicy_Off(t *testing.T) {
	off := Policy{On: false, Plane: MCP}
	in := "k " + awsKeyID
	if got := string(off.Scrub([]byte(in))); got != in {
		t.Errorf("off scrubbed: %q", got)
	}
	got, cut := off.Body([]byte(in), 5, false)
	if string(got) != in[:5] || !cut {
		t.Errorf("off Body = %q, %v; want the plain cap", got, cut)
	}
}

func TestBody_ScrubsBeforeCap(t *testing.T) {
	// The key starts before the cap and ends after it: capping first would
	// store "AKIAIOSF" with nothing to recognise it by.
	in := "0123456789 " + awsKeyID + " tail"
	got, cut := on.Body([]byte(in), 19, false)
	if !cut {
		t.Error("truncated flag lost")
	}
	if strings.Contains(string(got), "AKIA") {
		t.Errorf("partial key stored: %q", got)
	}
	if string(got) != "0123456789 [REDACTE" {
		t.Errorf("got %q", got)
	}

	// Fits after scrubbing: not truncated.
	got, cut = on.Body([]byte("k "+awsKeyID), 100, false)
	if cut || string(got) != "k [REDACTED:aws_key_id]" {
		t.Errorf("got %q, %v", got, cut)
	}
}

// A body already cut at the source (a bounded stream capture) has its
// tail checked: the secret's end is gone, so only its start is there.
func TestBody_CutTail(t *testing.T) {
	cases := []struct{ in, want string }{
		{"data: " + awsKeyID[:10], "data: [REDACTED:aws_key_id]"},
		{"tok " + fakeJWT()[:30], "tok [REDACTED:jwt]"},
		{"Authorization: Bearer abc", "Authorization: Bearer [REDACTED:bearer]"},
		{`{"password":"hun`, `{"password":"[REDACTED:password]`},
		{"client_secret=ab", "client_secret=[REDACTED:secret]"},
		{"k " + fakeSKKey("sk-ant-")[:12], "k [REDACTED:api_key]"},
		{"pem " + fakePrivateKey("\n")[:60], "pem [REDACTED:private_key]"},
		{"aws_secret_access_key=" + fakeAWSSecret()[:12], "aws_secret_access_key=[REDACTED:aws_secret]"},
		{"plain text ends mid-word", "plain text ends mid-word"},
	}
	for _, c := range cases {
		got, cut := on.Body([]byte(c.in), 0, true)
		if string(got) != c.want || !cut {
			t.Errorf("Body(%q, cut) = %q, %v; want %q", c.in, got, cut, c.want)
		}
	}
	// A long cut body: only its end is checked, and the head is kept.
	long := strings.Repeat("x", 200<<10) + " Authorization: Bearer abc"
	if got, _ := on.Body([]byte(long), 0, true); !strings.HasSuffix(string(got), "Bearer [REDACTED:bearer]") || len(got) < 200<<10 {
		t.Errorf("long cut body tail = %q", got[len(got)-40:])
	}
	// Not cut: the tail rules do not apply.
	if got, _ := on.Body([]byte("Authorization: Bearer abc"), 0, false); string(got) != "Authorization: Bearer abc" {
		t.Errorf("tail rule applied to a whole body: %q", got)
	}
}

func TestCounts(t *testing.T) {
	before := counter(MCP, KindJWT)
	Policy{On: true, Plane: MCP}.Scrub([]byte(fakeJWT() + " " + fakeJWT()))
	if got := counter(MCP, KindJWT) - before; got != 2 {
		t.Errorf("mcp jwt count +%d, want +2", got)
	}
	n := 0
	Range(func(plane, kind string, _ uint64) {
		n++
		if plane != "llm" && plane != "mcp" {
			t.Errorf("plane %q", plane)
		}
		if _, ok := kindIndex[kind]; !ok {
			t.Errorf("kind %q", kind)
		}
	})
	if n != 2*len(kinds) {
		t.Errorf("Range yielded %d series", n)
	}
}

// A 4 MiB mixed body scrubs in well under a second (linear-time RE2).
func TestScrub_LargeBody(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	var b strings.Builder
	for b.Len() < 4<<20 {
		b.WriteString(`{"role":"user","content":"Lorem ipsum dolor sit amet, password reset 550e8400-e29b-41d4-a716-446655440000 `)
		b.WriteString(strings.Repeat("A", 200))
		b.WriteString(`"},`)
	}
	b.WriteString(awsKeyID)
	start := time.Now()
	out := on.Scrub([]byte(b.String()))
	el := time.Since(start)
	if !bytes.HasSuffix(out, []byte("[REDACTED:aws_key_id]")) {
		t.Error("key at the end not redacted")
	}
	// The race detector slows regexp matching several times over; the
	// bound is only meaningful without it.
	if !raceEnabled && el > 2*time.Second {
		t.Errorf("4 MiB scrub took %v", el)
	}
	t.Logf("4 MiB scrub: %v", el)
}

func BenchmarkScrub1MiB(b *testing.B) {
	body := []byte(strings.Repeat(`{"role":"user","content":"Lorem ipsum dolor sit amet, consectetur adipiscing elit"},`, (1<<20)/80))
	b.SetBytes(int64(len(body)))
	for b.Loop() {
		on.Scrub(body)
	}
}

func counter(p Plane, kind string) uint64 { return counts[p][kindIndex[kind]].Load() }
