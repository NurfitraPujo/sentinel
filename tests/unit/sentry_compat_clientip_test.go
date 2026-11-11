package unit

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/clientip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scRelayRequest is what core's relay consumer's request looks like to the ingestor.
func scRelayRequest(remote, xff string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/27690631804612/envelope/", nil)
	r.RemoteAddr = remote
	if xff != "" {
		r.Header.Set("X-Forwarded-For", xff)
	}
	r.Header.Set("User-Agent", "Dart/3.11 (dart:io)")
	return r
}

func scInfo(t *testing.T, res *clientip.Resolver, r *http.Request) clientip.Info {
	t.Helper()
	var got clientip.Info
	var ok bool
	clientip.Middleware(res, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got, ok = clientip.RequestInfoFromContext(r.Context())
	})).ServeHTTP(httptest.NewRecorder(), r)
	require.True(t, ok)
	return got
}

// P2.11, resolver half (the chain half is in sentry_compat_relay_test.go, B7).
func TestSentryCompatResolverBelievesRelay(t *testing.T) {
	_, compat, err := clientip.Resolvers("", "10.20.0.0/16")
	require.NoError(t, err)

	want := clientip.Info{ClientIP: netip.MustParseAddr("203.0.113.9"), HasClientIP: true, UserAgent: "Dart/3.11 (dart:io)", PeerTrusted: true}
	assert.Equal(t, want, scInfo(t, compat, scRelayRequest("10.20.3.4:5555", "203.0.113.9")), "trusted peer: XFF believed")
	assert.Equal(t, want, scInfo(t, compat, scRelayRequest("10.20.3.4:5555", "203.0.113.9, 10.20.9.9")), "an internal hop appended")

	priv := scInfo(t, compat, scRelayRequest("10.20.3.4:5555", "192.168.1.5"))
	assert.False(t, priv.HasClientIP, "a private XFF is no caller address")
	assert.True(t, priv.PeerTrusted)

	// An untrusted peer's XFF is not read: the caller is the socket address.
	un := scInfo(t, compat, scRelayRequest("198.51.100.20:5555", "203.0.113.9"))
	assert.Equal(t, netip.MustParseAddr("198.51.100.20"), un.ClientIP)
	assert.True(t, un.HasClientIP)
	assert.False(t, un.PeerTrusted)

	for _, bad := range []string{"10.20.0.0/33", "::ffff:10.0.0.0/104"} {
		_, _, err := clientip.Resolvers("", bad)
		require.Error(t, err, bad)
		assert.True(t, strings.HasPrefix(err.Error(), "SENTRY_COMPAT_TRUSTED_PROXY_CIDRS:"), err.Error())
		assert.False(t, strings.HasPrefix(err.Error(), "TRUSTED_PROXY_CIDRS:"), err.Error())
	}
	_, _, err = clientip.Resolvers("10.20.0.0/33", "")
	require.Error(t, err)
	assert.True(t, strings.HasPrefix(err.Error(), "TRUSTED_PROXY_CIDRS:"), err.Error())
	// ParseTrusted's own messages are unchanged.
	_, err = clientip.ParseTrusted("nope")
	require.EqualError(t, err, `TRUSTED_PROXY_CIDRS: "nope" is not a CIDR or IP address`)
}

// P2.12
func TestIngestResolverIgnoresCompatList(t *testing.T) {
	ingest, compat, err := clientip.Resolvers("", "10.20.0.0/16")
	require.NoError(t, err)
	r := scRelayRequest("10.20.3.4:5555", "203.0.113.9")
	_, ok := ingest.Resolve(r)
	assert.False(t, ok, "/ingest's resolver: the private peer is the caller and is not public")
	assert.Empty(t, ingest.Trusted())
	assert.False(t, ingest.PeerTrusted(r))
	assert.Len(t, compat.Trusted(), 1)

	ingest, compat, err = clientip.Resolvers("192.0.2.0/24", "")
	require.NoError(t, err)
	want := []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}
	assert.Equal(t, want, ingest.Trusted())
	assert.Equal(t, want, compat.Trusted())
	r = scRelayRequest("192.0.2.7:1234", "203.0.113.5")
	for name, res := range map[string]*clientip.Resolver{"ingest": ingest, "compat": compat} {
		got, ok := res.Resolve(r)
		require.True(t, ok, name)
		assert.Equal(t, "203.0.113.5", got.String(), name)
	}
}

// P7.8, PeerTrusted half: the field is additive on the ingest resolver too. (The source scan half is B8's.)
func TestSentryCompatPeerTrustedInfo(t *testing.T) {
	ingest, _, err := clientip.Resolvers("10.20.0.0/16", "")
	require.NoError(t, err)
	assert.True(t, scInfo(t, ingest, scRelayRequest("10.20.3.4:5555", "")).PeerTrusted)
	assert.False(t, scInfo(t, ingest, scRelayRequest("198.51.100.20:5555", "10.20.3.4")).PeerTrusted, "an address inside XFF never trusts the request")
	assert.False(t, scInfo(t, clientip.NewResolver(nil), scRelayRequest("10.20.3.4:5555", "")).PeerTrusted)
}
