package unit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/clientip"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/enrich"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/geoip"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/service"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/validation"
)

// sentinel-event-enrichment spec §5 / §10.2 TASK-03: the reloadable GeoLite2-City reader and the geo
// fill, over MaxMind's committed test databases (tests/fixtures/geoip, §5.6) and a fake object source.

// geoBuildEpoch is both fixtures' Metadata.BuildEpoch (spec §5.6). Both are older than the default
// 720h max age, so every test that expects an answer injects Now relative to it.
var geoBuildEpoch = time.Date(2026, 2, 4, 22, 49, 29, 0, time.UTC)

func geoFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "fixtures", "geoip", name))
	require.NoError(t, err)
	return b
}

// fakeGeoSource is a counting geoip.ObjectSource. body == nil means the object does not exist.
type fakeGeoSource struct {
	mu      sync.Mutex
	etag    string
	body    []byte
	statErr error
	stats   int
	gets    int

	// getErrs fails the next getErrs Gets with a transport error; readErrs hands the next readErrs Gets
	// a body that breaks mid-read. Both model a connection reset, never a bad object.
	getErrs  int
	readErrs int
	// statErrs fails the next statErrs Stats with a transport error (an S3 5xx, a DNS blip).
	statErrs int
	// swapOnGet, when set, replaces the object just before the next Get is served: the refresh job's
	// PutObject landing between a pod's Stat and its Get.
	swapOnGet *struct {
		etag string
		body []byte
	}
}

// geoBrokenBody is a body whose connection drops after a few bytes.
type geoBrokenBody struct{ r io.Reader }

func (b *geoBrokenBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if err == io.EOF {
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}

func (f *fakeGeoSource) serve(etag string, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.etag, f.body, f.statErr = etag, body, nil
}

func (f *fakeGeoSource) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stats, f.gets
}

func (f *fakeGeoSource) Stat(_ context.Context, _ string) (geoip.ObjectInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stats++
	if f.statErr != nil {
		return geoip.ObjectInfo{}, f.statErr
	}
	if f.statErrs > 0 {
		f.statErrs--
		return geoip.ObjectInfo{}, errors.New("503 Service Unavailable")
	}
	if f.body == nil {
		return geoip.ObjectInfo{}, geoip.ErrNotFound
	}
	return geoip.ObjectInfo{ETag: f.etag, Size: int64(len(f.body))}, nil
}

func (f *fakeGeoSource) Get(_ context.Context, _, etag string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	if sw := f.swapOnGet; sw != nil {
		f.swapOnGet = nil
		f.etag, f.body = sw.etag, sw.body
	}
	if f.body == nil {
		return nil, geoip.ErrNotFound
	}
	if etag != "" && etag != f.etag {
		return nil, geoip.ErrETagMismatch
	}
	if f.getErrs > 0 {
		f.getErrs--
		return nil, errors.New("read tcp 10.0.0.9:41234->obs: connection reset by peer")
	}
	if f.readErrs > 0 {
		f.readErrs--
		return io.NopCloser(&geoBrokenBody{r: bytes.NewReader(append([]byte(nil), f.body[:1024]...))}), nil
	}
	return io.NopCloser(bytes.NewReader(append([]byte(nil), f.body...))), nil
}

// geoLog captures the loader's JSON log lines.
type geoLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (g *geoLog) Write(p []byte) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.buf.Write(p)
}

func (g *geoLog) lines(t *testing.T) []map[string]any {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(g.buf.String()), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(l), &m), l)
		out = append(out, m)
	}
	return out
}

func (g *geoLog) reset() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.buf.Reset()
}

type geoClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *geoClock) Now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *geoClock) set(t time.Time) { c.mu.Lock(); defer c.mu.Unlock(); c.now = t }

func newGeoLoader(src geoip.ObjectSource, clock *geoClock) (*geoip.Loader, *geoLog) {
	log := &geoLog{}
	cfg := geoip.Config{Bucket: "sentinel-test", Prefix: "geoip/", MaxAge: geoip.DefaultMaxAge, Now: clock.Now, RetryBackoff: time.Millisecond}
	return geoip.NewLoaderWithSource(cfg, src, slog.New(slog.NewJSONHandler(log, nil))), log
}

