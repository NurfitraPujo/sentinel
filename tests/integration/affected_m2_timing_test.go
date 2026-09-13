package integration

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/NurfitraPujo/sentinel/apps/processor-go/store"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// sentinel-affected-users §13 M2, as decision 29 narrowed it: the cost of the §4.3 rollup is taken
// AT THE STORE BOUNDARY — a Go loop driving store.StoreEvent against the suite's disposable Postgres
// with only a pgx pool (the setup TestEventIdempotency_DuplicateDoesNotFeedAlertsOrSearchIndex
// uses: no ingestor, no NATS) — and reported as wall-clock p95 per call in µs, side by side for the
// three pools §13 names:
//
//	(i)   pool 0      — no ids at all: the unchanged path, the baseline
//	(ii)  pool 10,000 — every event a NEW (issue, id) pair: two upserts that INSERT + two bumps
//	(iii) pool 50     — steady state: the pairs are exhausted early and the upserts hit DO UPDATE
//
// The same fingerprint set is used for all three runs so the issues-row contention is equal. Event
// i carries fingerprint i % fingerprints and, when the pool is non-zero, user/tenant id i % pool;
// with defaultM2Fingerprints coprime with both pool sizes the (fingerprint, id) pairs walk the whole
// product before one repeats, which is what makes (ii) "every event new" for the first
// lcm(53, 10000) = 530,000 events and (iii) "steady" after the first lcm(53, 50) = 2,650.
//
// This is a MEASUREMENT, not a gate (§13: "report all three p95s side by side; do not gate"). It
// runs only when SENTINEL_M2_TIMING=1 — 30,000 sequential StoreEvent calls have no place in a CI
// run — and it never fails on a number: the one criterion it reads, a (ii)-versus-(i) delta above
// +50 %, is CALLED OUT in the output with the upsert's plan attached, exactly as §13 asks. It DOES
// fail on correctness — every event must store, every run's issues must satisfy §4.6's invariant,
// and the distinct-pair counts must be the ones the pairing predicts — because a timing of a write
// that did not happen is not a timing.
//
// Overrides: SENTINEL_M2_EVENTS (default 10,000; §13's floor) and SENTINEL_M2_FINGERPRINTS
// (default 53). Same venue caveat as the rest of §13: a local disposable Postgres is the LABELLED
// FALLBACK (decision 20) and these numbers are evidence about plan shape, never a criterion met.
const (
	defaultM2Events       = 10000
	defaultM2Fingerprints = 53
)

// m2Pools is §13's three pool sizes, in the order the report lists them.
var m2Pools = []int{0, 10000, 50}

// m2Run is one pool's outcome: the sorted per-call samples and what the database says happened.
type m2Run struct {
	pool          int
	samples       []time.Duration // sorted ascending after the run
	issues        int
	distinctPairs int // rows in issue_affected_users for the run's project == upserts that INSERTed
	userBumps     int // sum(affected_user_count) over the run's issues — must equal distinctPairs
	projectID     string
}

func TestAffectedRollup_M2StoreEventTiming(t *testing.T) {
	if os.Getenv("SENTINEL_M2_TIMING") != "1" {
		t.Skip("SENTINEL_M2_TIMING is unset — the decision-29 timing loop runs only when asked for (§13 M2)")
	}
	events := m2EnvInt(t, "SENTINEL_M2_EVENTS", defaultM2Events)
	fingerprints := m2EnvInt(t, "SENTINEL_M2_FINGERPRINTS", defaultM2Fingerprints)

	s, pool := newEIDStoreAndPoolFromEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	runs := make([]m2Run, 0, len(m2Pools))
	for _, poolSize := range m2Pools {
		runs = append(runs, m2DriveStoreEvent(t, ctx, s, pool, poolSize, events, fingerprints))
	}

	t.Log("\n" + m2Report(runs, events, fingerprints))

	// §13: "a (ii) delta above +50 % is not a failure but must be called out as a finding with the
	// plan of the upsert attached". The plan is attached either way — it is the same statement
	// whichever side of the line the number falls and the reviewer reads the two together; only
	// the FINDING line changes.
	delta := m2DeltaPercent(m2Percentile(runs[0].samples, 0.95), m2Percentile(runs[1].samples, 0.95))
	if delta > 50 {
		t.Logf("FINDING (§13 M2): pool 10,000 p95 is %+.1f%% over pool 0 — above the +50%% line; the upsert's plan follows", delta)
	} else {
		t.Logf("pool 10,000 p95 is %+.1f%% over pool 0 — within the +50%% line; the upsert's plan follows for the record", delta)
	}
	t.Log("\n" + m2ExplainUpsert(t, ctx, pool, runs[1]))
}

