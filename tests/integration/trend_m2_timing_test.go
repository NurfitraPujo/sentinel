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

// sentinel-issue-trend §13 M2, the sibling of affected_m2_timing_test.go's loop: the cost of §4.3's
// bucket upsert (StoreEvent step 4b) is taken AT THE STORE BOUNDARY — a Go loop driving
// store.StoreEvent against the suite's Postgres with only a pgx pool (no ingestor, no NATS) — and
// reported as wall-clock p95 per call in µs, side by side for the four shapes §13 names, every one
// of them with user/tenant id pool 0 so no affected-rollup statement runs and the bucket write is
// the only variable:
//
//	(i)   baseline — shape (iii)'s inputs with the bucket write COMPILED OUT (decision 20:
//	      `go test -tags m2_nobucket`, which flips store's writeOccurrenceBucket constant to false)
//	(ii)  spread   — 1,000 fingerprints, timestamps spread over 24 hours (many bucket rows, each cold)
//	(iii) steady   — 1,000 fingerprints, one hour (one bucket row per issue, each hit events/1,000 times)
//	(iv)  storm    — one fingerprint, one hour: ONE hot bucket row (the intake's runaway loop, flow I8)
//
// Run (i) is a separate `go test` invocation because the constant is compile-time: the same test,
// same inputs, same database as (iii) with one constant folded away. This test detects which build
// it is in at runtime — it stores one probe event and looks for its bucket row — so the bucket build
// runs (ii)–(iv) and the m2_nobucket build runs (i) only, and neither can silently run the other's
// shapes. Pass the (i) run's p95 back to the bucket build as SENTINEL_M2_BASELINE_P95_US=<µs> and it
// prints the (iii)-over-(i) delta §13 calls the secondary number.
//
// This is a MEASUREMENT, not a gate (§13: "report all four p95s side by side; do not gate"). It runs
// only when SENTINEL_M2_TIMING=1 and never fails on a number: the one line it reads, (iv) over (iii)
// above +50 %, is CALLED OUT as a finding with the upsert's plan and the table's dead-tuple count
// (pg_stat_user_tables.n_dead_tup) attached — the number a cap follow-up hangs on (decisions 9,
// 20). It DOES fail on correctness, as the affected-users loop does: every event must store, every
// run's bucket rows must be exactly the (issue, UTC hour, count) set the inputs predict, sum(count)
// must equal the events stored, and after (iv) there must be exactly ONE bucket row whose count
// equals the events stored (flow I8) — a timing of a write that did not happen is not a timing.
//
// Overrides: SENTINEL_M2_EVENTS (default 10,000; §13's floor) and SENTINEL_M2_TREND_FINGERPRINTS
// (default 1,000; §13's shape). Same venue caveat as every §13 row: a local Postgres is the LABELLED
// FALLBACK (decision 15) and these numbers are evidence about plan shape, never a criterion met.
const defaultTrendM2Fingerprints = 1000

// trendM2Shape is one of §13's four shapes: how many fingerprints the events cycle over and how the
// event timestamps are placed.
type trendM2Shape struct {
	label        string
	fingerprints int
	spreadHours  int // 1 = every event in one hour; 24 = event i lands in hour i % 24
}

// trendM2Run is one shape's outcome: the sorted per-call samples and what the database says landed.
type trendM2Run struct {
	shape      trendM2Shape
	samples    []time.Duration // sorted ascending after the run
	issues     int
	bucketRows int
	bucketSum  int
	projectID  string
	hotIssueID string    // the issue of fingerprint 0 — the storm's one issue, the plan's target
	hour       time.Time // the one-hour shapes' bucket_start
}

