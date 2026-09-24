package unit

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/clientip"
)

// sentinel-event-enrichment spec §3.7 / §10.2 TASK-01 criterion 2: "A spoofed X-Forwarded-For from an
// untrusted hop is not trusted". Every case drives clientip.Resolver.Resolve, which includes the
// public-caller rule (§3.5) and the no-derive marker (§3.8), with an httptest request whose RemoteAddr
// is set by hand.

// enrTestList stands in for Phase 4's configured list: a public proxy tier and loopback (§3.7).
const enrTestList = "203.0.113.0/24,127.0.0.0/8"

type walkCase struct {
	name       string
	trusted    string
	remoteAddr string
	xff        []string // one element per header line; nil = no header
	marker     *string  // X-Sentinel-Derive-IP value; nil = absent
	want       string   // "" = no caller IP
}

func enrRequest(c walkCase) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/ingest/batch", nil)
	r.RemoteAddr = c.remoteAddr
	for _, line := range c.xff {
		r.Header.Add("X-Forwarded-For", line)
	}
	if c.marker != nil {
		r.Header.Set(clientip.DeriveIPHeader, *c.marker)
	}
	return r
}

func runWalkCases(t *testing.T, cases []walkCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			trusted, err := clientip.ParseTrusted(c.trusted)
			require.NoError(t, err)
			got, ok := clientip.NewResolver(trusted).Resolve(enrRequest(c))
			if c.want == "" {
				assert.Falsef(t, ok, "no caller IP expected, got %s", got)
				return
			}
			require.True(t, ok, "a caller IP was expected")
			assert.Equal(t, c.want, got.String())
		})
	}
}

func strp(s string) *string { return &s }

func TestClientIPWalk(t *testing.T) {
	forty := make([]string, 40)
	for i := range forty {
		forty[i] = fmt.Sprintf("203.0.113.%d", 100+i)
	}
	runWalkCases(t, []walkCase{
		{"C1 empty list: a spoofed XFF is not trusted", "", "198.51.100.77:4411", []string{"6.6.6.6"}, nil, "198.51.100.77"},
		{"C2 untrusted peer: a spoofed XFF is not trusted", enrTestList, "198.51.100.77:4411", []string{"6.6.6.6"}, nil, "198.51.100.77"},
		{"C3 trusted peer, one hop", enrTestList, "203.0.113.10:40022", []string{"198.51.100.23"}, nil, "198.51.100.23"},
		{"C4 the untrusted hop is the caller, the spoof left of it is never reached", enrTestList, "203.0.113.10:40022", []string{"6.6.6.6, 198.51.100.23"}, nil, "198.51.100.23"},
		{"C5 every hop is infrastructure", enrTestList, "203.0.113.10:40022", []string{"203.0.113.11"}, nil, ""},
		{"C6 trusted peer, no header", enrTestList, "203.0.113.10:40022", nil, nil, ""},
		{"C7 an unreadable hop stops the walk", enrTestList, "203.0.113.10:40022", []string{"garbage, 203.0.113.11"}, nil, ""},
		{"C7b an empty hop stops the walk", enrTestList, "203.0.113.10:40022", []string{"198.51.100.23, , 203.0.113.11"}, nil, ""},
		{"C8 port stripped", enrTestList, "203.0.113.10:40022", []string{"198.51.100.23:61000, 203.0.113.11"}, nil, "198.51.100.23"},
		{"C9 mapped peer unmapped before the check", enrTestList, "[::ffff:203.0.113.10]:40022", []string{"2001:db8::1, 203.0.113.11"}, nil, "2001:db8::1"},
		{"C10 header lines joined in order", enrTestList, "203.0.113.10:40022", []string{"198.51.100.23", "203.0.113.11"}, nil, "198.51.100.23"},
		{"C10b bracketed v6 hop with port", enrTestList, "203.0.113.10:40022", []string{"[2001:db8::7]:443"}, nil, "2001:db8::7"},
		{"C11 40 trusted entries: none", enrTestList, "203.0.113.10:40022", []string{strings.Join(forty, ", ")}, nil, ""},
		// Without the 32-entry cap this walk would reach the public 198.51.100.23 at the left end.
		{"C11b the cap, not the left end, stops a long chain", enrTestList, "203.0.113.10:40022", []string{"198.51.100.23, " + strings.Join(forty, ", ")}, nil, ""},
		{"C11c 30 trusted entries stay under the cap", enrTestList, "203.0.113.10:40022", []string{"198.51.100.23, " + strings.Join(forty[:30], ", ")}, nil, "198.51.100.23"},
		{"C13 a private walk result is not a caller", enrTestList, "127.0.0.1:5000", []string{"10.1.2.3"}, nil, ""},
		{"C14 the shared-ELB range is not a caller", enrTestList, "127.0.0.1:5000", []string{"100.125.64.17"}, nil, ""},
		{"C15 empty list, loopback peer: the header is ignored", "", "127.0.0.1:5000", []string{"198.51.100.23"}, nil, ""},
		{"C18 zoned link-local peer is neither trusted nor public", enrTestList, "[fe80::1%eth0]:40022", []string{"198.51.100.23"}, nil, ""},
		{"C19 a mapped CGNAT hop is unmapped", enrTestList, "203.0.113.10:40022", []string{"::ffff:100.125.64.17"}, nil, ""},
		{"public v6 peer, empty list", "", "[2001:db8::5]:443", nil, nil, "2001:db8::5"},
		{"unparseable RemoteAddr", "", "not-an-address", nil, nil, ""},
	})
}

