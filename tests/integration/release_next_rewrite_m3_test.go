package integration

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// sentinel-releases-snooze §13 M3 (decisions 15, 30, 35, 49): `release_next_rewrite_ms` — the §4.3
// post-commit fan-out for project A with 500 `next` issues on one new release, and the hourly
// catch-up once over the same 500, each timed and each with its plan pasted. It runs on the SEEDED
// COMPOSE DATABASE (decision 35): apps/dashboard-web/scripts/releases-snooze-seed.sql loaded
// immediately before, FORCE_TESTCONTAINERS unset so TestMain points POSTGRES_* at the compose
// Postgres — and it SKIPS ITSELF unless it finds the seed's organization, so the testcontainers
// suite (CI's, and every FORCE_TESTCONTAINERS=1 run) never sees it. It needs the compose dashboard
// too: the catch-up is driven through the REAL `POST /api/cron/retention` (cleanupRetainedData's
// last statement, retention.ts: catchUpNextReleases), never a copy of its SQL run from here.
//
// In §13's order, because the first take consumes the rows:
//
//  1. the fan-out — 470 `next` issues are inserted beside the seed's 30 and all 500 get
//     resolved_at = now − 12 h, AFTER every seeded release's first_seen (the newest is load − 1 day;
//     decision 49's instruction), so with or without the earliest-newer clause the new release is
//     the one they read; the driver ingests one never-seen version through ProcessorService with a
//     client clock of now − 1 h, and the fan-out rewrites exactly the 500 — asserted;
//  2. the driver resets the same 500 to `next` (resolved_at untouched);
//  3. the catch-up — the compose dashboard's retention sweep is POSTed once; its last statement
//     rewrites the same 500 to the EARLIEST release newer than each resolved_at, which is the new
//     version (the I19 rule) — asserted.
//
// Two controls sit beside the 500 through both takes and must be UNTOUCHED: a `next` issue resolved
// AFTER the release's first_seen (now − 30 min > now − 1 h — the strict comparison), and the seed's
// 30 issues resolved in an EXPLICIT version (their (id, version) digest is compared before and after).
//
// Timing (§13: reported, not gated): for each take, EXPLAIN (ANALYZE, BUFFERS) of the statement AS
// THE CODE ISSUES IT — the fan-out's text is read out of processor_service.go's
// releaseNextRewriteQuery and the catch-up's out of retention.ts's catchUpNextReleases at test time,
// so the plan can never drift from the shipped statement — over the 500 rows inside a transaction
// that is ROLLED BACK (the rows stay `next` for the real take that follows), which gives the
// statement's own execution time; and the wall clock around the real call that carried it
// (ProcessEvent for the fan-out — the whole store + post-commit path; the cron POST for the
// catch-up — the whole sweep). Both plans must use idx_issues_project_resolved_next (§13's
// secondary) — asserted on the plan text. No retention cron runs on compose unless
// RETENTION_CRON_ENABLED=true (it ships off), and the compose processor writes nothing to project A
// unless someone ingests, so the seed's 30 `next` rows are still there for this driver.
//
// DASHBOARD_URL (default http://localhost:3000) and CRON_SECRET (default docker-compose.yml's
// dev value) name the compose dashboard. The seed is CONSUMED by a run: re-load it before the next.
const (
	rsSeedOrganizationID = "00000000-0000-4000-8000-0000000000c1"
	rsSeedProjectA       = "00000000-0000-4000-8000-0000000000c2"
	m3NextIssues         = 500 // §13: the seed's 30 plus 470 inserted by the driver
	m3SeedNextIssues     = 30
	m3DriverFingerprint  = "fp-rs-m3-driver" // the unrelated issue whose first occurrence carries the release
	m3ControlFingerprint = "fp-rs-m3-control-late"
)

