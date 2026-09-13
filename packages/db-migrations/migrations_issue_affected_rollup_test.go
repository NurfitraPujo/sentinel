package dbmigrations

// sentinel-affected-users §4.6 (be.md task 2, criterion 1). Replays
// migrations/1724500000_add_issue_affected_rollup.sql up -> down -> up against a real Postgres in
// the style bd08ca2 established for this package, and then replays it a SECOND time under a
// different goose ledger table against the same database -- the situation hazard
// sentinel_goose_target names: one flat directory, three ledgers, one physical database, so every
// statement in the file runs again on a schema that already has every object it creates. An
// unguarded CREATE TABLE or ADD COLUMN passes the first replay and fails the second, which is why
// the second one is here and not assumed.
//
// What is asserted is the SHAPE the contract paints -- the two counters as integer NOT NULL
// DEFAULT 0, the composite primary keys, the `<> ''` CHECKs, the three indexes -- and the two
// behaviours only a live constraint can prove: a duplicate (issue, user) pair is rejected by the
// PK (that is what makes "distinct" a database fact), an empty id is rejected by the CHECK
// (decision 5's tripwire), and deleting an issue cascades its side rows.
//
// BUG-01's follow-on, migrations/1724600000_add_issues_project_affected_users_index.sql, is covered
// at the end of this file: its replay, and the PLAN of §4.1's query in both environment forms.
//
// Same database contract as migrations_oncall_weekday_test.go: MIGRATIONS_TEST_DB_URL at a
// DISPOSABLE Postgres whose role may CREATE DATABASE, self-skipping without one,
// MIGRATIONS_TEST_REQUIRED=1 to make the skip a failure. Never the shared dev database.
//
//	MIGRATIONS_TEST_DB_URL='postgres://sentinel:changeme@localhost:55432/sentinel?sslmode=disable' \
//	  GOWORK=off go test ./... -run 'AffectedRollup|AffectedUsers' -count=1 -v

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
)

const (
	// 1724400000 is the environments migration -- the last version before this one and the one
	// whose `issues` shape (NOT NULL environment, no default) the seed below has to satisfy.
	affectedRollupBaseVersion int64 = 1724400000
	affectedRollupVersion     int64 = 1724500000
)

// TestAffectedRollupReplay drives up -> down -> up and asserts the schema in each state.
func TestAffectedRollupReplay(t *testing.T) {
	db, cleanup := freshMigrationDatabase(t, "affected")
	defer cleanup()

	ctx := context.Background()
	migrateTo(t, db, affectedRollupBaseVersion)

	// An issue that predates the migration: decision 4 says it starts at 0, and the DEFAULT is
	// what puts it there -- the assertion below reads the column back on THIS row.
	issueID := seedIssue(t, db, "pre-existing")

	migrateTo(t, db, affectedRollupVersion)
	assertAffectedRollupShape(t, db, true)

	var users, tenants int
	if err := db.QueryRow(
		`SELECT affected_user_count, affected_tenant_count FROM issues WHERE id = $1`, issueID,
	).Scan(&users, &tenants); err != nil {
		t.Fatalf("read the pre-existing issue's counters: %v", err)
	}
	if users != 0 || tenants != 0 {
		t.Errorf("pre-existing issue has counts (%d, %d), want (0, 0): decision 4 -- no backfill, DEFAULT 0", users, tenants)
	}

	// The PK is the ON CONFLICT target of §4.3: a second row for the same pair must be rejected,
	// not stored twice. The CHECK is decision 5's tripwire: '' must never become a phantom user.
	mustExec(t, db, `INSERT INTO issue_affected_users (issue_id, user_id, first_seen, last_seen)
		VALUES ('`+issueID+`', 'u-1', now(), now())`)
	assertRejectedBy(t, db,
		`INSERT INTO issue_affected_users (issue_id, user_id, first_seen, last_seen)
		 VALUES ('`+issueID+`', 'u-1', now(), now())`,
		"issue_affected_users_pkey")
	assertRejectedBy(t, db,
		`INSERT INTO issue_affected_users (issue_id, user_id, first_seen, last_seen)
		 VALUES ('`+issueID+`', '', now(), now())`,
		"issue_affected_users_user_id_check")
	mustExec(t, db, `INSERT INTO issue_affected_tenants (issue_id, tenant_id, first_seen, last_seen)
		VALUES ('`+issueID+`', 't-1', now(), now())`)
	assertRejectedBy(t, db,
		`INSERT INTO issue_affected_tenants (issue_id, tenant_id, first_seen, last_seen)
		 VALUES ('`+issueID+`', 't-1', now(), now())`,
		"issue_affected_tenants_pkey")
	assertRejectedBy(t, db,
		`INSERT INTO issue_affected_tenants (issue_id, tenant_id, first_seen, last_seen)
		 VALUES ('`+issueID+`', '', now(), now())`,
		"issue_affected_tenants_tenant_id_check")

	// ON DELETE CASCADE: §4.5's orphan rule, a manual-report delete and a project delete all rely
	// on it -- "no code" is only true if the constraint is there.
	mustExec(t, db, `DELETE FROM issues WHERE id = '`+issueID+`'`)
	assertRowCount(t, db, "issue_affected_users", 0)
	assertRowCount(t, db, "issue_affected_tenants", 0)

	if err := goose.DownToContext(ctx, db, migrationsDir(), affectedRollupBaseVersion); err != nil {
		t.Fatalf("goose down to %d failed: %v", affectedRollupBaseVersion, err)
	}
	assertAffectedRollupShape(t, db, false)

	migrateTo(t, db, affectedRollupVersion)
	assertAffectedRollupShape(t, db, true)
}

