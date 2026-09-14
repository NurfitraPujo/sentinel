package integration

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/NurfitraPujo/sentinel/apps/processor-go/store"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// sentinel-releases-snooze §13 M1 (decisions 11, 35, 46), the sibling of affected_m2_timing_test.go's
// and trend_m2_timing_test.go's loops: the cost of §4.3's hot-row release upsert (StoreEvent step 4c)
// and of step 0's two plain `releases` reads is taken AT THE STORE BOUNDARY — a Go loop driving
// store.StoreEvent against the suite's Postgres with only a pgx pool (no ingestor, no NATS) — and
// reported as `release_store_event_ms`: the driver's own wall clock per call (time.Since around the
// call, as m2DriveStoreEvent takes it), p50/p95/p99 in µs, side by side for the four runs §13 names,
// every one with user/tenant id pool 0 (no affected-rollup statement) and the bucket write ON (the
// shipped binary), so the release work is the only variable:
//
//	(i)   baseline — shape (iii)'s inputs with the release reads and the upsert COMPILED OUT
//	      (decision 11: `go test -tags m1_norelease`, which flips store's writeRelease constant to
//	      false — release_write_off.go)
//	(ii)  spread   — 1,000 fingerprints across 50 versions (event i carries version i % 50: 50
//	      `releases` rows, each hit events/50 times)
//	(iii) current  — 1,000 fingerprints, ONE version: the production shape, every event bumps one
//	      `releases` row (the hot row)
//	(iv)  resolved — both step-0 reads on every event (decision 46): V is ingested FIRST (1,000
//	      untimed events, one per fingerprint), then a later version X (one untimed event), the
//	      1,000 issues are resolved in X exactly as the dashboard's PATCH would write them, and
//	      shape (iii)'s 10,000 events carrying V are driven. rowR.first_seen < rowX.first_seen is
//	      *no* on every event (§4.3's table, decision 1), so both reads fire every time and the
//	      regression arm never runs — a correctness gate of ZERO regressions (regression_count
//	      unchanged on all 1,000, every row still `resolved` in X) proves the run measured what it
//	      claims and not the regression arm
//
// Run (i) is a separate `go test` invocation because the constant is compile-time: the same test,
// same inputs, same database shape as (iii) with one constant folded away. This test detects which
// build it is in at runtime — it stores one probe event with a release and looks for its `releases`
// row — so the shipped build runs (ii)–(iv) and the m1_norelease build runs (i) only, and neither
// can silently run the other's shapes. Pass the (i) run's p95 back to the shipped build as
// SENTINEL_M1_BASELINE_P95_US=<µs> and it prints the (iii)-over-(i) delta §13 calls the primary
// number.
//
// This is a MEASUREMENT, not a gate (§13: "report all four p95s side by side; do not gate"). It runs
// only when SENTINEL_M1_TIMING=1 and never fails on a number: the one line it reads, (iii) over (i)
// above +25 %, is CALLED OUT as a finding with the upsert's plan, `pg_stat_user_tables.n_dead_tup`
// for `releases` and a `pg_stat_activity` lock-wait sample attached — all three are attached either
// way, because the reviewer reads them together and only the FINDING line changes. It DOES fail on
// correctness, as its siblings do: every event must store; after (iii) exactly one `releases` row
// whose occurrence_count equals the events stored and whose last_seen is the max event timestamp;
// after (ii) exactly 50 rows whose counts sum to the events stored; after (iv) zero regressions —
// a timing of a write that did not happen, or of the wrong arm, is not a timing.
//
// Overrides: SENTINEL_M1_EVENTS (default 10,000; §13's floor) and SENTINEL_M1_FINGERPRINTS (default
// 1,000; §13's shape). Venue (decisions 23, 35): a fresh testcontainers Postgres
// (FORCE_TESTCONTAINERS=1) — no seed, and never a database a compose processor is also writing to.
// The numbers are the LABELLED FALLBACK: evidence about plan shape, never the criterion met.
const (
	defaultM1Events       = 10000
	defaultM1Fingerprints = 1000
	m1SpreadVersions      = 50
)

