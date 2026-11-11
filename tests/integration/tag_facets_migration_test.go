package integration

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	dbmigrations "github.com/NurfitraPujo/sentinel/packages/db-migrations"
	"github.com/google/uuid"
)

// sentinel-issue-tag-facets spec §8.1 and §10.2 "Compatibility". These are the spec's
// TestTagFacetsMigration_* tests; they live here on tagFacetsDB rather than in db_migrations_test.go,
// whose database helper skips without SENTINEL_E2E=1 (plan §7 A2: contract decision 5 forbids a proof
// that can skip).

var tfFunctions = []string{"issue_tag_derived", "issue_tag_pairs", "issue_tag_counted", "issue_tag_values_add",
	"issue_tag_values_rebuild_locked", "issue_tag_values_subtract", "error_occurrences_tag_rollup_mark",
	"error_occurrences_tag_rollup_ins", "error_occurrences_tag_rollup_del", "error_occurrences_tag_rollup_upd",
	"error_occurrences_tag_rollup_trunc", "issue_tag_values_backfill_batch", "issue_tag_values_rebuild"}

var tfRelations = []string{"issue_tag_values", "issue_tag_values_pkey", "issue_tag_values_backfill",
	"issue_tag_values_backfill_pkey", "issue_tag_values_drift", "issue_tag_values_drift_pkey",
	"idx_issue_tag_values_key_value", "idx_error_occurrences_user_id", "idx_error_occurrences_device_model",
	"idx_error_occurrences_issue_platform", "idx_error_occurrences_issue_tags"}

type tfObjects struct{ Triggers, Functions, Relations, Columns int64 }

func tfCountObjects(t *testing.T, db *sql.DB) tfObjects {
	t.Helper()
	var o tfObjects
	err := db.QueryRow(`
		SELECT (SELECT count(*) FROM pg_trigger WHERE tgrelid = 'error_occurrences'::regclass
		          AND tgname LIKE 'trg_error_occurrences_tag_rollup_%' AND NOT tgisinternal),
		       (SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		         WHERE n.nspname = current_schema() AND p.proname = ANY($1)),
		       (SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		         WHERE n.nspname = current_schema() AND c.relname = ANY($2) AND c.relkind IN ('r', 'i')),
		       (SELECT count(*) FROM pg_attribute WHERE attrelid = 'error_occurrences'::regclass
		          AND attname = 'tag_rollup_applied' AND NOT attisdropped)`,
		tfFunctions, tfRelations).Scan(&o.Triggers, &o.Functions, &o.Relations, &o.Columns)
	if err != nil {
		t.Fatalf("count objects: %v", err)
	}
	return o
}

// tfLedgerAt creates a goose ledger `name` that has applied every version below `version` (a copy of
// the helper's ledger up to there), so `up` under it replays exactly the tag-facets file: a second
// ledger meeting the same database (spec T15).
func tfLedgerAt(t *testing.T, db *sql.DB, name string, version int64) {
	t.Helper()
	if _, err := db.Exec(fmt.Sprintf(`CREATE TABLE %q (LIKE %q INCLUDING ALL)`, name, tagFacetsLedger)); err != nil {
		t.Fatalf("create ledger %s: %v", name, err)
	}
	if _, err := db.Exec(fmt.Sprintf(`INSERT INTO %q (version_id, is_applied, tstamp)
		SELECT version_id, is_applied, tstamp FROM %q WHERE version_id < $1 ORDER BY id`, name, tagFacetsLedger), version); err != nil {
		t.Fatalf("fill ledger %s: %v", name, err)
	}
}