func TestClientIPMarker(t *testing.T) {
	runWalkCases(t, []walkCase{
		{"M1 direct public peer, no marker: its own address", "", "198.51.100.200:4411", nil, nil, "198.51.100.200"},
		{"M2 the marker withholds the tunnel server's address", "", "198.51.100.200:4411", nil, strp("off"), ""},
		{"M3 the marker wins over a trusted chain", enrTestList, "203.0.113.10:40022", []string{"198.51.100.23"}, strp("off"), ""},
		{"M4 any other value also means do not derive", "", "198.51.100.200:4411", nil, strp("on"), ""},
		{"M4 an empty value also means do not derive", "", "198.51.100.200:4411", nil, strp(""), ""},
		{"M5 the tunnel server itself trusted, no marker", enrTestList, "203.0.113.10:40022", nil, nil, ""},
	})
}

// huaweiTrustedList reads config.ingestor.trustedProxyCidrs from the staging overlay (the T26 parser),
// asserting it is declared, empty, and equal to production's. Adding any range to either overlay turns
// this, and H1-H3, red.
func huaweiTrustedList(t *testing.T) string {
	t.Helper()
	chart := filepath.Join("..", "..", "deploy", "helm", "sentinel")
	lists := map[string][]any{}
	for _, f := range []string{"values-huawei-staging.yml", "values-huawei-production.yml"} {
		v := readHelmValues(t, filepath.Join(chart, f))
		raw := helmValue(t, v, "config", "ingestor", "trustedProxyCidrs")
		list, ok := raw.([]any)
		require.Truef(t, ok, "%s: config.ingestor.trustedProxyCidrs must be a YAML list, got %T", f, raw)
		lists[f] = list
	}
	assert.Empty(t, lists["values-huawei-staging.yml"],
		"contract decision 11: no forwarding header is believed on Huawei; the staging list must be empty")
	assert.Equal(t, lists["values-huawei-staging.yml"], lists["values-huawei-production.yml"],
		"staging and production trust the same (empty) list")
	entries := make([]string, len(lists["values-huawei-staging.yml"]))
	for i, e := range lists["values-huawei-staging.yml"] {
		entries[i] = fmt.Sprint(e)
	}
	return strings.Join(entries, ",")
}

