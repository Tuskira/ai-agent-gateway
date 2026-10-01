// Package s3 is a sink.BodyStore on S3-compatible object storage (AWS S3,
// Cloudflare R2, MinIO, ...). Each call's bodies become two objects:
//
//	<prefix>/<key>/request
//	<prefix>/<key>/response
//
// and the ref is "s3://<bucket>/<prefix>/<key>". Get reads the bucket from
// the ref, not from Config, so refs stay resolvable after a prefix change.
package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink"
)

const refScheme = "s3://"

// Config configures the S3 store. Only Bucket is required.
type Config struct {
	Bucket string
	// Prefix is prepended to every object key ("llm-bodies" → llm-bodies/<key>/request).
	// Leading/trailing slashes are ignored; empty stores at the bucket root.
	Prefix string
	// Region defaults to the AWS default chain's region (AWS_REGION, profile),
	// or "us-east-1" when Endpoint is set and none is found (MinIO ignores it,
	// but SigV4 needs one).
	Region string
	// Endpoint overrides the S3 endpoint for S3-compatible stores
	// (e.g. http://minio:9000). Empty = AWS S3.
	Endpoint string
	// ForcePathStyle addresses buckets as <endpoint>/<bucket> instead of
	// <bucket>.<endpoint>; MinIO and most self-hosted stores need it.
	ForcePathStyle bool
	// AccessKeyID/SecretAccessKey select static credentials. Empty = the AWS
	// default credential chain (env, shared profile, IRSA / instance role).
	AccessKeyID     string
	SecretAccessKey string
}

// Store writes bodies to one bucket. It implements sink.BodyStore.
type Store struct {
	client *awss3.Client
	bucket string
	prefix string
}

var _ sink.BodyStore = (*Store)(nil)

// New builds the S3 client. It does not contact the bucket: a bad bucket or
// credentials surface on the first Put, which the recorder turns into an
// inline fallback rather than a startup failure.
func New(ctx context.Context, c Config) (*Store, error) {
	if strings.TrimSpace(c.Bucket) == "" {
		return nil, errors.New("bodystore(s3): bucket is required")
	}
	var opts []func(*config.LoadOptions) error
	if c.Region != "" {
		opts = append(opts, config.WithRegion(c.Region))
	}
	if c.AccessKeyID != "" {
		opts = append(opts, config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(c.AccessKeyID, c.SecretAccessKey, "")))
	}
	awsCfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("bodystore(s3): load aws config: %w", err)
	}
	if awsCfg.Region == "" {
		if c.Endpoint == "" {
			return nil, errors.New("bodystore(s3): region is required (set region or AWS_REGION)")
		}
		awsCfg.Region = "us-east-1"
	}
	client := awss3.NewFromConfig(awsCfg, func(o *awss3.Options) {
		o.UsePathStyle = c.ForcePathStyle
		if c.Endpoint != "" {
			o.BaseEndpoint = aws.String(c.Endpoint)
			// Default (when-supported) checksums send headers not every
			// S3-compatible store accepts; only checksum where S3 requires it.
			o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
			o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
		}
	})
	return &Store{client: client, bucket: c.Bucket, prefix: strings.Trim(c.Prefix, "/")}, nil
}

// Put uploads the request then the response object and returns the ref.
func (s *Store) Put(ctx context.Context, key string, req, resp []byte) (string, error) {
	if key == "" || strings.Contains(key, "/") {
		return "", fmt.Errorf("bodystore(s3): invalid key %q", key)
	}
	base := key
	if s.prefix != "" {
		base = s.prefix + "/" + key
	}
	for _, o := range []struct {
		name string
		body []byte
	}{{"request", req}, {"response", resp}} {
		_, err := s.client.PutObject(ctx, &awss3.PutObjectInput{
			Bucket:        aws.String(s.bucket),
			Key:           aws.String(base + "/" + o.name),
			Body:          bytes.NewReader(o.body),
			ContentLength: aws.Int64(int64(len(o.body))),
			ContentType:   aws.String("application/octet-stream"),
		})
		if err != nil {
			return "", fmt.Errorf("bodystore(s3): put %s: %w", o.name, err)
		}
	}
	return refScheme + s.bucket + "/" + base, nil
}

// Get downloads both objects named by ref. A missing object is
// sink.ErrBodyNotFound.
func (s *Store) Get(ctx context.Context, ref string) ([]byte, []byte, error) {
	rest, ok := strings.CutPrefix(ref, refScheme)
	bucket, base, _ := strings.Cut(rest, "/")
	if !ok || bucket == "" || base == "" {
		return nil, nil, fmt.Errorf("bodystore(s3): not an s3 ref: %q", ref)
	}
	req, err := s.get(ctx, bucket, base+"/request")
	if err != nil {
		return nil, nil, err
	}
	resp, err := s.get(ctx, bucket, base+"/response")
	if err != nil {
		return nil, nil, err
	}
	return req, resp, nil
}

func (s *Store) get(ctx context.Context, bucket, key string) ([]byte, error) {
	out, err := s.client.GetObject(ctx, &awss3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		if isNotFound(err) {
			return nil, sink.ErrBodyNotFound
		}
		return nil, fmt.Errorf("bodystore(s3): get %s: %w", key, err)
	}
	defer out.Body.Close()
	b, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, fmt.Errorf("bodystore(s3): read %s: %w", key, err)
	}
	return b, nil
}

// isNotFound matches a missing object. Some S3-compatible stores return a
// generic API error rather than the typed NoSuchKey, so the code is checked too.
func isNotFound(err error) bool {
	var nsk *types.NoSuchKey
	if errors.As(err, &nsk) {
		return true
	}
	var apiErr smithy.APIError
	return errors.As(err, &apiErr) && (apiErr.ErrorCode() == "NoSuchKey" || apiErr.ErrorCode() == "NotFound")
}

// Close is a no-op: the SDK client holds only pooled HTTP connections.
func (s *Store) Close() error { return nil }
