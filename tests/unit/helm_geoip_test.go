package unit

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sentinel-event-enrichment §10.2 TASK-05 (and the chart side of TASK-01): the values-file assertions
// the chart gate cannot make, by parsing and merging the values files as Helm would (T26: no helm exec,
// because go-root runs tests/unit with no helm on the runner).

func enrChart() string { return filepath.Join("..", "..", "deploy", "helm", "sentinel") }

// TestHelmTrustedProxyCidrsEmptyOnK8s: both overlays DECLARE config.ingestor.trustedProxyCidrs and it
// is an empty list (contract decision 11), so a missing key and a non-empty list are both red. The
// chart default is also empty: trust nothing unless a deployment says otherwise.
func TestHelmTrustedProxyCidrsEmptyOnK8s(t *testing.T) {
	base := readHelmValues(t, filepath.Join(enrChart(), "values.yaml"))
	def, ok := helmValue(t, base, "config", "ingestor", "trustedProxyCidrs").([]any)
	require.True(t, ok, "values.yaml: config.ingestor.trustedProxyCidrs must be a list")
	assert.Empty(t, def, "values.yaml: the default trusts nothing")

	for _, f := range []string{"values-k8s-staging.yml", "values-k8s-production.yml"} {
		overlay := readHelmValues(t, filepath.Join(enrChart(), f))
		list, ok := helmValue(t, overlay, "config", "ingestor", "trustedProxyCidrs").([]any)
		require.Truef(t, ok, "%s must declare config.ingestor.trustedProxyCidrs as a list", f)
		assert.Emptyf(t, list, "%s: contract decision 11 -- no forwarding header is believed on K8s", f)
	}
}

var enrOverlays = []string{"values-k8s-staging.yml", "values-k8s-production.yml"}

// TestHelmGeoIPOverlaysPinStorage: both overlays turn geo and the refresh job on and pin the storage
// they read and write (endpoint and bucket equal to the file's own config.s3, region, prefix), so what
// is deployed never depends on a chart default.
func TestHelmGeoIPOverlaysPinStorage(t *testing.T) {
	base := readHelmValues(t, filepath.Join(enrChart(), "values.yaml"))
	assert.Equal(t, "false", helmPath(t, base, "geoip", "enabled"), "values.yaml: geo ships off")
	assert.Equal(t, "false", helmPath(t, base, "geoip", "refreshCron", "enabled"), "values.yaml: the job ships off")

	for _, f := range enrOverlays {
		t.Run(f, func(t *testing.T) {
			o := readHelmValues(t, filepath.Join(enrChart(), f))
			assert.Equal(t, "true", helmPath(t, o, "geoip", "enabled"))
			assert.Equal(t, "true", helmPath(t, o, "geoip", "refreshCron", "enabled"))
			assert.Equal(t, helmPath(t, o, "config", "s3", "endpoint"), helmPath(t, o, "geoip", "storage", "endpoint"))
			assert.Equal(t, helmPath(t, o, "config", "s3", "bucket"), helmPath(t, o, "geoip", "storage", "bucket"))
			assert.Equal(t, "us-east-1", helmPath(t, o, "geoip", "storage", "region"))
			assert.Equal(t, "geoip/", helmPath(t, o, "geoip", "storage", "prefix"))
			assert.Equal(t, "256Mi", helmPath(t, helmDeepMerge(base, o), "ingestor", "resources", "requests", "memory"),
				"the resident database needs the larger request")
		})
	}
	assert.Equal(t, "256Mi", helmPath(t, base, "ingestor", "resources", "requests", "memory"))
}

// helmWalk calls fn for every map key in v with its dotted path.
func helmWalk(v any, path string, fn func(path, key string, val any)) {
	switch m := v.(type) {
	case map[string]any:
		for k, val := range m {
			fn(path, k, val)
			helmWalk(val, path+"."+k, fn)
		}
	case []any:
		for i, val := range m {
			helmWalk(val, fmt.Sprintf("%s[%d]", path, i), fn)
		}
	}
}

// kubesealCiphertext is the shape kubeseal-encryptor output takes (every sealed value in the overlays).
var kubesealCiphertext = regexp.MustCompile(`^Ag[A-Za-z0-9+/]{100,}={0,2}$`)

// TestHelmMaxMindKeyOnlySealed: the MaxMind keys live only in secrets.sealedData (TASK-07 seals them),
// never in secrets.data or anywhere else, and a MAXMIND_LICENSE_KEY value is never plaintext.
func TestHelmMaxMindKeyOnlySealed(t *testing.T) {
	for _, f := range append([]string{"values.yaml"}, enrOverlays...) {
		v := readHelmValues(t, filepath.Join(enrChart(), f))
		helmWalk(v, "", func(path, key string, val any) {
			if !strings.HasPrefix(key, "MAXMIND_") {
				return
			}
			assert.Equalf(t, ".secrets.sealedData", path, "%s: %s%s is outside secrets.sealedData", f, path, "."+key)
			if key == "MAXMIND_LICENSE_KEY" {
				assert.Regexpf(t, kubesealCiphertext, fmt.Sprint(val), "%s: MAXMIND_LICENSE_KEY must be kubeseal ciphertext", f)
			}
		})
	}
	base := readHelmValues(t, filepath.Join(enrChart(), "values.yaml"))
	data, _ := helmValue(t, base, "secrets", "data").(map[string]any)
	for k := range data {
		assert.Falsef(t, strings.HasPrefix(k, "MAXMIND_"), "values.yaml secrets.data must not carry %s", k)
	}
}

// TestHelmGeoIPRefreshBudget: the download budget (spec §8.2) is two runs a week, no retries, no overlap.
func TestHelmGeoIPRefreshBudget(t *testing.T) {
	base := readHelmValues(t, filepath.Join(enrChart(), "values.yaml"))
	for _, f := range append([]string{""}, enrOverlays...) {
		merged := base
		if f != "" {
			merged = helmDeepMerge(base, readHelmValues(t, filepath.Join(enrChart(), f)))
		}
		fields := strings.Fields(helmPath(t, merged, "geoip", "refreshCron", "schedule"))
		require.Lenf(t, fields, 5, "%s: a five-field cron schedule", f)
		assert.Lenf(t, strings.Split(fields[4], ","), 2, "%s: exactly two day-of-week values", f)
		assert.NotContainsf(t, fields[4], "*", "%s: day-of-week", f)
		assert.NotContainsf(t, fields[4], "-", "%s: day-of-week is a list, not a range", f)
		assert.Equalf(t, "0", helmPath(t, merged, "geoip", "refreshCron", "backoffLimit"), "%s: a retry could be a second download", f)
	}
	tpl, err := os.ReadFile(filepath.Join(enrChart(), "templates", "geoip-refresh-cronjob.yaml"))
	require.NoError(t, err)
	assert.Contains(t, string(tpl), "concurrencyPolicy: Forbid")
	assert.Contains(t, string(tpl), "optional: true")
}
