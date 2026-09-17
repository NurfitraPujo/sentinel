package symbolicate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
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
	// ForcePathStyle is the raw S3_FORCE_PATH_STYLE value ("" = unset): an explicit addressing-style
	// override that wins over UsePathStyle's host rule. Parsed in NewS3Fetcher, so a typo is a
	// startup error naming the variable, never a silent default.
	ForcePathStyle string
}

// UsePathStyle decides whether endpointHost (a `host` or `host:port`, as url.URL.Host gives it)
// is addressed path-style (`http://host/bucket/key`) or virtual-hosted (`https://bucket.host/key`).
// The rule is by host shape, so nothing in a deployment has to say which flavour of store it is:
//
//   - a bare hostname with no dots (`minio`, `localhost`) or an IP literal (`127.0.0.1`, `[::1]`)
//     -> path-style. That is MinIO in compose/dev/tests, where `bucket.minio` resolves nowhere.
//   - anything with a dot (`obs.ap-southeast-3.myhuaweicloud.com`, `s3.us-east-1.amazonaws.com`)
//     -> virtual-hosted, which every hosted S3-compatible store supports and Huawei OBS REQUIRES:
//     path-style there is "VirtualHostDomainRequired", which is how the first production sourcemap
//     upload failed 4/4 on 2026-09-17 while both clients still hard-coded path-style.
//
// override is S3_FORCE_PATH_STYLE and wins when non-empty, in strconv.ParseBool syntax (`true`,
// `false`, `1`, `0`, ...); any other value is an error naming the variable. The dashboard's
// storage.ts `usePathStyle` is the same rule for the write side and the two must not drift: this
// fetcher reads what that module wrote, through the same endpoint.
func UsePathStyle(endpointHost, override string) (bool, error) {
	if v := strings.TrimSpace(override); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return false, fmt.Errorf("%s: %q is not a boolean (true/false)", EnvS3ForcePathStyle, override)
		}
		return b, nil
	}
	host := endpointHost
	if h, _, err := net.SplitHostPort(endpointHost); err == nil {
		host = h // strips the port and the IPv6 brackets
	}
	host = strings.Trim(host, "[]") // a bracketed IPv6 literal with no port has no port to split
	if host == "" {
		return false, fmt.Errorf("%s: %q has no host", EnvS3Endpoint, endpointHost)
	}
	if net.ParseIP(host) != nil {
		return true, nil
	}
	return !strings.Contains(host, "."), nil
}

// S3Fetcher is the production Fetcher: one GET of `storage_key` from the bucket the dashboard
// wrote it to (§4.1). Built like apps/dashboard-web/src/lib/server/storage.ts's client -- the
// addressing style decided per endpoint by UsePathStyle, and region `us-east-1` (MinIO ignores
// it; the SDK's signer needs one). No checksum trailer, no retries beyond the client's own: a
// failure is `fetch_error` and the negative cache stops the retry storm.
type S3Fetcher struct {
	client    *minio.Client
	bucket    string
	readLimit int64
	pathStyle bool
}

// PathStyle reports the addressing style the client was built with (true = path-style). The
// minio client does not expose its BucketLookup, so this is what the unit test reads.
func (f *S3Fetcher) PathStyle() bool { return f.pathStyle }

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
	pathStyle, err := UsePathStyle(u.Host, cfg.ForcePathStyle)
	if err != nil {
		return nil, fmt.Errorf("symbolicate: %w", err)
	}
	lookup := minio.BucketLookupDNS
	if pathStyle {
		lookup = minio.BucketLookupPath
	}
	client, err := minio.New(u.Host, &minio.Options{
		Creds:        credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure:       strings.EqualFold(u.Scheme, "https"),
		Region:       "us-east-1",
		BucketLookup: lookup,
	})
	if err != nil {
		return nil, fmt.Errorf("symbolicate: s3 client: %w", err)
	}
	return &S3Fetcher{client: client, bucket: cfg.Bucket, readLimit: maxParseBytes + 1, pathStyle: pathStyle}, nil
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
