package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	tc "github.com/NurfitraPujo/sentinel/tests/integration/testcontainers"
)

// sentinel-event-enrichment spec §10.2 (G3): server-side enrichment through the REAL ingestor image and
// processor, into error_occurrences. The suite's shared ingestor runs with TRUSTED_PROXY_CIDRS unset, as
// on Huawei; cases that need another configuration start a second ingestor with StartIngestorWithEnv,
// sharing the suite's Postgres, NATS and Redis. The ingestor runs with NetworkMode "host", so its peer
// for this test process is 127.0.0.1 (T23), which the public-caller rule (§3.5) never stores.

// enrStartIngestor starts one more ingestor container with extraEnv and returns its base URL. It is
// terminated when the test ends.
func enrStartIngestor(t *testing.T, extraEnv map[string]string) string {
	t.Helper()
	if testConfig.Host == "" {
		requireInfra(t, fmt.Errorf("the suite is not on the testcontainers path (FORCE_TESTCONTAINERS=1)"),
			"a second ingestor container")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c, err := tc.StartIngestorWithEnv(ctx, testConfig.Host, testConfig.Port, testConfig.User,
		testConfig.Password, testConfig.DB, natsConfig.URL, redisCfg.Addr, extraEnv)
	require.NoError(t, err, "second ingestor container (env %v)", extraEnv)
	t.Cleanup(func() { _ = c.Terminate(context.Background()) })
	return c.URL()
}

// enrPost posts raw JSON to url with the API key and any extra headers.
func enrPost(t *testing.T, url, apiKey string, headers map[string]string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("X-API-Key", apiKey)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	requireInfra(t, err, fmt.Sprintf("ingestor HTTP service at %s", url))
	return resp
}

// enrStored is what enrichment writes, read back from the processor's occurrence row.
type enrStored struct {
	IP       string
	Geo      map[string]interface{} // nil when event_user->'geo' is null or absent
	Contexts map[string]interface{}
}

func enrReadStored(t *testing.T, pool *pgxpool.Pool, projectID, eventID string) enrStored {
	t.Helper()
	occ := waitForOccurrence(t, pool, projectID, eventID)
	var s enrStored
	var geo, contexts []byte
	require.NoError(t, pool.QueryRow(context.Background(), `
		SELECT coalesce(event_user->>'ip_address', ''), event_user->'geo', contexts
		  FROM error_occurrences WHERE id = $1`, occ).Scan(&s.IP, &geo, &contexts))
	if len(geo) > 0 && string(geo) != "null" {
		require.NoError(t, json.Unmarshal(geo, &s.Geo))
	}
	if len(contexts) > 0 && string(contexts) != "null" {
		require.NoError(t, json.Unmarshal(contexts, &s.Contexts))
	}
	return s
}

// enrProject seeds a project and key for one test and removes them afterwards.
func enrProject(t *testing.T, tag string) (pool *pgxpool.Pool, projectName, apiKey, projectID string) {
	t.Helper()
	cfg := eventV2E2EConfig()
	pool = newPostgresPool(t, cfg)
	t.Cleanup(pool.Close)
	projectName = fmt.Sprintf("enr-%s-%d", tag, time.Now().UnixNano())
	apiKey = "enr-key-" + projectName
	projectID = createTestProject(t, pool, projectName, apiKey)
	t.Cleanup(func() { cleanupProject(t, pool, projectID) })
	return pool, projectName, apiKey, projectID
}

func enrEvent(projectName, eventID string, extra map[string]interface{}) []byte {
	ev := map[string]interface{}{
		"project_key": projectName, "event_id": eventID, "environment": "test",
		"platform": "go", "error_class": "EnrichmentProbe", "message": "enrichment " + eventID,
	}
	for k, v := range extra {
		ev[k] = v
	}
	b, _ := json.Marshal(ev)
	return b
}

// TestEnrichment_AutoIPUntrustedListIgnoresSpoofedXFF is §10.2 TASK-01 criterion 2, integration (i):
// TRUSTED_PROXY_CIDRS unset (the suite's own ingestor, as on Huawei). {{auto}} with X-Forwarded-For
// 6.6.6.6 is stored EMPTY: the header is not believed, and the peer (127.0.0.1) is not public.
func TestEnrichment_AutoIPUntrustedListIgnoresSpoofedXFF(t *testing.T) {
	pool, name, key, projectID := enrProject(t, "auto-untrusted")
	cfg := eventV2E2EConfig()
	eventID := fmt.Sprintf("evt-enr-i-%d", time.Now().UnixNano())

	resp := enrPost(t, cfg.IngesterURL+"/ingest", key, map[string]string{"X-Forwarded-For": "6.6.6.6"},
		enrEvent(name, eventID, map[string]interface{}{"user": map[string]interface{}{"ip_address": "{{auto}}"}}))
	resp.Body.Close()
	require.Equal(t, http.StatusAccepted, resp.StatusCode)

	got := enrReadStored(t, pool, projectID, eventID)
	assert.Equal(t, "", got.IP, "never 6.6.6.6, never 127.0.0.1, never the literal {{auto}}")
}

// TestEnrichment_AutoIPTrustedListWalksRightToLeft is integration (ii), and TASK-01 "wired to main()":
// with TRUSTED_PROXY_CIDRS=127.0.0.0/8,::1/128 the loopback peer is our proxy, and the walk returns the
// rightmost untrusted hop. It can only store 198.51.100.23 if main parses the list and wraps the ingest
// chains in clientip.Middleware.
func TestEnrichment_AutoIPTrustedListWalksRightToLeft(t *testing.T) {
	url := enrStartIngestor(t, map[string]string{"TRUSTED_PROXY_CIDRS": "127.0.0.0/8,::1/128"})
	pool, name, key, projectID := enrProject(t, "auto-trusted")

	single := fmt.Sprintf("evt-enr-ii-single-%d", time.Now().UnixNano())
	batch := fmt.Sprintf("evt-enr-ii-batch-%d", time.Now().UnixNano())
	xff := map[string]string{"X-Forwarded-For": "6.6.6.6, 198.51.100.23"}
	auto := map[string]interface{}{"user": map[string]interface{}{"ip_address": "{{auto}}"}}

	resp := enrPost(t, url+"/ingest", key, xff, enrEvent(name, single, auto))
	resp.Body.Close()
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
	resp = enrPost(t, url+"/ingest/batch", key, xff, []byte("["+string(enrEvent(name, batch, auto))+"]"))
	resp.Body.Close()
	require.Equal(t, http.StatusAccepted, resp.StatusCode)

	for _, id := range []string{single, batch} {
		assert.Equal(t, "198.51.100.23", enrReadStored(t, pool, projectID, id).IP, id)
	}
}

// enrFakeS3 is an S3-shaped httptest server on 127.0.0.1 holding at most one object (spec §10.2 TASK-03
// wiring row). The ingestor container runs with host networking, so it reaches this server directly.
// body == nil answers every object request 404 NoSuchKey.
type enrFakeS3 struct {
	*httptest.Server
	mu    sync.Mutex
	heads int
	gets  int
}

func enrStartFakeS3(t *testing.T, body []byte) *enrFakeS3 {
	t.Helper()
	f := &enrFakeS3{}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	f.Server = &httptest.Server{Listener: l, Config: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		if r.Method == http.MethodHead {
			f.heads++
		} else {
			f.gets++
		}
		f.mu.Unlock()
		if body == nil {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusNotFound)
			if r.Method != http.MethodHead {
				fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>NoSuchKey</Code><Message>The specified key does not exist.</Message></Error>`)
			}
			return
		}
		w.Header().Set("ETag", `"enr-fixture"`)
		w.Header().Set("Last-Modified", "Wed, 04 Feb 2026 22:49:29 GMT")
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		if r.Method != http.MethodHead {
			_, _ = w.Write(body)
		}
	})}}
	f.Start()
	t.Cleanup(f.Close)
	return f
}

// waitFor blocks until the loader has made the request it needs (a HEAD for a missing object, a GET
// for a present one), then leaves a moment for the swap. The loader's first poll runs at startup.
func (f *enrFakeS3) waitFor(t *testing.T, get bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		done := f.heads > 0 && (!get || f.gets > 0)
		f.mu.Unlock()
		if done {
			time.Sleep(time.Second)
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("the ingestor's geo loader never reached the fake S3 at %s", f.URL)
}

// enrGeoEnv points a container at a fake S3. GEOIP_MAX_AGE=876000h (100 years): the 2026-02-04 fixture is
// otherwise stale under the default 720h and the test could not pass (spec §5.4).
func enrGeoEnv(f *enrFakeS3) map[string]string {
	return map[string]string{
		"GEOIP_S3_ENDPOINT":   f.URL,
		"GEOIP_S3_BUCKET":     "sentinel-enr",
		"GEOIP_S3_PREFIX":     "geoip/",
		"GEOIP_S3_ACCESS_KEY": "enr-access",
		"GEOIP_S3_SECRET_KEY": "enr-secret",
		"GEOIP_MAX_AGE":       "876000h",
	}
}

var enrLondon = map[string]interface{}{
	"city": "London", "region": "United Kingdom", "country_code": "GB", "subdivision": "England",
}

// TestEnrichment_GeoFromObjectStorage is TASK-03 "wired to main()": the container loads the fixture from
// a fake S3 through GEOIP_S3_*, and an event carrying 81.2.69.142 is stored with that geo.
func TestEnrichment_GeoFromObjectStorage(t *testing.T) {
	mmdb, err := os.ReadFile("../fixtures/geoip/GeoLite2-City-Test.mmdb")
	require.NoError(t, err)
	s3 := enrStartFakeS3(t, mmdb)
	url := enrStartIngestor(t, enrGeoEnv(s3))
	s3.waitFor(t, true)
	pool, name, key, projectID := enrProject(t, "geo")

	t.Run("a body IP is looked up", func(t *testing.T) {
		eventID := fmt.Sprintf("evt-enr-geo-%d", time.Now().UnixNano())
		resp := enrPost(t, url+"/ingest", key, nil,
			enrEvent(name, eventID, map[string]interface{}{"user": map[string]interface{}{"ip_address": "81.2.69.142"}}))
		resp.Body.Close()
		require.Equal(t, http.StatusAccepted, resp.StatusCode)
		got := enrReadStored(t, pool, projectID, eventID)
		assert.Equal(t, "81.2.69.142", got.IP)
		assert.Equal(t, enrLondon, got.Geo)
	})

	t.Run("a sent geo is stored as sent", func(t *testing.T) {
		eventID := fmt.Sprintf("evt-enr-geo-sent-%d", time.Now().UnixNano())
		sent := map[string]interface{}{"city": "Berlin", "country_code": "DE"}
		resp := enrPost(t, url+"/ingest", key, nil, enrEvent(name, eventID, map[string]interface{}{
			"user": map[string]interface{}{"ip_address": "81.2.69.142", "geo": sent}}))
		resp.Body.Close()
		require.Equal(t, http.StatusAccepted, resp.StatusCode)
		got := enrReadStored(t, pool, projectID, eventID)
		assert.Equal(t, "Berlin", got.Geo["city"])
		assert.Equal(t, "DE", got.Geo["country_code"])
	})
}

// TestEnrichment_GeoMissingObjectIsNullAndHealthy: a container whose object 404s stores the event with
// null geo, and its /health is 200 (a missing database is never a failure, spec §5.4).
func TestEnrichment_GeoMissingObjectIsNullAndHealthy(t *testing.T) {
	s3 := enrStartFakeS3(t, nil)
	url := enrStartIngestor(t, enrGeoEnv(s3))
	s3.waitFor(t, false)
	pool, name, key, projectID := enrProject(t, "geo-missing")

	eventID := fmt.Sprintf("evt-enr-geo-404-%d", time.Now().UnixNano())
	resp := enrPost(t, url+"/ingest", key, nil,
		enrEvent(name, eventID, map[string]interface{}{"user": map[string]interface{}{"ip_address": "81.2.69.142"}}))
	resp.Body.Close()
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
	got := enrReadStored(t, pool, projectID, eventID)
	assert.Equal(t, "81.2.69.142", got.IP)
	assert.Nil(t, got.Geo)

	health, err := http.Get(url + "/health")
	require.NoError(t, err)
	health.Body.Close()
	assert.Equal(t, http.StatusOK, health.StatusCode)
}

// ---- The tunnel's real output through the real ingestor (spec §4.5 G3 bullet; §10.2 TASK-01 criterion 1,
// TASK-04 criterion 1, TASK-06 criteria 1 and 2). The fixtures are packages/sdk-js/tests/tunnel-fixtures/*.json,
// written by the sdk-js suite from the route's real forwards.

const enrTunnelDir = "../../packages/sdk-js/tests/tunnel-fixtures"

type enrTunnelFixture struct {
	Forwarded struct {
		Path    string            `json:"path"`
		Headers map[string]string `json:"headers"`
		Body    string            `json:"body"`
	} `json:"forwarded"`
	Expect struct {
		ClientIP string `json:"client_ip"`
	} `json:"expect"`
}

func enrLoadTunnel(t *testing.T, name string) enrTunnelFixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(enrTunnelDir, name+".json"))
	require.NoError(t, err)
	var f enrTunnelFixture
	require.NoError(t, json.Unmarshal(raw, &f))
	return f
}

// enrForwardHeaders is the fixture's forward headers with the API key replaced by the test project's (enrPost
// sets it) and no project key header.
func enrForwardHeaders(h map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range h {
		if strings.EqualFold(k, "x-api-key") || strings.EqualFold(k, "x-project-key") {
			continue
		}
		out[k] = v
	}
	return out
}

// enrEventIDs returns the event_id of every item in a forwarded body.
func enrEventIDs(t *testing.T, body string) []string {
	t.Helper()
	var items []map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(body), &items))
	ids := make([]string, len(items))
	for i, it := range items {
		ids[i] = fmt.Sprint(it["event_id"])
	}
	return ids
}

// TestEnrichment_TunnelForwardsThroughTheIngestor posts every route fixture's forward, byte for byte, to the
// suite's ingestor (TRUSTED_PROXY_CIDRS unset, as on Huawei; its peer is 127.0.0.1). The stored IP is the
// browser's for tunnel-clean, tunnel-spoofed-xff, tunnel-auto-ip and tunnel-browser-sent-ip (never 6.6.6.6) and
// none for tunnel-no-ip, never the tunnel server's address; tunnel-clean's contexts carry browser and OS.
func TestEnrichment_TunnelForwardsThroughTheIngestor(t *testing.T) {
	pool, _, key, projectID := enrProject(t, "tunnel")
	cfg := eventV2E2EConfig()
	for _, name := range []string{"tunnel-clean", "tunnel-spoofed-xff", "tunnel-auto-ip", "tunnel-browser-sent-ip", "tunnel-no-ip"} {
		t.Run(name, func(t *testing.T) {
			f := enrLoadTunnel(t, name)
			require.Equal(t, "/ingest/batch", f.Forwarded.Path)
			resp := enrPost(t, cfg.IngesterURL+f.Forwarded.Path, key, enrForwardHeaders(f.Forwarded.Headers), []byte(f.Forwarded.Body))
			resp.Body.Close()
			require.Equal(t, http.StatusAccepted, resp.StatusCode)
			for _, id := range enrEventIDs(t, f.Forwarded.Body) {
				got := enrReadStored(t, pool, projectID, id)
				assert.Equal(t, f.Expect.ClientIP, got.IP, "never 6.6.6.6, never 127.0.0.1")
				if name == "tunnel-clean" {
					assert.Equal(t, map[string]interface{}{"name": "Chrome", "version": "128.0.0"}, got.Contexts["browser"])
					assert.Equal(t, map[string]interface{}{"name": "Windows", "version": ">=10"}, got.Contexts["os"])
				}
			}
		})
	}
}

// TestEnrichment_TunnelRebuildThroughTheIngestor posts each tunnel-user-rebuild.json output as a batch, with
// tunnel-clean's forward headers, to a geo-configured container: the stored IP is the case's (none for
// no-ip-folded) and the geo is the lookup's GB / London (or null), never the browser's XX / XY / Fake.
func TestEnrichment_TunnelRebuildThroughTheIngestor(t *testing.T) {
	mmdb, err := os.ReadFile("../fixtures/geoip/GeoLite2-City-Test.mmdb")
	require.NoError(t, err)
	s3 := enrStartFakeS3(t, mmdb)
	url := enrStartIngestor(t, enrGeoEnv(s3))
	s3.waitFor(t, true)
	pool, _, key, projectID := enrProject(t, "tunnel-rebuild")
	headers := enrForwardHeaders(enrLoadTunnel(t, "tunnel-clean").Forwarded.Headers)

	raw, err := os.ReadFile(filepath.Join(enrTunnelDir, "tunnel-user-rebuild.json"))
	require.NoError(t, err)
	var f struct {
		Cases []struct {
			Name   string  `json:"name"`
			IP     *string `json:"ip"`
			Output string  `json:"output"`
		} `json:"cases"`
	}
	require.NoError(t, json.Unmarshal(raw, &f))
	require.Len(t, f.Cases, 6)
	for _, c := range f.Cases {
		t.Run(c.Name, func(t *testing.T) {
			resp := enrPost(t, url+"/ingest/batch", key, headers, []byte(c.Output))
			resp.Body.Close()
			require.Equal(t, http.StatusAccepted, resp.StatusCode)
			for _, id := range enrEventIDs(t, c.Output) {
				got := enrReadStored(t, pool, projectID, id)
				if c.IP == nil {
					assert.Equal(t, "", got.IP)
					assert.Nil(t, got.Geo)
					continue
				}
				assert.Equal(t, *c.IP, got.IP, "never 6.6.6.6")
				assert.Equal(t, enrLondon, got.Geo, "the lookup's geo, never the browser's XX / XY / Fake")
			}
		})
	}
}