// geoIngestGeo runs one payload with a public body IP through the real IngestAt, with the loader as the
// enricher, and returns the published user.geo (nil when none). IngestAt must succeed whatever the
// loader's state.
func geoIngestGeo(t *testing.T, loader *geoip.Loader) map[string]string {
	t.Helper()
	pub := &fakeEventPublisher{}
	svc, err := service.NewIngestService(pub, service.WithEnricher(&enrich.Enricher{Geo: loader}))
	require.NoError(t, err)
	p := validIngestPayload()
	p.User = &validation.UserInfo{IPAddress: "81.2.69.142"}
	require.NoError(t, svc.IngestAt(context.Background(), p, time.Now().UTC()))
	ev := publishedEvent(t, pub)
	require.NotNil(t, ev.GetUser())
	if ev.GetUser().GetGeo() == nil {
		return nil
	}
	g := ev.GetUser().GetGeo()
	return map[string]string{"city": g.GetCity(), "region": g.GetRegion(), "country_code": g.GetCountryCode(), "subdivision": g.GetSubdivision()}
}

var geoLondon = map[string]string{"city": "London", "region": "United Kingdom", "country_code": "GB", "subdivision": "England"}

func geoStateLines(t *testing.T, log *geoLog) []map[string]any {
	var out []map[string]any
	for _, l := range log.lines(t) {
		if l["event"] == geoip.EventState {
			out = append(out, l)
		}
	}
	return out
}

func TestGeoIPMissingLogsOnceAndIngestionContinues(t *testing.T) {
	clock := &geoClock{now: geoBuildEpoch.Add(24 * time.Hour)}
	src := &fakeGeoSource{}
	loader, log := newGeoLoader(src, clock)
	for i := 0; i < 5; i++ {
		loader.Poll(context.Background())
	}
	lines := log.lines(t)
	require.Len(t, lines, 1, "five not-found polls write exactly one line: %v", lines)
	assert.Equal(t, "unavailable", lines[0]["to"])
	assert.Equal(t, "not_found", lines[0]["reason"])
	assert.Equal(t, "WARN", lines[0]["level"])
	assert.Nil(t, geoIngestGeo(t, loader), "no database: no geo, and the event is still accepted")
}

func TestGeoIPCorruptLogsOnceAndIngestionContinues(t *testing.T) {
	clock := &geoClock{now: geoBuildEpoch.Add(24 * time.Hour)}
	full := geoFixture(t, "GeoLite2-City-Test.mmdb")
	src := &fakeGeoSource{}
	src.serve(`"c"`, full[:len(full)/2]) // truncated in-test; no broken binary is committed
	loader, log := newGeoLoader(src, clock)
	for i := 0; i < 5; i++ {
		loader.Poll(context.Background())
	}
	lines := log.lines(t)
	require.Len(t, lines, 1, "five corrupt polls write exactly one line: %v", lines)
	assert.Equal(t, "unavailable", lines[0]["to"])
	assert.Equal(t, "corrupt", lines[0]["reason"])
	assert.Nil(t, geoIngestGeo(t, loader))
}

func TestGeoIPFetchErrorLogsOnce(t *testing.T) {
	clock := &geoClock{now: geoBuildEpoch.Add(24 * time.Hour)}
	src := &fakeGeoSource{statErr: io.ErrUnexpectedEOF}
	loader, log := newGeoLoader(src, clock)
	for i := 0; i < 5; i++ {
		loader.Poll(context.Background())
	}
	lines := log.lines(t)
	require.Len(t, lines, 1)
	assert.Equal(t, "fetch_error", lines[0]["reason"])
	assert.Contains(t, lines[0]["error"], "unexpected EOF")
	assert.Nil(t, geoIngestGeo(t, loader))
}

