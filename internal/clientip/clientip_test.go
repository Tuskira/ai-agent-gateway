package clientip

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func req(remote string, xff ...string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = remote
	for _, v := range xff {
		r.Header.Add("X-Forwarded-For", v)
	}
	return r
}

func TestIP(t *testing.T) {
	cidr := MustNew("10.0.0.0/8", "192.168.1.1", "fd00::/8")
	tests := []struct {
		name string
		res  *Resolver
		req  *http.Request
		want string
	}{
		{"nil resolver ignores xff", nil, req("203.0.113.9:1234", "198.51.100.7"), "203.0.113.9"},
		{"no trusted proxies ignores xff", MustNew(), req("127.0.0.1:1", "198.51.100.7"), "127.0.0.1"},
		{"untrusted peer ignores xff", cidr, req("203.0.113.9:1", "198.51.100.7"), "203.0.113.9"},
		{"trusted peer, single client", cidr, req("10.1.2.3:1", "198.51.100.7"), "198.51.100.7"},
		{"trusted peer, no xff", cidr, req("10.1.2.3:1"), "10.1.2.3"},
		{"rightmost untrusted wins over spoofed leftmost", cidr, req("10.1.2.3:1", "1.1.1.1, 198.51.100.7"), "198.51.100.7"},
		{"spoofed leftmost cannot frame victim", cidr, req("10.1.2.3:1", "198.51.100.7, 203.0.113.50"), "203.0.113.50"},
		{"skips trusted hops from the right", cidr, req("10.1.2.3:1", "198.51.100.7, 10.9.9.9, 10.8.8.8"), "198.51.100.7"},
		{"single-IP trusted entry", cidr, req("192.168.1.1:1", "198.51.100.7"), "198.51.100.7"},
		{"all trusted -> leftmost", cidr, req("10.1.2.3:1", "10.5.5.5, 10.6.6.6"), "10.5.5.5"},
		{"multiple header lines are one list", cidr, req("10.1.2.3:1", "1.1.1.1", "198.51.100.7"), "198.51.100.7"},
		{"malformed entry stops walk", cidr, req("10.1.2.3:1", "198.51.100.7, garbage, 10.9.9.9"), "10.9.9.9"},
		{"malformed rightmost returns peer", cidr, req("10.1.2.3:1", "198.51.100.7, garbage"), "10.1.2.3"},
		{"empty entry is malformed", cidr, req("10.1.2.3:1", "198.51.100.7,, 10.9.9.9"), "10.9.9.9"},
		{"ipv6 client", cidr, req("10.1.2.3:1", "2001:db8::1"), "2001:db8::1"},
		{"ipv6 bracketed with port", cidr, req("10.1.2.3:1", "[2001:db8::1]:443"), "2001:db8::1"},
		{"ipv4 with port", cidr, req("10.1.2.3:1", "198.51.100.7:5555"), "198.51.100.7"},
		{"ipv6 trusted peer", cidr, req("[fd00::5]:1", "2001:db8::2"), "2001:db8::2"},
		{"ipv4-mapped normalized", cidr, req("10.1.2.3:1", "::ffff:198.51.100.7"), "198.51.100.7"},
		{"ipv4-mapped peer matches v4 cidr", cidr, req("[::ffff:10.1.2.3]:1", "198.51.100.7"), "198.51.100.7"},
		{"unparseable peer returned verbatim", cidr, req("@", "198.51.100.7"), "@"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.res.IP(tc.req); got != tc.want {
				t.Fatalf("IP = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNewErrors(t *testing.T) {
	for _, bad := range []string{"10.0.0.0/99", "not-an-ip", "10.0.0.256"} {
		if _, err := New([]string{bad}); err == nil {
			t.Errorf("New(%q) = nil error, want error", bad)
		}
	}
	if r, err := New([]string{"", "  "}); err != nil || r.Trusts() {
		t.Errorf("blank entries: err=%v trusts=%v", err, r.Trusts())
	}
}