// TestAffectedRollupReplayUnderSecondLedger applies the whole directory under `schema_migrations`
// and then again under `dashboard_migrations` on the SAME database. Every statement in
// 1724500000 runs twice against one schema; the second pass is the one that fails on any
// statement that is not guarded.
func TestAffectedRollupReplayUnderSecondLedger(t *testing.T) {
	db, cleanup := freshMigrationDatabase(t, "affected_ledger")
	defer cleanup()

	migrateTo(t, db, affectedRollupVersion)
	assertAffectedRollupShape(t, db, true)

	goose.SetTableName("dashboard_migrations")
	defer goose.SetTableName("schema_migrations")
	if err := goose.UpToContext(context.Background(), db, migrationsDir(), affectedRollupVersion); err != nil {
		t.Fatalf("replaying the directory under a second goose ledger failed -- an unguarded statement in 1724500000: %v", err)
	}

	var applied int
	if err := db.QueryRow(
		`SELECT count(*) FROM dashboard_migrations WHERE version_id = $1 AND is_applied`, affectedRollupVersion,
	).Scan(&applied); err != nil {
		t.Fatalf("read the second ledger: %v", err)
	}
	if applied != 1 {
		t.Errorf("dashboard_migrations records %d applied rows for %d, want 1", applied, affectedRollupVersion)
	}
	assertAffectedRollupShape(t, db, true)
}

// assertAffectedRollupShape checks every object §4.6 creates is present (want=true) or absent
// (want=false): the two side tables, the two counters with their type/nullability/default, the
// composite primary keys, the CHECKs, and the three indexes.
func assertAffectedRollupShape(t *testing.T, db *sql.DB, want bool) {
	t.Helper()

	for _, table := range []string{"issue_affected_users", "issue_affected_tenants"} {
		assertTableExists(t, db, table, want)
	}
	for _, column := range []string{"affected_user_count", "affected_tenant_count"} {
		assertColumnExists(t, db, "issues", column, want)
		if want {
			assertColumnShape(t, db, "issues", column, "integer", "NO", "0")
		}
	}
	for _, index := range []string{
		"idx_issue_affected_users_last_seen",
		"idx_issue_affected_tenants_last_seen",
		"idx_issues_project_env_affected_users",
	} {
		assertIndexExists(t, db, index, want)
	}
	if !want {
		return
	}

	assertConstraint(t, db, "issue_affected_users", "issue_affected_users_pkey", "PRIMARY KEY (issue_id, user_id)")
	assertConstraint(t, db, "issue_affected_tenants", "issue_affected_tenants_pkey", "PRIMARY KEY (issue_id, tenant_id)")
	assertConstraint(t, db, "issue_affected_users", "issue_affected_users_user_id_check", "CHECK (((user_id)::text <> ''::text))")
	assertConstraint(t, db, "issue_affected_tenants", "issue_affected_tenants_tenant_id_check", "CHECK (((tenant_id)::text <> ''::text))")
	// The key order IS §4.1's ORDER BY, `id DESC` included (decision 22); a reordered index would
	// still exist and still be named right, so the definition is what is pinned.
	assertIndexDefinition(t, db, "idx_issues_project_env_affected_users",
		"(project_id, environment, affected_user_count DESC, last_seen DESC, id DESC)")
}