func TestGeoIPStale(t *testing.T) {
	for _, c := range []struct {
		age   time.Duration
		state string
		geo   map[string]string
	}{
		{719 * time.Hour, "ready", geoLondon},
		{721 * time.Hour, "stale", nil},
	} {
		t.Run(c.state, func(t *testing.T) {
			clock := &geoClock{now: geoBuildEpoch.Add(c.age)}
			src := &fakeGeoSource{}
			src.serve(`"a"`, geoFixture(t, "GeoLite2-City-Test.mmdb"))
			loader, log := newGeoLoader(src, clock)
			for i := 0; i < 5; i++ {
				loader.Poll(context.Background())
			}
			lines := log.lines(t)
			require.Len(t, lines, 1, "%v", lines)
			assert.Equal(t, c.state, lines[0]["to"])
			assert.Equal(t, "2026-02-04T22:49:29Z", lines[0]["build_time"])
			assert.Equal(t, c.geo, geoIngestGeo(t, loader))
		})
	}
}

// TestGeoIPStalenessReevaluatedEveryTick: a database that ages past the limit while running stops
// answering within one poll, with no new object and no fetch.
func TestGeoIPStalenessReevaluatedEveryTick(t *testing.T) {
	clock := &geoClock{now: geoBuildEpoch.Add(719 * time.Hour)}
	src := &fakeGeoSource{}
	src.serve(`"a"`, geoFixture(t, "GeoLite2-City-Test.mmdb"))
	loader, log := newGeoLoader(src, clock)
	loader.Poll(context.Background())
	require.Equal(t, geoLondon, geoIngestGeo(t, loader))

	clock.set(geoBuildEpoch.Add(721 * time.Hour))
	loader.Poll(context.Background())
	_, gets := src.counts()
	assert.Equal(t, 1, gets, "an unchanged ETag is never fetched again")
	lines := geoStateLines(t, log)
	require.Len(t, lines, 2)
	assert.Equal(t, "stale", lines[1]["to"])
	assert.Nil(t, geoIngestGeo(t, loader))
}

// geoRetype rewrites the metadata's database_type (same length, so the file stays valid and verifies).
func geoRetype(t *testing.T, b []byte, from, to string) []byte {
	t.Helper()
	require.Equal(t, len(from), len(to))
	out := append([]byte(nil), b...)
	meta := bytes.LastIndex(out, []byte("\xab\xcd\xefMaxMind.com"))
	require.Positive(t, meta)
	i := bytes.Index(out[meta:], []byte(from))
	require.Positive(t, i)
	copy(out[meta+i:], to)
	return out
}

func TestGeoIPWrongType(t *testing.T) {
	clock := &geoClock{now: geoBuildEpoch.Add(24 * time.Hour)}
	src := &fakeGeoSource{}
	src.serve(`"w"`, geoRetype(t, geoFixture(t, "GeoLite2-City-Test.mmdb"), "GeoLite2-City", "GeoLite2-Asn0"))
	loader, log := newGeoLoader(src, clock)
	for i := 0; i < 5; i++ {
		loader.Poll(context.Background())
	}
	lines := log.lines(t)
	require.Len(t, lines, 1, "%v", lines)
	assert.Equal(t, "unavailable", lines[0]["to"])
	assert.Equal(t, "wrong_type", lines[0]["reason"])
	assert.Nil(t, geoIngestGeo(t, loader))
}

func TestGeoIPRecovery(t *testing.T) {
	clock := &geoClock{now: geoBuildEpoch.Add(24 * time.Hour)}
	src := &fakeGeoSource{}
	loader, log := newGeoLoader(src, clock)
	loader.Poll(context.Background())
	loader.Poll(context.Background())
	src.serve(`"a"`, geoFixture(t, "GeoLite2-City-Test.mmdb"))
	for i := 0; i < 3; i++ {
		loader.Poll(context.Background())
	}
	lines := geoStateLines(t, log)
	require.Len(t, lines, 2, "%v", lines)
	assert.Equal(t, "unavailable", lines[0]["to"])
	assert.Equal(t, "ready", lines[1]["to"], "recovery writes exactly one ready line")
	assert.Equal(t, "INFO", lines[1]["level"])
	assert.Equal(t, geoLondon, geoIngestGeo(t, loader))
}

