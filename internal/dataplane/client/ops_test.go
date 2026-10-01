package client

import (
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

func TestServerRequests(t *testing.T) {
	cases := []struct {
		name string
		conn *store.Connector
		want ServerRequestPolicy
	}{
		{
			name: "nil connector",
			conn: nil,
			want: ServerRequestPolicy{},
		},
		{
			name: "absent metadata",
			conn: &store.Connector{},
			want: ServerRequestPolicy{},
		},
		{
			name: "absent server_requests key",
			conn: &store.Connector{Metadata: map[string]any{"tool_arg_overrides": map[string]any{"x": "y"}}},
			want: ServerRequestPolicy{},
		},
		{
			name: "all true",
			conn: &store.Connector{Metadata: map[string]any{
				"server_requests": map[string]any{"sampling": true, "elicitation": true, "roots": true},
			}},
			want: ServerRequestPolicy{Sampling: true, Elicitation: true, Roots: true},
		},
		{
			name: "partial object: only sampling set",
			conn: &store.Connector{Metadata: map[string]any{
				"server_requests": map[string]any{"sampling": true},
			}},
			want: ServerRequestPolicy{Sampling: true},
		},
		{
			name: "explicit false round-trips false",
			conn: &store.Connector{Metadata: map[string]any{
				"server_requests": map[string]any{"sampling": false, "elicitation": true},
			}},
			want: ServerRequestPolicy{Elicitation: true},
		},
		{
			name: "wrong type: string instead of bool is treated as false",
			conn: &store.Connector{Metadata: map[string]any{
				"server_requests": map[string]any{"sampling": "true", "elicitation": true},
			}},
			want: ServerRequestPolicy{Elicitation: true},
		},
		{
			name: "wrong type: number instead of bool is treated as false",
			conn: &store.Connector{Metadata: map[string]any{
				"server_requests": map[string]any{"roots": 1},
			}},
			want: ServerRequestPolicy{},
		},
		{
			name: "wrong type: server_requests itself not an object",
			conn: &store.Connector{Metadata: map[string]any{"server_requests": "on"}},
			want: ServerRequestPolicy{},
		},
		{
			name: "unknown key inside server_requests is ignored, known keys still read",
			conn: &store.Connector{Metadata: map[string]any{
				"server_requests": map[string]any{"sampling": true, "bogus": true},
			}},
			want: ServerRequestPolicy{Sampling: true},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ServerRequests(tc.conn)
			if got != tc.want {
				t.Errorf("ServerRequests() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestServerRequestPolicy_Allows(t *testing.T) {
	cases := []struct {
		name   string
		policy ServerRequestPolicy
		method string
		want   bool
	}{
		{"sampling allowed", ServerRequestPolicy{Sampling: true}, "sampling/createMessage", true},
		{"sampling denied", ServerRequestPolicy{}, "sampling/createMessage", false},
		{"elicitation allowed", ServerRequestPolicy{Elicitation: true}, "elicitation/create", true},
		{"elicitation denied", ServerRequestPolicy{}, "elicitation/create", false},
		{"roots allowed", ServerRequestPolicy{Roots: true}, "roots/list", true},
		{"roots denied", ServerRequestPolicy{}, "roots/list", false},
		{
			name:   "a field being true does not leak into another method",
			policy: ServerRequestPolicy{Sampling: true, Elicitation: true, Roots: true},
			method: "unknown/method",
			want:   false,
		},
		{"unknown method on empty policy", ServerRequestPolicy{}, "unknown/method", false},
		{"empty method string", ServerRequestPolicy{Sampling: true}, "", false},
		{"case-sensitive: wrong case is unknown", ServerRequestPolicy{Sampling: true}, "Sampling/CreateMessage", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.policy.Allows(tc.method); got != tc.want {
				t.Errorf("Allows(%q) = %v, want %v", tc.method, got, tc.want)
			}
		})
	}
}