// m2DriveStoreEvent seeds a project for one pool size, declares `production` (so the registry
// touch matches one row, as it does in a real deployment), drives `events` StoreEvent calls
// sequentially on one goroutine — one call at a time, so a sample is one transaction's wall clock
// and nothing else — and reads back what landed.
func m2DriveStoreEvent(t *testing.T, ctx context.Context, s store.IssueStore, pool *pgxpool.Pool, poolSize, events, fingerprints int) m2Run {
	t.Helper()
	projectID := eidSeedProject(t, pool, fmt.Sprintf("m2-pool%d", poolSize))
	_, err := pool.Exec(ctx,
		`INSERT INTO project_environments (project_id, name, display_name) VALUES ($1, 'production', 'Production')`,
		projectID)
	require.NoError(t, err)

	fps := make([]string, fingerprints)
	for i := range fps {
		fps[i] = fmt.Sprintf("fp-m2-%d", i) // the SAME set for every pool: the run's project scopes it
	}

	samples := make([]time.Duration, 0, events)
	for i := 0; i < events; i++ {
		occ := eidOccurrenceIn("production", fmt.Sprintf("evt-m2-%d-%d", poolSize, i))
		if poolSize > 0 {
			// < 6 digits, so neither id is the normaliser's <NUMERIC_ID> shape — irrelevant at this
			// boundary (StoreEvent never normalises) but it keeps the ids readable in psql.
			occ.AffectedUserID = fmt.Sprintf("m2-user-%d", i%poolSize)
			occ.AffectedTenantID = fmt.Sprintf("m2-tenant-%d", i%poolSize)
		}
		issue := eidIssueIn(projectID, "production", fps[i%fingerprints])

		start := time.Now()
		_, stored, err := s.StoreEvent(ctx, issue, occ, "")
		samples = append(samples, time.Since(start))

		require.NoError(t, err, "pool %d event %d", poolSize, i)
		require.True(t, stored, "pool %d event %d: a fresh event_id must store", poolSize, i)
	}
	sort.Slice(samples, func(a, b int) bool { return samples[a] < samples[b] })

	run := m2Run{pool: poolSize, samples: samples, projectID: projectID}
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*)::int,
		       coalesce(sum(affected_user_count), 0)::int,
		       (SELECT count(*) FROM issue_affected_users a JOIN issues i ON i.id = a.issue_id WHERE i.project_id = $1)::int
		  FROM issues WHERE project_id = $1`, projectID).Scan(&run.issues, &run.userBumps, &run.distinctPairs))

	// Correctness before timing: the invariant (§4.6) on every issue the run touched, and the
	// distinct-pair count the pairing predicts — min(events, lcm(fingerprints, pool)) — so a run
	// that quietly skipped the rollup cannot report a flattering p95.
	var violations int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*)::int FROM issues i
		 WHERE i.project_id = $1
		   AND (i.affected_user_count   <> (SELECT count(*) FROM issue_affected_users   a WHERE a.issue_id = i.id)
		     OR i.affected_tenant_count <> (SELECT count(*) FROM issue_affected_tenants a WHERE a.issue_id = i.id))`,
		projectID).Scan(&violations))
	require.Zero(t, violations, "pool %d: §4.6 invariant violated on %d issue(s)", poolSize, violations)
	require.Equal(t, fingerprints, run.issues, "pool %d: one issue per fingerprint", poolSize)
	require.Equal(t, m2ExpectedPairs(events, fingerprints, poolSize), run.distinctPairs, "pool %d: distinct (issue, user) pairs", poolSize)
	require.Equal(t, run.distinctPairs, run.userBumps, "pool %d: the count was bumped once per NEW pair and never otherwise", poolSize)
	return run
}

