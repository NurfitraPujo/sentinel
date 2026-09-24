package unit

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/clientip"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/enrich"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/validation"
)

// sentinel-event-enrichment spec §4.5 (G2): the sdk-js tunnel's REAL forwards, committed by
// packages/sdk-js/tests/tunnel-fixtures.test.ts, replayed through the ingestor's real resolver and enrichment.
// This is the cross-language proof of TASK-06 (browser's IP, never the web server's; no client forwarding
// header), TASK-01 criterion 1 (tunnel-auto-ip) and TASK-04 criterion 1 (tunnel-clean's browser and OS).

const tunnelFixtureDir = "../../packages/sdk-js/tests/tunnel-fixtures"

var tunnelRouteFixtures = []string{"tunnel-clean", "tunnel-spoofed-xff", "tunnel-auto-ip", "tunnel-browser-sent-ip", "tunnel-no-ip"}

type tunnelFixture struct {
	Name    string `json:"name"`
	Inbound struct {
		Headers map[string]string `json:"headers"`
		Body    string            `json:"body"`
	} `json:"inbound"`
	Forwarded struct {
		Path    string            `json:"path"`
		Headers map[string]string `json:"headers"`
		Body    string            `json:"body"`
	} `json:"forwarded"`
	Expect struct {
		ClientIP  string `json:"client_ip"`
		UserAgent string `json:"user_agent"`
	} `json:"expect"`
}

func loadTunnelFixture(t *testing.T, name string) tunnelFixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(tunnelFixtureDir, name+".json"))
	require.NoError(t, err, "%s.json must be committed (run the sdk-js suite)", name)
	var f tunnelFixture
	require.NoError(t, json.Unmarshal(raw, &f))
	require.Equal(t, name, f.Name)
	return f
}

// tunnelReplay runs a forward through clientip.Middleware and enrich.Apply exactly as the ingestor's batch handler
// would: the forward's headers, the given peer and extra X-Forwarded-For, then the body decoded with encoding/json
// into []validation.ErrorPayload (main.go's decoder).
func tunnelReplay(t *testing.T, trusted, remoteAddr, xff string, headers map[string]string, body string, e *enrich.Enricher) []validation.ErrorPayload {
	t.Helper()
	prefixes, err := clientip.ParseTrusted(trusted)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/ingest/batch", strings.NewReader(body))
	req.RemoteAddr = remoteAddr
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	var items []validation.ErrorPayload
	clientip.Middleware(clientip.NewResolver(prefixes), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&items))
		info, ok := clientip.RequestInfoFromContext(r.Context())
		require.True(t, ok)
		for i := range items {
			enrich.Apply(&items[i], info, e)
		}
	})).ServeHTTP(httptest.NewRecorder(), req)
	require.NotEmpty(t, items)
	return items
}

func storedIP(p validation.ErrorPayload) string {
	if p.User == nil {
		return ""
	}
	return p.User.IPAddress
}

// TestTunnelFixtureShape asserts the tunnel's forward itself: no X-Forwarded-For, the marker off, the
// browser's User-Agent, and every forwarded item's IP equal to expect.client_ip (no user when it is "").
func TestTunnelFixtureShape(t *testing.T) {
	for _, name := range tunnelRouteFixtures {
		t.Run(name, func(t *testing.T) {
			f := loadTunnelFixture(t, name)
			for k := range f.Forwarded.Headers {
				assert.NotEqualf(t, "x-forwarded-for", strings.ToLower(k), "the tunnel never forwards a forwarding header")
			}
			assert.Equal(t, "off", f.Forwarded.Headers["x-sentinel-derive-ip"])
			assert.Equal(t, f.Expect.UserAgent, f.Forwarded.Headers["user-agent"])
			assert.Equal(t, "/ingest/batch", f.Forwarded.Path)

			var fwd, in []map[string]any
			require.NoError(t, json.Unmarshal([]byte(f.Forwarded.Body), &fwd))
			require.NoError(t, json.Unmarshal([]byte(f.Inbound.Body), &in))
			require.Len(t, fwd, len(in))
			for i, item := range fwd {
				if f.Expect.ClientIP == "" {
					assert.NotContains(t, item, "user", "no IP derived: no user at all")
				} else {
					assert.Equal(t, map[string]any{"ip_address": f.Expect.ClientIP}, item["user"])
				}
				if u, ok := in[i]["user"].(map[string]any); ok && u["ip_address"] != f.Expect.ClientIP {
					if fu, ok := item["user"].(map[string]any); ok {
						assert.NotEqual(t, u["ip_address"], fu["ip_address"], "the browser-sent IP never survives")
					}
				}
			}
			assert.NotContains(t, f.Forwarded.Body, "6.6.6.6")
		})
	}
}

// TestTunnelFixtureHuaweiReplay: each forward with Huawei's shape — the trusted list read from the staging
// overlay (empty), a node-side peer, and an ELB-style appended header that must be ignored.
func TestTunnelFixtureHuaweiReplay(t *testing.T) {
	huawei := huaweiTrustedList(t)
	_, e := enrReadyLoader(t)
	for _, name := range tunnelRouteFixtures {
		t.Run(name, func(t *testing.T) {
			f := loadTunnelFixture(t, name)
			items := tunnelReplay(t, huawei, "10.200.32.66:51000", "6.6.6.6, 172.16.3.117", f.Forwarded.Headers, f.Forwarded.Body, e)
			for _, p := range items {
				assert.Equal(t, f.Expect.ClientIP, storedIP(p), "never 6.6.6.6, 172.16.3.117 or the node-side peer")
			}
			if name == "tunnel-clean" {
				require.NotNil(t, items[0].Contexts)
				assert.Equal(t, map[string]interface{}{"name": "Chrome", "version": "128.0.0"}, items[0].Contexts["browser"],
					"TASK-04 criterion 1: the browser from the forwarded User-Agent")
				assert.Equal(t, map[string]interface{}{"name": "Windows", "version": ">=10"}, items[0].Contexts["os"])
			}
		})
	}
}

