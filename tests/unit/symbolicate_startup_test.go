package unit

import (
	"testing"

	"github.com/NurfitraPujo/sentinel/apps/processor-go/symbolicate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// envMap is os.LookupEnv over a map: a key absent from the map is UNSET, a key present with "" is
// set-and-empty — the distinction SYMBOLICATE_PLATFORMS turns on (contract §4.4 gate 1).
func envMap(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

// TestStartupCheck_RefusesToStartWithoutCredentials is decision 32 / flow S8b at the unit
// boundary: with the stage on (SYMBOLICATE_PLATFORMS unset = the default set, or set non-empty)
// and S3_ACCESS_KEY / S3_SECRET_KEY absent, symbolicate.StartupCheck returns the error main.go
// log.Fatalf's BEFORE Subscribe, and that error — the process's last log line — names BOTH
// variables. Absent means unset OR set to the empty string (a chart that renders an empty
// secretKeyRef is the same rollout mistake as one that omits it).
func TestStartupCheck_RefusesToStartWithoutCredentials(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
	}{
		{"platforms unset (default set), both credentials unset", map[string]string{}},
		{"platforms set, both credentials unset", map[string]string{"SYMBOLICATE_PLATFORMS": "javascript,browser"}},
		{"only the access key", map[string]string{"S3_ACCESS_KEY": "minioadmin"}},
		{"only the secret key", map[string]string{"S3_SECRET_KEY": "minioadmin"}},
		{"both set to the empty string", map[string]string{"S3_ACCESS_KEY": "", "S3_SECRET_KEY": ""}},
		{"whitespace is absent", map[string]string{"S3_ACCESS_KEY": "  ", "S3_SECRET_KEY": "minioadmin"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg, err := symbolicate.ParseConfig(envMap(c.env))
			require.NoError(t, err)
			require.True(t, cfg.Enabled(), "precondition: the stage is on")
			err = symbolicate.StartupCheck(cfg, symbolicate.S3ConfigFromEnv(envMap(c.env)))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "S3_ACCESS_KEY", "the last log line must name the variable")
			assert.Contains(t, err.Error(), "S3_SECRET_KEY", "the last log line must name the variable")
			assert.Contains(t, err.Error(), "SYMBOLICATE_PLATFORMS", "and say which knob turns the requirement off")
		})
	}
}

// TestStartupCheck_StartsWithCredentials: both present → nil; the value is not inspected (a wrong
// key is a runtime `fetch_error` per map, D7 — never a startup failure).
func TestStartupCheck_StartsWithCredentials(t *testing.T) {
	env := map[string]string{"S3_ACCESS_KEY": "minioadmin", "S3_SECRET_KEY": "minioadmin", "S3_ENDPOINT": "http://minio:9000", "S3_BUCKET": "sentinel-attachments"}
	cfg, err := symbolicate.ParseConfig(envMap(env))
	require.NoError(t, err)
	s3 := symbolicate.S3ConfigFromEnv(envMap(env))
	assert.NoError(t, symbolicate.StartupCheck(cfg, s3))
	assert.Equal(t, symbolicate.S3Config{Endpoint: "http://minio:9000", Bucket: "sentinel-attachments", AccessKey: "minioadmin", SecretKey: "minioadmin"}, s3)
}