func seedIssue(t *testing.T, db *sql.DB, name string) string {
	t.Helper()

	var orgID, projectID, issueID string
	if err := db.QueryRow(
		`INSERT INTO organizations (name, slug) VALUES ($1, $1) RETURNING id`, name,
	).Scan(&orgID); err != nil {
		t.Fatalf("seed organization %s: %v", name, err)
	}
	if err := db.QueryRow(
		`INSERT INTO projects (name, organization_id, api_key, api_key_hash) VALUES ($1, $2, $3, $4) RETURNING id`,
		name, orgID, name, name,
	).Scan(&projectID); err != nil {
		t.Fatalf("seed project %s: %v", name, err)
	}
	if err := db.QueryRow(
		`INSERT INTO issues (project_id, environment, fingerprint, message, error_class)
		 VALUES ($1, 'production', $2, $3, 'Error') RETURNING id`,
		projectID, name, name,
	).Scan(&issueID); err != nil {
		t.Fatalf("seed issue %s: %v", name, err)
	}
	return issueID
}

func assertRejectedBy(t *testing.T, db *sql.DB, stmt, constraint string) {
	t.Helper()

	_, err := db.Exec(stmt)
	if err == nil {
		t.Fatalf("%q was accepted; expected %s to reject it", stmt, constraint)
	}
	if !strings.Contains(err.Error(), constraint) {
		t.Fatalf("%q was rejected by something other than %s: %v", stmt, constraint, err)
	}
}

func assertRowCount(t *testing.T, db *sql.DB, table string, want int) {
	t.Helper()

	var got int
	if err := db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&got); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	if got != want {
		t.Errorf("%s holds %d rows, want %d", table, got, want)
	}
}

func assertTableExists(t *testing.T, db *sql.DB, table string, want bool) {
	t.Helper()

	var exists bool
	if err := db.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = $1)`,
		table,
	).Scan(&exists); err != nil {
		t.Fatalf("introspect table %s: %v", table, err)
	}
	if exists != want {
		t.Errorf("table %s exists = %v, want %v", table, exists, want)
	}
}

func assertColumnShape(t *testing.T, db *sql.DB, table, column, dataType, isNullable, columnDefault string) {
	t.Helper()

	var gotType, gotNullable string
	var gotDefault sql.NullString
	if err := db.QueryRow(
		`SELECT data_type, is_nullable, column_default FROM information_schema.columns
		 WHERE table_name = $1 AND column_name = $2`,
		table, column,
	).Scan(&gotType, &gotNullable, &gotDefault); err != nil {
		t.Fatalf("introspect %s.%s: %v", table, column, err)
	}
	if gotType != dataType || gotNullable != isNullable || gotDefault.String != columnDefault {
		t.Errorf("%s.%s is (%s, nullable=%s, default=%q), want (%s, nullable=%s, default=%q)",
			table, column, gotType, gotNullable, gotDefault.String, dataType, isNullable, columnDefault)
	}
}

func assertIndexExists(t *testing.T, db *sql.DB, index string, want bool) {
	t.Helper()

	var exists bool
	if err := db.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE schemaname = 'public' AND indexname = $1)`, index,
	).Scan(&exists); err != nil {
		t.Fatalf("introspect index %s: %v", index, err)
	}
	if exists != want {
		t.Errorf("index %s exists = %v, want %v", index, exists, want)
	}
}

func assertIndexDefinition(t *testing.T, db *sql.DB, index, wantSuffix string) {
	t.Helper()

	var def string
	if err := db.QueryRow(`SELECT indexdef FROM pg_indexes WHERE indexname = $1`, index).Scan(&def); err != nil {
		t.Fatalf("read the definition of %s: %v", index, err)
	}
	if !strings.HasSuffix(def, wantSuffix) {
		t.Errorf("index %s is %q, want a definition ending in %q", index, def, wantSuffix)
	}
}

