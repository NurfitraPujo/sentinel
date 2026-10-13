package unit

import (
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// awsProductionRelayCidr is core's worker subnet on AWS production (the private subnet every production EKS node is
// in), the one entry values.aws.production.yml carries since the mobile relay rollout
// (sentinel-mobile-relay-enable-prod; core-sentinel-mobile-relay spec 7.2 R5).
const awsProductionRelayCidr = "10.11.64.0/20"

// minRelayCidrBits is the widest prefix (smallest bit count) the production list may hold: nothing broader than /16.
const minRelayCidrBits = 16

// P2.13 (sentinel-sentry-compat-ingest, contract D15): config.ingestor.sentryCompatTrustedProxyCidrs ships
// EMPTY in values.yaml and in every overlay except values.aws.production.yml, whose list is set by the mobile relay
// rollout (sentinel-mobile-relay-enable-prod) to exactly awsProductionRelayCidr. The production entries are held to a
// sanity bound (each a valid CIDR, none /0, none broader than /16, no duplicates) so a typo cannot widen the trust.
func TestHelmSentryCompatTrustedProxyCidrs(t *testing.T) {
	chart := enrChart()
	base := readHelmValues(t, filepath.Join(chart, "values.yaml"))
	def, ok := helmValue(t, base, "config", "ingestor", "sentryCompatTrustedProxyCidrs").([]any)
	require.True(t, ok, "values.yaml: config.ingestor.sentryCompatTrustedProxyCidrs must be a list")
	assert.Empty(t, def, "values.yaml: the default trusts nothing extra")

	var overlays []string
	for _, pat := range []string{"values*.yml", "values*.yaml"} {
		m, err := filepath.Glob(filepath.Join(chart, pat))
		require.NoError(t, err)
		for _, f := range m {
			if filepath.Base(f) != "values.yaml" {
				overlays = append(overlays, f)
			}
		}
	}
	require.GreaterOrEqual(t, len(overlays), 5, "the chart has five overlays (spec T25); a glob finding fewer is a broken glob")

	for _, f := range overlays {
		ov := readHelmValues(t, f)
		cfg, _ := ov["config"].(map[string]any)
		ing, _ := cfg["ingestor"].(map[string]any)
		v, present := ing["sentryCompatTrustedProxyCidrs"]
		name := filepath.Base(f)
		if name == "values.aws.production.yml" {
			list, isList := v.([]any)
			require.Truef(t, present && isList, "%s must set config.ingestor.sentryCompatTrustedProxyCidrs explicitly to a list", name)
			assertRelayProductionCidrs(t, name, list)
			continue
		}
		if name == "values.aws.staging.yml" {
			list, isList := v.([]any)
			require.Truef(t, present && isList, "%s must set config.ingestor.sentryCompatTrustedProxyCidrs explicitly to []", name)
			assert.Emptyf(t, list, "%s: shipped empty (D15)", name)
			continue
		}
		if present {
			list, isList := v.([]any)
			require.Truef(t, isList, "%s: sentryCompatTrustedProxyCidrs must be a list", name)
			assert.Emptyf(t, list, "%s: shipped empty (D15)", name)
		}
	}

	raw, err := os.ReadFile(filepath.Join(chart, "templates", "ingestor.yaml"))
	require.NoError(t, err)
	tpl := string(raw)
	i := strings.Index(tpl, "name: SENTRY_COMPAT_TRUSTED_PROXY_CIDRS")
	require.GreaterOrEqual(t, i, 0, "templates/ingestor.yaml must render SENTRY_COMPAT_TRUSTED_PROXY_CIDRS")
	assert.Contains(t, tpl[i:i+200], `join "," .Values.config.ingestor.sentryCompatTrustedProxyCidrs`)

	// The global list is untouched: still [] in values.yaml and both Huawei overlays.
	g, ok := helmValue(t, base, "config", "ingestor", "trustedProxyCidrs").([]any)
	require.True(t, ok)
	assert.Empty(t, g)
	for _, f := range []string{"values-huawei-staging.yml", "values-huawei-production.yml"} {
		ov := readHelmValues(t, filepath.Join(chart, f))
		l, ok := helmValue(t, ov, "config", "ingestor", "trustedProxyCidrs").([]any)
		require.True(t, ok, f)
		assert.Empty(t, l, f)
	}
}

// assertRelayProductionCidrs holds values.aws.production.yml's list to exactly [awsProductionRelayCidr] and to the
// sanity bound on every entry: a string that parses as a CIDR, not /0, not broader than minRelayCidrBits, and not a
// duplicate (compared masked, so 10.11.64.1/20 repeats 10.11.64.0/20).
func assertRelayProductionCidrs(t *testing.T, name string, list []any) {
	t.Helper()
	var got []string
	seen := map[netip.Prefix]bool{}
	for i, e := range list {
		s, ok := e.(string)
		require.Truef(t, ok, "%s: sentryCompatTrustedProxyCidrs[%d] must be a string, got %T", name, i, e)
		p, err := netip.ParsePrefix(s)
		require.NoErrorf(t, err, "%s: sentryCompatTrustedProxyCidrs[%d] %q must be a CIDR", name, i, s)
		assert.NotZerof(t, p.Bits(), "%s: %q is /0, which trusts every address", name, s)
		assert.GreaterOrEqualf(t, p.Bits(), minRelayCidrBits, "%s: %q is broader than /%d", name, s, minRelayCidrBits)
		assert.Falsef(t, seen[p.Masked()], "%s: %q is a duplicate entry", name, s)
		seen[p.Masked()] = true
		got = append(got, s)
	}
	assert.Equalf(t, []string{awsProductionRelayCidr}, got, "%s: the production list is core's worker subnet and nothing else", name)
}
