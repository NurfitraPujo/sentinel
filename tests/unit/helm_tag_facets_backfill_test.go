package unit

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sentinel-issue-tag-facets spec §4.1 and §10.2 "Chart": the backfill ships as a CronJob that never runs
// by itself and that ArgoCD syncs without running. No helm exec (T26: go-root has no helm); the template
// text and the values files are parsed, as helm_geoip_test.go does. G5 renders it with helm on top.

func tfbTemplate(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(enrChart(), "templates", "tag-facets-backfill-cronjob.yaml"))
	require.NoError(t, err, "templates/tag-facets-backfill-cronjob.yaml must exist")
	return string(b)
}

func TestHelmTagFacetsBackfillIsASuspendedCronJob(t *testing.T) {
	tpl := tfbTemplate(t)
	assert.Regexp(t, `(?m)^kind: CronJob$`, tpl)
	assert.NotRegexp(t, `(?m)^kind: Job$`, tpl, "the backfill must never render a plain Job (immutability, Missing)")
	// suspend is a LITERAL, never a values expression, so no overlay can switch the CronJob on.
	assert.Regexp(t, `(?m)^  suspend: true$`, tpl)
	assert.NotRegexp(t, `suspend: \{\{`, tpl)
	// 31 February: a schedule that never fires even if suspend were removed.
	assert.Contains(t, tpl, `schedule: "0 0 31 2 *"`)
	assert.Contains(t, tpl, "concurrencyPolicy: Forbid")
	assert.Contains(t, tpl, "activeDeadlineSeconds: 21600")
	assert.Contains(t, tpl, "backoffLimit: 3")
	assert.Contains(t, tpl, "ttlSecondsAfterFinished: 604800")
	assert.Contains(t, tpl, "restartPolicy: Never")
	// Anchored to the rendered position, not just present anywhere in the file (tfb-helm-backfill-test-weak):
	// a plain Contains passes even if the include sits inside a {{/* ... */}} template comment (inert,
	// never renders) as long as the substring still appears in the source text.
	assert.Regexp(t, `(?m)^\s*\{\{-\s*include "sentinel\.postgresEnv" \. \| nindent 16 \}\}\s*$`, tpl,
		`the env block must actually render include "sentinel.postgresEnv", not merely mention it in a comment`)
	// Anchored to the SAME variable the image: line renders from (tfb-helm-backfill-test-weak): a plain
	// Contains passes even if the image: line is switched to read .Values.images.ingestor.repository
	// directly while the now-dead "$img := .Values.images.processor" line is left behind, still
	// containing the substring.
	assert.Regexp(t, `(?m)^\{\{-\s*\$img\s*:=\s*\.Values\.images\.processor\s*-\}\}\s*$`, tpl,
		`$img must be assigned from .Values.images.processor`)
	assert.Regexp(t, `(?m)^\s*image:\s*"\{\{\s*\$img\.repository\s*\}\}:\{\{\s*\$tag\s*\}\}"\s*$`, tpl,
		`the container's image: line must actually render through $img (which the previous check pins to .Values.images.processor), not some other value path`)
	assert.Contains(t, tpl, "{{- if .Values.tagFacetsBackfill.enabled }}")
	for _, forbidden := range []string{"argocd.argoproj.io/hook", "hook-delete-policy", "sync-wave", "helm.sh/hook"} {
		assert.NotContains(t, tpl, forbidden, "the backfill must stay out of the ArgoCD sync")
	}
}

func TestHelmTagFacetsBackfillArgs(t *testing.T) {
	tpl := tfbTemplate(t)
	args := regexp.MustCompile(`(?m)^\s*args:\s*\[(.*)\]\s*$`).FindStringSubmatch(tpl)
	require.NotNil(t, args, "the container has a one-line args list")
	for _, want := range []string{`"backfill-tag-facets"`, `"-batch"`, `.Values.tagFacetsBackfill.batch`, `"-pause"`,
		`.Values.tagFacetsBackfill.pause`, `"-duty"`, `.Values.tagFacetsBackfill.duty`} {
		assert.Contains(t, args[1], want)
	}
	assert.True(t, strings.HasPrefix(strings.TrimSpace(args[1]), `"backfill-tag-facets"`), "the subcommand is the first arg")

	base := readHelmValues(t, filepath.Join(enrChart(), "values.yaml"))
	assert.Equal(t, "true", helmPath(t, base, "tagFacetsBackfill", "enabled"))
	assert.Equal(t, "200", helmPath(t, base, "tagFacetsBackfill", "batch"))
	assert.Equal(t, "200ms", helmPath(t, base, "tagFacetsBackfill", "pause"))
	assert.Equal(t, "0.25", helmPath(t, base, "tagFacetsBackfill", "duty"))
	// Neither K8s overlay turns it off: the operator must be able to create the Job after the deploy.
	for _, f := range enrOverlays {
		o := readHelmValues(t, filepath.Join(enrChart(), f))
		assert.Equal(t, "true", helmPath(t, helmDeepMerge(base, o), "tagFacetsBackfill", "enabled"), f)
	}
}
