package unit

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/geoip"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/symbolicate"
)

// sentinel-event-enrichment §10.2 TASK-05 "Switching storage from OBS to AWS S3 is a values/secret
// change only": the geo S3 client takes endpoint, region, bucket, prefix and path-style from config.
// The real minio-go client runs against an httptest fake through a capturing transport, which also
// dials the fake for any host, so a virtual-hosted request's Host header can be observed.

type capturedReq struct {
	method, host, path, auth, ifMatch string
}

type capturingTransport struct {
	mu   sync.Mutex
	reqs []capturedReq
	rt   http.RoundTripper
}

func (c *capturingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.mu.Lock()
	c.reqs = append(c.reqs, capturedReq{r.Method, r.Host, r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("If-Match")})
	c.mu.Unlock()
	return c.rt.RoundTrip(r)
}

// geoS3Fake serves one object at any path, with an ETag, and answers HEAD like S3. A GET whose If-Match
// names another ETag gets S3's 412 PreconditionFailed.
func geoS3Fake(t *testing.T, body []byte) (*httptest.Server, *capturingTransport) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if m := r.Header.Get("If-Match"); m != "" && m != `"fixture"` {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusPreconditionFailed)
			_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>PreconditionFailed</Code>`+
				`<Message>At least one of the pre-conditions you specified did not hold</Message></Error>`)
			return
		}
		w.Header().Set("ETag", `"fixture"`)
		w.Header().Set("Last-Modified", "Wed, 04 Feb 2026 22:49:29 GMT")
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		if r.Method == http.MethodHead {
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()
	base := &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}}
	return srv, &capturingTransport{rt: base}
}

func geoS3Roundtrip(t *testing.T, cfg geoip.Config) []capturedReq {
	t.Helper()
	src, err := geoip.NewS3Source(cfg)
	require.NoError(t, err)
	info, err := src.Stat(context.Background(), cfg.ObjectKey())
	require.NoError(t, err)
	assert.Equal(t, "fixture", info.ETag, "minio-go strips the quotes")
	rc, err := src.Get(context.Background(), cfg.ObjectKey(), info.ETag)
	require.NoError(t, err)
	b, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	assert.Equal(t, "mmdb-bytes", string(b))
	ct := cfg.Transport.(*capturingTransport)
	ct.mu.Lock()
	defer ct.mu.Unlock()
	return append([]capturedReq(nil), ct.reqs...)
}

func TestGeoIPS3PathStyle(t *testing.T) {
	srv, ct := geoS3Fake(t, []byte("mmdb-bytes"))
	reqs := geoS3Roundtrip(t, geoip.Config{
		Endpoint: srv.URL, Region: "us-east-1", Bucket: "sentinel-staging", Prefix: "geoip/",
		AccessKey: "ak", SecretKey: "sk", Transport: ct,
	})
	require.NotEmpty(t, reqs)
	for _, r := range reqs {
		assert.Equal(t, "/sentinel-staging/geoip/GeoLite2-City.mmdb", r.path, "an IP-literal endpoint is path-style")
		assert.Equal(t, srv.Listener.Addr().String(), r.host)
	}
	assert.Equal(t, http.MethodHead, reqs[0].method)
	assert.Equal(t, http.MethodGet, reqs[len(reqs)-1].method)
	assert.Equal(t, `"fixture"`, reqs[len(reqs)-1].ifMatch, "the Get is pinned to the Stat's ETag")
}

// enr-code-geoip-get-not-pinned-to-etag, through the real minio-go client: a Get pinned to an ETag the
// object no longer has fails with geoip.ErrETagMismatch (S3's 412), on Get or on the first Read.
func TestGeoIPS3GetETagMismatch(t *testing.T) {
	srv, ct := geoS3Fake(t, []byte("mmdb-bytes"))
	cfg := geoip.Config{
		Endpoint: srv.URL, Region: "us-east-1", Bucket: "sentinel-staging", Prefix: "geoip/",
		AccessKey: "ak", SecretKey: "sk", Transport: ct,
	}
	src, err := geoip.NewS3Source(cfg)
	require.NoError(t, err)
	rc, err := src.Get(context.Background(), cfg.ObjectKey(), "older")
	if err == nil {
		_, err = io.ReadAll(rc)
		_ = rc.Close()
	}
	require.Error(t, err)
	assert.ErrorIs(t, err, geoip.ErrETagMismatch)
}

func TestGeoIPS3VirtualHostAndRegion(t *testing.T) {
	_, ct := geoS3Fake(t, []byte("mmdb-bytes"))
	reqs := geoS3Roundtrip(t, geoip.Config{
		Endpoint: "http://obs.example.com", Region: "ap-southeast-1", Bucket: "sentinel-prod",
		Prefix: "geo/", ForcePathStyle: "false", AccessKey: "ak", SecretKey: "sk", Transport: ct,
	})
	require.NotEmpty(t, reqs)
	for _, r := range reqs {
		assert.Equal(t, "sentinel-prod.obs.example.com", r.host, "virtual-hosted: the bucket is in the Host")
		assert.Equal(t, "/geo/GeoLite2-City.mmdb", r.path, "the prefix is used verbatim")
		assert.Contains(t, r.auth, "/ap-southeast-1/s3/aws4_request", "the SigV4 scope carries the configured region")
	}
}

func TestGeoIPS3ForcePathStyleOverride(t *testing.T) {
	_, ct := geoS3Fake(t, []byte("mmdb-bytes"))
	reqs := geoS3Roundtrip(t, geoip.Config{
		Endpoint: "http://obs.example.com", Bucket: "sentinel-b", Prefix: "geoip/",
		ForcePathStyle: "true", AccessKey: "ak", SecretKey: "sk", Transport: ct,
	})
	for _, r := range reqs {
		assert.Equal(t, "/sentinel-b/geoip/GeoLite2-City.mmdb", r.path)
		assert.Contains(t, r.auth, "/us-east-1/s3/aws4_request", "an empty region defaults to us-east-1")
	}
}

// TestGeoIPUsePathStyleParity: the rule is DUPLICATED from the processor (spec §8.3); this holds the
// two copies equal over one table, so neither can drift silently.
func TestGeoIPUsePathStyleParity(t *testing.T) {
	hosts := []string{"minio", "minio:9000", "localhost", "127.0.0.1", "127.0.0.1:9000", "[::1]:9000", "[::1]",
		"obs.example.com", "s3.us-east-1.amazonaws.com", "", ":9000"}
	overrides := []string{"", "true", "false", "1", "0", " TRUE ", "maybe"}
	for _, h := range hosts {
		for _, o := range overrides {
			want, wantErr := symbolicate.UsePathStyle(h, o)
			got, gotErr := geoip.UsePathStyle(h, o)
			assert.Equalf(t, want, got, "host %q override %q", h, o)
			assert.Equalf(t, wantErr != nil, gotErr != nil, "host %q override %q: error parity", h, o)
		}
	}
}
