package integration

// sentinel-issue-tag-facets (spec §10.1, §10.2): the one database helper every tag-facets proof uses, the
// shared check query, and the fixtures and statements the proofs share.
//
// THESE PROOFS NEVER SKIP (contract decision 5). tagFacetsDB provisions its own database on the Postgres
// TestMain started and calls t.Fatalf on every error. It never reaches the repository's skip-on-missing-
// infrastructure helper (setup_helper_test.go), which skips unless SENTINEL_E2E=1, a variable
// `task test-integration` does not set. tests/unit/tag_facets_gate_test.go enforces it: no tag_facets_*
// file may name that helper or any skip call, and tagFacetsDB may call no package-local function that is
// declared outside a tag_facets_* file. That is also why the admin DSN and the database-name rewrite are
// re-derived here rather than borrowed from db_migrations_test.go.
//
// No tag-facets test calls t.Parallel(): goose's ledger name is process-global (goose.SetTableName) and
// tagFacetsDB migrates under its own ledger.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/NurfitraPujo/sentinel/apps/processor-go/store"
	dbmigrations "github.com/NurfitraPujo/sentinel/packages/db-migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// tagFacetsCheckSQL is spec §10.2's check query, verbatim: every exactness proof asserts all five columns
// are 0. It is scoped to the issues the test created ($1) and its oracle counts only COUNTED occurrences
// (issue_tag_counted, spec §3.6), so it asserts on the state the test made and nothing else. The dashboard
// suites carry the same text in apps/dashboard-web/src/lib/server/tag-facets-check.sql.ts.
const tagFacetsCheckSQL = `
WITH bf AS (SELECT cursor_id, done FROM issue_tag_values_backfill),
oracle AS (SELECT o.issue_id, p.key, p.value, count(*) AS n
             FROM error_occurrences o CROSS JOIN bf
             CROSS JOIN LATERAL issue_tag_pairs(o.tags, o.contexts, o.event_user, o.release_version, o.environment) p
            WHERE o.issue_id = ANY($1::uuid[]) AND issue_tag_counted(o.tag_rollup_applied, o.id, bf.cursor_id, bf.done)
            GROUP BY 1, 2, 3),
r AS (SELECT * FROM issue_tag_values WHERE issue_id = ANY($1::uuid[]))
SELECT
 (SELECT count(*) FROM r t LEFT JOIN oracle o USING (issue_id, key, value)
   WHERE t.value <> '' AND o.n IS DISTINCT FROM t.count)                                     AS named_mismatch,
 (SELECT count(*) FROM (SELECT issue_id, key, sum(n) s FROM oracle GROUP BY 1, 2) o
   FULL JOIN (SELECT issue_id, key, sum(count) s FROM r GROUP BY 1, 2) x USING (issue_id, key)
   WHERE o.s IS DISTINCT FROM x.s)                                                            AS key_total_mismatch,
 (SELECT count(*) FROM oracle o
   WHERE NOT EXISTS (SELECT 1 FROM r t WHERE (t.issue_id, t.key, t.value) = (o.issue_id, o.key, o.value))
     AND NOT EXISTS (SELECT 1 FROM r t WHERE (t.issue_id, t.key, t.value) = (o.issue_id, o.key, ''))) AS value_nowhere,
 (SELECT count(*) FROM r WHERE count <= 0)                                                    AS zero_rows,
 (SELECT count(*) FROM (SELECT 1 FROM r WHERE value <> '' GROUP BY issue_id, key HAVING count(*) > 1000) x) AS over_cap
`

const (
	tagFacetsLedger        = "tagfacets_migrations"
	tagFacetsMigrationsDir = "../../packages/db-migrations/migrations"
)

type tagFacetsOpts struct {
	// UpTo, when non-zero, migrates only up to that version (goose UpTo): the pre-migration state the
	// backfill and migration tests seed. The test then calls env.migrateRest() to apply the rest.
	UpTo int64
}