// m1Shape is one of §13's four runs: how many fingerprints the events cycle over and how many
// versions they carry (one, or event i → version i % versions).
type m1Shape struct {
	label        string
	fingerprints int
	versions     int
	resolvedInX  bool // run (iv): the issues are pre-created, resolved in X, and every event carries V
}

// m1Run is one run's outcome: the sorted per-call samples and what the database says landed.
type m1Run struct {
	shape        m1Shape
	samples      []time.Duration // sorted ascending after the run
	issues       int
	occurrences  int
	releaseRows  int
	releaseSum   int64
	regressions  int64 // sum(regression_count) over the run's issues — run (iv)'s gate
	stillResolvd int   // run (iv): issues still `resolved` in X after the drive
	projectID    string
	hotVersion   string    // the version the plan is taken against ((iii)'s one row; (iv)'s V)
	maxCreatedAt time.Time // the max event timestamp the loop wrote
}

func TestRelease_M1StoreEventTiming(t *testing.T) {
	if os.Getenv("SENTINEL_M1_TIMING") != "1" {
		t.Skip("SENTINEL_M1_TIMING is unset — the §13 M1 timing loop runs only when asked for")
	}
	events := m2EnvInt(t, "SENTINEL_M1_EVENTS", defaultM1Events)
	fingerprints := m2EnvInt(t, "SENTINEL_M1_FINGERPRINTS", defaultM1Fingerprints)

	s, pool := newEIDStoreAndPoolFromEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()

	releaseBuild := m1ReleaseWriteCompiledIn(t, ctx, s, pool)

	current := m1Shape{label: "(iii) current", fingerprints: fingerprints, versions: 1}
	if !releaseBuild {
		// The m1_norelease build: run (i) is shape (iii)'s inputs, and the ONLY thing it may report
		// is that no `releases` row was written — anything else means the tag did not take.
		base := m1Drive(t, ctx, s, pool, m1Shape{label: "(i) baseline", fingerprints: fingerprints, versions: 1}, events, false)
		t.Log("\n" + m1Report([]m1Run{base}, events))
		p95 := m2Percentile(base.samples, 0.95).Microseconds()
		t.Logf("(i) baseline p95 = %d µs — built with -tags m1_norelease, zero releases rows written; pass SENTINEL_M1_BASELINE_P95_US=%d to the shipped build for the (iii)-over-(i) delta", p95, p95)
		return
	}

	shapes := []m1Shape{
		{label: "(ii) spread", fingerprints: fingerprints, versions: m1SpreadVersions},
		current,
		{label: "(iv) resolved", fingerprints: fingerprints, versions: 1, resolvedInX: true},
	}
	runs := make([]m1Run, 0, len(shapes))
	for _, shape := range shapes {
		runs = append(runs, m1Drive(t, ctx, s, pool, shape, events, true))
	}
	t.Log("\n" + m1Report(runs, events))

	currentP95 := m2Percentile(runs[1].samples, 0.95)
	resolvedP95 := m2Percentile(runs[2].samples, 0.95)

	// §13's primary number: (iii) over (i), across the two builds. The +25 % line is read only
	// when the baseline was passed in; the plan, n_dead_tup and the lock-wait sample are attached
	// either way.
	if raw := os.Getenv("SENTINEL_M1_BASELINE_P95_US"); raw != "" {
		baseP95 := time.Duration(m2EnvInt(t, "SENTINEL_M1_BASELINE_P95_US", 0)) * time.Microsecond
		delta := m2DeltaPercent(baseP95, currentP95)
		if delta > 25 {
			t.Logf("FINDING (§13 M1): (iii) current p95 %d µs is %+.1f%% over (i) baseline p95 %d µs (the -tags m1_norelease run, same inputs, one constant flipped) — above the +25%% line; the upsert's plan, n_dead_tup and the lock-wait sample follow",
				currentP95.Microseconds(), delta, baseP95.Microseconds())
		} else {
			t.Logf("(iii) current p95 %d µs is %+.1f%% over (i) baseline p95 %d µs (the -tags m1_norelease run, same inputs, one constant flipped) — within the +25%% line; the upsert's plan, n_dead_tup and the lock-wait sample follow for the record",
				currentP95.Microseconds(), delta, baseP95.Microseconds())
		}
	} else {
		t.Log("(iii) over (i): run `go test -tags m1_norelease` with the same SENTINEL_M1_* first and pass its p95 as SENTINEL_M1_BASELINE_P95_US")
	}
	// §13's secondary number: (iv) over (iii) — the two reads, on the same binary.
	t.Logf("(iv) resolved p95 %d µs is %+.1f%% over (iii) current p95 %d µs — the two step-0 reads; reported, not bounded",
		resolvedP95.Microseconds(), m2DeltaPercent(currentP95, resolvedP95), currentP95.Microseconds())

	t.Log("\n" + m1ExplainUpsert(t, ctx, pool, runs[1]))
	t.Log("\n" + m1DeadTuples(t, ctx, pool))
	t.Log("\n" + m1LockWaitSample(t, ctx, pool))
}

