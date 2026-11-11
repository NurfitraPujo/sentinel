package unit

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/clientip"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/sentrycompat"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/event"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/masker"
	sentinelv1 "github.com/NurfitraPujo/sentinel/gen/sentinel/v1"
)

const (
	scTrustedPeer   = "10.20.3.4:5555"
	scUntrustedPeer = "198.51.100.20:5555"
)

// scLogCapture records every slog record the handlers write.
type scLogCapture struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (c *scLogCapture) Enabled(context.Context, slog.Level) bool { return true }
func (c *scLogCapture) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recs = append(c.recs, r.Clone())
	return nil
}
func (c *scLogCapture) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *scLogCapture) WithGroup(string) slog.Handler      { return c }

func (c *scLogCapture) byEvent(name string) []slog.Record {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []slog.Record
	for _, r := range c.recs {
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "event" && a.Value.String() == name {
				out = append(out, r)
				return false
			}
			return true
		})
	}
	return out
}

// scRelayHarness is spec §11.2's TASK-07 harness: the real chain with the compat resolver trusting 10.20.0.0/16.
type scRelayHarness struct {
	t     *testing.T
	env   http.Handler
	store http.Handler
	pub   *fakeEventPublisher
	logs  *scLogCapture
}

func scNewRelayHarness(t *testing.T) *scRelayHarness {
	t.Helper()
	return scNewRelayHarnessLists(t, "", "10.20.0.0/16")
}

// scNewRelayHarnessLists is the same chain over TRUSTED_PROXY_CIDRS = global and SENTRY_COMPAT_TRUSTED_PROXY_CIDRS = compat.
func scNewRelayHarnessLists(t *testing.T, global, compat string) *scRelayHarness {
	t.Helper()
	_, compatRes, err := clientip.Resolvers(global, compat)
	require.NoError(t, err)
	svc, pub := newTestIngestService(t)
	logs := &scLogCapture{}
	cfg := sentrycompat.Config{Ingest: svc, Scope: scScope, Logger: slog.New(logs)}
	a, rl := scAuthenticator(), scLimiter(t)
	return &scRelayHarness{t: t, pub: pub, logs: logs,
		env:   sentrycompat.Chain(compatRes, a, rl, sentrycompat.NewEnvelopeHandler(cfg)),
		store: sentrycompat.Chain(compatRes, a, rl, sentrycompat.NewStoreHandler(cfg))}
}

// scRelayEventBody is the Flutter event with its tag pairs turned into an object and mut applied.
func scRelayEventBody(t *testing.T, mut func(m map[string]any)) []byte {
	t.Helper()
	m := scFixtureMap(t, "flutter-typeerror-unhandled.json")
	tags := map[string]any{}
	for _, pair := range m["tags"].([]any) {
		kv := pair.([]any)
		tags[kv[0].(string)] = kv[1]
	}
	m["tags"] = tags
	if mut != nil {
		mut(m)
	}
	return scJSON(t, m)
}

// send presents the RELAY key (sentinel-relay-key-trust): the only credential that makes relay headers count.
func (h *scRelayHarness) send(peer string, hdr map[string]string, body []byte) (*sentinelv1.ErrorEvent, *httptest.ResponseRecorder) {
	h.t.Helper()
	return h.sendKey(scRelayKey, peer, hdr, body)
}

// sendIngest presents an ordinary ingest key.
func (h *scRelayHarness) sendIngest(peer string, hdr map[string]string, body []byte) (*sentinelv1.ErrorEvent, *httptest.ResponseRecorder) {
	h.t.Helper()
	return h.sendKey(scKey, peer, hdr, body)
}

