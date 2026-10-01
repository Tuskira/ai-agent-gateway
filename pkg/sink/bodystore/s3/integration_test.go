//go:build integration

// Run against a real S3-compatible store, e.g. MinIO (the minio/minio Docker
// Hub image is no longer published; Chainguard's build is):
//
//	docker run -d --rm --name bs-minio --tmpfs /data:uid=65532,gid=65532 -p 127.0.0.1:19000:9000 \
//	  -e MINIO_ROOT_USER=minio -e MINIO_ROOT_PASSWORD=minio12345 cgr.dev/chainguard/minio server /data
//	GATEWAY_TEST_S3_ENDPOINT=http://127.0.0.1:19000 GATEWAY_TEST_S3_ACCESS_KEY=minio \
//	  GATEWAY_TEST_S3_SECRET_KEY=minio12345 go test -tags integration ./pkg/sink/bodystore/s3/... -v
//	docker rm -f bs-minio
package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink"
)

func TestIntegration_PutGet(t *testing.T) {
	endpoint := os.Getenv("GATEWAY_TEST_S3_ENDPOINT")
	accessKey := os.Getenv("GATEWAY_TEST_S3_ACCESS_KEY")
	secretKey := os.Getenv("GATEWAY_TEST_S3_SECRET_KEY")
	if endpoint == "" || accessKey == "" || secretKey == "" {
		t.Skip("GATEWAY_TEST_S3_ENDPOINT/ACCESS_KEY/SECRET_KEY not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	bucket := fmt.Sprintf("gw-bodystore-it-%d", time.Now().UnixNano())
	s, err := New(ctx, Config{
		Bucket: bucket, Prefix: "llm-bodies", Region: "us-east-1", Endpoint: endpoint,
		ForcePathStyle: true, AccessKeyID: accessKey, SecretAccessKey: secretKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.client.CreateBucket(ctx, &awss3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		list, err := s.client.ListObjectsV2(ctx, &awss3.ListObjectsV2Input{Bucket: aws.String(bucket)})
		if err == nil {
			for _, o := range list.Contents {
				_, _ = s.client.DeleteObject(ctx, &awss3.DeleteObjectInput{Bucket: aws.String(bucket), Key: o.Key})
			}
		}
		if _, err := s.client.DeleteBucket(ctx, &awss3.DeleteBucketInput{Bucket: aws.String(bucket)}); err != nil {
			t.Errorf("delete bucket: %v", err)
		}
	})

	req, resp := []byte(`{"model":"claude"}`), append([]byte("event\x00"), bytes.Repeat([]byte("x"), 1<<20)...)
	ref, err := s.Put(ctx, "req-1", req, resp)
	if err != nil {
		t.Fatal(err)
	}
	if want := "s3://" + bucket + "/llm-bodies/req-1"; ref != want {
		t.Errorf("ref = %q, want %q", ref, want)
	}
	head, err := s.client.HeadObject(ctx, &awss3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String("llm-bodies/req-1/response")})
	if err != nil {
		t.Fatal(err)
	}
	if ct := aws.ToString(head.ContentType); ct != "application/octet-stream" {
		t.Errorf("content-type = %q", ct)
	}
	gotReq, gotResp, err := s.Get(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotReq, req) || !bytes.Equal(gotResp, resp) {
		t.Errorf("round trip mismatch: req %d/%d bytes, resp %d/%d bytes", len(gotReq), len(req), len(gotResp), len(resp))
	}

	// Empty request body round-trips as empty.
	ref, err = s.Put(ctx, "req-empty", nil, []byte("only-response"))
	if err != nil {
		t.Fatal(err)
	}
	gotReq, gotResp, err = s.Get(ctx, ref)
	if err != nil || len(gotReq) != 0 || string(gotResp) != "only-response" {
		t.Errorf("empty-body Get = %q / %q / %v", gotReq, gotResp, err)
	}

	if _, _, err := s.Get(ctx, "s3://"+bucket+"/llm-bodies/does-not-exist"); !errors.Is(err, sink.ErrBodyNotFound) {
		t.Errorf("bogus ref err = %v, want ErrBodyNotFound", err)
	}
}
