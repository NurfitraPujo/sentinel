package unit

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	sentinelv1 "github.com/NurfitraPujo/sentinel/gen/sentinel/v1"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/clientip"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/enrich"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/service"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/validation"
)

// sentinel-event-enrichment §7.3 / §10.2 "Compatibility": an event that already carries every enriched
// field is stored exactly as today; enrichment does no network I/O on the request path; and it runs
// BEFORE the bounds step, so a derived value is bounded and marked exactly as a sent one.

func enrFullInfo() clientip.Info {
	return clientip.Info{ClientIP: netip.MustParseAddr("81.2.69.142"), HasClientIP: true, UserAgent: uaChromeWindows}
}

// enrReadyLoader is a geo loader over the fixture, ready, with a counting source.
func enrReadyLoader(t *testing.T) (*fakeGeoSource, *enrich.Enricher) {
	t.Helper()
	clock := &geoClock{now: geoBuildEpoch.Add(24 * time.Hour)}
	src := &fakeGeoSource{}
	src.serve(`"a"`, geoFixture(t, "GeoLite2-City-Test.mmdb"))
	loader, _ := newGeoLoader(src, clock)
	loader.Poll(context.Background())
	require.Equal(t, "GeoLite2-City", loader.DatabaseType())
	return src, &enrich.Enricher{Geo: loader, UA: sharedUAParser()}
}

func TestEnrichSentFieldsByteForByte(t *testing.T) {
	_, e := enrReadyLoader(t)
	for _, platform := range []string{"javascript", "go"} {
		p := &validation.ErrorPayload{
			Platform: platform,
			Metadata: map[string]interface{}{"runtime": "browser"},
			User: &validation.UserInfo{ID: "u1", IPAddress: "10.1.2.3",
				Geo: &validation.GeoInfo{City: "Berlin", Region: "Berlin", CountryCode: "DE", Subdivision: "BE"}},
			Contexts: map[string]map[string]interface{}{
				"browser":   {"name": "Sent", "version": "1"},
				"os":        {"name": "SentOS"},
				"client_os": {"version": "x"},
			},
			Request: &validation.RequestInfo{Headers: map[string]string{"User-Agent": uaSafariMac}},
		}
		before, err := json.Marshal(p)
		require.NoError(t, err)
		enrich.Apply(p, enrFullInfo(), e)
		after, err := json.Marshal(p)
		require.NoError(t, err)
		assert.JSONEq(t, string(before), string(after), "%s: every sent field is stored exactly as sent", platform)
	}
}

// TestEnrichNoNetworkIOOnTheRequestPath: after the loader's first load, 100 IngestAt calls make ZERO
// S3 calls. The deadline is a hang guard only; no wall-clock bound is asserted.
func TestEnrichNoNetworkIOOnTheRequestPath(t *testing.T) {
	src, e := enrReadyLoader(t)
	stats, gets := src.counts()
	require.Equal(t, 1, stats)
	require.Equal(t, 1, gets)

	pub := &fakeEventPublisher{}
	svc, err := service.NewIngestService(pub, service.WithEnricher(e))
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var filled int
	for i := 0; i < 100; i++ {
		// Hits (81.2.69.142) and misses (documentation addresses), through the connection path.
		ip := "81.2.69.142"
		if i%2 == 1 {
			ip = fmt.Sprintf("198.51.100.%d", i)
		}
		info := clientip.Info{ClientIP: netip.MustParseAddr(ip), HasClientIP: true, UserAgent: uaChromeWindows}
		p := validIngestPayload()
		p.Platform = "cocoa"
		require.NoError(t, svc.IngestAt(clientip.WithRequestInfo(ctx, info), p, time.Now().UTC()))
		if p.User != nil && p.User.Geo != nil {
			filled++
		}
	}
	require.NoError(t, ctx.Err(), "hang guard")
	assert.Equal(t, 50, filled, "the hits were looked up, so the loop is not vacuous")
	stats2, gets2 := src.counts()
	assert.Equal(t, stats, stats2, "no Stat from the request path")
	assert.Equal(t, gets, gets2, "no Get from the request path")
}

// longCityLookup is §5.6's stub: GeoLite2 has no 200-rune city, so a stub supplies one.
type longCityLookup struct{}

func (longCityLookup) Lookup(netip.Addr) (validation.GeoInfo, bool) {
	return validation.GeoInfo{City: strings.Repeat("ł", 200), CountryCode: "GB"}, true
}

func TestEnrichRunsBeforeTheBounds(t *testing.T) {
	pub := &fakeEventPublisher{}
	svc, err := service.NewIngestService(pub, service.WithEnricher(&enrich.Enricher{Geo: longCityLookup{}}))
	require.NoError(t, err)
	p := validIngestPayload()
	p.User = &validation.UserInfo{IPAddress: "81.2.69.142"}
	require.NoError(t, svc.IngestAt(context.Background(), p, time.Now().UTC()))

	require.Len(t, pub.published, 1)
	var ev sentinelv1.ErrorEvent
	require.NoError(t, proto.Unmarshal(pub.published[0], &ev))
	require.NotNil(t, ev.GetUser().GetGeo())
	assert.Equal(t, validation.MaxGeoTextLength, utf8.RuneCountInString(ev.GetUser().GetGeo().GetCity()),
		"the derived 200-rune city is truncated to 128 runes, exactly as a sent one")
	assert.Contains(t, fmt.Sprint(p.Metadata["server_truncated"]), "user.geo", "and marked in server_truncated")
}