func (h *scRelayHarness) sendKey(key, peer string, hdr map[string]string, body []byte) (*sentinelv1.ErrorEvent, *httptest.ResponseRecorder) {
	h.t.Helper()
	h.pub.published = nil
	if body == nil {
		body = scRelayEventBody(h.t, nil)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/"+scPathID+"/envelope/", bytes.NewReader(scEnvelope(map[string]any{}, scEvent(body))))
	r.SetPathValue("project_id", scPathID)
	r.RemoteAddr = peer
	r.Header.Set("X-Sentry-Auth", "Sentry sentry_key="+key)
	r.Header.Set("X-Forwarded-For", "203.0.113.9")
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.env.ServeHTTP(w, r)
	require.Equal(h.t, http.StatusOK, w.Code, w.Body.String())
	return publishedEvent(h.t, h.pub), w
}

func scRelayHdr(auth string, extra ...string) map[string]string {
	h := map[string]string{"X-Sentinel-Relay-Auth": auth}
	if len(extra)%2 == 0 {
		for i := 0; i < len(extra); i += 2 {
			h[extra[i]] = extra[i+1]
		}
	}
	return h
}

var scUserHdr = scRelayHdr("user", "X-Sentinel-Relay-User-Id", "core-user-42", "X-Sentinel-Relay-Tenant-Id", "company-7")

// scStable clears what differs between two requests of the same event: the receipt stamp and the clamped time.
func scStable(e *sentinelv1.ErrorEvent) *sentinelv1.ErrorEvent {
	c := proto.Clone(e).(*sentinelv1.ErrorEvent)
	c.ReceivedAt, c.Timestamp = nil, nil
	return c
}

// P7.1
func TestRelayIdentityUserWithRelayKey(t *testing.T) {
	h := scNewRelayHarness(t)
	base, _ := h.send(scTrustedPeer, nil, nil)
	got, _ := h.send(scTrustedPeer, scUserHdr, nil)
	assert.Equal(t, "core-user-42", got.User.Id)
	assert.Equal(t, "user-00000001", base.User.Id)
	assert.True(t, proto.Equal(base.User.Geo, got.User.Geo), "geo is kept")
	assert.Equal(t, "203.0.113.9", got.User.IpAddress)
	assert.Equal(t, base.User.IpAddress, got.User.IpAddress)
	assert.Equal(t, base.User.Username, got.User.Username)
	assert.Equal(t, "company-7", got.Tags["tenant_id"])
	assert.Equal(t, "user", got.Tags["relay_auth"])
	want := map[string]string{}
	for k, v := range base.Tags {
		want[k] = v
	}
	want["tenant_id"], want["relay_auth"] = "company-7", "user"
	assert.Equal(t, want, got.Tags, "the other 22 tags are unchanged")
	assert.Len(t, base.Tags, 22)
}

// P7.2
func TestRelayIdentityAnonymousWithRelayKey(t *testing.T) {
	h := scNewRelayHarness(t)
	body := scRelayEventBody(t, func(m map[string]any) {
		m["tags"].(map[string]any)["tenant_id"] = "app-tenant"
		m["tags"].(map[string]any)["tenant_id\u0000"] = "victim-tenant" // finding d14-nul-tag-key
		m["extra"] = map[string]any{"user_id": "u1", "userId": "u2", "user": "u3", "tenant_id": "t1", "tenantId": "t2", "organization_id": "t3", "other": "x"}
	})
	base, _ := h.send(scTrustedPeer, nil, body)
	got, _ := h.send(scTrustedPeer, scRelayHdr("anonymous", "X-Sentinel-Relay-User-Id", "stray-user", "X-Sentinel-Relay-Tenant-Id", "stray-tenant"), body)

	assert.Equal(t, "anonymous", got.Tags["relay_auth"])
	assert.NotContains(t, got.Tags, "tenant_id", "the body's tenant_id, and its NUL-keyed twin, are both gone")
	assert.Equal(t, "", got.User.Id)
	assert.True(t, proto.Equal(base.User.Geo, got.User.Geo))
	assert.Equal(t, base.User.IpAddress, got.User.IpAddress)
	md := got.Metadata.AsMap()
	for _, k := range []string{"user_id", "userId", "user", "tenant_id", "tenantId", "organization_id"} {
		assert.NotContains(t, md, k, "the processor's metadata fallback for attribution")
	}
	assert.Equal(t, "x", md["other"])

	// The proof is not vacuous: without the relay header the processor attributes the event to the body's ids.
	prep := func(e *sentinelv1.ErrorEvent) *event.ErrorEvent {
		data, err := proto.Marshal(e)
		require.NoError(t, err)
		evt, err := event.Deserialize(data)
		require.NoError(t, err)
		evt.Prepare(masker.NewMasker())
		return evt
	}
	b := prep(base)
	assert.Equal(t, "user-00000001", b.AffectedUserID)
	assert.NotEmpty(t, b.AffectedTenantID)
	a := prep(got)
	assert.Empty(t, a.AffectedUserID)
	assert.Empty(t, a.AffectedTenantID)
}

// P7.3 (sentinel-relay-key-trust §4.2): an ingest key's relay headers change nothing, from ANY peer, including one
// inside the compat trusted list, and an address inside X-Forwarded-For does not change that.
func TestRelayIdentityIgnoredWithIngestKey(t *testing.T) {
	h := scNewRelayHarness(t)
	for _, peer := range []string{scUntrustedPeer, scTrustedPeer} {
		without, _ := h.sendIngest(peer, nil, nil)
		with, _ := h.sendIngest(peer, scUserHdr, nil)
		assert.True(t, proto.Equal(scStable(without), scStable(with)), peer+": an ingest key's relay headers change nothing")
		assert.NotContains(t, with.Tags, "relay_auth", peer)
		assert.NotContains(t, with.Tags, "tenant_id", peer)
		assert.Equal(t, "user-00000001", with.User.Id, peer)
	}
	hdr := scRelayHdr("user", "X-Sentinel-Relay-User-Id", "core-user-42", "X-Sentinel-Relay-Tenant-Id", "company-7", "X-Forwarded-For", "10.20.3.4")
	viaXFF, _ := h.sendIngest(scUntrustedPeer, hdr, nil)
	assert.NotContains(t, viaXFF.Tags, "relay_auth")
	assert.Equal(t, "user-00000001", viaXFF.User.Id)

	// The converse: the same headers from the same untrusted peer ARE honoured with a relay key.
	viaKey, _ := h.send(scUntrustedPeer, scUserHdr, nil)
	assert.Equal(t, "core-user-42", viaKey.User.Id)
	assert.Equal(t, "user", viaKey.Tags["relay_auth"])
}

// P7.4
func TestRelayIdentityInvalidAuthValue(t *testing.T) {
	h := scNewRelayHarness(t)
	ids := []string{"X-Sentinel-Relay-User-Id", "core-user-42", "X-Sentinel-Relay-Tenant-Id", "company-7"}
	got, _ := h.send(scUntrustedPeer, scRelayHdr("USER ", ids...), nil)
	assert.Equal(t, "user", got.Tags["relay_auth"], "trimmed and case-insensitive")
	assert.Equal(t, "core-user-42", got.User.Id)
	for _, v := range []string{"admin", ""} {
		got, w := h.send(scTrustedPeer, scRelayHdr(v, ids...), nil)
		assert.Equal(t, http.StatusOK, w.Code, "a trusted relay's bad value never refuses the envelope")
		assert.NotContains(t, got.Tags, "relay_auth", v)
		assert.NotContains(t, got.Tags, "tenant_id", v)
		assert.Equal(t, "user-00000001", got.User.Id, v)
	}
	assert.Len(t, h.logs.byEvent("sentrycompat.relay_auth_invalid"), 1, "the invalid non-empty value is logged once; the empty one is not")
	for _, r := range h.logs.byEvent("sentrycompat.relay_auth_invalid") {
		r.Attrs(func(a slog.Attr) bool {
			assert.NotContains(t, a.Value.String(), "admin")
			return true
		})
	}
}

// P7.5
func TestRelayIdentityPartialUserHeaders(t *testing.T) {
	h := scNewRelayHarness(t)
	got, _ := h.send(scTrustedPeer, scRelayHdr("user", "X-Sentinel-Relay-User-Id", "core-user-42"), nil)
	assert.Equal(t, "core-user-42", got.User.Id)
	assert.Equal(t, "user", got.Tags["relay_auth"])
	assert.NotContains(t, got.Tags, "tenant_id")

	got, _ = h.send(scTrustedPeer, scRelayHdr("user", "X-Sentinel-Relay-Tenant-Id", "company-7"), nil)
	assert.Equal(t, "company-7", got.Tags["tenant_id"])
	assert.Equal(t, "user-00000001", got.User.Id, "the event's own user id stays")

	body := scRelayEventBody(t, func(m map[string]any) { m["tags"].(map[string]any)["tenant_id"] = "app-value" })
	got, _ = h.send(scTrustedPeer, scRelayHdr("user", "X-Sentinel-Relay-Tenant-Id", "company-7"), body)
	assert.Equal(t, "company-7", got.Tags["tenant_id"], "the relay's value replaces the app's")
	got, _ = h.send(scTrustedPeer, scRelayHdr("user", "X-Sentinel-Relay-User-Id", "core-user-42"), body)
	assert.Equal(t, "app-value", got.Tags["tenant_id"], "an absent header overrides nothing")

	// no user object at all: the relay's id allocates one
	noUser := scRelayEventBody(t, func(m map[string]any) { delete(m, "user") })
	got, _ = h.send(scTrustedPeer, scUserHdr, noUser)
	assert.Equal(t, "core-user-42", got.User.Id)
}

// P7.6
func TestRelayIdentityTagsSurviveBounds(t *testing.T) {
	h := scNewRelayHarness(t)
	body := scRelayEventBody(t, func(m map[string]any) {
		tags := map[string]any{}
		for i := 0; i < 60; i++ {
			tags[fmt.Sprintf("a%02d", i)] = "v"
		}
		m["tags"] = tags
	})
	got, _ := h.send(scTrustedPeer, scUserHdr, body)
	assert.Len(t, got.Tags, 50)
	assert.Equal(t, "user", got.Tags["relay_auth"])
	assert.Equal(t, "company-7", got.Tags["tenant_id"])
	for i := 0; i < 48; i++ {
		assert.Contains(t, got.Tags, fmt.Sprintf("a%02d", i))
	}
	assert.NotContains(t, got.Tags, "a48")
	// Without the relay headers the bounds step keeps a00..a49, as it does for any SDK.
	base, _ := h.send(scTrustedPeer, nil, body)
	assert.Len(t, base.Tags, 50)
	assert.Contains(t, base.Tags, "a49")
}

// P7.7
func TestRelayIdentityOnStore(t *testing.T) {
	h := scNewRelayHarness(t)
	h.pub.published = nil
	r := httptest.NewRequest(http.MethodPost, "/api/"+scPathID+"/store/", bytes.NewReader(scRelayEventBody(t, nil)))
	r.SetPathValue("project_id", scPathID)
	r.RemoteAddr = scTrustedPeer
	r.Header.Set("X-Sentry-Auth", "Sentry sentry_key="+scRelayKey)
	r.Header.Set("X-Forwarded-For", "203.0.113.9")
	for k, v := range scUserHdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.store.ServeHTTP(w, r)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	got := publishedEvent(t, h.pub)
	assert.Equal(t, "core-user-42", got.User.Id)
	assert.Equal(t, "company-7", got.Tags["tenant_id"])
	assert.Equal(t, "user", got.Tags["relay_auth"])
	assert.Equal(t, "203.0.113.9", got.User.IpAddress)
}

// P7.8: the headers are read by the compat package alone, so /ingest cannot honour them.
func TestRelayHeadersOnlyInCompat(t *testing.T) {
	root := filepath.Join("..", "..")
	var offenders []string
	for _, dir := range []string{"apps", "packages", "tools", "cmd"} {
		_ = filepath.Walk(filepath.Join(root, dir), func(p string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(p, ".go") {
				return nil
			}
			slash := filepath.ToSlash(p)
			if strings.Contains(slash, "apps/ingestor-go/sentrycompat/") || strings.Contains(slash, "/node_modules/") {
				return nil
			}
			raw, err := os.ReadFile(p)
			if err == nil && strings.Contains(strings.ToLower(string(raw)), "x-sentinel-relay-") {
				offenders = append(offenders, slash)
			}
			return nil
		})
	}
	assert.Empty(t, offenders, "only apps/ingestor-go/sentrycompat reads the relay identity headers")
	assert.Equal(t, "X-Sentinel-Relay-Auth", sentrycompat.HeaderRelayAuth)
	assert.Equal(t, "X-Sentinel-Relay-User-Id", sentrycompat.HeaderRelayUserID)
	assert.Equal(t, "X-Sentinel-Relay-Tenant-Id", sentrycompat.HeaderRelayTenantID)

	// The additive Info field: /ingest's own resolver fills PeerTrusted, and nothing on its path reads it.
	ingest, _, err := clientip.Resolvers("10.20.0.0/16", "")
	require.NoError(t, err)
	assert.True(t, scInfo(t, ingest, scRelayRequest(scTrustedPeer, "")).PeerTrusted)
}

// P7.9 (D14)
func TestRelayAuthTagFromBodyAlwaysRemoved(t *testing.T) {
	for _, js := range []string{
		`{"tags":{"relay_auth":"user"}}`, `{"tags":[["relay_auth","user"]]}`,
		`{"tags":{"relay_auth\u0000":"user"}}`, `{"tags":[["relay_auth\u0000","user"]]}`,
	} {
		p, err := sentrycompat.MapEvent([]byte(js), sentrycompat.EnvelopeHeader{})
		require.NoError(t, err)
		assert.NotContains(t, p.Tags, "relay_auth", js)
	}

	h := scNewRelayHarness(t)
	forms := map[string]func(m map[string]any){
		"object":          func(m map[string]any) { m["tags"].(map[string]any)["relay_auth"] = "user" },
		"object with NUL": func(m map[string]any) { m["tags"].(map[string]any)["relay_auth\u0000"] = "user" },
		"pairs": func(m map[string]any) {
			m["tags"] = append(scTagPairs(m["tags"].(map[string]any)), []any{"relay_auth", "user"}, []any{"relay_auth\u0000", "user"})
		},
	}
	for name, mut := range forms {
		body := scRelayEventBody(t, mut)
		got, _ := h.sendIngest(scUntrustedPeer, nil, body)
		assert.NotContains(t, got.Tags, "relay_auth", name+": ingest key, no relay headers")
		got, _ = h.sendIngest(scTrustedPeer, scRelayHdr("anonymous"), body)
		assert.NotContains(t, got.Tags, "relay_auth", name+": ingest key ignores the header, body tag dropped")
		got, _ = h.send(scTrustedPeer, nil, body)
		assert.NotContains(t, got.Tags, "relay_auth", name+": relay key, Auth absent")
		got, _ = h.send(scTrustedPeer, scRelayHdr("admin"), body)
		assert.NotContains(t, got.Tags, "relay_auth", name+": relay key, Auth invalid")
		got, _ = h.send(scTrustedPeer, scRelayHdr("anonymous"), body)
		assert.Equal(t, "anonymous", got.Tags["relay_auth"], name+": the header's value, not the body's")
	}
}

func scTagPairs(m map[string]any) []any {
	var out []any
	for k, v := range m {
		out = append(out, []any{k, v})
	}
	return out
}

// P7.10 (D14, D1)
func TestRelayIngestKeyKeepsBodyIdentity(t *testing.T) {
	h := scNewRelayHarness(t)
	body := scRelayEventBody(t, func(m map[string]any) {
		m["tags"].(map[string]any)["tenant_id"] = "app-tenant"
		m["tags"].(map[string]any)["relay_auth"] = "user"
		m["extra"] = map[string]any{"user_id": "body-extra-user"}
	})
	got, _ := h.sendIngest(scTrustedPeer, scUserHdr, body)
	assert.Equal(t, "user-00000001", got.User.Id)
	assert.Equal(t, "app-tenant", got.Tags["tenant_id"])
	assert.Equal(t, "body-extra-user", got.Metadata.AsMap()["user_id"])
	assert.NotContains(t, got.Tags, "relay_auth")
}

// P7.11: the ignored-headers line for a non-relay key.
func TestRelayHeadersIgnoredLogged(t *testing.T) {
	h := scNewRelayHarness(t)
	forbidden := []string{"user", "core-user-42", "company-7", "203.0.113.9", "198.51.100.20", scKey, scRelayKey}
	h.sendIngest(scUntrustedPeer, scUserHdr, nil)
	recs := h.logs.byEvent("sentrycompat.relay_headers_ignored")
	require.Len(t, recs, 1)
	assert.Equal(t, slog.LevelInfo, recs[0].Level)
	seen := map[string]string{}
	recs[0].Attrs(func(a slog.Attr) bool { seen[a.Key] = a.Value.String(); return true })
	assert.Equal(t, "not_relay_key", seen["reason"])
	assert.Equal(t, "ingest", seen["scope"])
	assert.Len(t, seen, 3, "the event name, the reason and the key's scope, nothing else")
	for k, v := range seen {
		for _, f := range forbidden {
			assert.NotEqual(t, f, v, "attribute %s carries a header value", k)
			assert.False(t, strings.Contains(v, "core-user-42") || strings.Contains(v, "company-7") || strings.Contains(v, scKey), k)
		}
	}
	assert.False(t, strings.Contains(recs[0].Message, "core-user-42") || strings.Contains(recs[0].Message, "203.0.113.9"))
	assert.Empty(t, h.logs.byEvent("sentrycompat.relay_peer_untrusted"), "the network-based line is gone")

	before := len(h.logs.byEvent("sentrycompat.relay_headers_ignored"))
	h.sendIngest(scUntrustedPeer, nil, nil) // no relay headers: no record
	h.send(scTrustedPeer, scUserHdr, nil)   // a relay key: honoured, no record
	assert.Len(t, h.logs.byEvent("sentrycompat.relay_headers_ignored"), before, "not for a request without the headers, not for a relay key")

	// Contract 4.2: once per request that carries ANY relay header. Each header alone, with an ingest key, logs once.
	for _, name := range []string{"X-Sentinel-Relay-User-Id", "X-Sentinel-Relay-Tenant-Id", "X-Sentinel-Relay-Auth"} {
		h := scNewRelayHarness(t)
		h.sendIngest(scUntrustedPeer, map[string]string{name: "x"}, nil)
		assert.Len(t, h.logs.byEvent("sentrycompat.relay_headers_ignored"), 1, "only %s was sent", name)
	}
}

// finding rev1-relay-trust-global-list-undocumented (D12), restated: a peer inside TRUSTED_PROXY_CIDRS ONLY has its
// X-Forwarded-For believed on the compat routes for a non-relay key, and no network membership grants relay identity.
func TestRelayIdentityIgnoredFromGlobalListPeer(t *testing.T) {
	const globalPeer = "10.30.3.4:5555"
	h := scNewRelayHarnessLists(t, "10.30.0.0/16", "10.20.0.0/16")
	base, _ := h.sendIngest(globalPeer, nil, nil)
	got, _ := h.sendIngest(globalPeer, scUserHdr, nil)
	assert.Equal(t, "203.0.113.9", got.User.IpAddress, "the global list still makes X-Forwarded-For believed for an ingest key")
	assert.Equal(t, base.User.Id, got.User.Id, "the relay's user id is not applied")
	assert.NotContains(t, got.Tags, "relay_auth")
	assert.NotContains(t, got.Tags, "tenant_id")
	assert.True(t, proto.Equal(scStable(base), scStable(got)), "headers from a global-list peer change nothing")

	anon, _ := h.sendIngest(globalPeer, scRelayHdr("anonymous"), nil)
	assert.Equal(t, base.User.Id, anon.User.Id, "anonymous stripping is not applied either")
	assert.NotContains(t, anon.Tags, "relay_auth")

	// A compat-list peer with an INGEST key is not believed either: the network no longer grants identity.
	inList, _ := h.sendIngest(scTrustedPeer, scUserHdr, nil)
	assert.Equal(t, base.User.Id, inList.User.Id)
	assert.NotContains(t, inList.Tags, "relay_auth")

	// A relay key is believed from a peer in NO list, with no list configured at all.
	empty := scNewRelayHarnessLists(t, "", "")
	relayed, _ := empty.send(scUntrustedPeer, scUserHdr, nil)
	assert.Equal(t, "core-user-42", relayed.User.Id)
	assert.Equal(t, "user", relayed.Tags["relay_auth"])
}

// sentinel-relay-key-trust K3: for a relay key the client IP is the rightmost X-Forwarded-For entry when it is public,
// and none otherwise; for any other key the union resolver decides, unchanged.
func TestRelayClientIPIsRightmostPublicXFF(t *testing.T) {
	h := scNewRelayHarnessLists(t, "", "")
	xff := func(v string) map[string]string { return map[string]string{"X-Forwarded-For": v} }

	got, _ := h.send(scUntrustedPeer, xff("198.51.100.77, 203.0.113.9"), nil)
	assert.Equal(t, "203.0.113.9", got.User.IpAddress, "the rightmost entry, not the leftmost (the phone's own claim)")

	// A relay key from a private peer (core's node) still gets the phone's address, with an empty trusted list.
	got, _ = h.send("10.200.32.66:4000", xff("203.0.113.9"), nil)
	assert.Equal(t, "203.0.113.9", got.User.IpAddress)

	// Rightmost private, or unparseable, or absent: no client IP, even though a public entry sits to its left.
	for _, v := range []string{"203.0.113.9, 10.1.2.3", "203.0.113.9, not-an-ip", "203.0.113.9, ", "127.0.0.1"} {
		got, _ = h.send(scUntrustedPeer, xff(v), nil)
		assert.Empty(t, got.User.IpAddress, v)
	}
	// Multiple header lines: the last line's last entry is the rightmost.
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.Header.Add("X-Forwarded-For", "198.51.100.1")
	r.Header.Add("X-Forwarded-For", "203.0.113.9")
	ip, ok := clientip.RelayedClientIP(r)
	require.True(t, ok)
	assert.Equal(t, "203.0.113.9", ip.String())

	// The X-Sentinel-Derive-IP opt-out (as in Resolve): a request carrying it gets no client IP, whatever its XFF.
	optOut := httptest.NewRequest(http.MethodPost, "/", nil)
	optOut.Header.Set("X-Forwarded-For", "203.0.113.9")
	optOut.Header.Set(clientip.DeriveIPHeader, "0")
	_, ok = clientip.RelayedClientIP(optOut)
	assert.False(t, ok, "the derive-IP opt-out wins over a public rightmost entry")

	// An ingest key from a peer inside neither list: the peer itself (198.51.100.20), XFF not read.
	got, _ = h.sendIngest(scUntrustedPeer, xff("203.0.113.9"), nil)
	assert.Equal(t, "198.51.100.20", got.User.IpAddress, "a non-relay key keeps the union resolver: XFF is not believed from an untrusted peer")
}

// Same at the resolver: PeerTrusted on the compat resolver follows the compat list alone, the walk the union.
func TestCompatResolverPeerTrustedIsCompatListOnly(t *testing.T) {
	_, compat, err := clientip.Resolvers("10.30.0.0/16", "10.20.0.0/16")
	require.NoError(t, err)
	g := scInfo(t, compat, scRelayRequest("10.30.3.4:5555", "203.0.113.9"))
	assert.False(t, g.PeerTrusted)
	assert.Equal(t, "203.0.113.9", g.ClientIP.String())
	assert.True(t, scInfo(t, compat, scRelayRequest("10.20.3.4:5555", "203.0.113.9")).PeerTrusted)

	_, compat, err = clientip.Resolvers("10.30.0.0/16", "")
	require.NoError(t, err)
	assert.False(t, scInfo(t, compat, scRelayRequest("10.30.3.4:5555", "")).PeerTrusted, "empty compat list: nothing is trusted")
}
