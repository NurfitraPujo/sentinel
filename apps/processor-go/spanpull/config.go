// Package spanpull is sentinel-span-ingest D3: when an error occurrence with a canonical trace id is stored in an
// allow-listed organization, the trace is queued and a worker fetches its spans from core's trace store (Tempo on
// Huawei, VictoriaTraces on AWS) twice — 2 and 10 minutes after the first error — into the spans table.
package spanpull

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
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

	// Duplicated counts the organizations refused because SPAN_PULL_SCOPES named them more than once.
	Duplicated int

	// gate holds the scopes that passed ValidateAgainstDB. ParseConfig always sets it, and until it is filled
	// Allowed is false for every organization. A Config built as a literal (tests) has no gate and trusts Scopes.
	gate *atomic.Pointer[map[string]string]
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
	cfg := Config{Scopes: map[string]string{}, Kind: kind, URL: url, gate: new(atomic.Pointer[map[string]string])}
	seen := map[string]bool{}
	dup := map[string]bool{}
	for _, pair := range strings.Split(scopes, ",") {
		pair = strings.ToLower(strings.TrimSpace(pair))
		if pair == "" {
			continue
		}
		org, project, ok := strings.Cut(pair, ":")
		if !ok || !uuidRe.MatchString(org) || !uuidRe.MatchString(project) {
			return Config{}, fmt.Errorf("SPAN_PULL_SCOPES: %q is not <org_uuid>:<project_uuid>", pair)
		}
		if seen[org] {
			dup[org] = true
		}
		seen[org] = true
		cfg.Scopes[org] = project
	}
	// A repeated organization is ambiguous (which core project governs its spans?), so it is refused entirely rather
	// than keeping the last value. It is not fatal: the processor also ingests errors.
	for org := range dup {
		delete(cfg.Scopes, org)
		cfg.Duplicated++
		slog.Error("span pull: organization is listed more than once in SPAN_PULL_SCOPES; refusing it entirely", slog.String("org", org))
	}
	if len(cfg.Scopes) == 0 {
		return Config{}, nil
	}
	return cfg, nil
}

// Enabled reports whether the feature is on.
func (c Config) Enabled() bool { return len(c.Scopes) > 0 && c.URL != "" && c.Kind != "" }

// Allowed reports whether orgID is a key of the scope map.
// With a gate (every Config from ParseConfig) it is false until validation has succeeded, and then only for the
// scopes that survived it.
func (c Config) Allowed(orgID string) bool {
	_, ok := c.effective()[strings.ToLower(orgID)]
	return ok
}

// Validated reports whether the scopes have been checked against the database and published (always true for a
// Config without a gate). Until then Allowed says nothing about an organization, only that it is not yet known.
func (c Config) Validated() bool { return c.gate == nil || c.gate.Load() != nil }

func (c Config) effective() map[string]string {
	if c.gate == nil {
		return c.Scopes
	}
	if m := c.gate.Load(); m != nil {
		return *m
	}
	return nil
}

// ScopeProject is the core project that governs orgID's pulled spans ("" if the organization is not configured).
func (c Config) ScopeProject(orgID string) string { return c.effective()[strings.ToLower(orgID)] }

const validationQueryTimeout = 5 * time.Second

// ScopeQuerier is the slice of a pgx pool that validation needs.
type ScopeQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// ValidateAgainstDB checks every scope against the projects table: the project must exist and belong to the
// organization it is mapped from. Bad entries are dropped with an ERROR naming org, project and reason. A database
// error (not "no such row") is returned and nothing is published, so Allowed stays false. On success the surviving
// scopes are published to every copy of this Config and the accepted and refused counts are returned.
func (c Config) ValidateAgainstDB(ctx context.Context, db ScopeQuerier) (accepted, refused int, err error) {
	ok := make(map[string]string, len(c.Scopes))
	for org, project := range c.Scopes {
		var owner *string
		qctx, cancel := context.WithTimeout(ctx, validationQueryTimeout)
		qerr := db.QueryRow(qctx, `SELECT organization_id::text FROM projects WHERE id = $1::uuid`, project).Scan(&owner)
		cancel()
		switch {
		case errors.Is(qerr, pgx.ErrNoRows):
			slog.ErrorContext(ctx, "span pull: scope refused: the project does not exist", slog.String("org", org), slog.String("project", project))
			refused++
		case qerr != nil:
			return 0, 0, qerr
		case owner == nil || !strings.EqualFold(*owner, org):
			slog.ErrorContext(ctx, "span pull: scope refused: the project belongs to a different organization", slog.String("org", org), slog.String("project", project))
			refused++
		default:
			ok[org] = project
		}
	}
	if c.gate != nil {
		c.gate.Store(&ok)
	}
	return len(ok), refused, nil
}

// RunValidation validates until it succeeds, retrying every interval (WARN per failure) so a database that is not
// ready yet neither blocks error ingest nor turns the feature off for the life of the process. It logs one INFO
// line with the accepted and refused counts (duplicates included in refused).
func (c Config) RunValidation(ctx context.Context, db ScopeQuerier, interval time.Duration) {
	for {
		accepted, refused, err := c.ValidateAgainstDB(ctx, db)
		if err == nil {
			slog.InfoContext(ctx, "span pull: scopes validated", slog.Int("accepted", accepted), slog.Int("refused", refused+c.Duplicated))
			return
		}
		if ctx.Err() != nil {
			return // shutting down
		}
		slog.WarnContext(ctx, "span pull: scope validation failed; nothing is enqueued or pulled until it succeeds", slog.String("error", err.Error()))
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}