// TestGeoIPReload: ETag "a" then "b" swaps without a restart. Both fixtures share a build epoch, so
// the swap is asserted through DatabaseType and the geoip.reloaded line, never build_time (§5.6).
func TestGeoIPReload(t *testing.T) {
	clock := &geoClock{now: geoBuildEpoch.Add(24 * time.Hour)}
	src := &fakeGeoSource{}
	src.serve(`"a"`, geoFixture(t, "GeoLite2-City-Test.mmdb"))
	loader, log := newGeoLoader(src, clock)
	loader.Poll(context.Background())
	require.Equal(t, "GeoLite2-City", loader.DatabaseType())

	src.serve(`"b"`, geoFixture(t, "GeoIP2-City-Test.mmdb"))
	loader.Poll(context.Background())
	loader.Poll(context.Background())
	assert.Equal(t, "GeoIP2-City", loader.DatabaseType())

	var reloaded []map[string]any
	for _, l := range log.lines(t) {
		if l["event"] == geoip.EventReloaded {
			reloaded = append(reloaded, l)
		}
	}
	require.Len(t, reloaded, 1)
	assert.Equal(t, `"b"`, reloaded[0]["etag"])
	assert.Equal(t, "GeoIP2-City", reloaded[0]["database_type"])
	assert.Equal(t, "2026-02-04T22:49:29Z", reloaded[0]["build_time"])
	_, gets := src.counts()
	assert.Equal(t, 2, gets, "one fetch per ETag")
	assert.Equal(t, geoLondon, geoIngestGeo(t, loader))
}

// TestGeoIPLoggingIsNotPerEvent: lookups log nothing.
func TestGeoIPLoggingIsNotPerEvent(t *testing.T) {
	clock := &geoClock{now: geoBuildEpoch.Add(24 * time.Hour)}
	src := &fakeGeoSource{}
	src.serve(`"a"`, geoFixture(t, "GeoLite2-City-Test.mmdb"))
	loader, log := newGeoLoader(src, clock)
	loader.Poll(context.Background())
	log.reset()
	for i := 0; i < 20; i++ {
		geoIngestGeo(t, loader)
		loader.Lookup(netip.MustParseAddr("1.1.1.1"))
	}
	assert.Empty(t, log.lines(t))
}

// geoCountingLookup records what enrich asked for; it answers London for anything.
type geoCountingLookup struct{ asked []netip.Addr }

func (g *geoCountingLookup) Lookup(a netip.Addr) (validation.GeoInfo, bool) {
	g.asked = append(g.asked, a)
	return validation.GeoInfo{City: "London", CountryCode: "GB", Region: "United Kingdom", Subdivision: "England"}, true
}

