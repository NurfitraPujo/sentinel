// Package geoip serves GeoLite2-City lookups from a database held in memory, loaded from S3-compatible
// object storage and reloaded without a restart when a newer object appears (sentinel-event-enrichment
// spec §5). Pods never contact MaxMind: only the geoip-refresh subcommand does, and it lives in
// geoip/refresh, which nothing on the serving path imports (§8.1).
//
// Every failure means "no geo", never an error: no configuration, a missing, unreadable, corrupt,
// wrong-type or stale database. Each is logged once per state change and never per event (§5.5).
package geoip

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/NurfitraPujo/sentinel/packages/shared-go/obs"
)

// The environment the pods and the refresh job read (spec §8.3). None collides with the shared
// S3_* keys the ConfigMap renders.
const (
	EnvS3Endpoint       = "GEOIP_S3_ENDPOINT"
	EnvS3Region         = "GEOIP_S3_REGION"
	EnvS3Bucket         = "GEOIP_S3_BUCKET"
	EnvS3Prefix         = "GEOIP_S3_PREFIX"
	EnvS3ForcePathStyle = "GEOIP_S3_FORCE_PATH_STYLE"
	EnvS3AccessKey      = "GEOIP_S3_ACCESS_KEY"
	EnvS3SecretKey      = "GEOIP_S3_SECRET_KEY"
	EnvPollInterval     = "GEOIP_POLL_INTERVAL"
	EnvMaxAge           = "GEOIP_MAX_AGE"
)

const (
	// ObjectName is the database's object name under the configured prefix.
	ObjectName = "GeoLite2-City.mmdb"
	// MaxObjectBytes caps a database read into memory (spec §5.4, §8.2). A larger object is corrupt.
	MaxObjectBytes = 256 << 20

	DefaultRegion       = "us-east-1"
	DefaultPrefix       = "geoip/"
	DefaultPollInterval = time.Hour
	DefaultMaxAge       = 720 * time.Hour // the GeoLite EULA's 30 days (spec §5.4)
	DefaultRetryBackoff = 2 * time.Second // first pause before retrying a transport failure in one poll

	minPollInterval = time.Minute
	minMaxAge       = time.Hour
)

// Config is the geo storage configuration, shared by the pods' Loader and the refresh job.
type Config struct {
	Endpoint       string // URL with scheme; https means TLS
	Region         string // SigV4 region
	Bucket         string // empty = geo disabled
	Prefix         string // used verbatim; the object is Prefix+ObjectName
	ForcePathStyle string // "" = the host-shape rule; otherwise strconv.ParseBool syntax
	AccessKey      string
	SecretKey      string

	PollInterval time.Duration // pods only
	MaxAge       time.Duration // pods only: older than this by build time is stale

	// RetryBackoff is the first pause before a poll retries a transport failure; 0 means
	// DefaultRetryBackoff. Not reachable from env: tests shrink it.
	RetryBackoff time.Duration

	// Now is the clock staleness is measured against; nil means time.Now. Not reachable from env:
	// tests inject it, because MaxMind's test database is older than DefaultMaxAge (spec §5.4).
	Now func() time.Time

	// Transport, when set, replaces the S3 client's HTTP transport. TEST-ONLY: the S3 client tests
	// capture the requests the real minio-go client sends through it (spec §10.2 TASK-05).
	Transport http.RoundTripper
}

// ObjectKey is the database's key in the bucket.
func (c Config) ObjectKey() string { return c.Prefix + ObjectName }

// ObjectPath is bucket/key, for log lines (not secret).
func (c Config) ObjectPath() string { return c.Bucket + "/" + c.ObjectKey() }

func (c Config) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// ConfigFromEnv reads spec §8.3's environment through lookup (os.LookupEnv in main). A poll interval
// under 1m or a max age under 1h, or either unparseable, falls back to the default with one Warn line:
// a geo misconfiguration costs a feature, never the pod (§5.4).
func ConfigFromEnv(lookup func(string) (string, bool), logger *slog.Logger) Config {
	get := func(k, def string) string {
		if v, ok := lookup(k); ok {
			return v
		}
		return def
	}
	c := Config{
		Endpoint:       strings.TrimSpace(get(EnvS3Endpoint, "")),
		Region:         strings.TrimSpace(get(EnvS3Region, "")),
		Bucket:         strings.TrimSpace(get(EnvS3Bucket, "")),
		Prefix:         get(EnvS3Prefix, DefaultPrefix),
		ForcePathStyle: get(EnvS3ForcePathStyle, ""),
		AccessKey:      get(EnvS3AccessKey, ""),
		SecretKey:      get(EnvS3SecretKey, ""),
	}
	if c.Region == "" {
		c.Region = DefaultRegion
	}
	c.PollInterval = durationEnv(lookup, logger, EnvPollInterval, DefaultPollInterval, minPollInterval)
	c.MaxAge = durationEnv(lookup, logger, EnvMaxAge, DefaultMaxAge, minMaxAge)
	return c
}

func durationEnv(lookup func(string) (string, bool), logger *slog.Logger, key string, def, min time.Duration) time.Duration {
	raw, ok := lookup(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return def
	}
	d, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil || d < min {
		logger.Warn(fmt.Sprintf("%s: %q is not a Go duration of at least %s; using %s", key, raw, min, def),
			slog.String(obs.LogKeyEvent, "geoip.config_default"))
		return def
	}
	return d
}

