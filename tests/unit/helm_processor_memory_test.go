package unit

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// The chart's processor memory triple -- SOURCEMAP_CACHE_BYTES, GOMEMLIMIT and
// resources.limits.memory -- is DERIVED from §13 M2's measured parsed:raw ratio, not asserted
// (contract §12, the OOM row; decisions 11, 35, 38). This test re-checks the derivation against
// the committed values so a knob turned on its own cannot silently put the chart back into the
// crash loop the review found (sentinel-sourcemaps/BUG-01): 128 MiB x 7.6 + 28 MiB against a
// 400MiB GOMEMLIMIT and a 512Mi limit.
//
// This is not a boundary contract §11 lists -- §11 has no chart row, and the only chart gate CI
// runs (lint-helm-k8s) renders and lints but checks no arithmetic. BUG-01's verification asks
// for the test anyway and says the absence of a covering test is part of the finding; this file
// is that absence, named. The two inputs below are M2's numbers, rounded UP so the check is
// conservative; re-derive them when M2 is re-taken on the staging rig (§13's held task).
const (
	// m2ParsedRawRatio is RSS delta / raw bytes with the cap-size map parsed and held:
	// 7.645 for the map alone, 7.686 for the whole bundle (be.md M2, 2026-09-15T14:09Z).
	m2ParsedRawRatio = 7.7
	// m2BaselineBytes is the processor's RSS after restart with the cache empty: 28.3 MiB.
	m2BaselineBytes = 29 << 20
)

// helmProcessorMemory is the triple as one values file (or one overlay merged over
// values.yaml, the way Helm deep-merges) renders it.
type helmProcessorMemory struct {
	cacheBytes    int64
	maxParseBytes int64
	gomemlimit    int64
	limitBytes    int64
}

// TestHelmProcessorMemoryIsDerivedFromM2 asserts, for values.yaml and for each K8s overlay
// merged over it, the two inequalities the derivation rests on and the ordering the knobs need:
//
//  1. steady state, the contract's own formula:
//     SOURCEMAP_CACHE_BYTES x ratio + baseline < GOMEMLIMIT
//  2. peak, the moment before eviction -- the cache admits a map only after parsing it
//     (apps/processor-go/symbolicate/cache.go, put), so with the cache full a cold map of the
//     largest parseable size is in RSS beside everything already held:
//     (SOURCEMAP_CACHE_BYTES + SOURCEMAP_MAX_PARSE_BYTES) x ratio + baseline < limits.memory
//  3. GOMEMLIMIT < limits.memory (decision 35: the runtime collects before the kernel kills)
//  4. SOURCEMAP_MAX_PARSE_BYTES <= SOURCEMAP_CACHE_BYTES (a map the processor may parse is never
//     too large for the cache -- otherwise it is fetched and parsed per event, §12's thrash row)
//
// Line 2 is deliberately checked against the kernel limit, not GOMEMLIMIT: crossing the soft
// limit transiently makes the runtime collect harder, crossing the hard one kills the pod.
func TestHelmProcessorMemoryIsDerivedFromM2(t *testing.T) {
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
				merged = helmDeepMerge(base, readHelmValues(t, filepath.Join(chart, c.overlay)))
			}
			m := helmProcessorMemoryFrom(t, merged)

			steady := float64(m.cacheBytes)*m2ParsedRawRatio + m2BaselineBytes
			peak := float64(m.cacheBytes+m.maxParseBytes)*m2ParsedRawRatio + m2BaselineBytes
			t.Logf("cache=%s parse=%s gomemlimit=%s limit=%s | steady=%s peak=%s",
				mib(m.cacheBytes), mib(m.maxParseBytes), mib(m.gomemlimit), mib(m.limitBytes),
				mib(int64(steady)), mib(int64(peak)))

			require.Less(t, steady, float64(m.gomemlimit),
				"steady state: SOURCEMAP_CACHE_BYTES x %.1f + %s = %s must be < GOMEMLIMIT %s (contract §12 OOM row)",
				m2ParsedRawRatio, mib(m2BaselineBytes), mib(int64(steady)), mib(m.gomemlimit))
			require.Less(t, peak, float64(m.limitBytes),
				"peak before eviction: (SOURCEMAP_CACHE_BYTES + SOURCEMAP_MAX_PARSE_BYTES) x %.1f + %s = %s must be < limits.memory %s",
				m2ParsedRawRatio, mib(m2BaselineBytes), mib(int64(peak)), mib(m.limitBytes))
			require.Less(t, m.gomemlimit, m.limitBytes,
				"GOMEMLIMIT %s must be below limits.memory %s (decision 35)", mib(m.gomemlimit), mib(m.limitBytes))
			require.LessOrEqual(t, m.maxParseBytes, m.cacheBytes,
				"SOURCEMAP_MAX_PARSE_BYTES %s must not exceed SOURCEMAP_CACHE_BYTES %s", mib(m.maxParseBytes), mib(m.cacheBytes))
		})
	}
}

