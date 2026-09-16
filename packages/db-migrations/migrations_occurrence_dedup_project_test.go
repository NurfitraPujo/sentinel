package dbmigrations

// sentinel-occurrence-dedup-project §4.2 (TASK-01; contract §11 row 2; flows M1–M9). Replays the
// feature's one migration against a real Postgres in the migrations_sourcemaps_test.go pattern:
//
//	migrations/1725200000_add_error_occurrences_project_id_dedup.sql
//	    the nullable `error_occurrences.project_id` column, the batched backfill from
//	    issues.project_id, the duplicate gate, drop-if-invalid, the CONCURRENTLY unique
//	    (project_id, event_id) index and its indisvalid check -- the file runs under goose's
//	    no-transaction directive, so a failure leaves every earlier block applied and the
//	    ledger unadvanced, and the next run replays the file top to bottom.
//
// up -> down -> up, then the whole directory again under two more goose ledgers on the SAME
// database -- the situation hazard sentinel_goose_target names: one flat directory, three
// ledgers, one physical database, so every block runs again on a schema that already has
// every object it creates and every row the first pass rewrote.
//
// What is asserted is the SHAPE the contract paints (§3: `project_id` uuid, nullable, no
// default, no FK; the index's exact pg_indexes definition, unique and partial on both columns
// being non-NULL; pg_index.indisvalid = true after every replay; the old
// uq_error_occurrences_issue_event untouched -- decision 5) and the behaviours only a live
// database can prove:
//
//   - the backfill (M3): every pre-existing row's project_id equals its issue's, the row with
//     no event_id included (it is outside the key, not outside the column);
//   - the duplicate gate (M4): a seeded (project_id, event_id) pair with two rows -- the same
//     event_id on two issues of one project, the cross-issue redelivery this feature closes --
//     makes the Up FAIL before the build, with the group count in the message, no index of the
//     name created, the column and the backfill left applied and the ledger unadvanced; the
//     RUNBOOK recipe (O2) run by the test, not the migration (decision 4), and the re-run passes;
//   - the gate's session check (decision 16): the same block RAISEs by name when
//     current_setting('statement_timeout') is not '15min';
//   - drop-if-invalid (M5): a failed CONCURRENTLY build left in the database -- an index of the
//     name that EXISTS with indisvalid = false, produced the real way, by building over a
//     violating pair -- is dropped and rebuilt valid on replay (a new OID), and a VALID index is
//     never dropped by a replay (the same OID under the second and third ledgers);
//   - the key itself: a second (project_id, event_id) on another issue of the project is refused
//     by the index's name; the same event_id under another project stores (R4, the tenancy pin);
//     a row with project_id NULL is outside the key (R3);
//   - Down (M9) removes the index, the column and the ledger row, the second Up recreates them;
//   - the file's text: the no-transaction directive on line 1 and nowhere else, the lock_timeout
//     SETs immediately before the ALTER block and immediately before the build (decision 16),
//     IF NOT EXISTS on the column and the index, CONCURRENTLY on every DROP INDEX, no DELETE, no
//     DROP of the old index (decision 21).
//
// Same database contract as migrations_oncall_weekday_test.go: MIGRATIONS_TEST_DB_URL at a
// DISPOSABLE Postgres whose role may CREATE DATABASE, self-skipping without one,
// MIGRATIONS_TEST_REQUIRED=1 to make the skip a failure. Never the shared dev database.
//
//	MIGRATIONS_TEST_DB_URL='postgres://sentinel:changeme@localhost:55432/sentinel?sslmode=disable' \
//	  GOWORK=off go test ./... -run 'OccurrenceDedupProject' -count=1 -v

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pressly/goose/v3"
)

const (
	// 1725100000 is the sourcemaps table -- the last version before this feature's file.
	occurrenceDedupBaseVersion int64 = 1725100000
	occurrenceDedupVersion     int64 = 1725200000

	occurrenceDedupFile = "1725200000_add_error_occurrences_project_id_dedup.sql"

	occurrenceDedupIndex        = "uq_error_occurrences_project_event"
	occurrenceDedupInvalidIndex = "uq_error_occurrences_project_event_invalid"
	occurrenceDedupOldIndex     = "uq_error_occurrences_issue_event"

	// pg_indexes.indexdef of contract §4.2's definition, as Postgres renders it: the same
	// columns, unique, partial on both being non-NULL.
	occurrenceDedupIndexDef = "CREATE UNIQUE INDEX uq_error_occurrences_project_event ON public.error_occurrences USING btree (project_id, event_id) WHERE ((project_id IS NOT NULL) AND (event_id IS NOT NULL))"

	// The build statement exactly as §4.2 writes it, which the text test pins verbatim.
	occurrenceDedupBuildStatement = "CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_error_occurrences_project_event ON error_occurrences (project_id, event_id) WHERE project_id IS NOT NULL AND event_id IS NOT NULL;"
)