// UsePathStyle is apps/processor-go/symbolicate.UsePathStyle, DUPLICATED rather than imported so the
// processor stays untouched and its sourcemap libraries stay out of the ingestor binary (spec §8.3).
// tests/unit/ingestor_geoip_s3_test.go runs both over one host table so they cannot drift silently.
//
// A bare hostname with no dots or an IP literal is path-style (MinIO in compose and tests); anything
// with a dot is virtual-hosted, which Huawei OBS requires. A non-empty override wins, in
// strconv.ParseBool syntax; any other value is an error naming the variable.
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
		host = h
	}
	host = strings.Trim(host, "[]")
	if host == "" {
		return false, fmt.Errorf("%s: %q has no host", EnvS3Endpoint, endpointHost)
	}
	if net.ParseIP(host) != nil {
		return true, nil
	}
	return !strings.Contains(host, "."), nil
}

// NewS3Client builds the minio-go client from c: endpoint scheme decides TLS, region from config, and
// path-style or virtual-host by UsePathStyle. It does not touch the network. The refresh job builds
// its client here too, so the pods and the job cannot address storage differently.
func NewS3Client(c Config) (*minio.Client, error) {
	if c.Endpoint == "" || c.Bucket == "" || c.AccessKey == "" || c.SecretKey == "" {
		return nil, fmt.Errorf("%s, %s, %s and %s are all required", EnvS3Endpoint, EnvS3Bucket, EnvS3AccessKey, EnvS3SecretKey)
	}
	u, err := url.Parse(c.Endpoint)
	if err != nil || u.Host == "" || u.Scheme == "" {
		return nil, fmt.Errorf("%s: %q is not a URL with a scheme and host", EnvS3Endpoint, c.Endpoint)
	}
	pathStyle, err := UsePathStyle(u.Host, c.ForcePathStyle)
	if err != nil {
		return nil, err
	}
	lookup := minio.BucketLookupDNS
	if pathStyle {
		lookup = minio.BucketLookupPath
	}
	region := c.Region
	if region == "" {
		region = DefaultRegion
	}
	return minio.New(u.Host, &minio.Options{
		Creds:        credentials.NewStaticV4(c.AccessKey, c.SecretKey, ""),
		Secure:       strings.EqualFold(u.Scheme, "https"),
		Region:       region,
		BucketLookup: lookup,
		Transport:    c.Transport,
	})
}

// ErrNotFound is what an ObjectSource returns when the object does not exist.
var ErrNotFound = errors.New("geoip: object not found")

// ErrETagMismatch is what an ObjectSource returns, from Get or from reading its body, when the object
// is no longer the one the ETag names (S3 412 Precondition Failed on If-Match).
var ErrETagMismatch = errors.New("geoip: object changed since stat (ETag mismatch)")

// ObjectInfo is what the loader needs from a stat.
type ObjectInfo struct {
	ETag         string
	Size         int64
	UserMetadata map[string]string
}

// ObjectSource is the loader's seam over object storage (spec §7.3): Stat on every poll, Get only when
// the ETag changed. Tests substitute a counting fake to prove the request path makes no S3 call.
//
// Get is pinned to etag, the one the preceding Stat returned (empty = unpinned): if the object changed
// in between, Get or the body's Read fails with ErrETagMismatch rather than handing back bytes that
// would be recorded under the wrong ETag.
type ObjectSource interface {
	Stat(ctx context.Context, key string) (ObjectInfo, error)
	Get(ctx context.Context, key, etag string) (io.ReadCloser, error)
}

// S3Source is the production ObjectSource over one bucket.
type S3Source struct {
	Client *minio.Client
	Bucket string
}

// NewS3Source builds an S3Source from c (see NewS3Client).
func NewS3Source(c Config) (*S3Source, error) {
	client, err := NewS3Client(c)
	if err != nil {
		return nil, err
	}
	return &S3Source{Client: client, Bucket: c.Bucket}, nil
}

func (s *S3Source) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	info, err := s.Client.StatObject(ctx, s.Bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return ObjectInfo{}, classifyS3(err)
	}
	return ObjectInfo{ETag: info.ETag, Size: info.Size, UserMetadata: info.UserMetadata}, nil
}

// Get sends If-Match: etag. minio-go's GetObject is lazy, so a 412 usually surfaces on the first Read;
// s3Body classifies read errors the same way Get does.
func (s *S3Source) Get(ctx context.Context, key, etag string) (io.ReadCloser, error) {
	opts := minio.GetObjectOptions{}
	if etag != "" {
		if err := opts.SetMatchETag(etag); err != nil {
			return nil, err
		}
	}
	obj, err := s.Client.GetObject(ctx, s.Bucket, key, opts)
	if err != nil {
		return nil, classifyS3(err)
	}
	return &s3Body{obj}, nil
}

type s3Body struct{ obj *minio.Object }

func (b *s3Body) Read(p []byte) (int, error) {
	n, err := b.obj.Read(p)
	if err != nil && err != io.EOF {
		err = classifyS3(err)
	}
	return n, err
}

func (b *s3Body) Close() error { return b.obj.Close() }

func classifyS3(err error) error {
	resp := minio.ToErrorResponse(err)
	if resp.StatusCode == http.StatusNotFound || resp.Code == minio.NoSuchKey || resp.Code == minio.NoSuchBucket {
		return fmt.Errorf("%w: %v", ErrNotFound, err)
	}
	if resp.StatusCode == http.StatusPreconditionFailed || resp.Code == "PreconditionFailed" {
		return fmt.Errorf("%w: %v", ErrETagMismatch, err)
	}
	return err
}
