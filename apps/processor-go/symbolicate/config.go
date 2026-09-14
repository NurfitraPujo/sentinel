package symbolicate

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The six processor knobs of contract §4.4 / §12, each with the value syntax the contract states
// beside its name (decision 27) -- an operator turning one reads values.yaml and nothing else, so
// the syntax is repeated here and enforced by ParseConfig rather than tolerated.
const (
	// EnvPlatforms is a comma-separated, lowercase list, whitespace around each item trimmed:
	// `javascript,browser`. Unset = that default; set and EMPTY = the stage is off (§4.4 gate 1).
	EnvPlatforms = "SYMBOLICATE_PLATFORMS"
	// EnvCacheBytes is a plain integer number of bytes (`134217728`); no `Mi`/`M` suffix.
	EnvCacheBytes = "SOURCEMAP_CACHE_BYTES"
	// EnvNegativeTTL is a Go duration string (`60s`, `1m`).
	EnvNegativeTTL = "SOURCEMAP_NEGATIVE_TTL"
	// EnvPositiveTTL is a Go duration string (`10m`).
	EnvPositiveTTL = "SOURCEMAP_POSITIVE_TTL"
	// EnvDeadline is a Go duration string (`5s`) -- decision 37.
	EnvDeadline = "SYMBOLICATE_DEADLINE"
	// EnvMaxParseBytes is a plain integer number of bytes; default = SOURCEMAP_MAX_BYTES's
	// default (`52428800`) -- decision 38.
	EnvMaxParseBytes = "SOURCEMAP_MAX_PARSE_BYTES"

	DefaultCacheBytes    int64 = 128 << 20 // 134217728 -- decision 11
	DefaultMaxParseBytes int64 = 50 << 20  // 52428800 = SOURCEMAP_MAX_BYTES -- decision 38
	DefaultNegativeTTL         = 60 * time.Second
	DefaultPositiveTTL         = 10 * time.Minute
	DefaultDeadline            = 5 * time.Second
)

// DefaultPlatforms is the gate set of decision 9 (OQ-5): `node` is not in it until a producer asks.
var DefaultPlatforms = []string{"javascript", "browser"}

// Config is the stage's configuration. Zero values are NOT defaults -- use DefaultConfig or
// ParseConfig so a zero Deadline cannot silently time every frame out.
type Config struct {
	// Platforms is the gate set (§4.4 gate 1); empty means the stage is off.
	Platforms []string
	// CacheBytes bounds the positive cache by the rows' raw size_bytes (§4.4 cache paragraph).
	// A map larger than this is fetched per event and never retained.
	CacheBytes int64
	// MaxParseBytes is decision 38's parse cap: a row over it is never fetched (`oversize`).
	MaxParseBytes int64
	// NegativeTTL bounds a negative entry (`no_map`, `fetch_error`, `map_error`, `oversize`).
	NegativeTTL time.Duration
	// PositiveTTL bounds a positive entry; a replaced map is picked up after it at the latest.
	PositiveTTL time.Duration
	// Deadline is decision 37's per-event wall-clock bound on the stage.
	Deadline time.Duration
}

// DefaultConfig is every knob at its contract default.
func DefaultConfig() Config {
	return Config{
		Platforms:     append([]string(nil), DefaultPlatforms...),
		CacheBytes:    DefaultCacheBytes,
		MaxParseBytes: DefaultMaxParseBytes,
		NegativeTTL:   DefaultNegativeTTL,
		PositiveTTL:   DefaultPositiveTTL,
		Deadline:      DefaultDeadline,
	}
}

// Enabled is §4.4's "set and empty = the stage is off".
func (c Config) Enabled() bool { return len(c.Platforms) > 0 }

// ParseConfig reads the six knobs through lookup (os.LookupEnv in main; a map in tests). The
// LookupEnv shape is required, not Getenv, because EnvPlatforms distinguishes unset (the default
// set) from set-and-empty (the stage off). A value that is set but does not parse in the stated
// syntax is an error naming the variable -- a `128Mi` in SOURCEMAP_CACHE_BYTES must stop the
// process at startup, not run with the default.
func ParseConfig(lookup func(string) (string, bool)) (Config, error) {
	cfg := DefaultConfig()

	if v, ok := lookup(EnvPlatforms); ok {
		cfg.Platforms = ParsePlatforms(v)
	}
	var err error
	if cfg.CacheBytes, err = parseBytes(lookup, EnvCacheBytes, cfg.CacheBytes); err != nil {
		return Config{}, err
	}
	if cfg.MaxParseBytes, err = parseBytes(lookup, EnvMaxParseBytes, cfg.MaxParseBytes); err != nil {
		return Config{}, err
	}
	if cfg.NegativeTTL, err = parseDuration(lookup, EnvNegativeTTL, cfg.NegativeTTL); err != nil {
		return Config{}, err
	}
	if cfg.PositiveTTL, err = parseDuration(lookup, EnvPositiveTTL, cfg.PositiveTTL); err != nil {
		return Config{}, err
	}
	if cfg.Deadline, err = parseDuration(lookup, EnvDeadline, cfg.Deadline); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// ParsePlatforms is EnvPlatforms's syntax: split on `,`, trim each, lower-case, drop empties.
func ParsePlatforms(v string) []string {
	var out []string
	for _, item := range strings.Split(v, ",") {
		if item = strings.ToLower(strings.TrimSpace(item)); item != "" {
			out = append(out, item)
		}
	}
	return out
}

func parseBytes(lookup func(string) (string, bool), key string, def int64) (int64, error) {
	raw, _ := lookup(key)
	v := strings.TrimSpace(raw)
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s: %q is not a plain positive integer number of bytes (no Mi/M suffix)", key, v)
	}
	return n, nil
}

func parseDuration(lookup func(string) (string, bool), key string, def time.Duration) (time.Duration, error) {
	raw, _ := lookup(key)
	v := strings.TrimSpace(raw)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s: %q is not a positive Go duration string (e.g. 5s, 1m)", key, v)
	}
	return d, nil
}