// TestOccurrenceDedupProjectReplay drives the file up (refused by the gate) -> up -> down ->
// up (over a seeded invalid index) over a database seeded with occurrences and asserts the
// schema, the backfill and the key in each state, then replays the directory under two more
// ledgers and asserts the valid index was left alone.
func TestOccurrenceDedupProjectReplay(t *testing.T) {
	db, cleanup := freshMigrationDatabase(t, "occdedup")
	defer cleanup()

	ctx := context.Background()
	migrateTo(t, db, occurrenceDedupBaseVersion)
	// Every ledger is at the base version before the feature lands -- production's shape when
	// the migrate Job runs this release, and the only order the directory permits with issue
	// rows present (1724400000_add_project_environments.sql refuses to run on a database whose
	// `issues` has rows). Each ledger then applies exactly this feature's file.
	for _, ledger := range []string{"dashboard_migrations", "processor_migrations"} {
		replayUnderLedger(t, db, ledger, occurrenceDedupBaseVersion)
	}
	assertOccurrenceDedupShape(t, db, false)

	// Project P with two issues, project Q with one. The pre-release population: rows with an
	// event_id, one without (outside the key, R3), the same event_id under both projects (R4),
	// and ONE violating pair -- event e9 stored on both of P's issues, the cross-issue redelivery
	// this feature exists to close, which §4.4's O1 would have counted and O2 would have handled
	// before the tag. Here it is left in so the gate is exercised.
	projectP, issueI1 := seedProjectIssue(t, db, "occdedup-p", "issue-i1")
	issueI2 := seedIssueInProject(t, db, projectP, "issue-i2")
	projectQ, issueJ1 := seedProjectIssue(t, db, "occdedup-q", "issue-j1")
	base := time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC)
	seedKeyedOccurrence(t, db, issueI1, "e1", base)
	seedKeyedOccurrence(t, db, issueI1, "e2", base.Add(time.Minute))
	seedKeyedOccurrence(t, db, issueI2, "e3", base.Add(2*time.Minute))
	seedKeyedOccurrence(t, db, issueJ1, "e1", base.Add(3*time.Minute))
	seedKeyedOccurrence(t, db, issueI1, "", base.Add(4*time.Minute))
	seedKeyedOccurrence(t, db, issueI1, "e9", base.Add(5*time.Minute))
	laterE9 := seedKeyedOccurrence(t, db, issueI2, "e9", base.Add(6*time.Minute))
	assertRowCount(t, db, "error_occurrences", 7)

	// First up: REFUSED by the gate (M4), by name, with the group count, before any index is
	// touched. The blocks before it stay applied (the file runs outside a transaction): the
	// column exists and the backfill has run -- which is what makes the gate able to group on
	// the column at all -- and the ledger has not advanced.
	goose.SetTableName("schema_migrations")
	err := goose.UpToContext(ctx, db, migrationsDir(), occurrenceDedupVersion)
	if err == nil {
		t.Fatal("goose up succeeded over a seeded (project_id, event_id) pair with two rows; the gate (c) must refuse it")
	}
	if !strings.Contains(err.Error(), "1 (project_id, event_id) group(s)") {
		t.Errorf("the gate's failure does not carry the group count: %v", err)
	}
	if !strings.Contains(err.Error(), occurrenceDedupIndex) {
		t.Errorf("the gate's failure does not name the index it protects: %v", err)
	}
	assertIndexExists(t, db, occurrenceDedupIndex, false)
	assertIndexExists(t, db, occurrenceDedupInvalidIndex, false)
	assertColumnExists(t, db, "error_occurrences", "project_id", true)
	assertLedgerHas(t, db, "schema_migrations", occurrenceDedupVersion, false)
	assertOccurrenceDedupBackfilled(t, db)
	assertRowCount(t, db, "error_occurrences", 7)

	// The RUNBOOK recipe (O2, decision 13): keep min(created_at), delete the later row. Run by
	// the test as the operator would, never by the migration (decision 4).
	if _, err := db.Exec(`DELETE FROM error_occurrences WHERE id = $1`, laterE9); err != nil {
		t.Fatalf("delete the later e9 row: %v", err)
	}

	// Second attempt on the same ledger: the column is found, the backfill has nothing left, the
	// gate counts zero, nothing to drop, the build runs, the index is valid.
	migrateTo(t, db, occurrenceDedupVersion)
	assertOccurrenceDedupShape(t, db, true)
	assertLedgerHas(t, db, "schema_migrations", occurrenceDedupVersion, true)
	assertOccurrenceDedupBackfilled(t, db)
	assertRowCount(t, db, "error_occurrences", 6)

	// The key. A second (P, e1) on ANOTHER issue of P is refused by the index's name -- the
	// cross-issue duplicate. The same e2 under Q stores (R4). A row whose project_id is NULL is
	// outside the key (R3) even when its event_id collides. The two probes are removed before
	// Down so the second Up's backfill and gate see the original population.
	assertRejectedBy(t, db,
		`INSERT INTO error_occurrences (issue_id, environment, platform, event_id, project_id)
		 VALUES ('`+issueI2+`', 'production', 'go', 'e1', '`+projectP+`')`,
		occurrenceDedupIndex)
	acceptedQ := seedKeyedOccurrenceWithProject(t, db, issueJ1, "e2", projectQ)
	acceptedNull := seedKeyedOccurrenceWithProject(t, db, issueI2, "e1", "")
	assertRowCount(t, db, "error_occurrences", 8)
	if _, err := db.Exec(`DELETE FROM error_occurrences WHERE id IN ($1, $2)`, acceptedQ, acceptedNull); err != nil {
		t.Fatalf("remove the key probes: %v", err)
	}

	// Down: the index, the column and the ledger row go; the old index and every row stay.
	if err := goose.DownToContext(ctx, db, migrationsDir(), occurrenceDedupBaseVersion); err != nil {
		t.Fatalf("goose down to %d failed: %v", occurrenceDedupBaseVersion, err)
	}
	assertOccurrenceDedupShape(t, db, false)
	assertLedgerHas(t, db, "schema_migrations", occurrenceDedupVersion, false)
	assertRowCount(t, db, "error_occurrences", 6)

	// A previous run's failed CONCURRENTLY build (M5's precondition), produced the real way: the
	// column and the backfill applied by an earlier attempt, a violating pair that arrived after
	// the gate, and a concurrent unique build that fails at its scan with 23505 and leaves an
	// index of the name that EXISTS and is NOT valid. Without block (d), IF NOT EXISTS would
	// skip the build and check (e) would refuse the file forever.
	mustExec(t, db, `ALTER TABLE error_occurrences ADD COLUMN project_id UUID`)
	mustExec(t, db, `UPDATE error_occurrences o SET project_id = i.project_id FROM issues i WHERE i.id = o.issue_id`)
	violating := seedKeyedOccurrenceWithProject(t, db, issueI2, "e1", projectP)
	if _, err := db.Exec(occurrenceDedupBuildStatement); err == nil {
		t.Fatal("a CONCURRENTLY unique build over a violating pair succeeded; the seeded invalid index cannot be produced")
	} else if !isSQLState(err, "23505") {
		t.Fatalf("the seeding build failed with %v, want 23505", err)
	}
	invalidOID := readIndexOID(t, db, occurrenceDedupIndex)
	if valid := readIndexValid(t, db, occurrenceDedupIndex); valid {
		t.Fatal("the failed concurrent build left a VALID index; the drop-if-invalid path cannot be exercised")
	}
	if _, err := db.Exec(`DELETE FROM error_occurrences WHERE id = $1`, violating); err != nil {
		t.Fatalf("remove the violating row: %v", err)
	}

	// Second up over the invalid index: (d) drops it, the build recreates it valid -- a NEW
	// index, not the old one found and kept.
	migrateTo(t, db, occurrenceDedupVersion)
	assertOccurrenceDedupShape(t, db, true)
	assertLedgerHas(t, db, "schema_migrations", occurrenceDedupVersion, true)
	assertOccurrenceDedupBackfilled(t, db)
	rebuiltOID := readIndexOID(t, db, occurrenceDedupIndex)
	if rebuiltOID == invalidOID {
		t.Errorf("after replay %s still has OID %d -- the invalid index was kept, not dropped and rebuilt", occurrenceDedupIndex, invalidOID)
	}
	assertIndexExists(t, db, occurrenceDedupInvalidIndex, false)
	assertRowCount(t, db, "error_occurrences", 6)

	// The second and third ledgers replay the file on a database that already has every object
	// it creates: the column is found, the backfill has nothing to do, the gate counts zero, the
	// VALID index is left alone (same OID -- (d) must never drop a valid index), the build is
	// skipped by IF NOT EXISTS, (e) passes, and the rows survive.
	for _, ledger := range []string{"dashboard_migrations", "processor_migrations"} {
		replayUnderLedger(t, db, ledger, occurrenceDedupVersion)
		assertLedgerHas(t, db, ledger, occurrenceDedupVersion, true)
		assertOccurrenceDedupShape(t, db, true)
		assertOccurrenceDedupBackfilled(t, db)
		if got := readIndexOID(t, db, occurrenceDedupIndex); got != rebuiltOID {
			t.Errorf("replay under %s rebuilt %s (OID %d -> %d); a valid index must be left alone", ledger, occurrenceDedupIndex, rebuiltOID, got)
		}
		assertRowCount(t, db, "error_occurrences", 6)
	}
}

