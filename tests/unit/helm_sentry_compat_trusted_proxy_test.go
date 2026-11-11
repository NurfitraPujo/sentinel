package unit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// P2.13 (sentinel-sentry-compat-ingest, contract D15): config.ingestor.sentryCompatTrustedProxyCidrs ships
// EMPTY in values.yaml and in every overlay; core-sentinel-mobile-relay sets it at its rollout.
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
		if name == "values.aws.staging.yml" || name == "values.aws.production.yml" {
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

	// The global list is untouched: still [] in values.yaml and both K8s overlays.
	g, ok := helmValue(t, base, "config", "ingestor", "trustedProxyCidrs").([]any)
	require.True(t, ok)
	assert.Empty(t, g)
	for _, f := range []string{"values-k8s-staging.yml", "values-k8s-production.yml"} {
		ov := readHelmValues(t, filepath.Join(chart, f))
		l, ok := helmValue(t, ov, "config", "ingestor", "trustedProxyCidrs").([]any)
		require.True(t, ok, f)
		assert.Empty(t, l, f)
	}
}