func TestRelease_M3NextRewriteOnSeededCompose(t *testing.T) {
	svc, pool, _ := newServiceAndPoolFromEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	var seeded bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM organizations WHERE id = $1)`, rsSeedOrganizationID).Scan(&seeded))
	if !seeded {
		t.Skip("the releases-snooze seed's organization is absent — §13 M3 runs on the SEEDED compose database only: load apps/dashboard-web/scripts/releases-snooze-seed.sql and run with FORCE_TESTCONTAINERS unset (decision 35)")
	}

	// A previous aborted run's rows would inflate the counts; the seed's own 30 are the
	// precondition that says the seed is fresh (the take consumes them).
	m3DeleteDriverRows(t, ctx, pool)
	t.Cleanup(func() { m3DeleteDriverRows(t, context.Background(), pool) })
	var seedNext int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*)::int FROM issues WHERE project_id = $1 AND status = 'resolved' AND resolved_in_version = 'next' AND fingerprint LIKE 'fp-rs-a-%'`,
		rsSeedProjectA).Scan(&seedNext))
	require.Equal(t, m3SeedNextIssues, seedNext, "the seed's %d `next` issues must be present and unconsumed — re-run releases-snooze-seed.sql before this take", m3SeedNextIssues)

	now := time.Now().UTC().Truncate(time.Second)
	resolvedAt := now.Add(-12 * time.Hour)      // after every seeded release (newest: load − 1 day)
	releaseFirstSeen := now.Add(-1 * time.Hour) // the new release's client clock: > resolvedAt
	controlResolvedAt := now.Add(-30 * time.Minute)
	version := fmt.Sprintf("v4.0.0+m3.%d", now.Unix()) // never seen: the seed tops out at v3.8.3

	// The 470 driver issues (+ one occurrence each at last_seen, so retention's orphan rule leaves
	// them alone), the seed's 30 moved to the same resolved_at, and the late control.
	_, err := pool.Exec(ctx, `
		INSERT INTO issues (id, project_id, environment, fingerprint, message, error_class, issue_type, status,
		                    first_seen, last_seen, count, resolved_in_version, resolved_at, resolved_by_type, resolved_by)
		SELECT gen_random_uuid(), $1, 'production', 'fp-rs-m3-' || n, 'm3 driver issue #' || n, 'M3DriverError', 'system_error',
		       'resolved', $2::timestamptz - interval '2 days', $2::timestamptz - interval '2 hours', 1, 'next', $2, 'user', 'm3-driver'
		  FROM generate_series(1, $3::int) AS n`,
		rsSeedProjectA, resolvedAt, m3NextIssues-m3SeedNextIssues)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO issues (id, project_id, environment, fingerprint, message, error_class, issue_type, status,
		                    first_seen, last_seen, count, resolved_in_version, resolved_at, resolved_by_type, resolved_by)
		VALUES (gen_random_uuid(), $1, 'production', $2, 'm3 control: resolved after the release', 'M3DriverError', 'system_error',
		        'resolved', $3::timestamptz - interval '2 days', $3::timestamptz - interval '2 hours', 1, 'next', $3, 'user', 'm3-driver')`,
		rsSeedProjectA, m3ControlFingerprint, controlResolvedAt)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO error_occurrences (issue_id, environment, platform, release_version, created_at)
		SELECT id, environment, 'go', NULL, last_seen FROM issues WHERE project_id = $1 AND fingerprint LIKE 'fp-rs-m3-%'`,
		rsSeedProjectA)
	require.NoError(t, err)
	tag, err := pool.Exec(ctx,
		`UPDATE issues SET resolved_at = $2 WHERE project_id = $1 AND status = 'resolved' AND resolved_in_version = 'next' AND fingerprint LIKE 'fp-rs-a-%'`,
		rsSeedProjectA, resolvedAt)
	require.NoError(t, err)
	require.EqualValues(t, m3SeedNextIssues, tag.RowsAffected())
	require.Equal(t, m3NextIssues, m3CountNext(t, ctx, pool, resolvedAt), "precondition: exactly 500 `next` issues at resolved_at = now − 12 h")
	explicitBefore := m3ExplicitSnapshot(t, ctx, pool)

	var b strings.Builder
	fmt.Fprintf(&b, "§13 M3 (sentinel-releases-snooze, labelled fallback) — release_next_rewrite_ms on the seeded compose database, project A, %d `next` issues (the seed's %d + %d inserted), resolved_at = %s, new release %s first seen %s\n",
		m3NextIssues, m3SeedNextIssues, m3NextIssues-m3SeedNextIssues, resolvedAt.Format(time.RFC3339), version, releaseFirstSeen.Format(time.RFC3339))

	// ---- (1) the fan-out ------------------------------------------------------------------------
	fanoutSQL := m3ExtractGoConst(t, "releaseNextRewriteQuery")
	fanoutPlan := m3Explain(t, ctx, pool, fanoutSQL, rsSeedProjectA, version, releaseFirstSeen)
	require.Contains(t, fanoutPlan, "idx_issues_project_resolved_next", "§13 M3 secondary: the fan-out's plan must use idx_issues_project_resolved_next")

	ev := buildValidProtoEvent("unused-project-key")
	ev.ProjectId = rsSeedProjectA // resolved by id (store.ResolveProjectID), not by name
	ev.Environment = "production"
	ev.Fingerprint = m3DriverFingerprint
	ev.ReleaseVersion = version
	ev.EventId = "evt-rs-m3-" + uuid.New().String()
	ev.Timestamp = timestamppb.New(releaseFirstSeen)
	ev.Metadata = nil
	start := time.Now()
	processOK(t, ctx, svc, ev)
	fanoutWall := time.Since(start)

	row := releaseRowOf(t, ctx, pool, rsSeedProjectA, version)
	require.True(t, row.found, "the driver's occurrence must have inserted the release row")
	require.True(t, row.firstSeen.Equal(releaseFirstSeen), "first_seen %s must be the event's clock %s", row.firstSeen, releaseFirstSeen)
	fanoutRewritten := m3CountRewrittenTo(t, ctx, pool, version)
	require.Equal(t, m3NextIssues, fanoutRewritten, "§13 M3 primary: the fan-out must rewrite exactly the 500 `next` issues with resolved_at < first_seen")
	require.Zero(t, m3CountNext(t, ctx, pool, resolvedAt), "no eligible `next` issue may be left behind by the fan-out")
	m3AssertControlsUntouched(t, ctx, pool, explicitBefore, "after the fan-out")

	fmt.Fprintf(&b, "\n(1) fan-out — rows rewritten: %d (asserted = %d); wall clock around ProcessEvent (store + post-commit rewrite): %.3f ms\n", fanoutRewritten, m3NextIssues, float64(fanoutWall.Microseconds())/1000)
	fmt.Fprintf(&b, "    the statement as processor_service.go issues it (releaseNextRewriteQuery), EXPLAIN (ANALYZE, BUFFERS) over the 500 in a rolled-back transaction taken immediately before the real take:\n%s", m3Indent(fanoutPlan))

	// ---- (2) the reset --------------------------------------------------------------------------
	tag, err = pool.Exec(ctx,
		`UPDATE issues SET resolved_in_version = 'next' WHERE project_id = $1 AND status = 'resolved' AND resolved_in_version = $2`,
		rsSeedProjectA, version)
	require.NoError(t, err)
	require.EqualValues(t, m3NextIssues, tag.RowsAffected(), "the reset must put exactly the 500 back to `next`")
	require.Equal(t, m3NextIssues, m3CountNext(t, ctx, pool, resolvedAt))
	fmt.Fprintf(&b, "\n(2) reset — %d rows back to `next`, resolved_at unchanged\n", tag.RowsAffected())

	// ---- (3) the catch-up -----------------------------------------------------------------------
	catchupSQL := m3ExtractRetentionCatchUp(t)
	catchupPlan := m3Explain(t, ctx, pool, catchupSQL)
	require.Contains(t, catchupPlan, "idx_issues_project_resolved_next", "§13 M3 secondary: the catch-up's plan must use idx_issues_project_resolved_next")

	dashboardURL := strings.TrimRight(m3Env("DASHBOARD_URL", "http://localhost:3000"), "/")
	cronSecret := m3Env("CRON_SECRET", "dev-only-cron-secret-change-me")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, dashboardURL+"/api/cron/retention", nil)
	require.NoError(t, err)
	req.Header.Set("x-cron-secret", cronSecret)
	start = time.Now()
	resp, err := (&http.Client{Timeout: 2 * time.Minute}).Do(req)
	catchupWall := time.Since(start)
	require.NoError(t, err, "POST %s/api/cron/retention — the compose dashboard must be reachable (DASHBOARD_URL)", dashboardURL)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "the retention sweep must complete (the catch-up is its LAST statement): %s", string(body))

	catchupRewritten := m3CountRewrittenTo(t, ctx, pool, version)
	require.Equal(t, m3NextIssues, catchupRewritten, "§13 M3 primary: the catch-up must rewrite exactly the same 500 to the earliest release newer than resolved_at — the new version")
	require.Zero(t, m3CountNext(t, ctx, pool, resolvedAt), "no eligible `next` issue may be left behind by the catch-up")
	m3AssertControlsUntouched(t, ctx, pool, explicitBefore, "after the catch-up")

	fmt.Fprintf(&b, "\n(3) catch-up — rows rewritten: %d (asserted = %d); wall clock around POST /api/cron/retention (the whole sweep, the catch-up last): %.3f ms; response: %s\n", catchupRewritten, m3NextIssues, float64(catchupWall.Microseconds())/1000, strings.TrimSpace(string(body)))
	fmt.Fprintf(&b, "    the statement as retention.ts issues it (catchUpNextReleases), EXPLAIN (ANALYZE, BUFFERS) over the 500 in a rolled-back transaction taken immediately before the real take:\n%s", m3Indent(catchupPlan))
	t.Log("\n" + b.String())
}