// TestOccurrenceDedupProjectGateChecksSession runs gate block (c) on its own, out of the file's
// text, in a session whose statement_timeout is and is not '15min' (decision 16): the SETs of
// the first block are session-scoped and reach the gate only on the same pooled connection, so
// the gate refuses to run the build behind a setting it cannot see. And with the setting in
// place it counts the groups on the column -- two seeded groups read back as 2.
func TestOccurrenceDedupProjectGateChecksSession(t *testing.T) {
	db, cleanup := freshMigrationDatabase(t, "occgate")
	defer cleanup()

	migrateTo(t, db, occurrenceDedupVersion)
	gate := gooseBlockContaining(t, occurrenceDedupFile, "current_setting('statement_timeout')")

	run := func(statementTimeout string) error {
		tx, err := db.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback()
		if statementTimeout != "" {
			if _, err := tx.Exec(`SET LOCAL statement_timeout = '` + statementTimeout + `'`); err != nil {
				t.Fatalf("set statement_timeout: %v", err)
			}
		}
		_, err = tx.Exec(strings.Join(gate, "\n"))
		return err
	}

	// The session default (0): refused by name.
	err := run("")
	if err == nil {
		t.Fatal("gate (c) passed with statement_timeout at its default; decision 16 says it must RAISE")
	}
	if !strings.Contains(err.Error(), "statement_timeout") {
		t.Errorf("gate (c)'s refusal does not name statement_timeout: %v", err)
	}
	// A different, non-default value: still refused.
	if err := run("10min"); err == nil || !strings.Contains(err.Error(), "statement_timeout") {
		t.Errorf("gate (c) with statement_timeout = '10min': got %v, want a refusal naming statement_timeout", err)
	}
	// '15min' and no groups: passes.
	if err := run("15min"); err != nil {
		t.Errorf("gate (c) with statement_timeout = '15min' and no duplicate group failed: %v", err)
	}

	// '15min' and two groups on the column: refused with the count. The seed writes the
	// column directly (the index is dropped for the seed, since it would refuse the rows).
	projectP, issueI1 := seedProjectIssue(t, db, "occgate-p", "issue-i1")
	issueI2 := seedIssueInProject(t, db, projectP, "issue-i2")
	mustExec(t, db, `DROP INDEX CONCURRENTLY `+occurrenceDedupIndex)
	seedKeyedOccurrenceWithProject(t, db, issueI1, "g1", projectP)
	seedKeyedOccurrenceWithProject(t, db, issueI2, "g1", projectP)
	seedKeyedOccurrenceWithProject(t, db, issueI1, "g2", projectP)
	seedKeyedOccurrenceWithProject(t, db, issueI2, "g2", projectP)
	// A third pair with project_id NULL is outside the predicate and must not be counted.
	seedKeyedOccurrenceWithProject(t, db, issueI1, "g3", "")
	seedKeyedOccurrenceWithProject(t, db, issueI2, "g3", "")
	err = run("15min")
	if err == nil {
		t.Fatal("gate (c) passed over two (project_id, event_id) groups")
	}
	if !strings.Contains(err.Error(), "2 (project_id, event_id) group(s)") {
		t.Errorf("gate (c)'s refusal does not carry the group count 2: %v", err)
	}
}

