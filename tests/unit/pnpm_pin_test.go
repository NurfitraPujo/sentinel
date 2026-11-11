package unit

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The sdk-js suite runs in two places: `task test-unit` (the plane's unit gate, widened by
// sentinel-event-enrichment contract decision 9) and CI's sdk-js-node job. Both must use the same
// pnpm, pinned to one exact version: pnpm 11 rewrites packages/sdk-js/pnpm-lock.yaml and fails
// `--frozen-lockfile`, and a floating `pnpm@10` resolves against the registry on every run, so two
// gate runs of one commit could use different pnpm versions (spec §10.1, enr-rev2-npx-pnpm-floating).
//
// This test holds the two pins together by parsing Taskfile.yml and ci.yml as YAML, with no exec
// (the T26 approach): every `pnpm@` in test-unit names one exact MAJOR.MINOR.PATCH, and the
// sdk-js-node job's pnpm/action-setup `version` is exactly that string. Bump both together.
var exactSemver = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

// pnpmAt matches every `pnpm@<version>` token in a command line; the version runs to the next
// whitespace, so a range such as `pnpm@^10` or `pnpm@10.x` is captured whole and then refused.
var pnpmAt = regexp.MustCompile(`pnpm@(\S+)`)

func TestPnpmPinTaskfileMatchesCI(t *testing.T) {
	root := filepath.Join("..", "..")

	taskfile := readHelmValues(t, filepath.Join(root, "Taskfile.yml"))
	testUnit, ok := yamlMap(taskfile, "tasks", "test-unit")
	require.True(t, ok, "Taskfile.yml: tasks.test-unit missing")
	cmds, ok := testUnit["cmds"].([]any)
	require.True(t, ok, "Taskfile.yml: tasks.test-unit.cmds is not a list")

	var pins []string
	for _, c := range cmds {
		line := fmt.Sprint(c)
		if m, isMap := c.(map[string]any); isMap {
			line = fmt.Sprint(m["cmd"])
		}
		for _, sub := range pnpmAt.FindAllStringSubmatch(line, -1) {
			pins = append(pins, sub[1])
		}
	}
	require.NotEmpty(t, pins, "Taskfile.yml test-unit must run the sdk-js suite through an exactly pinned `npx -y pnpm@X.Y.Z`")

	want := pins[0]
	for _, p := range pins {
		require.Regexpf(t, exactSemver, p,
			"Taskfile.yml test-unit: pnpm@%s is not an exact MAJOR.MINOR.PATCH (a bare major or a range floats)", p)
		require.Equalf(t, want, p, "Taskfile.yml test-unit pins two different pnpm versions")
	}

	ci := readHelmValues(t, filepath.Join(root, ".github", "workflows", "ci.yml"))
	job, ok := yamlMap(ci, "jobs", "sdk-js-node")
	require.True(t, ok, "ci.yml: jobs.sdk-js-node missing")
	steps, ok := job["steps"].([]any)
	require.True(t, ok, "ci.yml: jobs.sdk-js-node.steps is not a list")

	var ciVersions []string
	for _, s := range steps {
		step, isMap := s.(map[string]any)
		if !isMap || !strings.HasPrefix(fmt.Sprint(step["uses"]), "pnpm/action-setup@") {
			continue
		}
		with, _ := step["with"].(map[string]any)
		require.NotNil(t, with, "ci.yml sdk-js-node: pnpm/action-setup has no `with`")
		ciVersions = append(ciVersions, fmt.Sprint(with["version"]))
	}
	require.Len(t, ciVersions, 1, "ci.yml sdk-js-node must have exactly one pnpm/action-setup step")
	require.Regexpf(t, exactSemver, ciVersions[0],
		"ci.yml sdk-js-node: pnpm/action-setup version %q is not an exact MAJOR.MINOR.PATCH", ciVersions[0])
	require.Equalf(t, want, ciVersions[0],
		"ci.yml sdk-js-node pins pnpm %s but Taskfile.yml test-unit pins %s; bump both together", ciVersions[0], want)
}

func yamlMap(v map[string]any, keys ...string) (map[string]any, bool) {
	cur := v
	for _, k := range keys {
		next, ok := cur[k].(map[string]any)
		if !ok {
			return nil, false
		}
		cur = next
	}
	return cur, true
}
