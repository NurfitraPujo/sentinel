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
// Same database contract as migrations_oncall_weekday_test.go: MIGRATIONS_TEST_DB_URL at a
// DISPOSABLE Postgres whose role may CREATE DATABASE, self-skipping without one,
// MIGRATIONS_TEST_REQUIRED=1 to make the skip a failure. Never the shared dev database.
//
//	MIGRATIONS_TEST_DB_URL='postgres://sentinel:changeme@localhost:55432/sentinel?sslmode=disable' \
//	  GOWORK=off go test ./... -run AffectedRollup -count=1 -v

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