// TestOccurrenceDedupProjectFileIsShapedAsDecided pins the file's text: the no-transaction
// directive on line 1 and that string nowhere else (§3's goose annotation hazard), every
// annotation one goose knows, SET lock_timeout = '5s' immediately before the ALTER block and
// SET lock_timeout = 0 immediately before the build (decision 16), statement_timeout = '15min',
// IF NOT EXISTS on the column and the index, the build statement verbatim, CONCURRENTLY on every
// DROP INDEX, no DELETE, no DROP of uq_error_occurrences_issue_event (decisions 4, 5, 21), the
// gate on the index's own predicate grouped on the column, the backfill's batch size stated in
// the header and used in the block (decision 20).
func TestOccurrenceDedupProjectFileIsShapedAsDecided(t *testing.T) {
	lines := fileLines(t, occurrenceDedupFile)
	if len(lines) == 0 || lines[0] != "-- +goose NO TRANSACTION" {
		t.Fatalf("%s: line 1 is %q, want the no-transaction directive (§4.2)", occurrenceDedupFile, lines[0])
	}
	if got := firstGooseAnnotation(t, occurrenceDedupFile); got != "NO TRANSACTION" {
		t.Errorf("%s: first goose annotation is %q", occurrenceDedupFile, got)
	}
	var directiveCount int
	known := map[string]bool{"NO TRANSACTION": true, "Up": true, "Down": true, "StatementBegin": true, "StatementEnd": true}
	for i, line := range lines {
		directiveCount += strings.Count(line, "NO TRANSACTION")
		if strings.HasPrefix(strings.TrimSpace(line), "--") && strings.Contains(line, "+goose") {
			_, annotation, _ := strings.Cut(line, "+goose")
			if !known[strings.TrimSpace(annotation)] {
				t.Errorf("%s:%d: %q is not an annotation goose knows; goose scans every comment line and fails the file (§3)", occurrenceDedupFile, i+1, line)
			}
		}
	}
	if directiveCount != 1 {
		t.Errorf("%s: the directive string occurs %d times, want exactly once, on line 1 (§3)", occurrenceDedupFile, directiveCount)
	}

	up, down := gooseBlocks(t, occurrenceDedupFile)
	sqlOf := func(block []string) []string {
		var out []string
		for _, line := range block {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "--") {
				continue
			}
			out = append(out, trimmed)
		}
		return out
	}
	joined := func(block []string) string { return strings.Join(strings.Fields(strings.Join(sqlOf(block), " ")), " ") }

	alterAt, buildAt := -1, -1
	for i, block := range up {
		s := sqlOf(block)
		if len(s) == 0 {
			continue
		}
		switch {
		case strings.HasPrefix(s[0], "ALTER TABLE error_occurrences ADD COLUMN IF NOT EXISTS project_id UUID"):
			alterAt = i
		case strings.HasPrefix(s[0], "CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_error_occurrences_project_event"):
			buildAt = i
		}
	}
	if alterAt < 1 {
		t.Fatalf("%s: no Up block starts with ALTER TABLE error_occurrences ADD COLUMN IF NOT EXISTS project_id UUID, or it is the first block", occurrenceDedupFile)
	}
	if buildAt < 1 {
		t.Fatalf("%s: no Up block starts with CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS %s, or it is the first block", occurrenceDedupFile, occurrenceDedupIndex)
	}
	// Decision 16: the block immediately before the ALTER ends with the 5s lock guard and also
	// sets the statement budget; the block immediately before the build is the lock guard's
	// release and nothing else.
	before := sqlOf(up[alterAt-1])
	if len(before) == 0 || before[len(before)-1] != "SET lock_timeout = '5s';" {
		t.Errorf("%s: the block before the ALTER block ends with %q, want SET lock_timeout = '5s'; (decision 16)", occurrenceDedupFile, before)
	}
	if !strings.Contains(joined(up[alterAt-1]), "SET statement_timeout = '15min';") {
		t.Errorf("%s: the block before the ALTER block does not SET statement_timeout = '15min' (decision 16)", occurrenceDedupFile)
	}
	if got := joined(up[buildAt-1]); got != "SET lock_timeout = 0;" {
		t.Errorf("%s: the block before the build is %q, want exactly SET lock_timeout = 0; (decision 16)", occurrenceDedupFile, got)
	}
	if got := joined(up[buildAt]); got != occurrenceDedupBuildStatement {
		t.Errorf("%s: the build block is %q, want §4.2's statement verbatim %q", occurrenceDedupFile, got, occurrenceDedupBuildStatement)
	}
	// Guard order (c) -> (d) -> build -> (e): the gate and the rename both come before the
	// build, the indisvalid check after it, the RESETs last.
	gateAt, renameAt, checkAt, resetAt := -1, -1, -1, -1
	for i, block := range up {
		j := joined(block)
		switch {
		case strings.Contains(j, "current_setting('statement_timeout')"):
			gateAt = i
		case strings.Contains(j, "RENAME TO "+occurrenceDedupInvalidIndex):
			renameAt = i
		case strings.Contains(j, "RESET statement_timeout;"):
			resetAt = i
		case strings.Contains(j, "indisvalid") && i > buildAt:
			checkAt = i
		}
	}
	if !(alterAt < gateAt && gateAt < renameAt && renameAt < buildAt && buildAt < checkAt && checkAt < resetAt && resetAt == len(up)-1) {
		t.Errorf("%s: block order is alter=%d gate=%d rename=%d build=%d check=%d reset=%d of %d; want alter < gate < rename < build < check < reset = last (§4.2)",
			occurrenceDedupFile, alterAt, gateAt, renameAt, buildAt, checkAt, resetAt, len(up))
	}
	// The gate: the index's own predicate, grouped on the column, never via the issues join
	// (decision 21).
	if gateAt >= 0 {
		g := joined(up[gateAt])
		if !strings.Contains(g, "WHERE project_id IS NOT NULL AND event_id IS NOT NULL") || !strings.Contains(g, "GROUP BY project_id, event_id") || !strings.Contains(g, "HAVING count(*) > 1") {
			t.Errorf("%s: gate (c) must filter on the index's predicate and group on (project_id, event_id) with HAVING count(*) > 1 (decision 21): %q", occurrenceDedupFile, g)
		}
		if strings.Contains(strings.ToUpper(g), "JOIN ISSUES") {
			t.Errorf("%s: gate (c) groups via the issues join; decision 21 says the column", occurrenceDedupFile)
		}
	}
	// The rename guard reads indisvalid and touches only the invalid index; the drop-if-invalid
	// is a top-level DROP INDEX CONCURRENTLY IF EXISTS of the renamed index (decision 21).
	if renameAt >= 0 {
		r := joined(up[renameAt])
		if !strings.Contains(r, "NOT indisvalid") {
			t.Errorf("%s: the rename in (d) is not guarded by an indisvalid = false lookup: %q", occurrenceDedupFile, r)
		}
		if got := joined(up[renameAt+1]); got != "DROP INDEX CONCURRENTLY IF EXISTS "+occurrenceDedupInvalidIndex+";" {
			t.Errorf("%s: the block after the rename is %q, want the concurrent drop of the renamed index", occurrenceDedupFile, got)
		}
	}
	// The backfill: batched by id range with a COMMIT per batch, the batch size 10,000 in the
	// block and stated in the header (decision 20).
	backfill := joined(gooseBlockContaining(t, occurrenceDedupFile, "SET project_id = i.project_id"))
	for _, want := range []string{"batch_size CONSTANT integer := 10000;", "AND project_id IS NULL ORDER BY id LIMIT batch_size", "COMMIT;", "AND o.project_id IS NULL;", "FROM issues i WHERE i.id = o.issue_id"} {
		if !strings.Contains(backfill, want) {
			t.Errorf("%s: backfill block lacks %q (decision 20 / §4.2 (b)): %q", occurrenceDedupFile, want, backfill)
		}
	}
	var headerStatesBatch bool
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "--") && strings.Contains(line, "10,000") && strings.Contains(line, "id range") {
			headerStatesBatch = true
			break
		}
	}
	if !headerStatesBatch {
		t.Errorf("%s: the header does not state the 10,000-row id-range batch (decision 20)", occurrenceDedupFile)
	}

	// Every DROP INDEX in the file, Up and Down, is CONCURRENTLY; there is at least one, so the
	// assertion is not vacuous; nothing DELETEs; the old index is never named in SQL.
	var dropCount int
	for _, block := range append(append([][]string{}, up...), down...) {
		for _, line := range sqlOf(block) {
			u := strings.ToUpper(line)
			if strings.Contains(u, "DROP INDEX") {
				dropCount++
				if !strings.Contains(u, "DROP INDEX CONCURRENTLY IF EXISTS") {
					t.Errorf("%s: %q is a DROP INDEX that is not CONCURRENTLY IF EXISTS (decision 21)", occurrenceDedupFile, line)
				}
			}
			if strings.HasPrefix(u, "DELETE") || strings.Contains(u, "DELETE FROM") {
				t.Errorf("%s: %q -- no migration in this feature deletes a row (decision 4)", occurrenceDedupFile, line)
			}
			if strings.Contains(line, occurrenceDedupOldIndex) {
				t.Errorf("%s: %q names %s; the old index is not dropped in this release (decision 5)", occurrenceDedupFile, line, occurrenceDedupOldIndex)
			}
		}
	}
	if dropCount == 0 {
		t.Errorf("%s: no DROP INDEX at all; (d) and Down both need one", occurrenceDedupFile)
	}
	// Down: the index concurrently, then the column, both IF EXISTS.
	d := joined(flatten(down))
	if !strings.Contains(d, "DROP INDEX CONCURRENTLY IF EXISTS "+occurrenceDedupIndex+";") || !strings.Contains(d, "ALTER TABLE error_occurrences DROP COLUMN IF EXISTS project_id;") {
		t.Errorf("%s: Down must drop the index CONCURRENTLY IF EXISTS and the column IF EXISTS (M9): %q", occurrenceDedupFile, d)
	}
	if strings.Index(d, "DROP INDEX CONCURRENTLY IF EXISTS "+occurrenceDedupIndex+";") > strings.Index(d, "DROP COLUMN") {
		t.Errorf("%s: Down drops the column before the index", occurrenceDedupFile)
	}
}