func TestTagFacetsMigration_ReplayAcrossLedgers(t *testing.T) {
	_, version, _ := tagFacetsMigrationFile(t)
	env := tagFacetsDB(t, tagFacetsOpts{}) // ledger A
	ctx := context.Background()
	projectID := tfSeedProject(t, env.Pool)
	issueID := tfSeedIssue(t, env.Pool, projectID, "production")
	tfInsertOccs(t, env.Pool, []tfOcc{{IssueID: issueID, ProjectID: projectID, Release: "1.0"}})
	cursor := uuid.NewString()
	if _, err := env.Pool.Exec(ctx, `UPDATE issue_tag_values_backfill SET cursor_id = $1, rows_counted = 7`, cursor); err != nil {
		t.Fatalf("set cursor: %v", err)
	}
	tfLedgerAt(t, env.DB, "tagfacets_ledger_b", version)
	tfLedgerAt(t, env.DB, "tagfacets_ledger_c", version)

	// A holder has read error_occurrences: ACCESS SHARE, which conflicts only with ACCESS EXCLUSIVE.
	holder := tfAcquire(t, env.Pool)
	htx, err := holder.Begin(ctx)
	if err != nil {
		t.Fatalf("holder begin: %v", err)
	}
	defer htx.Rollback(ctx)
	if _, err := htx.Exec(ctx, `SELECT 1 FROM error_occurrences LIMIT 1`); err != nil {
		t.Fatalf("holder read: %v", err)
	}
	for _, ledger := range []string{"tagfacets_ledger_b", "tagfacets_ledger_c"} {
		start := time.Now()
		if err := dbmigrations.RunMigrations(ctx, env.DB, "up", dbmigrations.MigrationOptions{TableName: ledger, Directory: tagFacetsMigrationsDir}); err != nil {
			t.Fatalf("replay under %s while a reader holds ACCESS SHARE: %v", ledger, err)
		}
		if n := tfScalar(t, env.Pool, fmt.Sprintf(`SELECT count(*) FROM %q WHERE version_id = $1 AND is_applied`, ledger), version); n != 1 {
			t.Fatalf("%s did not record %d", ledger, version)
		}
		t.Logf("replay under %s: %s", ledger, time.Since(start))
	}
	if err := htx.Commit(ctx); err != nil {
		t.Fatalf("holder commit: %v", err)
	}
	if o := tfCountObjects(t, env.DB); o != (tfObjects{5, 13, 11, 1}) {
		t.Fatalf("after two replays: %+v, want one of each trigger, 13 functions, 11 relations, 1 column", o)
	}
	var gotCursor string
	var rows int64
	if err := env.Pool.QueryRow(ctx, `SELECT cursor_id::text, rows_counted FROM issue_tag_values_backfill`).Scan(&gotCursor, &rows); err != nil {
		t.Fatalf("read state: %v", err)
	}
	if n := tfScalar(t, env.Pool, `SELECT count(*) FROM issue_tag_values_backfill`); n != 1 || gotCursor != cursor || rows != 7 {
		t.Fatalf("a replay changed the backfill state: %d row(s), cursor %s (want %s), rows_counted %d (want 7)", n, gotCursor, cursor, rows)
	}
	tfCheck(t, env.Pool, issueID)
}

func TestTagFacetsMigration_DownThenUp(t *testing.T) {
	_, version, _ := tagFacetsMigrationFile(t)
	env := tagFacetsDB(t, tagFacetsOpts{UpTo: version})
	ctx := context.Background()
	opts := dbmigrations.MigrationOptions{TableName: tagFacetsLedger, Directory: tagFacetsMigrationsDir}
	if o := tfCountObjects(t, env.DB); o != (tfObjects{5, 13, 11, 1}) {
		t.Fatalf("after up: %+v", o)
	}
	if err := dbmigrations.RunMigrations(ctx, env.DB, "down", opts); err != nil {
		t.Fatalf("down: %v", err)
	}
	if o := tfCountObjects(t, env.DB); o != (tfObjects{}) {
		t.Fatalf("after down: %+v, want nothing left", o)
	}
	if n := tfScalar(t, env.Pool, `SELECT count(*) FROM pg_class WHERE relname LIKE 'idx_%_invalid' OR relname LIKE 'issue_tag_values%'`); n != 0 {
		t.Fatalf("down left %d relation(s)", n)
	}
	if err := dbmigrations.RunMigrations(ctx, env.DB, "up", opts); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if o := tfCountObjects(t, env.DB); o != (tfObjects{5, 13, 11, 1}) {
		t.Fatalf("after down, up: %+v", o)
	}
	projectID := tfSeedProject(t, env.Pool)
	issueID := tfSeedIssue(t, env.Pool, projectID, "production")
	tfInsertOccs(t, env.Pool, []tfOcc{{IssueID: issueID, ProjectID: projectID, Release: "1.0"}})
	tfCheck(t, env.Pool, issueID)
}