// m1ReleaseWriteCompiledIn stores one probe event carrying a release and reports whether step 4c
// wrote its `releases` row — the runtime signature of store's writeRelease constant, which the test
// package cannot see. The probe's project is cleaned up with the test.
func m1ReleaseWriteCompiledIn(t *testing.T, ctx context.Context, s store.IssueStore, pool *pgxpool.Pool) bool {
	t.Helper()
	projectID := eidSeedProject(t, pool, "m1rel-probe")
	occ := releaseOccurrence("v0.0.1-probe", time.Now().UTC())
	issue := eidIssueIn(projectID, "production", "fp-m1rel-probe")
	_, stored, err := s.StoreEvent(ctx, issue, occ, "v0.0.1-probe")
	require.NoError(t, err)
	require.True(t, stored)
	return releaseRowOf(t, ctx, pool, projectID, "v0.0.1-probe").found
}

// m1Version is the version event i carries under a shape: the one version for the single-version
// shapes, `v1.<i % versions>.0` for the spread.
func m1Version(shape m1Shape, i int) string {
	if shape.resolvedInX {
		return "v3.0.0" // V — the incoming version, first seen BEFORE X
	}
	if shape.versions <= 1 {
		return "v2.0.0"
	}
	return fmt.Sprintf("v1.%d.0", i%shape.versions)
}