// TestTunnelFixturePublicPeerReplay (enr-rev3-null-ip-server-address, spec §3.8-§3.9): tunnel-no-ip's forward
// replayed with an empty list and a PUBLIC peer standing in for a web server the ingestor sees directly. Two
// defences stop the fill — the marker and I3t — so the replay runs in four variants, each defence alone, and
// the last proves the case is not vacuous.
func TestTunnelFixturePublicPeerReplay(t *testing.T) {
	f := loadTunnelFixture(t, "tunnel-no-ip")
	withoutRuntime := func(body string) string {
		var items []map[string]any
		require.NoError(t, json.Unmarshal([]byte(body), &items))
		for _, it := range items {
			if md, ok := it["metadata"].(map[string]any); ok {
				delete(md, "runtime")
			}
		}
		b, err := json.Marshal(items)
		require.NoError(t, err)
		return string(b)
	}
	withoutMarker := map[string]string{}
	for k, v := range f.Forwarded.Headers {
		if k != "x-sentinel-derive-ip" {
			withoutMarker[k] = v
		}
	}
	for _, c := range []struct {
		name    string
		headers map[string]string
		body    string
		want    string
	}{
		{"as forwarded (marker + runtime browser)", f.Forwarded.Headers, f.Forwarded.Body, ""},
		{"marker removed: I3t alone (a pre-0.2.0 tunnel's shape)", withoutMarker, f.Forwarded.Body, ""},
		{"runtime removed from every item: the marker alone", f.Forwarded.Headers, withoutRuntime(f.Forwarded.Body), ""},
		{"both removed: the fill really runs (M1)", withoutMarker, withoutRuntime(f.Forwarded.Body), "198.51.100.200"},
	} {
		t.Run(c.name, func(t *testing.T) {
			items := tunnelReplay(t, "", "198.51.100.200:4411", "", c.headers, c.body, nil)
			for _, p := range items {
				assert.Equal(t, c.want, storedIP(p))
			}
		})
	}
}

type tunnelRebuildFixture struct {
	Name  string `json:"name"`
	Cases []struct {
		Name   string            `json:"name"`
		IP     *string           `json:"ip"`
		Input  []json.RawMessage `json:"input"`
		Output string            `json:"output"`
	} `json:"cases"`
}

var tunnelUserKeysRe = regexp.MustCompile(`"user":\{([^{}]*)\}`)

// TestTunnelFixtureRebuildDecode (coordinator ruling on enr-rev3-casefold-ip-key): the tunnel's rebuilt output,
// decoded with the ingestor's own encoding/json into []validation.ErrorPayload, carries the tunnel's IP and the
// lookup's geo for every fold case — never 6.6.6.6, never the browser's XX / XY / Fake.
func TestTunnelFixtureRebuildDecode(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(tunnelFixtureDir, "tunnel-user-rebuild.json"))
	require.NoError(t, err)
	var f tunnelRebuildFixture
	require.NoError(t, json.Unmarshal(raw, &f))
	require.Len(t, f.Cases, 6)
	_, e := enrReadyLoader(t)
	huawei := huaweiTrustedList(t)
	clean := loadTunnelFixture(t, "tunnel-clean")

	for _, c := range f.Cases {
		t.Run(c.Name, func(t *testing.T) {
			// On the raw output: every user object's keys ⊆ {id, username, email, ip_address}, and no two fold-equal.
			var rawItems []map[string]json.RawMessage
			require.NoError(t, json.Unmarshal([]byte(c.Output), &rawItems))
			for _, it := range rawItems {
				if u, ok := it["user"]; ok {
					var keys map[string]json.RawMessage
					require.NoError(t, json.Unmarshal(u, &keys))
					seen := []string{}
					for k := range keys {
						assert.Contains(t, []string{"id", "username", "email", "ip_address"}, k)
						for _, s := range seen {
							assert.Falsef(t, strings.EqualFold(s, k), "fold-equal keys %q and %q", s, k)
						}
						seen = append(seen, k)
					}
				}
			}
			assert.NotEmpty(t, tunnelUserKeysRe.FindAllString(c.Output, -1), "the case carries a user object")

			items := tunnelReplay(t, huawei, "10.200.32.66:51000", "6.6.6.6, 172.16.3.117", clean.Forwarded.Headers, c.Output, e)
			for _, p := range items {
				if c.IP == nil {
					assert.Equal(t, "", storedIP(p))
					if p.User != nil {
						assert.Nil(t, p.User.Geo, "no IP: no geo, and never the browser's")
					}
					continue
				}
				require.NotNil(t, p.User)
				assert.Equal(t, *c.IP, p.User.IPAddress, "never 6.6.6.6")
				require.NotNil(t, p.User.Geo, "the lookup of the tunnel-written IP")
				assert.Equal(t, validation.GeoInfo{City: "London", Region: "United Kingdom", CountryCode: "GB", Subdivision: "England"}, *p.User.Geo)
			}
		})
	}
}