type tagFacetsEnv struct {
	DB    *sql.DB
	Pool  *pgxpool.Pool
	Store store.IssueStore
	// DSN is the key=value DSN of this test's own database.
	DSN         string
	Name        string
	migrateRest func()
}

// tagFacetsDB provisions a disposable database, migrates it under its own ledger, and returns it. Every
// step fails the test on error (spec §10.1 steps 1-4); nothing here may skip.
func tagFacetsDB(t *testing.T, opts tagFacetsOpts) *tagFacetsEnv {
	t.Helper()
	ctx := context.Background()

	// 1. The admin DSN: the environment TestMain sets (setup_test.go).
	admin := tagFacetsAdminDSN()
	if admin == "" {
		t.Fatalf("tagFacetsDB: POSTGRES_HOST not configured")
	}

	// 2. Connect, ping, create the database, and drop it on cleanup.
	adminDB, err := sql.Open("pgx", admin)
	if err != nil {
		t.Fatalf("tagFacetsDB: open admin: %v", err)
	}
	defer adminDB.Close()
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := adminDB.PingContext(pingCtx); err != nil {
		t.Fatalf("tagFacetsDB: ping admin: %v", err)
	}
	name := fmt.Sprintf("tagfacets_%s_%d", tagFacetsLabel(t.Name()), time.Now().UnixNano())
	if _, err := adminDB.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %q", name)); err != nil {
		t.Fatalf("tagFacetsDB: create database %s: %v", name, err)
	}
	t.Cleanup(func() {
		a, err := sql.Open("pgx", admin)
		if err != nil {
			t.Errorf("tagFacetsDB cleanup: open admin: %v", err)
			return
		}
		defer a.Close()
		if _, err := a.Exec(fmt.Sprintf("DROP DATABASE IF EXISTS %q WITH (FORCE)", name)); err != nil {
			t.Errorf("tagFacetsDB cleanup: drop %s: %v", name, err)
		}
	})

	// 3. Migrate under this helper's own ledger, fully or up to opts.UpTo.
	dsn := tagFacetsWithDB(admin, name)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("tagFacetsDB: open %s: %v", name, err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("tagFacetsDB: ping %s: %v", name, err)
	}
	migOpts := dbmigrations.MigrationOptions{TableName: tagFacetsLedger, Directory: tagFacetsMigrationsDir}
	if opts.UpTo != 0 {
		if err := dbmigrations.BaselineVersion(ctx, db, opts.UpTo, migOpts); err != nil {
			t.Fatalf("tagFacetsDB: migrate %s up to %d: %v", name, opts.UpTo, err)
		}
	} else if err := dbmigrations.RunMigrations(ctx, db, "up", migOpts); err != nil {
		t.Fatalf("tagFacetsDB: migrate %s: %v", name, err)
	}

	// 4. A pgx pool on the new database, pinged, and the store over it.
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("tagFacetsDB: pool %s: %v", name, err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("tagFacetsDB: ping pool %s: %v", name, err)
	}

	env := &tagFacetsEnv{DB: db, Pool: pool, Store: store.NewStore(pool), DSN: dsn, Name: name}
	env.migrateRest = func() {
		t.Helper()
		if err := dbmigrations.RunMigrations(ctx, db, "up", migOpts); err != nil {
			t.Fatalf("tagFacetsDB: migrate %s rest: %v", name, err)
		}
	}
	return env
}

// tagFacetsAdminDSN returns the Postgres TestMain provisioned. Under FORCE_TESTCONTAINERS=1 that is
// testConfig, read first because the POSTGRES_* environment is NOT stable across the run: tc.Setup with
// PostgresResource starts its own container, overwrites POSTGRES_* process-wide, and its cleanup
// terminates that container without restoring them, so every later reader of the environment dials a
// dead port (measured 2026-09-25: 21 existing tests skipped on "postgres ping unavailable" and every
// tag-facets proof failed on "connection refused"). In compose mode testConfig is empty and the
// environment TestMain defaults is the only source.
func tagFacetsAdminDSN() string {
	if testConfig.Host != "" {
		return "host=" + testConfig.Host + " port=" + testConfig.Port + " user=" + testConfig.User +
			" password=" + testConfig.Password + " dbname=" + testConfig.DB + " sslmode=disable"
	}
	host := os.Getenv("POSTGRES_HOST")
	if host == "" {
		return ""
	}
	return "host=" + host + " port=" + os.Getenv("POSTGRES_PORT") + " user=" + os.Getenv("POSTGRES_USER") +
		" password=" + os.Getenv("POSTGRES_PASSWORD") + " dbname=" + os.Getenv("POSTGRES_DB") + " sslmode=disable"
}