// assertOccurrenceDedupShape checks every object §4.2 creates is present (want=true) or absent
// (want=false): the column (uuid, nullable, no default, no FK) and the index with its exact
// definition, unique, valid; and that the old index is there either way (decision 5).
func assertOccurrenceDedupShape(t *testing.T, db *sql.DB, want bool) {
	t.Helper()

	assertColumnExists(t, db, "error_occurrences", "project_id", want)
	assertIndexExists(t, db, occurrenceDedupIndex, want)
	assertIndexExists(t, db, occurrenceDedupOldIndex, true)
	if !want {
		return
	}

	assertColumnShape(t, db, "error_occurrences", "project_id", "uuid", "YES", "")
	var fkCount int
	if err := db.QueryRow(
		`SELECT count(*) FROM pg_constraint c
		  WHERE c.conrelid = 'error_occurrences'::regclass AND c.contype = 'f'
		    AND (SELECT attnum FROM pg_attribute WHERE attrelid = c.conrelid AND attname = 'project_id') = ANY (c.conkey)`,
	).Scan(&fkCount); err != nil {
		t.Fatalf("count foreign keys on error_occurrences.project_id: %v", err)
	}
	if fkCount != 0 {
		t.Errorf("error_occurrences.project_id carries %d foreign key(s), want none (decision 3)", fkCount)
	}

	var def string
	if err := db.QueryRow(`SELECT indexdef FROM pg_indexes WHERE schemaname = 'public' AND indexname = $1`, occurrenceDedupIndex).Scan(&def); err != nil {
		t.Fatalf("read the definition of %s: %v", occurrenceDedupIndex, err)
	}
	if def != occurrenceDedupIndexDef {
		t.Errorf("%s is\n  %q\nwant §4.2's definition\n  %q", occurrenceDedupIndex, def, occurrenceDedupIndexDef)
	}
	var unique, valid bool
	if err := db.QueryRow(
		`SELECT indisunique, indisvalid FROM pg_index WHERE indexrelid = $1::regclass`, occurrenceDedupIndex,
	).Scan(&unique, &valid); err != nil {
		t.Fatalf("introspect %s: %v", occurrenceDedupIndex, err)
	}
	if !unique {
		t.Errorf("%s is not UNIQUE", occurrenceDedupIndex)
	}
	if !valid {
		t.Errorf("%s has pg_index.indisvalid = false; the key is not enforced (§4.2 (e))", occurrenceDedupIndex)
	}
	assertIndexExists(t, db, occurrenceDedupInvalidIndex, false)
}