// TestHelmProcessorMemoryRejectsTheShippedDefaults pins the values the review found, so the
// derivation above is known to be able to fail: the numbers the chart shipped with before
// BUG-01 fail the steady-state line by a factor of ~2.5.
func TestHelmProcessorMemoryRejectsTheShippedDefaults(t *testing.T) {
	shipped := helmProcessorMemory{
		cacheBytes:    134217728, // 128 MiB
		maxParseBytes: 52428800,  // 50 MiB
		gomemlimit:    400 << 20, // "400MiB"
		limitBytes:    512 << 20, // 512Mi
	}
	steady := float64(shipped.cacheBytes)*m2ParsedRawRatio + m2BaselineBytes
	require.Greater(t, steady, float64(shipped.gomemlimit),
		"the pre-BUG-01 defaults must fail the contract's formula, or this file asserts nothing")
}

func readHelmValues(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var v map[string]any
	require.NoError(t, yaml.Unmarshal(raw, &v), path)
	return v
}

// helmDeepMerge is the subset of Helm's values coalescing this test needs: maps merge key by
// key, the overlay wins on every scalar, and a key the overlay omits keeps the base's value.
func helmDeepMerge(base, overlay map[string]any) map[string]any {
	out := make(map[string]any, len(base))
	for k, v := range base {
		out[k] = v
	}
	for k, ov := range overlay {
		om, oIsMap := ov.(map[string]any)
		bm, bIsMap := out[k].(map[string]any)
		if oIsMap && bIsMap {
			out[k] = helmDeepMerge(bm, om)
			continue
		}
		out[k] = ov
	}
	return out
}

func helmProcessorMemoryFrom(t *testing.T, v map[string]any) helmProcessorMemory {
	t.Helper()
	return helmProcessorMemory{
		cacheBytes:    plainIntegerBytes(t, helmPath(t, v, "config", "processor", "sourcemapCacheBytes")),
		maxParseBytes: plainIntegerBytes(t, helmPath(t, v, "config", "processor", "sourcemapMaxParseBytes")),
		gomemlimit:    goMemLimitBytes(t, helmPath(t, v, "processor", "gomemlimit")),
		limitBytes:    k8sQuantityBytes(t, helmPath(t, v, "processor", "resources", "limits", "memory")),
	}
}

func helmPath(t *testing.T, v map[string]any, keys ...string) string {
	t.Helper()
	var cur any = v
	for _, k := range keys {
		m, ok := cur.(map[string]any)
		require.Truef(t, ok, "%s: not a map at %q", strings.Join(keys, "."), k)
		cur, ok = m[k]
		require.Truef(t, ok, "%s: key %q missing", strings.Join(keys, "."), k)
	}
	return fmt.Sprint(cur)
}

// plainIntegerBytes is the syntax SOURCEMAP_CACHE_BYTES / SOURCEMAP_MAX_PARSE_BYTES take: digits
// only, no suffix (values.yaml says so beside each key).
func plainIntegerBytes(t *testing.T, s string) int64 {
	t.Helper()
	n, err := strconv.ParseInt(s, 10, 64)
	require.NoErrorf(t, err, "plain integer bytes expected, got %q", s)
	return n
}

var (
	goMemLimitRe  = regexp.MustCompile(`^(\d+)(B|KiB|MiB|GiB|TiB)?$`)
	k8sQuantityRe = regexp.MustCompile(`^(\d+)(Ki|Mi|Gi|Ti|k|M|G|T)?$`)
)

// goMemLimitBytes parses the Go runtime's GOMEMLIMIT syntax: an integer with an optional
// B/KiB/MiB/GiB/TiB suffix.
func goMemLimitBytes(t *testing.T, s string) int64 {
	t.Helper()
	m := goMemLimitRe.FindStringSubmatch(s)
	require.NotNilf(t, m, "GOMEMLIMIT syntax expected (e.g. 800MiB), got %q", s)
	n, _ := strconv.ParseInt(m[1], 10, 64)
	return n * map[string]int64{"": 1, "B": 1, "KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30, "TiB": 1 << 40}[m[2]]
}

// k8sQuantityBytes parses the binary and decimal SI forms a resources.limits.memory takes.
func k8sQuantityBytes(t *testing.T, s string) int64 {
	t.Helper()
	m := k8sQuantityRe.FindStringSubmatch(s)
	require.NotNilf(t, m, "Kubernetes quantity expected (e.g. 1Gi), got %q", s)
	n, _ := strconv.ParseInt(m[1], 10, 64)
	return n * map[string]int64{"": 1, "Ki": 1 << 10, "Mi": 1 << 20, "Gi": 1 << 30, "Ti": 1 << 40,
		"k": 1e3, "M": 1e6, "G": 1e9, "T": 1e12}[m[2]]
}

func mib(b int64) string { return fmt.Sprintf("%.1f MiB", float64(b)/(1<<20)) }