func TestClientIPHuawei(t *testing.T) {
	huawei := huaweiTrustedList(t)
	runWalkCases(t, []walkCase{
		{"H1 a direct node-port caller's XFF is ignored, and its own address is private", huawei, "10.200.32.66:51000", []string{"6.6.6.6"}, nil, ""},
		{"H2 the ELB backend range: header ignored, peer is CGNAT", huawei, "100.125.64.17:40022", []string{"198.51.100.23, 172.16.3.117"}, nil, ""},
		{"H3 a node's container gateway after SNAT", huawei, "172.16.3.1:40022", []string{"6.6.6.6, 10.200.32.20"}, nil, ""},
	})
}

func TestClientIPParseTrusted(t *testing.T) {
	for _, c := range []struct {
		in   string
		want []string
	}{
		{"", nil},
		{" , ,", nil},
		{"10.0.0.0/8", []string{"10.0.0.0/8"}},
		{" 10.1.2.3/8 ,, 127.0.0.1 , ::1 ", []string{"10.0.0.0/8", "127.0.0.1/32", "::1/128"}},
		{"2001:db8::/32", []string{"2001:db8::/32"}},
	} {
		got, err := clientip.ParseTrusted(c.in)
		require.NoError(t, err, c.in)
		var gotStrs []string
		for _, p := range got {
			gotStrs = append(gotStrs, p.String())
		}
		assert.Equal(t, c.want, gotStrs, c.in)
	}

	for _, c := range []struct{ in, msg string }{
		{"10.0.0.0/33", `TRUSTED_PROXY_CIDRS: "10.0.0.0/33" is not a CIDR or IP address`},                                                // C12
		{"10.0.0.0/8, bogus", `TRUSTED_PROXY_CIDRS: "bogus" is not a CIDR or IP address`},                                                // C12
		{"::ffff:203.0.113.0/120", `TRUSTED_PROXY_CIDRS: "::ffff:203.0.113.0/120" is an IPv4-mapped IPv6 prefix; write it in IPv4 form`}, // C16
		{"::ffff:203.0.113.10", `TRUSTED_PROXY_CIDRS: "::ffff:203.0.113.10" is an IPv4-mapped IPv6 prefix; write it in IPv4 form`},       // C17
	} {
		_, err := clientip.ParseTrusted(c.in)
		require.Error(t, err, c.in)
		assert.Equal(t, c.msg, err.Error())
	}
}

// TestClientIPStartupExitsOnBadTrustedList is C12/C16/C17 at main(): the ingestor binary, given a bad
// TRUSTED_PROXY_CIDRS, logs the §3.3 text and exits 1 before it connects to anything or listens. No
// Postgres, NATS or Redis is needed, because the parse runs first.
func TestClientIPStartupExitsOnBadTrustedList(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "ingestor")
	build := exec.Command("go", "build", "-o", bin, "./apps/ingestor-go")
	build.Dir = filepath.Join("..", "..")
	out, err := build.CombinedOutput()
	require.NoError(t, err, string(out))

	for _, c := range []struct{ env, want string }{
		{"10.0.0.0/33", `TRUSTED_PROXY_CIDRS: \"10.0.0.0/33\" is not a CIDR or IP address`},
		{"::ffff:203.0.113.0/120", "is an IPv4-mapped IPv6 prefix; write it in IPv4 form"},
		{"::ffff:203.0.113.10", "is an IPv4-mapped IPv6 prefix; write it in IPv4 form"},
	} {
		t.Run(c.env, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, bin)
			cmd.Env = append(os.Environ(),
				"TRUSTED_PROXY_CIDRS="+c.env,
				// Anything past the parse would try these and hang or fail differently.
				"POSTGRES_HOST=127.0.0.1", "POSTGRES_PORT=1", "OTEL_SDK_DISABLED=true")
			out, err := cmd.CombinedOutput()
			var exitErr *exec.ExitError
			require.ErrorAs(t, err, &exitErr, "the ingestor must exit non-zero: %s", out)
			assert.Equal(t, 1, exitErr.ExitCode(), string(out))
			assert.Contains(t, string(out), c.want)
			assert.NotContains(t, string(out), "database", "it must exit before connecting to anything")
		})
	}
}