// TestGeoIPLookupRules is §5.2/§5.3 through enrich.Apply.
func TestGeoIPLookupRules(t *testing.T) {
	clock := &geoClock{now: geoBuildEpoch.Add(24 * time.Hour)}
	src := &fakeGeoSource{}
	src.serve(`"a"`, geoFixture(t, "GeoLite2-City-Test.mmdb"))
	loader, _ := newGeoLoader(src, clock)
	loader.Poll(context.Background())
	e := &enrich.Enricher{Geo: loader}

	// The fixture's London row, with region = the COUNTRY name (Relay's mapping).
	p := &validation.ErrorPayload{Platform: "go", User: &validation.UserInfo{IPAddress: "81.2.69.142"}}
	enrich.Apply(p, clientip.Info{}, e)
	require.NotNil(t, p.User.Geo)
	assert.Equal(t, validation.GeoInfo{City: "London", Region: "United Kingdom", CountryCode: "GB", Subdivision: "England"}, *p.User.Geo)

	// A mapped form of the same address is unmapped first.
	p = &validation.ErrorPayload{Platform: "go", User: &validation.UserInfo{IPAddress: "::ffff:81.2.69.142"}}
	enrich.Apply(p, clientip.Info{}, e)
	require.NotNil(t, p.User.Geo)
	assert.Equal(t, "GB", p.User.Geo.CountryCode)
	assert.Equal(t, "::ffff:81.2.69.142", p.User.IPAddress, "the body IP itself is never rewritten")

	// A derived IP gets geo too ({{auto}} with a public caller).
	p = &validation.ErrorPayload{Platform: "go", User: &validation.UserInfo{IPAddress: "{{auto}}"}}
	enrich.Apply(p, clientip.Info{ClientIP: netip.MustParseAddr("81.2.69.142"), HasClientIP: true}, e)
	require.NotNil(t, p.User.Geo)
	assert.Equal(t, "London", p.User.Geo.City)

	// Not in the database: nothing.
	p = &validation.ErrorPayload{Platform: "go", User: &validation.UserInfo{IPAddress: "198.51.100.23"}}
	enrich.Apply(p, clientip.Info{}, e)
	assert.Nil(t, p.User.Geo)

	// Private, loopback, link-local, unspecified, CGNAT, and unparseable: skipped WITHOUT a lookup.
	stub := &geoCountingLookup{}
	for _, ip := range []string{"10.0.0.1", "127.0.0.1", "169.254.1.1", "fe80::1%eth0", "0.0.0.0", "::", "100.125.64.17", "not-an-ip", " 81.2.69.142"} {
		p = &validation.ErrorPayload{Platform: "go", User: &validation.UserInfo{IPAddress: ip}}
		enrich.Apply(p, clientip.Info{}, &enrich.Enricher{Geo: stub})
		assert.Nilf(t, p.User.Geo, "%q gets no geo", ip)
	}
	assert.Empty(t, stub.asked, "no lookup for a non-public or unparseable address")

	// A sent geo, including {}, is never touched; and no user means no lookup.
	for _, sent := range []*validation.GeoInfo{{}, {City: "Berlin", CountryCode: "DE"}} {
		want := *sent
		p = &validation.ErrorPayload{Platform: "go", User: &validation.UserInfo{IPAddress: "81.2.69.142", Geo: sent}}
		enrich.Apply(p, clientip.Info{}, &enrich.Enricher{Geo: stub})
		assert.Equal(t, want, *p.User.Geo)
	}
	p = &validation.ErrorPayload{Platform: "go"}
	enrich.Apply(p, clientip.Info{}, &enrich.Enricher{Geo: stub})
	assert.Nil(t, p.User)
	assert.Empty(t, stub.asked)
}

// TestGeoIPDisabledAndMisconfigured: no bucket is "disabled"; a bucket with no endpoint or credentials,
// or a bad path-style override, is "unavailable/config". Each is logged once, and never fatal.
func TestGeoIPDisabledAndMisconfigured(t *testing.T) {
	for _, c := range []struct {
		name, state, reason string
		cfg                 geoip.Config
	}{
		{"no bucket", "disabled", "", geoip.Config{}},
		{"no endpoint", "unavailable", "config", geoip.Config{Bucket: "b", AccessKey: "a", SecretKey: "s"}},
		{"no credentials", "unavailable", "config", geoip.Config{Bucket: "b", Endpoint: "http://minio:9000"}},
		{"bad path style", "unavailable", "config", geoip.Config{Bucket: "b", Endpoint: "http://minio:9000", AccessKey: "a", SecretKey: "s", ForcePathStyle: "maybe"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			log := &geoLog{}
			loader := geoip.NewLoader(c.cfg, slog.New(slog.NewJSONHandler(log, nil)))
			for i := 0; i < 3; i++ {
				loader.Poll(context.Background())
			}
			state, reason := loader.State()
			assert.Equal(t, c.state, state)
			assert.Equal(t, c.reason, reason)
			assert.Len(t, log.lines(t), 1)
			assert.Nil(t, geoIngestGeo(t, loader))
		})
	}
}

