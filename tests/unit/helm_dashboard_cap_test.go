package unit

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// The dashboard's per-release sourcemap cap reaches the pod through ONE chart key --
// .Values.config.dashboard.sourcemapMaxFilesPerRelease, which templates/dashboard.yaml renders
// verbatim as SOURCEMAP_MAX_FILES_PER_RELEASE -- and both K8s overrides pin that key
// explicitly, so the deployed value is whatever the override file on main says, never the route's
// code default (sentinel-sourcemaps-turbopack-grouping contract §4.1). This test pins the three
// files to the number the route constant, its unit test and docs/sdk-specification.md §8 also
// state (§4.1 places 1-6), so a values-file typo in one place -- "200", "", the old "500" left
// behind in one overlay -- fails here instead of surfacing as the staging pod's env at merge.
//
// The chart gate CI runs (lint-helm-k8s) renders and lints but checks no value; the boundary
// contract §11 names for this is `helm template -- tests/unit/helm_*_test.go`, which is this file.
const dashboardSourcemapMaxFilesPerRelease = "2000"

// TestHelmDashboardCap asserts config.dashboard.sourcemapMaxFilesPerRelease renders "2000" from
// values.yaml alone and from each K8s overlay deep-merged over it, the way Helm coalesces
// --values files.
func TestHelmDashboardCap(t *testing.T) {
	chart := filepath.Join("..", "..", "deploy", "helm", "sentinel")
	base := readHelmValues(t, filepath.Join(chart, "values.yaml"))

	cases := []struct {
		name    string
		overlay string
	}{
		{"values.yaml", ""},
		{"values-k8s-staging.yml over values.yaml", "values-k8s-staging.yml"},
		{"values-k8s-production.yml over values.yaml", "values-k8s-production.yml"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			merged := base
			if c.overlay != "" {
				overlay := readHelmValues(t, filepath.Join(chart, c.overlay))
				// The overlay must pin the key itself: contract §4.1 places 4 and 5 are the
				// values the pods actually read, and an overlay that merely inherits the chart
				// default would make "what is deployed" depend on which file a reader opened.
				require.Equal(t, dashboardSourcemapMaxFilesPerRelease,
					helmPath(t, overlay, "config", "dashboard", "sourcemapMaxFilesPerRelease"),
					"%s must pin config.dashboard.sourcemapMaxFilesPerRelease explicitly", c.overlay)
				merged = helmDeepMerge(base, overlay)
			}
			require.Equal(t, dashboardSourcemapMaxFilesPerRelease,
				helmPath(t, merged, "config", "dashboard", "sourcemapMaxFilesPerRelease"),
				"SOURCEMAP_MAX_FILES_PER_RELEASE as templates/dashboard.yaml renders it from %s", c.name)
		})
	}
}