// assertOccurrenceDedupBackfilled checks the backfill's result (M3): no row is left with a NULL
// project_id and every row's project_id equals its issue's.
func assertOccurrenceDedupBackfilled(t *testing.T, db *sql.DB) {
	t.Helper()

	var nulls, mismatched int
	if err := db.QueryRow(
		`SELECT count(*) FILTER (WHERE o.project_id IS NULL),
		        count(*) FILTER (WHERE o.project_id IS DISTINCT FROM i.project_id)
		   FROM error_occurrences o JOIN issues i ON i.id = o.issue_id`,
	).Scan(&nulls, &mismatched); err != nil {
		t.Fatalf("read the backfill's result: %v", err)
	}
	if nulls != 0 || mismatched != 0 {
		t.Errorf("after the backfill %d row(s) have project_id NULL and %d differ from their issue's project_id, want 0 and 0 (§4.2 (b))", nulls, mismatched)
	}
}

// seedKeyedOccurrence inserts one pre-release occurrence -- no project_id column yet -- with the
// given event_id ("" seeds a SQL NULL, the "no key" row) at the given instant, returning its id.
func seedKeyedOccurrence(t *testing.T, db *sql.DB, issueID, eventID string, at time.Time) string {
	t.Helper()

	var id string
	if err := db.QueryRow(
		`INSERT INTO error_occurrences (issue_id, environment, platform, event_id, created_at)
		 VALUES ($1, 'production', 'go', NULLIF($2, ''), $3) RETURNING id`,
		issueID, eventID, at,
	).Scan(&id); err != nil {
		t.Fatalf("seed occurrence (%s, %q): %v", issueID, eventID, err)
	}
	return id
}