func tagFacetsWithDB(dsn, dbName string) string {
	parts := strings.Fields(dsn)
	for i, p := range parts {
		if strings.HasPrefix(p, "dbname=") {
			parts[i] = "dbname=" + dbName
		}
	}
	return strings.Join(parts, " ")
}

var tagFacetsLabelRe = regexp.MustCompile(`[^a-z0-9]+`)

func tagFacetsLabel(testName string) string {
	l := tagFacetsLabelRe.ReplaceAllString(strings.ToLower(strings.TrimPrefix(testName, "TestTagFacets")), "_")
	l = strings.Trim(l, "_")
	if len(l) > 30 {
		l = l[:30]
	}
	return l
}

// tagFacetsMigrationFile finds the one *_add_issue_tag_values.sql in the migrations directory and derives
// its version and the greatest version below it, so no test hard-codes either (plan §6: a renumbering is
// one git mv).
func tagFacetsMigrationFile(t *testing.T) (path string, version, prev int64) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(tagFacetsMigrationsDir, "*.sql"))
	if err != nil {
		t.Fatalf("glob migrations: %v", err)
	}
	var versions []int64
	for _, f := range files {
		base := filepath.Base(f)
		v, err := strconv.ParseInt(strings.SplitN(base, "_", 2)[0], 10, 64)
		if err != nil {
			continue
		}
		versions = append(versions, v)
		if strings.HasSuffix(base, "_add_issue_tag_values.sql") {
			if path != "" {
				t.Fatalf("two *_add_issue_tag_values.sql migrations: %s and %s", path, f)
			}
			path, version = f, v
		}
	}
	if path == "" {
		t.Fatalf("no *_add_issue_tag_values.sql in %s", tagFacetsMigrationsDir)
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i] < versions[j] })
	for _, v := range versions {
		if v < version {
			prev = v
		}
	}
	if prev == 0 {
		t.Fatalf("no migration below %d", version)
	}
	return path, version, prev
}

// ---------------------------------------------------------------------------------------------------
// The check query and the rollup snapshot
// ---------------------------------------------------------------------------------------------------

type tagFacetsCheckResult struct {
	NamedMismatch, KeyTotalMismatch, ValueNowhere, ZeroRows, OverCap int64
}

func tfCheckResult(t *testing.T, q tfQuerier, issueIDs ...string) tagFacetsCheckResult {
	t.Helper()
	var r tagFacetsCheckResult
	if err := q.QueryRow(context.Background(), tagFacetsCheckSQL, issueIDs).Scan(
		&r.NamedMismatch, &r.KeyTotalMismatch, &r.ValueNowhere, &r.ZeroRows, &r.OverCap); err != nil {
		t.Fatalf("check query: %v", err)
	}
	return r
}

// tfCheck fails the test unless every column of the check query is 0 over issueIDs.
func tfCheck(t *testing.T, q tfQuerier, issueIDs ...string) {
	t.Helper()
	if len(issueIDs) == 0 {
		t.Fatalf("tfCheck: no issue ids (the check query must be scoped)")
	}
	if r := tfCheckResult(t, q, issueIDs...); r != (tagFacetsCheckResult{}) {
		t.Fatalf("check query over %d issue(s) not all zero: %+v", len(issueIDs), r)
	}
}