// m3CountNext is how many of project A's issues are `next` at exactly the driver's resolved_at —
// the 500 (the seed's 30 moved there and the 470 inserted), never the late control.
func m3CountNext(t *testing.T, ctx context.Context, pool *pgxpool.Pool, resolvedAt time.Time) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*)::int FROM issues WHERE project_id = $1 AND status = 'resolved' AND resolved_in_version = 'next' AND resolved_at = $2`,
		rsSeedProjectA, resolvedAt).Scan(&n))
	return n
}

// m3CountRewrittenTo is how many of project A's resolved issues read `version`.
func m3CountRewrittenTo(t *testing.T, ctx context.Context, pool *pgxpool.Pool, version string) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*)::int FROM issues WHERE project_id = $1 AND status = 'resolved' AND resolved_in_version = $2`,
		rsSeedProjectA, version).Scan(&n))
	return n
}

// m3ExplicitSnapshot is (id → resolved_in_version) over the seed's explicitly-resolved issues of
// project A, taken BEFORE either take, so "untouched" is an equality on the same ids after each — a
// count would not notice a version swapped for another.
func m3ExplicitSnapshot(t *testing.T, ctx context.Context, pool *pgxpool.Pool) map[string]string {
	t.Helper()
	rows, err := pool.Query(ctx,
		`SELECT id::text, resolved_in_version FROM issues
		  WHERE project_id = $1 AND status = 'resolved' AND resolved_in_version <> 'next' AND fingerprint LIKE 'fp-rs-a-%'`,
		rsSeedProjectA)
	require.NoError(t, err)
	defer rows.Close()
	snapshot := map[string]string{}
	for rows.Next() {
		var id, version string
		require.NoError(t, rows.Scan(&id, &version))
		snapshot[id] = version
	}
	require.NoError(t, rows.Err())
	require.Len(t, snapshot, 30, "the seed's explicitly-resolved issues are the second control")
	return snapshot
}