// seedKeyedOccurrenceWithProject inserts one occurrence with project_id written directly -- what
// the new processor does -- or NULL when projectID is "" (a rollout-window row, §8).
func seedKeyedOccurrenceWithProject(t *testing.T, db *sql.DB, issueID, eventID, projectID string) string {
	t.Helper()

	var id string
	if err := db.QueryRow(
		`INSERT INTO error_occurrences (issue_id, environment, platform, event_id, project_id)
		 VALUES ($1, 'production', 'go', $2, NULLIF($3, '')::uuid) RETURNING id`,
		issueID, eventID, projectID,
	).Scan(&id); err != nil {
		t.Fatalf("seed occurrence (%s, %q, project %q): %v", issueID, eventID, projectID, err)
	}
	return id
}

func readIndexOID(t *testing.T, db *sql.DB, index string) int64 {
	t.Helper()

	var oid int64
	if err := db.QueryRow(`SELECT $1::regclass::oid`, index).Scan(&oid); err != nil {
		t.Fatalf("read the OID of %s: %v", index, err)
	}
	return oid
}

func readIndexValid(t *testing.T, db *sql.DB, index string) bool {
	t.Helper()

	var valid bool
	if err := db.QueryRow(`SELECT indisvalid FROM pg_index WHERE indexrelid = $1::regclass`, index).Scan(&valid); err != nil {
		t.Fatalf("read indisvalid of %s: %v", index, err)
	}
	return valid
}