// TestGeoIPConfigFromEnv pins §8.3's env names, defaults and the Warn-and-default rule for durations.
func TestGeoIPConfigFromEnv(t *testing.T) {
	env := map[string]string{
		"GEOIP_S3_ENDPOINT": "https://obs.ap-southeast-3.myhuaweicloud.com", "GEOIP_S3_BUCKET": "sentinel-staging",
		"GEOIP_S3_ACCESS_KEY": "ak", "GEOIP_S3_SECRET_KEY": "sk", "GEOIP_S3_REGION": "ap-southeast-1",
		"GEOIP_S3_PREFIX": "geo/", "GEOIP_S3_FORCE_PATH_STYLE": "false", "GEOIP_POLL_INTERVAL": "5m", "GEOIP_MAX_AGE": "876000h",
	}
	log := &geoLog{}
	logger := slog.New(slog.NewJSONHandler(log, nil))
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	c := geoip.ConfigFromEnv(lookup, logger)
	assert.Equal(t, geoip.Config{Endpoint: env["GEOIP_S3_ENDPOINT"], Region: "ap-southeast-1", Bucket: "sentinel-staging",
		Prefix: "geo/", ForcePathStyle: "false", AccessKey: "ak", SecretKey: "sk",
		PollInterval: 5 * time.Minute, MaxAge: 876000 * time.Hour}, c)
	assert.Equal(t, "geo/GeoLite2-City.mmdb", c.ObjectKey())
	assert.Empty(t, log.lines(t))

	c = geoip.ConfigFromEnv(func(string) (string, bool) { return "", false }, logger)
	assert.Equal(t, "us-east-1", c.Region)
	assert.Equal(t, "geoip/", c.Prefix)
	assert.Equal(t, time.Hour, c.PollInterval)
	assert.Equal(t, 720*time.Hour, c.MaxAge)

	env = map[string]string{"GEOIP_POLL_INTERVAL": "30s", "GEOIP_MAX_AGE": "soon"}
	c = geoip.ConfigFromEnv(lookup, logger)
	assert.Equal(t, time.Hour, c.PollInterval, "under 1m falls back to the default")
	assert.Equal(t, 720*time.Hour, c.MaxAge, "unparseable falls back to the default")
	assert.Len(t, log.lines(t), 2, "one Warn line each")
}

// enr-code-geoip-transient-fetch-blacklists-etag: a transport failure while fetching a good object (a
// Get error, or a body that breaks mid-read) is not a verdict on the object. It is never recorded as a
// failed ETag, and the object is fetched again: within the same poll, or at the latest on the next.
func TestGeoIPTransientFetchErrorIsRetried(t *testing.T) {
	for _, c := range []struct {
		name   string
		inject func(*fakeGeoSource)
	}{
		{"get error", func(f *fakeGeoSource) { f.getErrs = 1 }},
		{"read error", func(f *fakeGeoSource) { f.readErrs = 1 }},
	} {
		t.Run(c.name, func(t *testing.T) {
			clock := &geoClock{now: geoBuildEpoch.Add(24 * time.Hour)}
			src := &fakeGeoSource{}
			src.serve(`"a"`, geoFixture(t, "GeoLite2-City-Test.mmdb"))
			c.inject(src)
			loader, log := newGeoLoader(src, clock)
			loader.Poll(context.Background())
			assert.Equal(t, geoLondon, geoIngestGeo(t, loader), "one transient failure is retried inside the same poll")
			loader.Poll(context.Background())
			lines := geoStateLines(t, log)
			require.Len(t, lines, 1, "a retried failure writes no line of its own: %v", lines)
			assert.Equal(t, "ready", lines[0]["to"])
			state, reason := loader.State()
			assert.Equal(t, "ready", state)
			assert.Equal(t, "", reason)
			_, gets := src.counts()
			assert.Equal(t, 2, gets, "the failed fetch is retried once, and the loaded ETag is not fetched again")
		})
	}
}

// A transport failure that outlasts one poll's retries leaves the pod unavailable/fetch_error, logged
// once, and the very next poll that can reach storage loads the SAME ETag.
func TestGeoIPPersistentFetchErrorRecoversOnNextPoll(t *testing.T) {
	clock := &geoClock{now: geoBuildEpoch.Add(24 * time.Hour)}
	src := &fakeGeoSource{}
	src.serve(`"a"`, geoFixture(t, "GeoLite2-City-Test.mmdb"))
	src.getErrs = 1 << 20
	loader, log := newGeoLoader(src, clock)
	for i := 0; i < 3; i++ {
		loader.Poll(context.Background())
	}
	state, reason := loader.State()
	require.Equal(t, "unavailable", state)
	require.Equal(t, "fetch_error", reason)
	assert.Nil(t, geoIngestGeo(t, loader))

	src.mu.Lock()
	src.getErrs = 0
	src.mu.Unlock()
	loader.Poll(context.Background())
	assert.Equal(t, geoLondon, geoIngestGeo(t, loader), "the ETag of a transport failure is not blacklisted")

	lines := geoStateLines(t, log)
	require.Len(t, lines, 2, "one unavailable line for three failed polls, one ready line on recovery: %v", lines)
	assert.Equal(t, "fetch_error", lines[0]["reason"])
	assert.Contains(t, lines[0]["error"], "connection reset")
	assert.Equal(t, "ready", lines[1]["to"])
}

