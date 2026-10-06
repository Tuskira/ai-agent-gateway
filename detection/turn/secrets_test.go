package turn

import (
	"strings"
	"sync"
	"testing"
)

// Fixture secrets are assembled at run time so the repo's own gitleaks
// scan does not flag this file.
func join(parts ...string) string { return strings.Join(parts, "") }

func TestScanSecrets(t *testing.T) {
	awsSecret := join("wJalrXUtnFEMI/K7MDENG", "/bPxRfiCYzq8Tq3Rk2Z")
	for _, tc := range []struct {
		name, text, kind string // kind "" = nothing found
	}{
		{"aws access key id", "AWS_ACCESS_KEY_ID=" + join("AKIA", "Z3MFKR7QW2LXB5TN"), "aws-access-token"},
		{"aws secret key", "aws_secret_access_key = " + awsSecret, "generic-api-key"},
		{"postgres password", "DATABASE_URL=postgres://app:" + join("S3cr3t", "Passw0rd") + "@db.internal:5432/prod", "connection-string-password"},
		{"mongodb+srv password", "mongodb+srv://svc:" + join("hunter2", "Longer") + "@cluster0.x.mongodb.net/app", "connection-string-password"},
		{"clipped private key", join("-----BEGIN OPENSSH ", "PRIVATE KEY-----\n") + strings.Repeat("b3BlbnNzaC1rZXktdjEAAAAABG5vbmU", 3) + "\n…[truncated]…", "private-key-unterminated"},
		{"aws documented example", "AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE", ""},
		{"template filler", "AWS_ACCESS_KEY_ID=AKIAXXXXXXXXXXXXXXXX", ""},
		{"env placeholder in url", "postgres://app:${DB_PASSWORD}@db:5432/prod", ""},
		{"angle placeholder in url", "postgres://app:<password>@db:5432/prod", ""},
		{"url without password", "postgres://app@db:5432/prod", ""},
		{"plain prose", "Ship the billing migration by Oct 15.", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := scanSecrets(tc.text)
			if tc.kind == "" {
				if len(got) != 0 {
					t.Fatalf("found %+v, want nothing", got)
				}
				return
			}
			found := false
			for _, m := range got {
				found = found || m.kind == tc.kind
			}
			if !found {
				t.Fatalf("found %+v, want kind %s", got, tc.kind)
			}
			if red := redactSecrets(tc.text); strings.Contains(red, got[0].secret) || !strings.Contains(red, "[REDACTED:") {
				t.Errorf("redacted = %q", red)
			}
		})
	}
}

// Redaction keeps the context around the secret and marks the rule.
func TestRedactSecretsKeepsContext(t *testing.T) {
	in := "connect with postgres://app:" + join("S3cr3t", "Passw0rd") + "@db.internal:5432/prod then run migrations"
	got := redactSecrets(in)
	want := "connect with postgres://app:[REDACTED:connection-string-password]@db.internal:5432/prod then run migrations"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

// The detector is shared by every request goroutine.
func TestScanSecretsConcurrent(t *testing.T) {
	text := "key " + join("AKIA", "Z3MFKR7QW2LXB5TN")
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				if len(scanSecrets(text)) != 1 {
					t.Error("missed the key")
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestPrivateKeyBlockRedactedWhole(t *testing.T) {
	body := `{"messages":[{"role":"user","content":"check this\n` + join("-----BEGIN OPENSSH ", "PRIVATE KEY-----") + `\nb3BlbnNzaC1rZXktdjEAAAAABG5vbmU\nAAAAEbm9uZQAAAAAAAAAB\n` + join("-----END OPENSSH ", "PRIVATE KEY-----") + `\nthanks"}]}`
	pt := PrepareRequest([]byte(body))
	if hits := pt.Secrets; len(hits) != 1 || hits[0].Kind != "private-key" {
		t.Fatalf("hits = %+v", hits)
	}
	s := pt.State
	if strings.Contains(s.UserText, "b3BlbnNzaC1rZXkt") || !strings.Contains(s.UserText, "[REDACTED:private-key]") || !strings.Contains(s.UserText, "thanks") {
		t.Errorf("redacted = %q", s.UserText)
	}
}
