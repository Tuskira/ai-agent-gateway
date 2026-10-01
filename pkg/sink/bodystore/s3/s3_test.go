package s3

import (
	"context"
	"testing"
)

// Unit checks that need no server; the S3 round trip is integration_test.go.

func TestNew_Validation(t *testing.T) {
	ctx := context.Background()
	if _, err := New(ctx, Config{}); err == nil {
		t.Error("New without bucket succeeded, want error")
	}
	// A custom endpoint with no region anywhere falls back to us-east-1.
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")
	t.Setenv("AWS_CONFIG_FILE", t.TempDir()+"/none")
	s, err := New(ctx, Config{Bucket: "b", Endpoint: "http://127.0.0.1:1", AccessKeyID: "k", SecretAccessKey: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.client.Options().Region; got != "us-east-1" {
		t.Errorf("region = %q, want us-east-1", got)
	}
}

func TestPutGet_RejectsBadInput(t *testing.T) {
	ctx := context.Background()
	s, err := New(ctx, Config{Bucket: "b", Prefix: "/p/", Region: "us-east-1", Endpoint: "http://127.0.0.1:1", AccessKeyID: "k", SecretAccessKey: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if s.prefix != "p" {
		t.Errorf("prefix = %q, want slashes trimmed", s.prefix)
	}
	for _, key := range []string{"", "a/b"} {
		if _, err := s.Put(ctx, key, nil, nil); err == nil {
			t.Errorf("Put(%q) succeeded, want error", key)
		}
	}
	for _, ref := range []string{"fs://x", "s3://", "s3://bucket", "s3:///key"} {
		if _, _, err := s.Get(ctx, ref); err == nil {
			t.Errorf("Get(%q) succeeded, want error", ref)
		}
	}
}
