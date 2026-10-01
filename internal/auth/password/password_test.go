package password

import (
	"errors"
	"strings"
	"testing"
)

func TestHashVerify(t *testing.T) {
	h, err := Hash("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=65536,t=3,p=2$") {
		t.Errorf("hash = %q, want argon2id PHC with m=65536,t=3,p=2", h)
	}
	if ok, err := Verify("correct horse battery", h); err != nil || !ok {
		t.Errorf("Verify(correct) = %v, %v; want true, nil", ok, err)
	}
	if ok, err := Verify("correct horse batterz", h); err != nil || ok {
		t.Errorf("Verify(wrong) = %v, %v; want false, nil", ok, err)
	}
	h2, _ := Hash("correct horse battery")
	if h == h2 {
		t.Error("two hashes of the same password are identical: salt is not random")
	}
}

func TestVerifyMalformed(t *testing.T) {
	good, _ := Hash("whatever-password")
	parts := strings.Split(good, "$")
	cases := map[string]string{
		"empty":            "",
		"plain text":       "hunter2",
		"wrong algorithm":  strings.Replace(good, "argon2id", "argon2i", 1),
		"too few parts":    strings.Join(parts[:5], "$"),
		"bad version":      strings.Replace(good, "v=19", "v=16", 1),
		"bad params":       strings.Replace(good, "m=65536,t=3,p=2", "m=x,t=y,p=z", 1),
		"zero memory":      strings.Replace(good, "m=65536", "m=0", 1),
		"huge memory":      strings.Replace(good, "m=65536", "m=4194304", 1),
		"huge iterations":  strings.Replace(good, "t=3", "t=999", 1),
		"zero lanes":       strings.Replace(good, "p=2", "p=0", 1),
		"bad salt base64":  strings.Join([]string{"", parts[1], parts[2], parts[3], "!!!", parts[5]}, "$"),
		"bad hash base64":  strings.Join([]string{"", parts[1], parts[2], parts[3], parts[4], "!!!"}, "$"),
		"empty hash field": strings.Join([]string{"", parts[1], parts[2], parts[3], parts[4], ""}, "$"),
	}
	for name, enc := range cases {
		ok, err := Verify("whatever-password", enc)
		if ok || !errors.Is(err, ErrMalformedHash) {
			t.Errorf("%s: Verify = %v, %v; want false, ErrMalformedHash", name, ok, err)
		}
	}
}

func TestDummyVerifyDoesNotPanicAndNeverMatches(t *testing.T) {
	DummyVerify("anything at all")
	DummyVerify("")
}

func TestValidatePolicy(t *testing.T) {
	cases := []struct {
		name, pw, user string
		want           error
	}{
		{"ok", "a-long-enough-pw", "alice", nil},
		{"exactly min", strings.Repeat("a", MinLength), "alice", nil},
		{"one short", strings.Repeat("a", MinLength-1), "alice", ErrTooShort},
		{"empty", "", "alice", ErrTooShort},
		{"exactly max", strings.Repeat("a", MaxLength), "alice", nil},
		{"one over max", strings.Repeat("a", MaxLength+1), "alice", ErrTooLong},
		{"equals username", "alice@example.com", "alice@example.com", ErrEqualsUsername},
		{"equals username other case", "Alice@Example.com", "alice@example.com", ErrEqualsUsername},
		{"contains username is fine", "alice@example.com-2026", "alice@example.com", nil},
		{"counts runes not bytes", strings.Repeat("é", MinLength), "alice", nil},
		{"multi-byte too short", strings.Repeat("é", MinLength-1), "alice", ErrTooShort},
	}
	for _, c := range cases {
		if got := ValidatePolicy(c.pw, c.user); !errors.Is(got, c.want) {
			t.Errorf("%s: ValidatePolicy = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestGenerateTemporary(t *testing.T) {
	seen := map[string]bool{}
	used := map[rune]bool{}
	for i := 0; i < 500; i++ {
		pw, err := GenerateTemporary()
		if err != nil {
			t.Fatal(err)
		}
		if len(pw) != TempLength {
			t.Fatalf("len = %d, want %d", len(pw), TempLength)
		}
		for _, r := range pw {
			if !strings.ContainsRune(tempAlphabet, r) {
				t.Fatalf("password %q has character %q outside the alphabet", pw, r)
			}
			used[r] = true
		}
		if seen[pw] {
			t.Fatalf("duplicate temporary password %q", pw)
		}
		seen[pw] = true
		if err := ValidatePolicy(pw, "x"); err != nil {
			t.Fatalf("a generated password fails the policy: %v", err)
		}
	}
	for _, amb := range "0O1Il" {
		if strings.ContainsRune(tempAlphabet, amb) {
			t.Errorf("alphabet contains ambiguous character %q", amb)
		}
	}
	if len(used) != len(tempAlphabet) {
		t.Errorf("500 passwords used %d of %d alphabet characters; sampling looks biased", len(used), len(tempAlphabet))
	}
}