func TestIssueTrend_M2StoreEventTiming(t *testing.T) {
	if os.Getenv("SENTINEL_M2_TIMING") != "1" {
		t.Skip("SENTINEL_M2_TIMING is unset — the §13 M2 timing loop runs only when asked for")
	}
	events := m2EnvInt(t, "SENTINEL_M2_EVENTS", defaultM2Events)
	fingerprints := m2EnvInt(t, "SENTINEL_M2_TREND_FINGERPRINTS", defaultTrendM2Fingerprints)

	s, pool := newEIDStoreAndPoolFromEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	bucketBuild := trendM2BucketWriteCompiledIn(t, ctx, s, pool)

	steady := trendM2Shape{label: "(iii) steady", fingerprints: fingerprints, spreadHours: 1}
	if !bucketBuild {
		// The m2_nobucket build: shape (i) is shape (iii)'s inputs, and the ONLY thing it may
		// report is that no bucket was written — anything else means the tag did not take.
		base := trendM2Drive(t, ctx, s, pool, trendM2Shape{label: "(i) baseline", fingerprints: fingerprints, spreadHours: 1}, events, false)
		t.Log("\n" + trendM2Report([]trendM2Run{base}, events))
		t.Logf("(i) baseline p95 = %d µs — built with -tags m2_nobucket, zero bucket rows written; pass SENTINEL_M2_BASELINE_P95_US=%d to the bucket build for the (iii)-over-(i) delta",
			m2Percentile(base.samples, 0.95).Microseconds(), m2Percentile(base.samples, 0.95).Microseconds())
		return
	}

	shapes := []trendM2Shape{
		{label: "(ii) spread", fingerprints: fingerprints, spreadHours: 24},
		steady,
		{label: "(iv) storm", fingerprints: 1, spreadHours: 1},
	}
	runs := make([]trendM2Run, 0, len(shapes))
	for _, shape := range shapes {
		runs = append(runs, trendM2Drive(t, ctx, s, pool, shape, events, true))
	}
	t.Log("\n" + trendM2Report(runs, events))

	steadyP95 := m2Percentile(runs[1].samples, 0.95)
	stormP95 := m2Percentile(runs[2].samples, 0.95)

	// §13's primary number: (iv) over (iii) on the same binary. The plan and the dead-tuple count
	// are attached either way — the reviewer reads the three together; only the FINDING line changes.
	delta := m2DeltaPercent(steadyP95, stormP95)
	if delta > 50 {
		t.Logf("FINDING (§13 M2): (iv) storm p95 is %+.1f%% over (iii) steady — above the +50%% line; the upsert's plan and n_dead_tup follow", delta)
	} else {
		t.Logf("(iv) storm p95 is %+.1f%% over (iii) steady — within the +50%% line; the upsert's plan and n_dead_tup follow for the record", delta)
	}
	t.Log("\n" + trendM2ExplainUpsert(t, ctx, pool, runs[2]))
	t.Log("\n" + trendM2DeadTuples(t, ctx, pool))

	// §13's secondary number: (iii) over (i), across the two builds.
	if raw := os.Getenv("SENTINEL_M2_BASELINE_P95_US"); raw != "" {
		baseP95 := time.Duration(m2EnvInt(t, "SENTINEL_M2_BASELINE_P95_US", 0)) * time.Microsecond
		t.Logf("(iii) steady p95 %d µs is %+.1f%% over (i) baseline p95 %d µs (the -tags m2_nobucket run, same inputs, one constant flipped) — reported, not bounded",
			steadyP95.Microseconds(), m2DeltaPercent(baseP95, steadyP95), baseP95.Microseconds())
	} else {
		t.Log("(iii) over (i): run `go test -tags m2_nobucket` with the same SENTINEL_M2_* first and pass its p95 as SENTINEL_M2_BASELINE_P95_US")
	}
}