// tfSnapshot renders every rollup row, in key order, so two states can be compared byte for byte.
func tfSnapshot(t *testing.T, q tfQuerier) string {
	t.Helper()
	rows, err := q.Query(context.Background(), `
		SELECT issue_id::text || '|' || key || '|' || value || '|' || count || '|' || first_seen::text || '|' || last_seen::text
		  FROM issue_tag_values ORDER BY issue_id, key, value COLLATE "C"`)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("snapshot scan: %v", err)
		}
		b.WriteString(s)
		b.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("snapshot rows: %v", err)
	}
	return b.String()
}

// tfCount is the rollup count of one (issue, key, value); 0 when the row does not exist.
func tfCount(t *testing.T, q tfQuerier, issueID, key, value string) int64 {
	t.Helper()
	var n int64
	err := q.QueryRow(context.Background(),
		`SELECT coalesce((SELECT count FROM issue_tag_values WHERE issue_id = $1 AND key = $2 AND value = $3), 0)`,
		issueID, key, value).Scan(&n)
	if err != nil {
		t.Fatalf("count %s %s=%q: %v", issueID, key, value, err)
	}
	return n
}

func tfScalar(t *testing.T, q tfQuerier, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := q.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("scalar %q: %v", query, err)
	}
	return n
}

type tfQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

type tfExecer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// ---------------------------------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------------------------------

