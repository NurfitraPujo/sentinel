package symbolicate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3Config is the processor's object-storage configuration -- the same four variables the
// dashboard's storage.ts reads (S3_ENDPOINT, S3_BUCKET, S3_ACCESS_KEY, S3_SECRET_KEY; contract
// §12, be.md task 6 builds it in main.go). Endpoint is a URL with a scheme (`http://minio:9000`,
// `https://obs.ap-southeast-3.myhuaweicloud.com`): the scheme decides TLS, exactly as the
// dashboard's client takes it.
type S3Config struct {
	Endpoint  string
	Bucket    string
	AccessKey string
	SecretKey string
}

// S3Fetcher is the production Fetcher: one GET of `storage_key` from the bucket the dashboard
// wrote it to (§4.1). Built like apps/dashboard-web/src/lib/server/storage.ts's client -- path-style
// addressing (MinIO has no virtual-host DNS; harmless against real S3/OBS) and region
// `us-east-1` (MinIO ignores it; the SDK's signer needs one). No checksum trailer, no retries
// beyond the client's own: a failure is `fetch_error` and the negative cache stops the retry
// storm.
type S3Fetcher struct {
	client    *minio.Client
	bucket    string
	readLimit int64
}

// NewS3Fetcher builds the client. maxParseBytes is Config.MaxParseBytes (decision 38): the
// service never asks for a row over it, so the body read is bounded at one byte past it (see
// Fetch). It does not touch the network: a wrong endpoint is a `fetch_error` per map at runtime
// (D7), never a startup failure -- the startup check of decision 32 is only for ABSENT
// credentials, in main.go.
func NewS3Fetcher(cfg S3Config, maxParseBytes int64) (*S3Fetcher, error) {
	if maxParseBytes <= 0 {
		return nil, errors.New("symbolicate: maxParseBytes must be positive (Config.MaxParseBytes)")
	}
	if cfg.Endpoint == "" || cfg.Bucket == "" || cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, errors.New("symbolicate: S3_ENDPOINT, S3_BUCKET, S3_ACCESS_KEY and S3_SECRET_KEY are all required")
	}
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("symbolicate: S3_ENDPOINT %q is not a URL with a scheme and host", cfg.Endpoint)
	}
	client, err := minio.New(u.Host, &minio.Options{
		Creds:        credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure:       strings.EqualFold(u.Scheme, "https"),
		Region:       "us-east-1",
		BucketLookup: minio.BucketLookupPath,
	})
	if err != nil {
		return nil, fmt.Errorf("symbolicate: s3 client: %w", err)
	}
	return &S3Fetcher{client: client, bucket: cfg.Bucket, readLimit: maxParseBytes + 1}, nil
}

// Fetch GETs the whole object. The service has already refused rows over MaxParseBytes (decision
// 38), so the read is bounded at that cap plus one byte -- an object longer than its row's
// size_bytes claimed is cut there and fails the sha256 check as `map_error` rather than growing
// the process without bound.
func (f *S3Fetcher) Fetch(ctx context.Context, storageKey string) ([]byte, error) {
	obj, err := f.client.GetObject(ctx, f.bucket, storageKey, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", storageKey, err)
	}
	defer obj.Close()
	raw, err := io.ReadAll(io.LimitReader(obj, f.readLimit))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", storageKey, err)
	}
	return raw, nil
}