// trendM2BucketWriteCompiledIn stores one probe event and reports whether step 4b wrote its bucket
// row — the runtime signature of store's writeOccurrenceBucket constant, which the test package
// cannot see. The probe's project is cleaned up with the test.
func trendM2BucketWriteCompiledIn(t *testing.T, ctx context.Context, s store.IssueStore, pool *pgxpool.Pool) bool {
	t.Helper()
	projectID := eidSeedProject(t, pool, "m2trend-probe")
	occ := eidOccurrenceIn("production", "evt-m2trend-probe")
	issue := eidIssueIn(projectID, "production", "fp-m2trend-probe")
	_, stored, err := s.StoreEvent(ctx, issue, occ, "")
	require.NoError(t, err)
	require.True(t, stored)
	var rows int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*)::int FROM issue_occurrence_buckets WHERE issue_id = $1`, issue.ID).Scan(&rows))
	return rows == 1
}

// trendM2Drive seeds a project for one shape, declares `production` (so the registry touch matches
// one row, as in a real deployment), drives `events` StoreEvent calls sequentially on one goroutine
// — one call at a time, so a sample is one transaction's wall clock and nothing else — and then
// asserts the bucket table holds exactly the (issue, UTC hour, count) set the inputs predict
// (expectBuckets) or nothing at all (the m2_nobucket build).
//
// Event i carries fingerprint i % shape.fingerprints and, for spreadHours = 24, the timestamp
// hour0 + (i % 24) h + 15 min; for spreadHours = 1 every event carries hour0 + 30 min. hour0 is
// pinned once per run so an hour boundary crossed mid-run cannot split a "one hour" shape in two.
func trendM2Drive(t *testing.T, ctx context.Context, s store.IssueStore, pool *pgxpool.Pool, shape trendM2Shape, events int, expectBuckets bool) trendM2Run {
	t.Helper()
	projectID := eidSeedProject(t, pool, "m2trend-"+strings.Fields(shape.label)[0])
	_, err := pool.Exec(ctx,
		`INSERT INTO project_environments (project_id, name, display_name) VALUES ($1, 'production', 'Production')`,
		projectID)
	require.NoError(t, err)

	fps := make([]string, shape.fingerprints)
	for i := range fps {
		fps[i] = fmt.Sprintf("fp-m2trend-%d", i) // the run's project scopes it
	}
	hour0 := time.Now().UTC().Truncate(time.Hour).Add(-time.Duration(shape.spreadHours-1) * time.Hour)

	type bucketKey struct {
		fp   int
		hour time.Time
	}
	expected := make(map[bucketKey]int, shape.fingerprints*shape.spreadHours)

	samples := make([]time.Duration, 0, events)
	for i := 0; i < events; i++ {
		fp := i % shape.fingerprints
		at := hour0.Add(30 * time.Minute)
		if shape.spreadHours > 1 {
			at = hour0.Add(time.Duration(i%shape.spreadHours)*time.Hour + 15*time.Minute)
		}
		expected[bucketKey{fp: fp, hour: at.Truncate(time.Hour)}]++

		occ := eidOccurrenceIn("production", fmt.Sprintf("evt-m2trend-%s-%d", strings.Fields(shape.label)[0], i))
		occ.CreatedAt = at
		issue := eidIssueIn(projectID, "production", fps[fp])

		start := time.Now()
		_, stored, err := s.StoreEvent(ctx, issue, occ, "")
		samples = append(samples, time.Since(start))

		require.NoError(t, err, "%s event %d", shape.label, i)
		require.True(t, stored, "%s event %d: a fresh event_id must store", shape.label, i)
	}
	sort.Slice(samples, func(a, b int) bool { return samples[a] < samples[b] })

	run := trendM2Run{shape: shape, samples: samples, projectID: projectID, hour: hour0.Add(30 * time.Minute).Truncate(time.Hour)}
	var occurrences int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM issues WHERE project_id = $1)::int,
		       (SELECT count(*) FROM error_occurrences o JOIN issues i ON i.id = o.issue_id WHERE i.project_id = $1)::int,
		       (SELECT count(*) FROM issue_occurrence_buckets b JOIN issues i ON i.id = b.issue_id WHERE i.project_id = $1)::int,
		       (SELECT coalesce(sum(b.count), 0) FROM issue_occurrence_buckets b JOIN issues i ON i.id = b.issue_id WHERE i.project_id = $1)::int`,
		projectID).Scan(&run.issues, &occurrences, &run.bucketRows, &run.bucketSum))
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT id::text FROM issues WHERE project_id = $1 AND environment = 'production' AND fingerprint = $2`,
		projectID, fps[0]).Scan(&run.hotIssueID))

	// Correctness before timing.
	require.Equal(t, shape.fingerprints, run.issues, "%s: one issue per fingerprint", shape.label)
	require.Equal(t, events, occurrences, "%s: every event stored an occurrence", shape.label)
	if !expectBuckets {
		require.Zero(t, run.bucketRows, "%s: the m2_nobucket build must write no bucket row", shape.label)
		return run
	}
	require.Equal(t, len(expected), run.bucketRows, "%s: one bucket row per distinct (issue, UTC hour)", shape.label)
	require.Equal(t, events, run.bucketSum, "%s: sum(count) over the run's buckets equals the events stored", shape.label)

	// Every row, exactly: (fingerprint, bucket_start, count) as the inputs predict — a run that
	// quietly skipped or double-counted an upsert cannot report a flattering p95.
	rows, err := pool.Query(ctx, `
		SELECT i.fingerprint, b.bucket_start, b.count
		  FROM issue_occurrence_buckets b JOIN issues i ON i.id = b.issue_id
		 WHERE i.project_id = $1`, projectID)
	require.NoError(t, err)
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var fp string
		var start time.Time
		var count int
		require.NoError(t, rows.Scan(&fp, &start, &count))
		var fpIndex int
		_, err := fmt.Sscanf(fp, "fp-m2trend-%d", &fpIndex)
		require.NoError(t, err)
		want, ok := expected[bucketKey{fp: fpIndex, hour: start.UTC()}]
		require.True(t, ok, "%s: unexpected bucket row (%s, %s)", shape.label, fp, start.UTC().Format(time.RFC3339))
		require.Equal(t, want, count, "%s: bucket (%s, %s) count", shape.label, fp, start.UTC().Format(time.RFC3339))
		seen++
	}
	require.NoError(t, rows.Err())
	require.Equal(t, len(expected), seen)

	if shape.fingerprints == 1 && shape.spreadHours == 1 {
		// Flow I8, stated on its own so the storm's gate reads as §13 wrote it.
		require.Equal(t, 1, run.bucketRows, "%s (I8): exactly one bucket row", shape.label)
		require.Equal(t, events, run.bucketSum, "%s (I8): its count equals the events stored", shape.label)
	}
	return run
}

// trendM2Report is §13's side-by-side table: p50 / p95 / p99 / max per call in µs per shape, each
// shape's p95 delta against (iii) steady when it is present, and the row counts that prove what
// each run actually wrote.
func trendM2Report(runs []trendM2Run, events int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "§13 M2 (sentinel-issue-trend, labelled fallback) — store.StoreEvent wall clock per call, %d events per shape, id pool 0 throughout, sequential, pgx pool only\n", events)
	fmt.Fprintf(&b, "%-14s %10s %10s %10s %10s %14s %8s %12s %11s\n", "shape", "p50 µs", "p95 µs", "p99 µs", "max µs", "p95 vs (iii)", "issues", "bucket rows", "sum(count)")
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
		fmt.Fprintf(&b, "%-14s %10d %10d %10d %10d %14s %8d %12d %11d\n",
			r.shape.label,
			m2Percentile(r.samples, 0.50).Microseconds(),
			p95.Microseconds(),
			m2Percentile(r.samples, 0.99).Microseconds(),
			r.samples[len(r.samples)-1].Microseconds(),
			vs,
			r.issues, r.bucketRows, r.bucketSum)
	}
	return b.String()
}

// trendM2ExplainUpsert attaches the plan §13 asks for beside a +50 % finding: EXPLAIN (ANALYZE,
// BUFFERS) of §4.3's upsert exactly as store.go issues it, against the storm's one hot row (the DO
// UPDATE arm every storm event pays) and against a fresh hour (the INSERT arm), each inside a
// transaction that is rolled back so the run's rows stay exactly as the loop wrote them.
func trendM2ExplainUpsert(t *testing.T, ctx context.Context, pool *pgxpool.Pool, run trendM2Run) string {
	t.Helper()
	const explain = `
		EXPLAIN (ANALYZE, BUFFERS)
		INSERT INTO issue_occurrence_buckets (issue_id, bucket_start, count)
		VALUES ($1, $2, 1)
		ON CONFLICT (issue_id, bucket_start) DO UPDATE
		   SET count = issue_occurrence_buckets.count + 1`

	var b strings.Builder
	for _, c := range []struct {
		label string
		at    time.Time
	}{
		{"HOT row (DO UPDATE arm — the storm's every event)", run.hour},
		{"NEW hour (INSERT arm — the first event of an (issue, hour))", run.hour.Add(-48 * time.Hour)},
	} {
		tx, err := pool.Begin(ctx)
		require.NoError(t, err)
		rows, err := tx.Query(ctx, explain, run.hotIssueID, c.at)
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

// trendM2DeadTuples reads pg_stat_user_tables for the bucket table after the runs — the dead-tuple
// count §13 attaches beside the finding (every upsert on an existing row leaves the old version for
// autovacuum). The cumulative stats are flushed by each backend shortly after its transaction ends;
// the one-second pause makes the read reflect the loop that just finished.
func trendM2DeadTuples(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	time.Sleep(time.Second)
	var live, dead, ins, upd, hotUpd int64
	var vacuumed, autovacuumed *time.Time
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT n_live_tup, n_dead_tup, n_tup_ins, n_tup_upd, n_tup_hot_upd, last_vacuum, last_autovacuum
		  FROM pg_stat_user_tables WHERE relname = 'issue_occurrence_buckets'`).
		Scan(&live, &dead, &ins, &upd, &hotUpd, &vacuumed, &autovacuumed))
	fmtTime := func(p *time.Time) string {
		if p == nil {
			return "never"
		}
		return p.UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf("pg_stat_user_tables for issue_occurrence_buckets after the runs: n_live_tup=%d n_dead_tup=%d n_tup_ins=%d n_tup_upd=%d n_tup_hot_upd=%d last_vacuum=%s last_autovacuum=%s",
		live, dead, ins, upd, hotUpd, fmtTime(vacuumed), fmtTime(autovacuumed))
}

// The shape arithmetic the report rests on, pinned without infrastructure: the (iii)/(iv) shapes
// share one hour, (ii) walks 24, and a run's expected bucket set is what the loop computes from
// the same formula — so the assertion in trendM2Drive is against the inputs, not a re-derivation.
func TestTrendM2Shapes_HourPlacement(t *testing.T) {
	hour0 := time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC)
	oneHour := hour0.Add(30 * time.Minute).Truncate(time.Hour)
	require.Equal(t, hour0, oneHour, "a one-hour shape's every event truncates to hour0")
	for i := 0; i < 48; i++ {
		at := hour0.Add(time.Duration(i%24)*time.Hour + 15*time.Minute)
		require.Equal(t, hour0.Add(time.Duration(i%24)*time.Hour), at.Truncate(time.Hour), "event %d", i)
	}
}