// A content failure still IS a verdict: a corrupt object is fetched once and then skipped, poll after
// poll, until its ETag changes, and then the new object loads.
func TestGeoIPCorruptObjectBlacklistedUntilETagChanges(t *testing.T) {
	clock := &geoClock{now: geoBuildEpoch.Add(24 * time.Hour)}
	full := geoFixture(t, "GeoLite2-City-Test.mmdb")
	src := &fakeGeoSource{}
	src.serve(`"c"`, full[:len(full)/2])
	loader, _ := newGeoLoader(src, clock)
	for i := 0; i < 5; i++ {
		loader.Poll(context.Background())
	}
	_, gets := src.counts()
	assert.Equal(t, 1, gets, "a corrupt object is not downloaded again while its ETag is unchanged")
	state, reason := loader.State()
	assert.Equal(t, "unavailable", state)
	assert.Equal(t, "corrupt", reason)

	src.serve(`"d"`, full)
	loader.Poll(context.Background())
	_, gets = src.counts()
	assert.Equal(t, 2, gets)
	assert.Equal(t, geoLondon, geoIngestGeo(t, loader))
}

// A newer object whose fetch keeps failing in transport never drops the verified database already
// serving: the pod keeps answering from "a", stays ready, and says so in ONE geoip.fetch_failed line
// however many polls fail. When the transport recovers, "b" loads.
func TestGeoIPFailedRefreshKeepsServing(t *testing.T) {
	clock := &geoClock{now: geoBuildEpoch.Add(24 * time.Hour)}
	src := &fakeGeoSource{}
	src.serve(`"a"`, geoFixture(t, "GeoLite2-City-Test.mmdb"))
	loader, log := newGeoLoader(src, clock)
	loader.Poll(context.Background())
	require.Equal(t, "GeoLite2-City", loader.DatabaseType())

	src.serve(`"b"`, geoFixture(t, "GeoIP2-City-Test.mmdb"))
	src.mu.Lock()
	src.getErrs = 1 << 20
	src.mu.Unlock()
	for i := 0; i < 3; i++ {
		loader.Poll(context.Background())
	}
	assert.Equal(t, "GeoLite2-City", loader.DatabaseType(), "the serving database is kept")
	assert.Equal(t, geoLondon, geoIngestGeo(t, loader))
	state, _ := loader.State()
	assert.Equal(t, "ready", state)
	var failed []map[string]any
	for _, l := range log.lines(t) {
		if l["event"] == "geoip.fetch_failed" {
			failed = append(failed, l)
		}
	}
	require.Len(t, failed, 1, "one line per failure streak, not per poll")
	assert.Equal(t, "WARN", failed[0]["level"])
	assert.Equal(t, "fetch_error", failed[0]["reason"])
	assert.Equal(t, `"a"`, failed[0]["serving_etag"])

	src.mu.Lock()
	src.getErrs = 0
	src.mu.Unlock()
	loader.Poll(context.Background())
	assert.Equal(t, "GeoIP2-City", loader.DatabaseType(), "the ETag was never blacklisted")
}

