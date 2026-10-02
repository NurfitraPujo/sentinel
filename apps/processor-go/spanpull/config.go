// Package spanpull is sentinel-span-ingest D3: when an error occurrence with a canonical trace id is stored in an
// allow-listed organization, the trace is queued and a worker fetches its spans from core's trace store (Tempo on
// Huawei, VictoriaTraces on AWS) twice — 2 and 10 minutes after the first error — into the spans table.
package spanpull

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

const (
	KindTempo  = "tempo"
	KindJaeger = "jaeger"

	FirstFetchDelay  = 2 * time.Minute
	SecondFetchDelay = 10 * time.Minute // after the first error, not after the first fetch
	FetchTimeout     = 5 * time.Second
	MaxSpansPerTrace = 5000
	// MaxAttemptsPerStage bounds retries of one fetch that errors (store down, 5xx): after this many the stage is
	// given up on and the trace moves on, so a dead store cannot pin queue rows forever.
	MaxAttemptsPerStage = 3
	RetryDelay          = 1 * time.Minute
	// ClaimLease is how long a claimed row is invisible to other workers; a crashed worker's row is due again after it.
	ClaimLease = 2 * time.Minute
)

// Config is the knobs of D3. The feature is OFF unless a scope map, TRACE_STORE_KIND and TRACE_STORE_URL are all set.
type Config struct {
	// Scopes is SPAN_PULL_SCOPES: organization UUID -> the UUID of that organization's CORE project. An organization
	// that is a key is the guard (trace ids are client-asserted: an error from any other organization never causes
	// core's trace to be copied into it), and the mapped project is what GOVERNS the pulled spans: they are core's
	// data, so they are shown only to a viewer who can reach that project and whose environment grant on it permits the
	// span's environment (D8) — whichever project's error happened to queue the trace.
	Scopes map[string]string
	Kind   string // TRACE_STORE_KIND: tempo | jaeger
	URL    string // TRACE_STORE_URL
}

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// ParseConfig reads the variables through lookup (os.LookupEnv in main, a map in tests). Unset or empty
// SPAN_PULL_SCOPES or TRACE_STORE_URL → (Config{}, nil): the feature is off. A value that is set but wrong is an
// error, so a typo stops the process instead of silently pulling nothing. SPAN_PULL_SCOPES is a comma-separated list
// of `<org_uuid>:<core_project_uuid>`.
func ParseConfig(lookup func(string) (string, bool)) (Config, error) {
	get := func(k string) string {
		v, _ := lookup(k)
		return strings.TrimSpace(v)
	}
	scopes, url, kind := get("SPAN_PULL_SCOPES"), strings.TrimRight(get("TRACE_STORE_URL"), "/"), strings.ToLower(get("TRACE_STORE_KIND"))
	if scopes == "" || url == "" {
		return Config{}, nil
	}
	if kind != KindTempo && kind != KindJaeger {
		return Config{}, fmt.Errorf("TRACE_STORE_KIND must be %q or %q, got %q", KindTempo, KindJaeger, kind)
	}
	cfg := Config{Scopes: map[string]string{}, Kind: kind, URL: url}
	for _, pair := range strings.Split(scopes, ",") {
		pair = strings.ToLower(strings.TrimSpace(pair))
		if pair == "" {
			continue
		}
		org, project, ok := strings.Cut(pair, ":")
		if !ok || !uuidRe.MatchString(org) || !uuidRe.MatchString(project) {
			return Config{}, fmt.Errorf("SPAN_PULL_SCOPES: %q is not <org_uuid>:<project_uuid>", pair)
		}
		cfg.Scopes[org] = project
	}
	if len(cfg.Scopes) == 0 {
		return Config{}, nil
	}
	return cfg, nil
}

// Enabled reports whether the feature is on.
func (c Config) Enabled() bool { return len(c.Scopes) > 0 && c.URL != "" && c.Kind != "" }

// Allowed reports whether orgID is a key of the scope map.
func (c Config) Allowed(orgID string) bool {
	_, ok := c.Scopes[strings.ToLower(orgID)]
	return ok
}

// ScopeProject is the core project that governs orgID's pulled spans ("" if the organization is not configured).
func (c Config) ScopeProject(orgID string) string { return c.Scopes[strings.ToLower(orgID)] }