// m2ExpectedPairs is how many distinct (fingerprint, id) pairs `events` events produce when event i
// carries fingerprint i % fingerprints and id i % pool: the pair sequence has period
// lcm(fingerprints, pool), so it is min(events, lcm). A pool of 0 carries no id and produces none.
func m2ExpectedPairs(events, fingerprints, pool int) int {
	if pool == 0 {
		return 0
	}
	l := fingerprints / m2GCD(fingerprints, pool) * pool
	if events < l {
		return events
	}
	return l
}

func m2GCD(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// m2Percentile is the nearest-rank percentile of an ASCENDING sorted sample: the value at rank
// ceil(p × n), which is the convention tests/load's p99 uses (an index at p × n, floored). Empty
// input is 0.
func m2Percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(p * float64(len(sorted)))
	if rank >= len(sorted) {
		rank = len(sorted) - 1
	}
	return sorted[rank]
}

// m2DeltaPercent is (b − a) / a as a percentage; +50 means b is 1.5× a. A zero baseline yields 0
// rather than a division by zero — it cannot happen for a real sample but the report must not panic.
func m2DeltaPercent(a, b time.Duration) float64 {
	if a == 0 {
		return 0
	}
	return (float64(b) - float64(a)) / float64(a) * 100
}

// m2Report is §13's side-by-side table: p50 / p95 / p99 / max per call in µs for the three pools,
// each pool's delta against pool 0, and the row counts that prove what each run actually wrote.
func m2Report(runs []m2Run, events, fingerprints int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "§13 M2 (decision 29, labelled fallback) — store.StoreEvent wall clock per call, %d events per pool over %d fingerprints, sequential, pgx pool only\n", events, fingerprints)
	fmt.Fprintf(&b, "%-12s %10s %10s %10s %10s %12s %8s %14s %11s\n", "pool", "p50 µs", "p95 µs", "p99 µs", "max µs", "p95 vs pool0", "issues", "distinct pairs", "user bumps")
	base := m2Percentile(runs[0].samples, 0.95)
	for _, r := range runs {
		p95 := m2Percentile(r.samples, 0.95)
		fmt.Fprintf(&b, "%-12d %10d %10d %10d %10d %+11.1f%% %8d %14d %11d\n",
			r.pool,
			m2Percentile(r.samples, 0.50).Microseconds(),
			p95.Microseconds(),
			m2Percentile(r.samples, 0.99).Microseconds(),
			r.samples[len(r.samples)-1].Microseconds(),
			m2DeltaPercent(base, p95),
			r.issues, r.distinctPairs, r.userBumps)
	}
	return b.String()
}