func m3AssertControlsUntouched(t *testing.T, ctx context.Context, pool *pgxpool.Pool, explicitBefore map[string]string, when string) {
	t.Helper()
	var controlVersion string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT resolved_in_version FROM issues WHERE project_id = $1 AND fingerprint = $2`,
		rsSeedProjectA, m3ControlFingerprint).Scan(&controlVersion))
	require.Equal(t, "next", controlVersion, "%s: a `next` issue resolved AFTER the release's first_seen must be untouched (strict >)", when)
	for id, before := range explicitBefore {
		var status, after string
		require.NoError(t, pool.QueryRow(ctx, `SELECT status, resolved_in_version FROM issues WHERE id = $1`, id).Scan(&status, &after))
		require.Equal(t, "resolved", status, "%s: explicit control %s", when, id)
		require.Equal(t, before, after, "%s: a resolved issue with an explicit version must be untouched (%s)", when, id)
	}
}

// m3DeleteDriverRows removes everything this driver inserts: its issues (occurrences cascade) and
// the release row its occurrence created. The seed's own 30 are left as the take left them — the
// seed is re-run before the next session.
func m3DeleteDriverRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(ctx, `DELETE FROM issues WHERE project_id = $1 AND fingerprint LIKE 'fp-rs-m3-%'`, rsSeedProjectA)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `DELETE FROM releases WHERE project_id = $1 AND version LIKE 'v4.0.0+m3.%'`, rsSeedProjectA)
	require.NoError(t, err)
}

// m3Explain runs EXPLAIN (ANALYZE, BUFFERS) of sql with args inside a transaction that is rolled
// back — the statement executes over the real rows, the plan and its execution time are read, and
// the rows are left exactly as they were for the real take.
func m3Explain(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, args ...interface{}) string {
	t.Helper()
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { require.NoError(t, tx.Rollback(ctx)) }()
	rows, err := tx.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS) "+sql, args...)
	require.NoError(t, err)
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var line string
		require.NoError(t, rows.Scan(&line))
		b.WriteString(line)
		b.WriteByte('\n')
	}
	require.NoError(t, rows.Err())
	return b.String()
}

// m3ExtractGoConst reads the raw-string value of `const <name> = \`…\“ out of
// apps/processor-go/service/processor_service.go — the statement exactly as the service issues it.
func m3ExtractGoConst(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(findProjectRoot(), "apps", "processor-go", "service", "processor_service.go")
	src, err := os.ReadFile(path)
	require.NoError(t, err)
	marker := "const " + name + " = `"
	start := strings.Index(string(src), marker)
	require.NotEqual(t, -1, start, "%s must declare %s as a raw-string const", path, name)
	rest := string(src)[start+len(marker):]
	end := strings.Index(rest, "`")
	require.NotEqual(t, -1, end)
	return strings.TrimSpace(rest[:end])
}