func TestClientIPIsPublic(t *testing.T) {
	notPublic := []string{
		"0.0.0.0", "::", "127.0.0.1", "::1", "10.0.0.1", "172.16.3.117", "192.168.1.1", "fd00::1",
		"169.254.169.254", "fe80::1", "fe80::1%eth0", "224.0.0.1", "100.64.0.1", "100.127.255.254",
		"100.125.64.17", "::ffff:10.0.0.1", "::ffff:100.125.64.17", "0.1.2.3", "240.0.0.1",
		"255.255.255.255", "fec0::1", "::10.0.0.1", "64:ff9b::a00:1", "64:ff9b:1::a00:1", "2002:a00:1::1",
		"ff02::1", "ff01::1",
	}
	public := []string{
		"198.51.100.23", "203.0.113.5", "81.2.69.142", "6.6.6.6", "2001:db8::1", "2001:db8::1%eth0",
		"100.63.255.255", "100.128.0.0", "1.0.0.1", "223.255.255.255", "2003::1", "64:ff9a::1",
		"::ffff:198.51.100.23",
	}
	for _, s := range notPublic {
		assert.Falsef(t, clientip.IsPublic(netip.MustParseAddr(s)), "%s must not be public", s)
	}
	for _, s := range public {
		assert.Truef(t, clientip.IsPublic(netip.MustParseAddr(s)), "%s must be public", s)
	}
}

// TestClientIPMiddlewareCarriesInfo proves the per-request data reaches the handler through the typed
// accessor (spec §7.2), User-Agent included even when no caller IP is derived.
func TestClientIPMiddlewareCarriesInfo(t *testing.T) {
	var got clientip.Info
	var ok bool
	h := clientip.Middleware(clientip.NewResolver(nil), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok = clientip.RequestInfoFromContext(r.Context())
	}))

	r := httptest.NewRequest(http.MethodPost, "/ingest", nil)
	r.RemoteAddr = "198.51.100.77:4411"
	r.Header.Set("User-Agent", "Mozilla/5.0 test")
	h.ServeHTTP(httptest.NewRecorder(), r)
	require.True(t, ok)
	assert.Equal(t, clientip.Info{ClientIP: netip.MustParseAddr("198.51.100.77"), HasClientIP: true, UserAgent: "Mozilla/5.0 test"}, got)

	r = httptest.NewRequest(http.MethodPost, "/ingest", nil)
	r.RemoteAddr = "198.51.100.77:4411"
	r.Header.Set("User-Agent", "Mozilla/5.0 test")
	r.Header.Set(clientip.DeriveIPHeader, "off")
	h.ServeHTTP(httptest.NewRecorder(), r)
	require.True(t, ok)
	assert.Equal(t, clientip.Info{UserAgent: "Mozilla/5.0 test"}, got, "the marker withholds the IP, not the User-Agent")

	_, ok = clientip.RequestInfoFromContext(context.Background())
	assert.False(t, ok, "a context without Info reports none")
}

// helmValue walks nested YAML maps and returns the raw value (helmPath stringifies it).
func helmValue(t *testing.T, v map[string]any, keys ...string) any {
	t.Helper()
	var cur any = v
	for _, k := range keys {
		m, ok := cur.(map[string]any)
		require.Truef(t, ok, "%s: not a map at %q", strings.Join(keys, "."), k)
		cur, ok = m[k]
		require.Truef(t, ok, "%s: key %q missing", strings.Join(keys, "."), k)
	}
	return cur
}