func isSQLState(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}

// gooseBlocks returns the raw lines of every StatementBegin/StatementEnd block of the file, in
// order, split into the Up and the Down section -- the text goose hands to Postgres one Exec at
// a time under the no-transaction directive.
func gooseBlocks(t *testing.T, file string) (up, down [][]string) {
	t.Helper()

	var (
		section string
		current []string
		inBlock bool
	)
	for _, line := range fileLines(t, file) {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "--") && strings.Contains(trimmed, "+goose") {
			_, annotation, _ := strings.Cut(trimmed, "+goose")
			switch strings.TrimSpace(annotation) {
			case "Up", "Down":
				section = strings.TrimSpace(annotation)
			case "StatementBegin":
				inBlock, current = true, nil
			case "StatementEnd":
				inBlock = false
				switch section {
				case "Up":
					up = append(up, current)
				case "Down":
					down = append(down, current)
				default:
					t.Fatalf("%s: a statement block outside Up/Down", file)
				}
			}
			continue
		}
		if inBlock {
			current = append(current, line)
		}
	}
	if len(up) == 0 {
		t.Fatalf("%s: no Up statement blocks", file)
	}
	return up, down
}

// gooseBlockContaining returns the first Up block whose text contains needle.
func gooseBlockContaining(t *testing.T, file, needle string) []string {
	t.Helper()

	up, _ := gooseBlocks(t, file)
	for _, block := range up {
		if strings.Contains(strings.Join(block, "\n"), needle) {
			return block
		}
	}
	t.Fatalf("%s: no Up block contains %q", file, needle)
	return nil
}

func flatten(blocks [][]string) []string {
	var out []string
	for _, block := range blocks {
		out = append(out, block...)
	}
	return out
}