func assertConstraint(t *testing.T, db *sql.DB, table, name, wantDef string) {
	t.Helper()

	var def string
	if err := db.QueryRow(
		`SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = $1 AND conrelid = $2::regclass`,
		name, table,
	).Scan(&def); err != nil {
		t.Fatalf("constraint %s on %s: %v", name, table, err)
	}
	if def != wantDef {
		t.Errorf("constraint %s on %s is %q, want %q", name, table, def, wantDef)
	}
}

// ---------------------------------------------------------------------------------------------
// BUG-01 — migrations/1724600000_add_issues_project_affected_users_index.sql.
//
// Review finding R1 on 1724500000:74: `idx_issues_project_env_affected_users` cannot serve §13 M1's
// named request (`?project=<id>&status=all&sort=affected_users`, i.e. environment=all) because a
// btree needs an equality on its second key before its order is the ORDER BY; without one the
// planner scans the project's rows and top-N sorts them at any size. 1724600000 adds
// `idx_issues_project_affected_users (project_id, affected_user_count DESC, last_seen DESC, id DESC)`
// and keeps the env-keyed one. Two things are pinned here: the schema (up -> down -> up, then the
// second-ledger replay, as above) and THE PLAN — the thing the finding is about — for both request
// shapes, on a seed large enough that the planner has a real choice.
// ---------------------------------------------------------------------------------------------

const affectedUsersProjectIndexVersion int64 = 1724600000

// TestAffectedUsersProjectIndexReplay drives 1724600000 up -> down -> up and asserts the index in
// each state, then replays the directory under a second ledger.
func TestAffectedUsersProjectIndexReplay(t *testing.T) {
	db, cleanup := freshMigrationDatabase(t, "affected_projidx")
	defer cleanup()

	ctx := context.Background()
	migrateTo(t, db, affectedRollupVersion)
	assertIndexExists(t, db, "idx_issues_project_affected_users", false)

	migrateTo(t, db, affectedUsersProjectIndexVersion)
	assertAffectedUsersProjectIndexShape(t, db, true)

	if err := goose.DownToContext(ctx, db, migrationsDir(), affectedRollupVersion); err != nil {
		t.Fatalf("goose down to %d failed: %v", affectedRollupVersion, err)
	}
	assertAffectedUsersProjectIndexShape(t, db, false)
	// The down of 1724600000 removes ONLY its own index; 1724500000's stays.
	assertAffectedRollupShape(t, db, true)

	migrateTo(t, db, affectedUsersProjectIndexVersion)
	assertAffectedUsersProjectIndexShape(t, db, true)

	goose.SetTableName("dashboard_migrations")
	defer goose.SetTableName("schema_migrations")
	if err := goose.UpToContext(ctx, db, migrationsDir(), affectedUsersProjectIndexVersion); err != nil {
		t.Fatalf("replaying the directory under a second goose ledger failed -- an unguarded statement in 1724600000: %v", err)
	}
	assertAffectedUsersProjectIndexShape(t, db, true)
}

// TestAffectedUsersSortPlanBothEnvironmentForms is the finding itself as a test. On one project
// with 2,000 issues spread over two environments (the committed seed's size), the §4.1 loader's
// query in BOTH shapes must be an ordered index scan with NO Sort node: the environment=all form
// on idx_issues_project_affected_users, the environment=<x> form on
// idx_issues_project_env_affected_users. Delete the CREATE INDEX in 1724600000 and the first
// assertion goes red with the finding's exact plan (idx_issues_project_id + Sort).
//
// `enable_seqscan = off` for the session: the finding recorded that the fallback was Seq Scan +
// top-N heapsort even with it off, so the assertion is about whether an index CAN serve the
// ORDER BY, not about the seq-scan/index-scan cost crossover at this row count.
func TestAffectedUsersSortPlanBothEnvironmentForms(t *testing.T) {
	db, cleanup := freshMigrationDatabase(t, "affected_plan")
	defer cleanup()

	migrateTo(t, db, affectedUsersProjectIndexVersion)
	projectID := seedProjectWithIssues(t, db, "plan", 2000)
	mustExec(t, db, `ANALYZE issues`)
	mustExec(t, db, `SET enable_seqscan = off`)

	// The SELECT list, predicate and ORDER BY are the loader's own, as captured in
	// apps/dashboard-web/scripts/affected-users-m1.sql: `project_id = $1 AND project_id IN (...)`
	// is the filter plus the tenancy guard, `status=all` adds nothing.
	const columns = `"id", "project_id", "fingerprint", "message", "error_class", "environment", "status", "first_seen", "last_seen", "count", "affected_user_count", "affected_tenant_count"`
	const order = ` order by "issues"."affected_user_count" desc, "issues"."last_seen" desc, "issues"."id" desc limit 20`

	environmentAll := `select ` + columns + ` from "issues" where ("issues"."project_id" = '` + projectID +
		`' and "issues"."project_id" in ('` + projectID + `'))` + order
	environmentOne := `select ` + columns + ` from "issues" where ("issues"."project_id" = '` + projectID +
		`' and "issues"."project_id" in ('` + projectID + `') and "issues"."environment" = 'production')` + order

	assertPlanIsOrderedIndexScan(t, db, "environment=all", environmentAll, "idx_issues_project_affected_users")
	assertPlanIsOrderedIndexScan(t, db, "environment=production", environmentOne, "idx_issues_project_env_affected_users")
}

