package unit

import (
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/auth"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/middleware"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/sentrycompat"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/service"
)

func scRead(t *testing.T, rel ...string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(append([]string{"..", ".."}, rel...)...))
	require.NoError(t, err)
	return string(b)
}

// section returns text from the first line starting with heading up to the next heading of the same or a higher
// level, or the end of the file.
func scSection(t *testing.T, doc, heading string) string {
	t.Helper()
	i := strings.Index(doc, heading+"\n")
	require.GreaterOrEqualf(t, i, 0, "missing heading %q", heading)
	level := len(heading) - len(strings.TrimLeft(heading, "#"))
	rest := doc[i+len(heading):]
	end := len(rest)
	for _, m := range regexp.MustCompile(`(?m)^(#{1,6}) `).FindAllStringSubmatchIndex(rest, -1) {
		if m[3]-m[2] <= level {
			end = m[0]
			break
		}
	}
	return doc[i : i+len(heading)+end]
}

// TestSentryCompatDocumented: the operator-facing text (spec §9) says what the code does.
func TestSentryCompatDocumented(t *testing.T) {
	sdk := scRead(t, "docs", "sdk-specification.md")
	sec := scSection(t, sdk, "## 10. Sentry-compatible ingest")
	for _, want := range []string{
		"/api/{project_id}/envelope/", "/api/{project_id}/store/", "X-Sentry-Auth", "sentry_key", "X-Sentry-Rate-Limits",
		"60::organization", "sentinel_compat_items_total", "1,000,000", "413", "X-Sentinel-Relay-Auth", "SENTRY_COMPAT_TRUSTED_PROXY_CIDRS",
		fmt.Sprint(sentrycompat.MaxCompressedBodyBytes), fmt.Sprint(sentrycompat.MaxDecompressedBodyBytes),
		fmt.Sprint(sentrycompat.MaxEventItemBytes), fmt.Sprint(sentrycompat.MaxEnvelopeItems), fmt.Sprint(sentrycompat.MaxHeaderLineBytes),
		"27690631804612", "4503599627370495",
	} {
		assert.Containsf(t, sec, want, "docs/sdk-specification.md §10 must mention %q", want)
	}

	rb := scRead(t, "deploy", "helm", "sentinel", "RUNBOOK.md")
	for _, want := range []string{
		"sentinel_compat_items_total", "ingest.event_shed", "sentryCompatTrustedProxyCidrs", "sentrycompat.relay_peer_untrusted", "narrowest",
	} {
		assert.Containsf(t, rb, want, "RUNBOOK.md must mention %q", want)
	}
	assert.Contains(t, rb, "### Sentry-compatible ingest (sentinel-sentry-compat-ingest)")

	relay := scSection(t, rb, "### Sentry-compatible ingest: trusting core's relay (sentinel-sentry-compat-ingest)")
	// D15: the entry names no value. No IPv4 literal, no IPv6 prefix.
	assert.Empty(t, regexp.MustCompile(`\b\d{1,3}(\.\d{1,3}){3}\b`).FindAllString(relay, -1), "the relay entry must not name an address")
	for _, tok := range regexp.MustCompile(`(?i)[0-9a-f]{0,4}(?::[0-9a-f]{0,4}){2,7}(?:/\d{1,3})?`).FindAllString(relay, -1) {
		tok = strings.TrimRight(tok, ":")
		if _, err := netip.ParsePrefix(tok); err == nil {
			t.Errorf("the relay entry names an IPv6 prefix: %q", tok)
		}
		if a, err := netip.ParseAddr(tok); err == nil && a.Is6() {
			t.Errorf("the relay entry names an IPv6 address: %q", tok)
		}
	}
	// The ruling for finding d14-off-while-list-empty: the rollout order sentence cannot be deleted silently.
	assert.Contains(t, relay, "anonymous")
	assert.Contains(t, relay, "before")
	assert.Contains(t, relay, "**before**")

	readme := scRead(t, "README.md")
	assert.Contains(t, readme, "docs/sdk-specification.md §10")
}

// TestSentryCompatNamesPinned pins every name the operator, core's relay and the dashboards depend on.
func TestSentryCompatNamesPinned(t *testing.T) {
	assert.Equal(t, "sentinel_compat_items_total", sentrycompat.MetricItems)
	assert.Equal(t, "type", sentrycompat.LabelType)
	assert.Equal(t, "POST /api/{project_id}/envelope/{$}", sentrycompat.PatternEnvelope)
	assert.Equal(t, "POST /api/{project_id}/envelope", sentrycompat.PatternEnvelopeNoSlash)
	assert.Equal(t, "POST /api/{project_id}/store/{$}", sentrycompat.PatternStore)
	assert.Equal(t, "POST /api/{project_id}/store", sentrycompat.PatternStoreNoSlash)
	assert.Equal(t, "project_id", sentrycompat.PathValueProjectID)
	assert.Equal(t, "60::organization", middleware.SentryRateLimitsValue)
	assert.Equal(t, 60, middleware.RetryAfterSeconds)
	assert.Equal(t, auth.ScopeSet{"ingest"}, auth.ScopesSentryCompat)
	assert.Equal(t, "X-Sentinel-Relay-Auth", sentrycompat.HeaderRelayAuth)
	assert.Equal(t, "X-Sentinel-Relay-User-Id", sentrycompat.HeaderRelayUserID)
	assert.Equal(t, "X-Sentinel-Relay-Tenant-Id", sentrycompat.HeaderRelayTenantID)
	assert.Equal(t, "user", sentrycompat.RelayAuthUser)
	assert.Equal(t, "anonymous", sentrycompat.RelayAuthAnonymous)
	assert.Equal(t, "tenant_id", sentrycompat.TagTenantID)
	assert.Equal(t, "relay_auth", sentrycompat.TagRelayAuth)
	assert.Equal(t, 1_000_000, service.MaxPublishBytes)
	assert.Equal(t, 4, service.MaxShedRounds)
	assert.Equal(t, 16<<10, sentrycompat.ShedSlack)
	assert.Equal(t, "sentry-compat", sentrycompat.Source)
	assert.Equal(t, 5<<20, sentrycompat.MaxCompressedBodyBytes)
	assert.Equal(t, 20<<20, sentrycompat.MaxDecompressedBodyBytes)
	assert.Equal(t, 1<<20, sentrycompat.MaxEventItemBytes)
	assert.Equal(t, 100, sentrycompat.MaxEnvelopeItems)
	assert.Equal(t, 64<<10, sentrycompat.MaxHeaderLineBytes)
}