// enr-code-geoip-stat-error-drops-serving-db: a failed Stat is a transport failure, never a reason to
// stop answering from a verified database. One Stat error is retried inside the poll; Stat errors that
// outlast the poll keep the database serving (one geoip.fetch_failed line), and the next reachable poll
// is an ordinary unchanged-ETag poll that downloads nothing.
func TestGeoIPStatErrorKeepsServing(t *testing.T) {
	clock := &geoClock{now: geoBuildEpoch.Add(24 * time.Hour)}
	src := &fakeGeoSource{}
	src.serve(`"a"`, geoFixture(t, "GeoLite2-City-Test.mmdb"))
	loader, log := newGeoLoader(src, clock)
	loader.Poll(context.Background())
	require.Equal(t, geoLondon, geoIngestGeo(t, loader))

	src.mu.Lock()
	src.statErrs = 1
	src.mu.Unlock()
	loader.Poll(context.Background())
	assert.Equal(t, geoLondon, geoIngestGeo(t, loader), "one Stat error keeps the loaded database serving")
	state, reason := loader.State()
	assert.Equal(t, "ready", state)
	assert.Equal(t, "", reason)
	for _, l := range log.lines(t) {
		assert.NotEqual(t, "geoip.fetch_failed", l["event"], "one Stat error is retried inside the poll, so it is not a failure")
	}

	src.mu.Lock()
	src.statErrs = 1 << 20
	src.mu.Unlock()
	for i := 0; i < 3; i++ {
		loader.Poll(context.Background())
	}
	assert.Equal(t, geoLondon, geoIngestGeo(t, loader), "Stat errors that outlast a poll keep it serving too")
	state, _ = loader.State()
	assert.Equal(t, "ready", state)
	var failed []map[string]any
	for _, l := range log.lines(t) {
		if l["event"] == "geoip.fetch_failed" {
			failed = append(failed, l)
		}
	}
	require.Len(t, failed, 1, "one line per failure streak")
	assert.Contains(t, failed[0]["error"], "503")
	require.Len(t, geoStateLines(t, log), 1, "no state line: the pod never stopped being ready")

	src.mu.Lock()
	src.statErrs = 0
	src.mu.Unlock()
	loader.Poll(context.Background())
	_, gets := src.counts()
	assert.Equal(t, 1, gets, "the unchanged object is not downloaded again")
}

// Staleness is still re-evaluated while storage is unreachable: a database that ages past the limit
// stops answering within one poll even though every Stat fails.
func TestGeoIPStatErrorStillGoesStale(t *testing.T) {
	clock := &geoClock{now: geoBuildEpoch.Add(719 * time.Hour)}
	src := &fakeGeoSource{}
	src.serve(`"a"`, geoFixture(t, "GeoLite2-City-Test.mmdb"))
	loader, _ := newGeoLoader(src, clock)
	loader.Poll(context.Background())
	require.Equal(t, geoLondon, geoIngestGeo(t, loader))

	src.mu.Lock()
	src.statErrs = 1 << 20
	src.mu.Unlock()
	clock.set(geoBuildEpoch.Add(721 * time.Hour))
	loader.Poll(context.Background())
	state, _ := loader.State()
	assert.Equal(t, "stale", state)
	assert.Nil(t, geoIngestGeo(t, loader))
}

// enr-code-geoip-get-not-pinned-to-etag: when a new object lands between a poll's Stat and its Get, the
// loader must not record the new bytes under the old ETag. The Get is pinned to the Stat's ETag, a
// mismatch is a transport-class retry (never a blacklist), and the loaded database carries the ETag of
// the bytes it was built from, so the next poll downloads nothing.
func TestGeoIPGetPinnedToStatETag(t *testing.T) {
	clock := &geoClock{now: geoBuildEpoch.Add(24 * time.Hour)}
	src := &fakeGeoSource{}
	src.serve(`"a"`, geoFixture(t, "GeoLite2-City-Test.mmdb"))
	src.swapOnGet = &struct {
		etag string
		body []byte
	}{`"b"`, geoFixture(t, "GeoIP2-City-Test.mmdb")}
	loader, log := newGeoLoader(src, clock)

	loader.Poll(context.Background())
	assert.Equal(t, "GeoIP2-City", loader.DatabaseType())
	lines := geoStateLines(t, log)
	require.NotEmpty(t, lines)
	last := lines[len(lines)-1]
	assert.Equal(t, "ready", last["to"])
	assert.Equal(t, `"b"`, last["etag"], "the database is labelled with the ETag of the bytes it holds")
	_, getsAfterFirst := src.counts()

	loader.Poll(context.Background())
	_, gets := src.counts()
	assert.Equal(t, getsAfterFirst, gets, "the object already loaded is not downloaded again")
	assert.Equal(t, "GeoIP2-City", loader.DatabaseType())
}