// TestStartupCheck_SetAndEmptyStartsWithoutS3 is the second half of criterion 2 (decision 27's
// kill switch): SYMBOLICATE_PLATFORMS set and EMPTY parses to no platforms — the stage off — so
// no credential is required, and the same empty set is what fingerprint.Compute's normalisation
// gate reads (TestCompute_NormalisationFollowsTheGateSet proves that side): neither symbolicates
// nor normalises.
func TestStartupCheck_SetAndEmptyStartsWithoutS3(t *testing.T) {
	env := map[string]string{"SYMBOLICATE_PLATFORMS": ""}
	cfg, err := symbolicate.ParseConfig(envMap(env))
	require.NoError(t, err)
	assert.False(t, cfg.Enabled(), "set and empty = off")
	assert.Empty(t, cfg.Platforms)
	assert.NoError(t, symbolicate.StartupCheck(cfg, symbolicate.S3ConfigFromEnv(envMap(env))), "no S3 requirement with the stage off")
	ok, skipped := symbolicate.Gate(cfg.Platforms, "javascript", "1.2.3", 3)
	assert.False(t, ok)
	assert.Equal(t, symbolicate.EventSkippedPlatform, skipped, "every event is skipped_platform: nothing is symbolicated")

	// Unset is NOT the same thing: it is the default set, and it does require credentials.
	cfg, err = symbolicate.ParseConfig(envMap(map[string]string{}))
	require.NoError(t, err)
	assert.Equal(t, symbolicate.DefaultPlatforms, cfg.Platforms)
	assert.Error(t, symbolicate.StartupCheck(cfg, symbolicate.S3ConfigFromEnv(envMap(map[string]string{}))))
}

// TestParseConfig_KnobSyntax pins the six knobs' value syntax as main.go reads them (decision 27):
// a comma-separated lowercase list with whitespace trimmed; plain integer bytes with NO Mi/M
// suffix; Go duration strings — and that a value in the wrong syntax is an error naming the
// variable, which main.go turns into a startup fatal rather than a silent default.
func TestParseConfig_KnobSyntax(t *testing.T) {
	cfg, err := symbolicate.ParseConfig(envMap(map[string]string{
		"SYMBOLICATE_PLATFORMS":     " javascript , Browser,node ",
		"SOURCEMAP_CACHE_BYTES":     "134217728",
		"SOURCEMAP_MAX_PARSE_BYTES": "52428800",
		"SOURCEMAP_NEGATIVE_TTL":    "60s",
		"SOURCEMAP_POSITIVE_TTL":    "10m",
		"SYMBOLICATE_DEADLINE":      "5s",
	}))
	require.NoError(t, err)
	assert.Equal(t, []string{"javascript", "browser", "node"}, cfg.Platforms)
	assert.Equal(t, int64(134217728), cfg.CacheBytes)
	assert.Equal(t, int64(52428800), cfg.MaxParseBytes)
	assert.Equal(t, "1m0s", cfg.NegativeTTL.String())
	assert.Equal(t, "10m0s", cfg.PositiveTTL.String())
	assert.Equal(t, "5s", cfg.Deadline.String())

	for key, bad := range map[string]string{
		"SOURCEMAP_CACHE_BYTES":     "128Mi",
		"SOURCEMAP_MAX_PARSE_BYTES": "50M",
		"SOURCEMAP_NEGATIVE_TTL":    "60",
		"SOURCEMAP_POSITIVE_TTL":    "ten minutes",
		"SYMBOLICATE_DEADLINE":      "0s",
	} {
		_, err := symbolicate.ParseConfig(envMap(map[string]string{key: bad}))
		require.Error(t, err, "%s=%q must not parse", key, bad)
		assert.Contains(t, err.Error(), key)
	}
}

// TestAckWait_Arithmetic pins decision 37's AckWait rule as main.go applies it to natsCfg:
// BatchSize (10) × SYMBOLICATE_DEADLINE + the write allowance, ≥ 10 × the deadline, 80s at the
// defaults, and it follows a raised deadline.
func TestAckWait_Arithmetic(t *testing.T) {
	def := symbolicate.DefaultConfig()
	assert.Equal(t, 80*1e9, float64(symbolicate.AckWait(10, def.Deadline)), "10 × 5s + 30s")
	assert.GreaterOrEqual(t, symbolicate.AckWait(10, def.Deadline), 10*def.Deadline)
	assert.Equal(t, symbolicate.AckWait(10, 2*def.Deadline)-symbolicate.AckWait(10, def.Deadline), 10*def.Deadline,
		"doubling the deadline moves AckWait by ten deadlines")
}
