package apikey

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGenerate(t *testing.T) {
	plaintext, hash, prefix, err := Generate()
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	if !strings.HasPrefix(plaintext, Prefix) {
		t.Errorf("plaintext = %q, want prefix %q", plaintext, Prefix)
	}
	if got := Hash(plaintext); got != hash {
		t.Errorf("Hash(plaintext) = %q, want %q", got, hash)
	}
	if prefix != plaintext[:PrefixDisplayLen] {
		t.Errorf("prefix = %q, want %q", prefix, plaintext[:PrefixDisplayLen])
	}
	if len(prefix) != PrefixDisplayLen {
		t.Errorf("len(prefix) = %d, want %d", len(prefix), PrefixDisplayLen)
	}
}

func TestGenerate_Unique(t *testing.T) {
	p1, h1, _, err := Generate()
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	p2, h2, _, err := Generate()
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	if p1 == p2 || h1 == h2 {
		t.Error("two Generate() calls produced the same key")
	}
}

func TestHash_Deterministic(t *testing.T) {
	if Hash("gk_abc") != Hash("gk_abc") {
		t.Error("Hash is not deterministic")
	}
	if Hash("gk_abc") == Hash("gk_abd") {
		t.Error("Hash collided for different inputs")
	}
}

func TestParseBearer(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(r *http.Request)
		want   string
		wantOK bool
	}{
		{
			name:   "authorization bearer gk",
			setup:  func(r *http.Request) { r.Header.Set("Authorization", "Bearer gk_abc123") },
			want:   "gk_abc123",
			wantOK: true,
		},
		{
			name:   "x-gateway-key gk",
			setup:  func(r *http.Request) { r.Header.Set("X-Gateway-Key", "gk_xyz789") },
			want:   "gk_xyz789",
			wantOK: true,
		},
		{
			name:   "no headers",
			setup:  func(r *http.Request) {},
			wantOK: false,
		},
		{
			name:   "non-gk bearer (e.g. JWT)",
			setup:  func(r *http.Request) { r.Header.Set("Authorization", "Bearer eyJhbGciOiJIUzI1NiJ9.x.y") },
			wantOK: false,
		},
		{
			name:   "authorization not bearer scheme",
			setup:  func(r *http.Request) { r.Header.Set("Authorization", "Basic dXNlcjpwYXNz") },
			wantOK: false,
		},
		{
			name:   "x-gateway-key non-gk",
			setup:  func(r *http.Request) { r.Header.Set("X-Gateway-Key", "sk_notours") },
			wantOK: false,
		},
		{
			name: "x-gateway-key takes precedence over authorization",
			setup: func(r *http.Request) {
				r.Header.Set("Authorization", "Bearer gk_fromauth")
				r.Header.Set("X-Gateway-Key", "gk_fromheader")
			},
			want:   "gk_fromheader",
			wantOK: true,
		},
		{
			// The collision case: a BYOK provider key in Authorization (OpenAI
			// "Bearer sk-...") must NOT shadow a valid gateway key in
			// X-Gateway-Key. The gateway key wins; Authorization rides along to
			// the upstream provider untouched.
			name: "byok authorization does not shadow x-gateway-key",
			setup: func(r *http.Request) {
				r.Header.Set("Authorization", "Bearer sk-openai-byok")
				r.Header.Set("X-Gateway-Key", "gk_gatewaykey")
			},
			want:   "gk_gatewaykey",
			wantOK: true,
		},
		{
			// Regression guard: a present-but-invalid X-Gateway-Key must fall
			// through to a valid gateway key in Authorization rather than
			// short-circuit to a 401.
			name: "invalid x-gateway-key falls through to authorization gk",
			setup: func(r *http.Request) {
				r.Header.Set("X-Gateway-Key", "not-a-gateway-key")
				r.Header.Set("Authorization", "Bearer gk_fromauth")
			},
			want:   "gk_fromauth",
			wantOK: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			tt.setup(r)

			got, ok := ParseBearer(r)
			if ok != tt.wantOK {
				t.Fatalf("ParseBearer() ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Errorf("ParseBearer() = %q, want %q", got, tt.want)
			}
		})
	}
}