func assertAffectedUsersProjectIndexShape(t *testing.T, db *sql.DB, want bool) {
	t.Helper()

	assertIndexExists(t, db, "idx_issues_project_affected_users", want)
	if want {
		// §4.1's ORDER BY in full, `id DESC` last (decision 22), under `project_id` alone.
		assertIndexDefinition(t, db, "idx_issues_project_affected_users",
			"(project_id, affected_user_count DESC, last_seen DESC, id DESC)")
	}
}

// seedProjectWithIssues creates one organization and project and n issues in it, alternating
// between two environments, with a skewed affected_user_count (most at 0-3, a few high) and a
// last_seen from generate_series so ties on (affected_user_count, last_seen) exist -- the
// committed seed's shape, small enough for a test.
func seedProjectWithIssues(t *testing.T, db *sql.DB, name string, n int) string {
	t.Helper()

	var orgID, projectID string
	if err := db.QueryRow(
		`INSERT INTO organizations (name, slug) VALUES ($1, $1) RETURNING id`, name,
	).Scan(&orgID); err != nil {
		t.Fatalf("seed organization %s: %v", name, err)
	}
	if err := db.QueryRow(
		`INSERT INTO projects (name, organization_id, api_key, api_key_hash) VALUES ($1, $2, $3, $4) RETURNING id`,
		name, orgID, name, name,
	).Scan(&projectID); err != nil {
		t.Fatalf("seed project %s: %v", name, err)
	}
	if _, err := db.Exec(`
		INSERT INTO issues (project_id, environment, fingerprint, message, error_class, last_seen, affected_user_count)
		SELECT $1,
		       CASE WHEN i % 2 = 0 THEN 'production' ELSE 'staging' END,
		       'fp-' || i, 'issue ' || i, 'Error',
		       now() - ((i % 60) || ' days')::interval,
		       CASE WHEN i % 100 = 0 THEN 500 + i WHEN i % 10 = 0 THEN 10 + (i % 90) ELSE i % 4 END
		FROM generate_series(1, $2) AS i`, projectID, n); err != nil {
		t.Fatalf("seed %d issues in %s: %v", n, name, err)
	}
	return projectID
}

// assertPlanIsOrderedIndexScan runs EXPLAIN on query and requires that the plan reads `issues`
// through index (an Index Scan or Index Only Scan naming it) and contains no Sort node -- the
// ORDER BY is produced by the index order, which is what makes the paginated read a range scan.
func assertPlanIsOrderedIndexScan(t *testing.T, db *sql.DB, label, query, index string) {
	t.Helper()

	rows, err := db.Query(`EXPLAIN ` + query)
	if err != nil {
		t.Fatalf("%s: EXPLAIN failed: %v", label, err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("%s: read plan: %v", label, err)
		}
		plan = append(plan, line)
	}
	text := strings.Join(plan, "\n")
	t.Logf("%s plan:\n%s", label, text)

	if !strings.Contains(text, "Index Scan using "+index) && !strings.Contains(text, "Index Only Scan using "+index) {
		t.Errorf("%s: plan does not read issues through %s:\n%s", label, index, text)
	}
	for _, line := range plan {
		if strings.Contains(line, "Sort") {
			t.Errorf("%s: plan has a Sort node -- the ORDER BY is not served by an index:\n%s", label, text)
			break
		}
	}
}
