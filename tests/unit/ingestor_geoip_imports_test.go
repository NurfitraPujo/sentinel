package unit

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sentinel-event-enrichment §10.2 TASK-05 "Pod starts and rollouts cause zero MaxMind downloads", (a):
// the packages the ingestor SERVES with cannot reach MaxMind's client at all, and geoip/refresh (the
// only code that contacts MaxMind) is imported by nothing but the ingestor's main, which reaches it only
// in the geoip-refresh subcommand branch. This replaces a "loader start plus polls: 0 MaxMind requests"
// test that could not fail (the counting fake was reachable only through client.WithEndpoint, which the
// loader never receives).

const ingestorModule = "github.com/NurfitraPujo/sentinel"

func goList(t *testing.T, args ...string) []string {
	t.Helper()
	cmd := exec.Command("go", append([]string{"list"}, args...)...)
	cmd.Dir = filepath.Join("..", "..")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	return strings.Split(strings.TrimSpace(string(out)), "\n")
}

func TestGeoIPServingPathNeverImportsMaxMindClient(t *testing.T) {
	deps := goList(t, "-deps",
		"./apps/ingestor-go/geoip", "./apps/ingestor-go/enrich", "./apps/ingestor-go/clientip", "./apps/ingestor-go/service")
	require.Contains(t, deps, ingestorModule+"/apps/ingestor-go/geoip", "the listing is not vacuous")
	for _, d := range deps {
		assert.Falsef(t, strings.HasPrefix(d, "github.com/maxmind/geoipupdate"), "serving path imports %s", d)
		assert.NotEqualf(t, ingestorModule+"/apps/ingestor-go/geoip/refresh", d, "serving path imports geoip/refresh")
	}
}

func TestGeoIPRefreshImportedOnlyByIngestorMain(t *testing.T) {
	refreshPkg := ingestorModule + "/apps/ingestor-go/geoip/refresh"
	var importers []string
	for _, line := range goList(t, "-f", `{{.ImportPath}} {{join .Imports " "}}`, "./apps/ingestor-go/...") {
		fields := strings.Fields(line)
		for _, imp := range fields[1:] {
			if imp == refreshPkg {
				importers = append(importers, fields[0])
			}
		}
	}
	assert.Equal(t, []string{ingestorModule + "/apps/ingestor-go"}, importers)
}
