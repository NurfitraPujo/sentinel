package unit

import (
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sentinel-reporting-and-dashboard D7/D8/D4/D20: the chart's values, read the way Helm merges them (no helm exec: go-root
// runs tests/unit with no helm on the runner). The default deployment (CCE) gets none of the new machinery; both EKS
// overlays turn it on with the contract's starting SLI set and their own SSM parameters.

func rdChart() string { return filepath.Join("..", "..", "deploy", "helm", "sentinel") }

func TestHelmReportingDashboardDefaultsAreOff(t *testing.T) {
	base := readHelmValues(t, filepath.Join(rdChart(), "values.yaml"))
	assert.Equal(t, "false", helmPath(t, base, "podMonitor", "enabled"), "the PodMonitor CRD belongs to the Prometheus Operator: off by default")
	assert.Equal(t, "false", helmPath(t, base, "sliCollectCron", "enabled"))
	assert.Equal(t, "5 * * * *", helmPath(t, base, "sliCollectCron", "schedule"), "D6: hourly at :05")
	slis, ok := helmValue(t, base, "serviceSlis").([]any)
	require.True(t, ok)
	assert.Empty(t, slis, "no SLIs unless an overlay declares them")
	assert.Equal(t, "", helmPath(t, base, "analytics", "databend", "url"), "no Databend unless an overlay sets it: the dashboard then says unavailable")
	assert.Equal(t, "60s", helmPath(t, base, "podMonitor", "insightsInterval"), "D8: insights at 60 s")
	assert.Equal(t, "30s", helmPath(t, base, "podMonitor", "interval"), "D8: 30 s")
}

func TestHelmReportingDashboardAWSOverlays(t *testing.T) {
	base := readHelmValues(t, filepath.Join(rdChart(), "values.yaml"))
	for env, cluster := range map[string]string{"production": "tuju-production", "staging": "tuju-staging"} {
		t.Run(env, func(t *testing.T) {
			v := helmDeepMerge(base, readHelmValues(t, filepath.Join(rdChart(), "values.aws."+env+".yml")))
			assert.Equal(t, "true", helmPath(t, v, "podMonitor", "enabled"), "D8: on in both AWS overlays")
			assert.Equal(t, "true", helmPath(t, v, "sliCollectCron", "enabled"))
			assert.Equal(t, cluster, helmPath(t, v, "analytics", "sliCluster"), "D6: environment comes from the cluster")
			assert.Equal(t, "http://databend-query.databend.svc.cluster.local:8000", helmPath(t, v, "analytics", "databend", "url"))
			assert.Equal(t, "sentinel_app", helmPath(t, v, "analytics", "databend", "user"), "D4")
			assert.Equal(t, "sentinel", helmPath(t, v, "analytics", "databend", "database"))
			assert.Equal(t, "http://monitoring.internal.tuju.ai:8428", helmPath(t, v, "analytics", "victoriaMetricsUrl"))

			secrets, ok := helmValue(t, v, "externalSecrets", "secrets").(map[string]any)
			require.True(t, ok)
			for _, k := range []string{"DATABEND_PASSWORD", "METRICS_SCRAPE_TOKEN"} {
				assert.Equal(t, "/tuju/"+env+"/sentinel/shared/"+k, secrets[k], "D4/D20: SSM path for %s", k)
			}

			// D7's starting set, id by id.
			slis, ok := helmValue(t, v, "serviceSlis").([]any)
			require.True(t, ok)
			byID := map[string]map[string]any{}
			for _, s := range slis {
				m := s.(map[string]any)
				byID[m["id"].(string)] = m
			}
			require.Len(t, byID, 4)
			assert.Equal(t, "http", byID["core-api"]["kind"])
			assert.Equal(t, "api_core_80", byID["core-api"]["source"])
			assert.EqualValues(t, 0.995, byID["core-api"]["availabilityTarget"])
			assert.EqualValues(t, 0.94, byID["core-api"]["latency"].(map[string]any)["target"])
			assert.Equal(t, "api", byID["core-api"]["service"])
			assert.Equal(t, "web_web_80", byID["web"]["source"])
			assert.EqualValues(t, 0.95, byID["web"]["latency"].(map[string]any)["target"])
			assert.Nil(t, byID["web"]["service"], "web events carry no service (contract §2)")
			for _, id := range []string{"core-jobs", "core-nats"} {
				assert.EqualValues(t, 0.995, byID[id]["availabilityTarget"], "%s carries the 0.995 availability target (sentinel-dashboard-metrics-improvement D7)", id)
				assert.Nil(t, byID[id]["latency"], id)
			}
			// every http latency `le` is one of the edge histogram's buckets (D7)
			le := regexp.MustCompile(`^(0\.5|1\.0|5\.0|10\.0|25\.0|50\.0|100\.0|250\.0|500\.0|1000\.0|2500\.0|5000\.0|10000\.0|30000\.0|60000\.0|300000\.0|600000\.0|1800000\.0|3600000\.0)$`)
			for id, s := range byID {
				if l, ok := s["latency"].(map[string]any); ok {
					assert.Regexp(t, le, l["le"], "%s latency.le", id)
				}
			}
		})
	}
}