// m1Drive seeds a project for one shape, declares `production` (so the registry touch matches one
// row, as in a real deployment), drives `events` StoreEvent calls sequentially on one goroutine —
// one call at a time, so a sample is one transaction's wall clock and nothing else — and then
// asserts the `releases` table holds exactly what the inputs predict (expectReleases) or nothing at
// all (the m1_norelease build).
//
// Event i carries fingerprint i % shape.fingerprints, the version m1Version gives it, and the
// timestamp t0 + i seconds — t0 six hours ago, so the max timestamp stays in the past and equals
// t0 + (events − 1) s, which is what the (iii) last_seen gate reads. Run (iv)'s pre-phase (untimed)
// ingests V once per fingerprint at t0 − 3 h, X once at t0 − 2 h, then resolves the 1,000 issues in
// X with resolved_at = now().
func m1Drive(t *testing.T, ctx context.Context, s store.IssueStore, pool *pgxpool.Pool, shape m1Shape, events int, expectReleases bool) m1Run {
	t.Helper()
	tag := strings.Fields(shape.label)[0]
	projectID := eidSeedProject(t, pool, "m1rel-"+tag)
	_, err := pool.Exec(ctx,
		`INSERT INTO project_environments (project_id, name, display_name) VALUES ($1, 'production', 'Production')`,
		projectID)
	require.NoError(t, err)

	fps := make([]string, shape.fingerprints)
	for i := range fps {
		fps[i] = fmt.Sprintf("fp-m1rel-%d", i) // the run's project scopes it
	}
	t0 := time.Now().UTC().Add(-6 * time.Hour).Truncate(time.Second)

	run := m1Run{shape: shape, projectID: projectID, hotVersion: m1Version(shape, 0)}

	if shape.resolvedInX {
		// Decision 46's pre-phase: V first (one occurrence per fingerprint creates the 1,000 issues
		// and the V row with first_seen = t0 − 3 h), then X (a separate fingerprint, so X's row is
		// first seen at t0 − 2 h > V's), then the resolution in X — the dashboard's write, verbatim
		// in effect: status, resolved_in_version, resolved_at, resolved_by*, no snooze columns.
		for i, fp := range fps {
			occ := releaseOccurrence("v3.0.0", t0.Add(-3*time.Hour).Add(time.Duration(i)*time.Second))
			occ.EventID = fmt.Sprintf("evt-m1rel-%s-pre-%d", tag, i)
			_, stored, err := s.StoreEvent(ctx, eidIssueIn(projectID, "production", fp), occ, "v3.0.0")
			require.NoError(t, err, "%s pre-phase V event %d", shape.label, i)
			require.True(t, stored, "%s pre-phase V event %d", shape.label, i)
		}
		occX := releaseOccurrence("v3.1.0", t0.Add(-2*time.Hour))
		occX.EventID = "evt-m1rel-" + tag + "-pre-x"
		_, stored, err := s.StoreEvent(ctx, eidIssueIn(projectID, "production", "fp-m1rel-x"), occX, "v3.1.0")
		require.NoError(t, err)
		require.True(t, stored)
		rowV, rowX := releaseRowOf(t, ctx, pool, projectID, "v3.0.0"), releaseRowOf(t, ctx, pool, projectID, "v3.1.0")
		require.True(t, rowV.found && rowX.found, "%s: both V and X rows must exist before the drive", shape.label)
		require.True(t, rowV.firstSeen.Before(rowX.firstSeen), "%s: V (%s) must be first seen before X (%s)", shape.label, rowV.firstSeen, rowX.firstSeen)
		tagRes, err := pool.Exec(ctx,
			`UPDATE issues SET status = 'resolved', resolved_in_version = 'v3.1.0', resolved_at = now(),
			        resolved_by_type = 'user', resolved_by = 'm1-driver'
			  WHERE project_id = $1 AND fingerprint <> 'fp-m1rel-x'`, projectID)
		require.NoError(t, err)
		require.EqualValues(t, shape.fingerprints, tagRes.RowsAffected(), "%s: every issue resolved in X", shape.label)
	}

	samples := make([]time.Duration, 0, events)
	for i := 0; i < events; i++ {
		version := m1Version(shape, i)
		at := t0.Add(time.Duration(i) * time.Second)
		occ := releaseOccurrence(version, at)
		occ.EventID = fmt.Sprintf("evt-m1rel-%s-%d", tag, i)
		issue := eidIssueIn(projectID, "production", fps[i%shape.fingerprints])

		start := time.Now()
		_, stored, err := s.StoreEvent(ctx, issue, occ, version)
		samples = append(samples, time.Since(start))

		require.NoError(t, err, "%s event %d", shape.label, i)
		require.True(t, stored, "%s event %d: a fresh event_id must store", shape.label, i)
		run.maxCreatedAt = at
	}
	sort.Slice(samples, func(a, b int) bool { return samples[a] < samples[b] })
	run.samples = samples

	require.NoError(t, pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM issues WHERE project_id = $1)::int,
		       (SELECT count(*) FROM error_occurrences o JOIN issues i ON i.id = o.issue_id WHERE i.project_id = $1)::int,
		       (SELECT count(*) FROM releases WHERE project_id = $1)::int,
		       (SELECT coalesce(sum(occurrence_count), 0) FROM releases WHERE project_id = $1)::bigint,
		       (SELECT coalesce(sum(regression_count), 0) FROM issues WHERE project_id = $1)::bigint,
		       (SELECT count(*) FROM issues WHERE project_id = $1 AND status = 'resolved' AND resolved_in_version = 'v3.1.0')::int`,
		projectID).Scan(&run.issues, &run.occurrences, &run.releaseRows, &run.releaseSum, &run.regressions, &run.stillResolvd))

	// Correctness before timing.
	expectedIssues, expectedOccurrences := shape.fingerprints, events
	if shape.resolvedInX {
		expectedIssues, expectedOccurrences = shape.fingerprints+1, events+shape.fingerprints+1 // + X's issue; + the pre-phase
	}
	require.Equal(t, expectedIssues, run.issues, "%s: one issue per fingerprint", shape.label)
	require.Equal(t, expectedOccurrences, run.occurrences, "%s: every event stored an occurrence", shape.label)
	if !expectReleases {
		require.Zero(t, run.releaseRows, "%s: the m1_norelease build must write no releases row", shape.label)
		return run
	}

	switch {
	case shape.resolvedInX:
		// Decision 46's gate: zero regressions, every issue still resolved in X, and V's row bumped
		// once per event on top of its pre-phase count.
		require.Zero(t, run.regressions, "%s (decision 46): regression_count must be unchanged on all %d issues — a non-zero count means the run measured the regression arm, not the two reads", shape.label, shape.fingerprints)
		require.Equal(t, shape.fingerprints, run.stillResolvd, "%s: every issue still `resolved` in X after the drive", shape.label)
		require.Equal(t, 2, run.releaseRows, "%s: exactly V and X", shape.label)
		rowV := releaseRowOf(t, ctx, pool, projectID, "v3.0.0")
		require.EqualValues(t, events+shape.fingerprints, rowV.occurrenceCount, "%s: V's occurrence_count = the pre-phase + the drive", shape.label)
		require.True(t, rowV.lastSeen.Equal(run.maxCreatedAt), "%s: V's last_seen %s = the max event timestamp %s", shape.label, rowV.lastSeen, run.maxCreatedAt)
	case shape.versions <= 1:
		// §13's (iii) gate: one row, occurrence_count = the events stored, last_seen = the max timestamp.
		require.Equal(t, 1, run.releaseRows, "%s: exactly one releases row", shape.label)
		row := releaseRowOf(t, ctx, pool, projectID, run.hotVersion)
		require.True(t, row.found)
		require.EqualValues(t, events, row.occurrenceCount, "%s: occurrence_count equals the events stored", shape.label)
		require.True(t, row.lastSeen.Equal(run.maxCreatedAt), "%s: last_seen %s = the max event timestamp %s", shape.label, row.lastSeen, run.maxCreatedAt)
		require.True(t, row.firstSeen.Equal(t0), "%s: first_seen %s = the first event's timestamp %s", shape.label, row.firstSeen, t0)
	default:
		require.Equal(t, shape.versions, run.releaseRows, "%s: one releases row per version", shape.label)
		require.EqualValues(t, events, run.releaseSum, "%s: sum(occurrence_count) equals the events stored", shape.label)
		// Every row, exactly: version i % versions is hit once per event that carries it.
		rows, err := pool.Query(ctx, `SELECT version, occurrence_count FROM releases WHERE project_id = $1`, projectID)
		require.NoError(t, err)
		defer rows.Close()
		for rows.Next() {
			var version string
			var count int64
			require.NoError(t, rows.Scan(&version, &count))
			var k int
			_, err := fmt.Sscanf(version, "v1.%d.0", &k)
			require.NoError(t, err, "%s: unexpected version %q", shape.label, version)
			require.EqualValues(t, m1SpreadCount(events, shape.versions, k), count, "%s: occurrence_count of %s", shape.label, version)
		}
		require.NoError(t, rows.Err())
	}
	return run
}

// m1SpreadCount is how many of `events` events carry version k when event i carries i % versions:
// events / versions, plus one for the k below the remainder.
func m1SpreadCount(events, versions, k int) int {
	n := events / versions
	if k < events%versions {
		n++
	}
	return n
}

// m1Report is §13's side-by-side table: p50 / p95 / p99 / max per call in µs per run, each run's
// p95 delta against (iii) current when it is present, and the row counts that prove what each run
// actually wrote — including run (iv)'s regression count, which must read 0.
func m1Report(runs []m1Run, events int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "§13 M1 (sentinel-releases-snooze, labelled fallback) — release_store_event_ms: store.StoreEvent wall clock per call, %d events per run, id pool 0, bucket write on, sequential, pgx pool only\n", events)
	fmt.Fprintf(&b, "%-15s %10s %10s %10s %10s %14s %8s %13s %15s %12s\n", "run", "p50 µs", "p95 µs", "p99 µs", "max µs", "p95 vs (iii)", "issues", "releases rows", "sum(occ_count)", "regressions")
	var base time.Duration
	for _, r := range runs {
		if strings.HasPrefix(r.shape.label, "(iii)") {
			base = m2Percentile(r.samples, 0.95)
		}
	}
	for _, r := range runs {
		p95 := m2Percentile(r.samples, 0.95)
		vs := "n/a"
		if base > 0 {
			vs = fmt.Sprintf("%+.1f%%", m2DeltaPercent(base, p95))
		}
		fmt.Fprintf(&b, "%-15s %10d %10d %10d %10d %14s %8d %13d %15d %12d\n",
			r.shape.label,
			m2Percentile(r.samples, 0.50).Microseconds(),
			p95.Microseconds(),
			m2Percentile(r.samples, 0.99).Microseconds(),
			r.samples[len(r.samples)-1].Microseconds(),
			vs,
			r.issues, r.releaseRows, r.releaseSum, r.regressions)
	}
	return b.String()
}

// m1ExplainUpsert attaches the plan §13 asks for beside a +25 % finding: EXPLAIN (ANALYZE, BUFFERS)
// of §4.3's step-4c upsert exactly as store.go issues it, against run (iii)'s one hot row (the DO
// UPDATE arm every production event pays) and against a never-seen version (the INSERT arm — a
// first sighting), each inside a transaction that is rolled back so the run's rows stay exactly as
// the loop wrote them.
func m1ExplainUpsert(t *testing.T, ctx context.Context, pool *pgxpool.Pool, run m1Run) string {
	t.Helper()
	const explain = `
		EXPLAIN (ANALYZE, BUFFERS)
		INSERT INTO releases (project_id, version, first_seen, last_seen, occurrence_count)
		VALUES ($1, $2, $3, $3, 1)
		ON CONFLICT (project_id, version) DO UPDATE SET
		  last_seen = GREATEST(releases.last_seen, EXCLUDED.last_seen),
		  occurrence_count = releases.occurrence_count + 1
		RETURNING (xmax = 0) AS inserted`

	var b strings.Builder
	for _, c := range []struct{ label, version string }{
		{"HOT row (DO UPDATE arm — run (iii)'s every event)", run.hotVersion},
		{"NEW version (INSERT arm — a first sighting)", "v2.0.0-explain-new"},
	} {
		tx, err := pool.Begin(ctx)
		require.NoError(t, err)
		rows, err := tx.Query(ctx, explain, run.projectID, c.version, time.Now().UTC())
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

// m1DeadTuples reads pg_stat_user_tables for `releases` after the runs — the dead-tuple count §13
// attaches beside the finding (every DO UPDATE on the hot row leaves the old version for
// autovacuum). The cumulative stats are flushed by each backend shortly after its transaction ends;
// the one-second pause makes the read reflect the loop that just finished.
func m1DeadTuples(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	time.Sleep(time.Second)
	var live, dead, ins, upd, hotUpd int64
	var vacuumed, autovacuumed *time.Time
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT n_live_tup, n_dead_tup, n_tup_ins, n_tup_upd, n_tup_hot_upd, last_vacuum, last_autovacuum
		  FROM pg_stat_user_tables WHERE relname = 'releases'`).
		Scan(&live, &dead, &ins, &upd, &hotUpd, &vacuumed, &autovacuumed))
	fmtTime := func(p *time.Time) string {
		if p == nil {
			return "never"
		}
		return p.UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf("pg_stat_user_tables for releases after the runs: n_live_tup=%d n_dead_tup=%d n_tup_ins=%d n_tup_upd=%d n_tup_hot_upd=%d last_vacuum=%s last_autovacuum=%s",
		live, dead, ins, upd, hotUpd, fmtTime(vacuumed), fmtTime(autovacuumed))
}