// m3ExtractRetentionCatchUp reads the catch-up statement out of
// apps/dashboard-web/src/lib/server/retention.ts's catchUpNextReleases — the `sql\`…\“ template
// tx.execute runs — and resolves its two Drizzle table interpolations to the table names, which is
// what Drizzle itself emits for them. The statement's RETURNING i.id is kept: it is how the
// function counts its rows, and EXPLAIN accepts it.
func m3ExtractRetentionCatchUp(t *testing.T) string {
	t.Helper()
	path := filepath.Join(findProjectRoot(), "apps", "dashboard-web", "src", "lib", "server", "retention.ts")
	src, err := os.ReadFile(path)
	require.NoError(t, err)
	fnStart := strings.Index(string(src), "async function catchUpNextReleases(")
	require.NotEqual(t, -1, fnStart, "%s must declare catchUpNextReleases", path)
	rest := string(src)[fnStart:]
	marker := "sql`"
	start := strings.Index(rest, marker)
	require.NotEqual(t, -1, start, "catchUpNextReleases must run one sql`…` template")
	rest = rest[start+len(marker):]
	end := strings.Index(rest, "`")
	require.NotEqual(t, -1, end)
	stmt := rest[:end]
	stmt = strings.ReplaceAll(stmt, "${issues}", `"issues"`)
	stmt = strings.ReplaceAll(stmt, "${releases}", `"releases"`)
	require.NotContains(t, stmt, "${", "the catch-up template must interpolate only the two tables: %s", stmt)
	return strings.TrimSpace(stmt)
}

func m3Indent(plan string) string {
	lines := strings.Split(strings.TrimRight(plan, "\n"), "\n")
	for i := range lines {
		lines[i] = "      " + lines[i]
	}
	return strings.Join(lines, "\n") + "\n"
}

func m3Env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