func TestTagFacetsMigration_OldInsertWorks(t *testing.T) {
	env := tagFacetsDB(t, tagFacetsOpts{})
	projectID := tfSeedProject(t, env.Pool)
	issueID := tfSeedIssue(t, env.Pool, projectID, "production")
	tag, err := env.Pool.Exec(context.Background(), tfOldProcessorInsert, tfOldInsertArgs(uuid.NewString(), issueID, projectID, "1.1", "evt-old-works")...)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("the f49721f insert text failed after the migration: %v (%d rows)", err, tag.RowsAffected())
	}
	tfCheck(t, env.Pool, issueID)
}

func TestTagFacetsMigration_BoundedLockWait(t *testing.T) {
	_, version, prev := tagFacetsMigrationFile(t)
	env := tagFacetsDB(t, tagFacetsOpts{UpTo: prev})
	ctx := context.Background()

	holder := tfAcquire(t, env.Pool)
	htx, err := holder.Begin(ctx)
	if err != nil {
		t.Fatalf("holder begin: %v", err)
	}
	defer htx.Rollback(ctx)
	if _, err := htx.Exec(ctx, `SELECT 1 FROM error_occurrences LIMIT 1`); err != nil {
		t.Fatalf("holder read: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- dbmigrations.RunMigrations(ctx, env.DB, "up", dbmigrations.MigrationOptions{TableName: tagFacetsLedger, Directory: tagFacetsMigrationsDir})
	}()

	// B queues for ACCESS EXCLUSIVE on error_occurrences (block C) ...
	var bPID int64
	deadline := time.Now().Add(10 * time.Second)
	for bPID == 0 && time.Now().Before(deadline) {
		_ = env.Pool.QueryRow(ctx, `SELECT coalesce((SELECT pid FROM pg_locks WHERE relation = 'error_occurrences'::regclass
			AND mode = 'AccessExclusiveLock' AND NOT granted LIMIT 1), 0)`).Scan(&bPID)
		if bPID == 0 {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if bPID == 0 {
		t.Fatalf("the migration never queued for ACCESS EXCLUSIVE on error_occurrences")
	}
	// ... then gives the queue back and sleeps between attempts, while the holder is still open.
	gaveBack := false
	deadline = time.Now().Add(10 * time.Second)
	for !gaveBack && time.Now().Before(deadline) {
		var ungranted int64
		var waitEvent sql.NullString
		_ = env.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM pg_locks WHERE pid = $1 AND NOT granted),
			(SELECT wait_event FROM pg_stat_activity WHERE pid = $1)`, bPID).Scan(&ungranted, &waitEvent)
		if ungranted == 0 && waitEvent.String == "PgSleep" {
			gaveBack = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !gaveBack {
		t.Fatalf("the migration never gave the lock queue back while a reader held ACCESS SHARE (no bounded wait)")
	}
	probe := tfAcquire(t, env.Pool)
	if _, err := probe.Exec(ctx, `SET statement_timeout = '500ms'`); err != nil {
		t.Fatalf("probe timeout: %v", err)
	}
	start := time.Now()
	if _, err := probe.Exec(ctx, `SELECT 1 FROM error_occurrences LIMIT 1`); err != nil {
		t.Fatalf("a read between attempts waited behind the migration: %v", err)
	}
	t.Logf("probe read between attempts: %s", time.Since(start))
	if err := htx.Commit(ctx); err != nil {
		t.Fatalf("holder commit: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("migration after the holder committed: %v", err)
		}
	case <-time.After(3 * time.Minute):
		t.Fatalf("the migration did not finish within 3 minutes of the holder committing")
	}
	if n := tfScalar(t, env.Pool, fmt.Sprintf(`SELECT count(*) FROM %q WHERE version_id = $1 AND is_applied`, tagFacetsLedger), version); n != 1 {
		t.Fatalf("the ledger did not record %d", version)
	}
}

// TestTagFacetsMigration_DownBoundedLockWait proves tfr-down-drop-table-access-exclusive-issues: Down's
// DROP TABLE issue_tag_values (its REFERENCES issues(id) FK takes AccessExclusiveLock on issues to drop)
// is a bounded-wait DO block like Up's blocks C/T1/TR, not a plain statement that queues behind a reader
// indefinitely.
func TestTagFacetsMigration_DownBoundedLockWait(t *testing.T) {
	// Up to and including the tag-facets migration, so the single down step below is its Down
	// whatever migrations are added after it.
	_, version, _ := tagFacetsMigrationFile(t)
	env := tagFacetsDB(t, tagFacetsOpts{UpTo: version})
	ctx := context.Background()
	opts := dbmigrations.MigrationOptions{TableName: tagFacetsLedger, Directory: tagFacetsMigrationsDir}

	holder := tfAcquire(t, env.Pool)
	htx, err := holder.Begin(ctx)
	if err != nil {
		t.Fatalf("holder begin: %v", err)
	}
	defer htx.Rollback(ctx)
	if _, err := htx.Exec(ctx, `SELECT 1 FROM issues LIMIT 1`); err != nil {
		t.Fatalf("holder read: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- dbmigrations.RunMigrations(ctx, env.DB, "down", opts)
	}()

	// The Down migration queues for ACCESS EXCLUSIVE on issues (the issue_tag_values table drop) ...
	var bPID int64
	deadline := time.Now().Add(10 * time.Second)
	for bPID == 0 && time.Now().Before(deadline) {
		_ = env.Pool.QueryRow(ctx, `SELECT coalesce((SELECT pid FROM pg_locks WHERE relation = 'issues'::regclass
			AND mode = 'AccessExclusiveLock' AND NOT granted LIMIT 1), 0)`).Scan(&bPID)
		if bPID == 0 {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if bPID == 0 {
		t.Fatalf("the Down migration never queued for ACCESS EXCLUSIVE on issues (issue_tag_values's FK drop)")
	}
	// ... then gives the queue back and sleeps between attempts, while the holder is still open.
	gaveBack := false
	deadline = time.Now().Add(10 * time.Second)
	for !gaveBack && time.Now().Before(deadline) {
		var ungranted int64
		var waitEvent sql.NullString
		_ = env.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM pg_locks WHERE pid = $1 AND NOT granted),
			(SELECT wait_event FROM pg_stat_activity WHERE pid = $1)`, bPID).Scan(&ungranted, &waitEvent)
		if ungranted == 0 && waitEvent.String == "PgSleep" {
			gaveBack = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !gaveBack {
		t.Fatalf("the Down migration never gave the lock queue back while a reader held ACCESS SHARE on issues (no bounded wait)")
	}
	probe := tfAcquire(t, env.Pool)
	if _, err := probe.Exec(ctx, `SET statement_timeout = '500ms'`); err != nil {
		t.Fatalf("probe timeout: %v", err)
	}
	start := time.Now()
	if _, err := probe.Exec(ctx, `SELECT 1 FROM issues LIMIT 1`); err != nil {
		t.Fatalf("a read between attempts waited behind the Down migration: %v", err)
	}
	t.Logf("probe read between attempts: %s", time.Since(start))
	if err := htx.Commit(ctx); err != nil {
		t.Fatalf("holder commit: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Down migration after the holder committed: %v", err)
		}
	case <-time.After(3 * time.Minute):
		t.Fatalf("the Down migration did not finish within 3 minutes of the holder committing")
	}
	if n := tfScalar(t, env.Pool, `SELECT count(*) FROM pg_class WHERE relname = 'issue_tag_values'`); n != 0 {
		t.Fatalf("issue_tag_values still exists after Down")
	}
}