// m1LockWaitSample is the pg_stat_activity lock-wait sample §13 attaches beside a finding: every
// backend currently waiting on a lock, by wait event. A sequential driver on one goroutine holds
// no second transaction open, so this reads empty by construction — attached so the reviewer sees
// that the p95 is the statement's own cost and not a wait behind another writer.
func m1LockWaitSample(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	rows, err := pool.Query(ctx, `
		SELECT wait_event_type, wait_event, state, count(*)::int
		  FROM pg_stat_activity
		 WHERE wait_event_type = 'Lock'
		 GROUP BY 1, 2, 3 ORDER BY 4 DESC`)
	require.NoError(t, err)
	defer rows.Close()
	var b strings.Builder
	b.WriteString("pg_stat_activity lock-wait sample after the runs (wait_event_type = 'Lock'):")
	n := 0
	for rows.Next() {
		var typ, event, state string
		var count int
		require.NoError(t, rows.Scan(&typ, &event, &state, &count))
		fmt.Fprintf(&b, "\n  %s/%s state=%s backends=%d", typ, event, state, count)
		n++
	}
	require.NoError(t, rows.Err())
	if n == 0 {
		b.WriteString(" none (no backend waiting on a lock — the driver is sequential)")
	}
	return b.String()
}

// The shape arithmetic the report rests on, pinned without infrastructure so a wrong version
// placement or a wrong spread prediction cannot hide behind a 40,000-call run.
func TestReleaseM1Shapes_VersionPlacement(t *testing.T) {
	spread := m1Shape{label: "(ii) spread", fingerprints: 1000, versions: m1SpreadVersions}
	require.Equal(t, "v1.0.0", m1Version(spread, 0))
	require.Equal(t, "v1.49.0", m1Version(spread, 49))
	require.Equal(t, "v1.0.0", m1Version(spread, 50), "event 50 wraps to the first version")
	require.Equal(t, "v1.7.0", m1Version(spread, 10007))
	require.Equal(t, "v2.0.0", m1Version(m1Shape{label: "(iii) current", fingerprints: 1000, versions: 1}, 12345), "one version, every event")
	require.Equal(t, "v3.0.0", m1Version(m1Shape{label: "(iv) resolved", fingerprints: 1000, versions: 1, resolvedInX: true}, 12345), "run (iv) always carries V")

	require.Equal(t, 200, m1SpreadCount(10000, 50, 0), "10,000 events over 50 versions: 200 each")
	require.Equal(t, 200, m1SpreadCount(10000, 50, 49))
	require.Equal(t, 3, m1SpreadCount(103, 50, 2), "103 events: versions 0..2 get one extra")
	require.Equal(t, 2, m1SpreadCount(103, 50, 3))
	total := 0
	for k := 0; k < 50; k++ {
		total += m1SpreadCount(103, 50, k)
	}
	require.Equal(t, 103, total, "the per-version counts sum to the events")
}