// m2ExplainUpsert attaches the plan §13 asks for beside a +50 % finding: EXPLAIN (ANALYZE, BUFFERS)
// of the §4.3 users upsert against the pool-10,000 project's side table, once for a NEW pair (the
// INSERT arm, what pool 10,000 pays on every event) and once for an EXISTING pair (the DO UPDATE
// arm, pool 50's steady state) — each inside a transaction that is rolled back, so the measurement
// leaves the run's rows exactly as the loop wrote them.
func m2ExplainUpsert(t *testing.T, ctx context.Context, pool *pgxpool.Pool, run m2Run) string {
	t.Helper()
	var issueID string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT id FROM issues WHERE project_id = $1 ORDER BY id LIMIT 1`, run.projectID).Scan(&issueID))
	var existingUser string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT user_id FROM issue_affected_users WHERE issue_id = $1 ORDER BY user_id LIMIT 1`, issueID).Scan(&existingUser))

	// The upsert exactly as store.go issues it (affectedUsersUpsertQuery), with the plan wrapped
	// around it; EXPLAIN ANALYZE executes the statement, hence the rollback.
	const explain = `
		EXPLAIN (ANALYZE, BUFFERS)
		INSERT INTO issue_affected_users (issue_id, user_id, first_seen, last_seen)
		VALUES ($1, $2, $3, $3)
		ON CONFLICT (issue_id, user_id) DO UPDATE
		   SET last_seen  = GREATEST(issue_affected_users.last_seen, EXCLUDED.last_seen),
		       first_seen = LEAST(issue_affected_users.first_seen, EXCLUDED.first_seen)
		RETURNING (xmax = 0) AS inserted`

	var b strings.Builder
	for _, c := range []struct{ label, userID string }{
		{"NEW pair (INSERT arm — pool 10,000's every event)", "m2-user-explain-new"},
		{"EXISTING pair (DO UPDATE arm — pool 50's steady state)", existingUser},
	} {
		tx, err := pool.Begin(ctx)
		require.NoError(t, err)
		rows, err := tx.Query(ctx, explain, issueID, c.userID, time.Now().UTC())
		require.NoError(t, err)
		fmt.Fprintf(&b, "-- %s\n", c.label)
		for rows.Next() {
			var line string
			require.NoError(t, rows.Scan(&line))
			b.WriteString(line)
			b.WriteByte('\n')
		}
		rows.Close()
		require.NoError(t, tx.Rollback(ctx))
	}
	return b.String()
}

// m2EnvInt reads a positive integer override, failing on a malformed one for the same reason
// tests/load's loadEnvInt does: a typo that silently runs the default is a number nobody can reproduce.
func m2EnvInt(t *testing.T, name string, def int) int {
	t.Helper()
	raw := os.Getenv(name)
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		t.Fatalf("%s must be a positive integer, got %q", name, raw)
	}
	return v
}

// ---------------------------------------------------------------------------------------------------
// The arithmetic the report rests on, pinned without infrastructure so a wrong percentile or a wrong
// pair prediction cannot hide behind a 30,000-call run.
// ---------------------------------------------------------------------------------------------------

func TestM2Percentile_NearestRankOnSortedSamples(t *testing.T) {
	samples := make([]time.Duration, 100)
	for i := range samples {
		samples[i] = time.Duration(i+1) * time.Microsecond // 1µs … 100µs
	}
	require.Equal(t, 51*time.Microsecond, m2Percentile(samples, 0.50))
	require.Equal(t, 96*time.Microsecond, m2Percentile(samples, 0.95))
	require.Equal(t, 100*time.Microsecond, m2Percentile(samples, 0.99))
	require.Equal(t, 100*time.Microsecond, m2Percentile(samples, 1.0), "p100 clamps to the last sample")
	require.Equal(t, time.Duration(0), m2Percentile(nil, 0.95), "no samples, no panic")
}

func TestM2DeltaPercent(t *testing.T) {
	require.InDelta(t, 50.0, m2DeltaPercent(200*time.Microsecond, 300*time.Microsecond), 1e-9)
	require.InDelta(t, -25.0, m2DeltaPercent(400*time.Microsecond, 300*time.Microsecond), 1e-9)
	require.Equal(t, 0.0, m2DeltaPercent(0, 300*time.Microsecond), "a zero baseline reports 0, never NaN/Inf")
}

func TestM2ExpectedPairs_WalksTheProductBeforeRepeating(t *testing.T) {
	require.Equal(t, 0, m2ExpectedPairs(10000, 53, 0), "pool 0 carries no id")
	require.Equal(t, 10000, m2ExpectedPairs(10000, 53, 10000), "pool 10,000: every one of 10,000 events is a new pair (lcm 530,000)")
	require.Equal(t, 2650, m2ExpectedPairs(10000, 53, 50), "pool 50: lcm(53, 50) = 2,650 pairs, then DO UPDATE for the remaining 7,350")
	require.Equal(t, 50, m2ExpectedPairs(100, 50, 50), "a non-coprime pairing: lcm(50, 50) = 50 pairs, however many events")
	require.Equal(t, 7, m2ExpectedPairs(7, 53, 10000), "fewer events than the period: every event is its own pair")
}