// tfSeedProject creates an organization and a project in the test's own database.
func tfSeedProject(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	ctx := context.Background()
	suffix := uuid.NewString()[:8]
	var orgID, projectID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO organizations (name, slug) VALUES ($1, $1) RETURNING id::text`, "tf-org-"+suffix).Scan(&orgID); err != nil {
		t.Fatalf("seed organization: %v", err)
	}
	key := "tf-key-" + suffix
	if err := pool.QueryRow(ctx,
		`INSERT INTO projects (name, api_key, api_key_hash, organization_id)
		 VALUES ($1, $2, encode(digest($3::bytea, 'sha256'), 'hex'), $4) RETURNING id::text`,
		"tf-project-"+suffix, key, key, orgID).Scan(&projectID); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	return projectID
}

// tfSeedIssue inserts one issue row directly and returns its id.
func tfSeedIssue(t *testing.T, q tfExecer, projectID, environment string) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := q.Exec(context.Background(),
		`INSERT INTO issues (id, project_id, environment, fingerprint, message, error_class, status, first_seen, last_seen, count)
		 VALUES ($1, $2, $3, $4, 'tag facets test', 'TagFacetsError', 'unresolved', now(), now(), 0)`,
		id, projectID, environment, "fp-tf-"+id); err != nil {
		t.Fatalf("seed issue: %v", err)
	}
	return id
}

// tfOcc is one occurrence row as the raw-insert fixtures write it. Empty JSON strings are SQL NULL.
type tfOcc struct {
	ID, IssueID, ProjectID, Environment, Release string
	Tags, Contexts, User                         string
	CreatedAt                                    time.Time
}

func (o tfOcc) withDefaults() tfOcc {
	if o.ID == "" {
		o.ID = uuid.NewString()
	}
	if o.Environment == "" {
		o.Environment = "production"
	}
	if o.CreatedAt.IsZero() {
		o.CreatedAt = time.Now().UTC()
	}
	return o
}

func tfNullJSON(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// tfInsertOccs inserts occs in ONE statement (so the statement trigger sees them as one transition table)
// with an error_search_index row each, and returns the ids in order.
func tfInsertOccs(t *testing.T, q tfExecer, occs []tfOcc) []string {
	t.Helper()
	ids, err := tfInsertOccsErr(q, occs)
	if err != nil {
		t.Fatalf("insert %d occurrence(s): %v", len(occs), err)
	}
	return ids
}

func tfInsertOccsErr(q tfExecer, occs []tfOcc) ([]string, error) {
	if len(occs) == 0 {
		return nil, nil
	}
	ctx := context.Background()
	var (
		b    strings.Builder
		args []any
		ids  []string
	)
	b.WriteString(`INSERT INTO error_occurrences (id, issue_id, project_id, environment, platform, release_version,
		stacktrace, metadata, created_at, tags, contexts, event_user) VALUES `)
	for i, o := range occs {
		o = o.withDefaults()
		ids = append(ids, o.ID)
		if i > 0 {
			b.WriteString(",")
		}
		n := len(args)
		fmt.Fprintf(&b, "($%d::uuid,$%d::uuid,$%d::uuid,$%d,'go',$%d,'[]','{}',$%d,$%d::jsonb,$%d::jsonb,$%d::jsonb)",
			n+1, n+2, n+3, n+4, n+5, n+6, n+7, n+8, n+9)
		args = append(args, o.ID, o.IssueID, o.ProjectID, o.Environment, o.Release, o.CreatedAt,
			tfNullJSON(o.Tags), tfNullJSON(o.Contexts), tfNullJSON(o.User))
	}
	if _, err := q.Exec(ctx, b.String(), args...); err != nil {
		return nil, err
	}
	if _, err := q.Exec(ctx,
		`INSERT INTO error_search_index (occurrence_id) SELECT unnest($1::uuid[])`, ids); err != nil {
		return nil, err
	}
	return ids, nil
}

// tfUser is the event_user JSON for one user id.
func tfUser(id string) string { return fmt.Sprintf(`{"id":%q}`, id) }

// ---------------------------------------------------------------------------------------------------
// The sweep twin: the EXACT statements deleteAgedOccurrences issues
// (apps/dashboard-web/src/lib/server/retention.ts, spec §5; the T18 twin precedent). Keep them in step.
// ---------------------------------------------------------------------------------------------------

const (
	tfSweepCandidatesFirst = `SELECT id, issue_id, created_at::text AS created_at_text FROM error_occurrences
         WHERE created_at < $1::timestamptz
         ORDER BY created_at, id
         LIMIT $2`
	tfSweepCandidatesAfter = `SELECT id, issue_id, created_at::text AS created_at_text FROM error_occurrences
         WHERE created_at < $1::timestamptz
           AND created_at >= $2::timestamptz
               AND (created_at, id) > ($2::timestamptz, $3::uuid)
         ORDER BY created_at, id
         LIMIT $4`
	tfSweepLockIssues = `SELECT id FROM issues WHERE id = ANY($1::uuid[])
                 ORDER BY id FOR UPDATE`
	tfSweepDelete = `DELETE FROM error_occurrences
                 WHERE id = ANY($1::uuid[])
                   AND created_at < $2::timestamptz
                RETURNING id`
)

type tfSweepCursor struct{ CreatedAt, ID string }

// tfSweepTwinBatch runs one batch: the candidate read outside a transaction, then, in one transaction,
// the issues locked in id order and the DELETE. It returns the rows deleted and the new cursor, or
// (0, nil) when the candidate read returned nothing.
func tfSweepTwinBatch(ctx context.Context, conn tfSweepConn, cutoff time.Time, after *tfSweepCursor, batch int) (int, *tfSweepCursor, error) {
	var rows pgx.Rows
	var err error
	if after == nil {
		rows, err = conn.Query(ctx, tfSweepCandidatesFirst, cutoff, batch)
	} else {
		rows, err = conn.Query(ctx, tfSweepCandidatesAfter, cutoff, after.CreatedAt, after.ID, batch)
	}
	if err != nil {
		return 0, nil, err
	}
	var ids, lastAt []string
	issueSet := map[string]bool{}
	var issueIDs []string
	for rows.Next() {
		var id, issueID, at string
		if err := rows.Scan(&id, &issueID, &at); err != nil {
			rows.Close()
			return 0, nil, err
		}
		ids = append(ids, id)
		lastAt = append(lastAt, at)
		if !issueSet[issueID] {
			issueSet[issueID] = true
			issueIDs = append(issueIDs, issueID)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, nil, err
	}
	if len(ids) == 0 {
		return 0, nil, nil
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return 0, nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, tfSweepLockIssues, issueIDs); err != nil {
		return 0, nil, err
	}
	tag, err := tx.Exec(ctx, tfSweepDelete, ids, cutoff)
	if err != nil {
		return 0, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, nil, err
	}
	return int(tag.RowsAffected()), &tfSweepCursor{CreatedAt: lastAt[len(lastAt)-1], ID: ids[len(ids)-1]}, nil
}

type tfSweepConn interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Begin(ctx context.Context) (pgx.Tx, error)
}

// tfSweepTwin runs batches until a candidate read returns nothing; it returns the rows deleted and the
// number of batches.
func tfSweepTwin(t *testing.T, conn tfSweepConn, cutoff time.Time, batch int) (deleted, batches int) {
	t.Helper()
	var after *tfSweepCursor
	for {
		n, next, err := tfSweepTwinBatch(context.Background(), conn, cutoff, after, batch)
		if err != nil {
			t.Fatalf("sweep twin batch %d: %v", batches+1, err)
		}
		if next == nil {
			return deleted, batches
		}
		deleted += n
		batches++
		after = next
	}
}

// ---------------------------------------------------------------------------------------------------
// Deterministic lock proofs (spec §10.2 "Lock proofs are deterministic, never timing-based")
// ---------------------------------------------------------------------------------------------------

// tfWaitBlockedBy polls (every 20 ms, up to 10 s) until backend pid waits on a lock held by holderPid.
func tfWaitBlockedBy(t *testing.T, q tfQuerier, pid, holderPid uint32) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var waiting bool
		err := q.QueryRow(context.Background(), `
			SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			                WHERE pid = $1 AND wait_event_type = 'Lock'
			                  AND pg_blocking_pids(pid) = ARRAY[$2::int])`, int(pid), int(holderPid)).Scan(&waiting)
		if err != nil {
			t.Fatalf("poll pg_stat_activity: %v", err)
		}
		if waiting {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("backend %d never waited on holder %d within 10 s (the lock under test was not taken first)", pid, holderPid)
}

// tfWaitAnyBlockedBy polls (every 20 ms, up to 10 s) until some backend, whose own pid the caller has no
// other way to learn (e.g. a connection tagfacets.Run manages internally), waits on a lock held by
// holderPid. Unlike tfWaitBlockedBy this does not pin which backend is waiting, only that one is — it
// exists to sequence a test deterministically on "the batch is now blocked", not to identify the batch's
// own pid.
func tfWaitAnyBlockedBy(t *testing.T, q tfQuerier, holderPid uint32) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var waiting bool
		err := q.QueryRow(context.Background(), `
			SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			                WHERE wait_event_type = 'Lock' AND $1 = ANY(pg_blocking_pids(pid)))`,
			int(holderPid)).Scan(&waiting)
		if err != nil {
			t.Fatalf("poll pg_stat_activity: %v", err)
		}
		if waiting {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no backend ever waited on holder %d within 10 s (the lock under test was not taken first)", holderPid)
}

// tfHeldRelationLocks returns "relname:mode" for every granted relation lock of backend pid.
func tfHeldRelationLocks(t *testing.T, q tfQuerier, pid uint32) map[string]bool {
	t.Helper()
	rows, err := q.Query(context.Background(), `
		SELECT c.relname || ':' || l.mode FROM pg_locks l JOIN pg_class c ON c.oid = l.relation
		 WHERE l.pid = $1 AND l.granted AND l.locktype = 'relation'`, int(pid))
	if err != nil {
		t.Fatalf("pg_locks: %v", err)
	}
	defer rows.Close()
	held := map[string]bool{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("pg_locks scan: %v", err)
		}
		held[s] = true
	}
	return held
}

func tfAcquire(t *testing.T, pool *pgxpool.Pool) *pgxpool.Conn {
	t.Helper()
	c, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	t.Cleanup(c.Release)
	return c
}

func tfPID(c *pgxpool.Conn) uint32 { return c.Conn().PgConn().PID() }
