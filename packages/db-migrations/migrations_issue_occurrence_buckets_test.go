package dbmigrations

// sentinel-issue-trend §4.6 (TASK-01, criterion 1; flows.md row I10). Replays the two migrations
// of the feature against a real Postgres in the style bd08ca2 established for this package:
//
//	migrations/1724700000_add_issue_occurrence_buckets.sql       the table, its index, the two
//	                                                              `issues` indexes -- DDL only
//	migrations/1724800000_backfill_issue_occurrence_buckets.sql  the one-time backfill, NO
//	                                                              TRANSACTION, WHERE NOT EXISTS,
//	                                                              ON CONFLICT DO NOTHING
//
// up -> down -> up, then the whole directory again under two more goose ledgers on the SAME
// database -- the situation hazard sentinel_goose_target names: one flat directory, three
// ledgers, one physical database, so every statement runs again on a schema that already has
// every object it creates and every row the first pass inserted.
//
// What is asserted is the SHAPE the contract paints (the composite PK, the `count > 0` CHECK,
// TIMESTAMPTZ, the three indexes with their exact key orders) and the behaviours only a live
// database can prove:
//
//   - the backfill inserts EXACTLY one row per (issue, UTC hour) over seeded occurrences, with
//     the counts §3's Go rule (`occ.CreatedAt.UTC().Truncate(time.Hour)`) predicts -- the
//     prediction is computed in Go from the same time.Time values that seeded the rows, and one
//     of them is stamped at a +05:30 wall clock while the connection's session TimeZone is
//     Asia/Kolkata, so a bare `date_trunc('hour', timestamptz)` (session-zone truncation) would
//     land that row in a different hour and go red here;
//   - the second and third ledgers insert NOTHING (`WHERE NOT EXISTS`): an occurrence added
//     between the passes has no bucket row after them -- delete the guard and this goes red,
//     because ON CONFLICT alone would happily insert the new hour;
//   - Down removes the table and all three indexes and both ledger rows (file 2's Down is empty
//     -- goose still deletes the version), and the second Up backfills again from scratch;
//   - the PK rejects a duplicate (issue, hour), the CHECK rejects `count = 0`, and deleting an
//     issue cascades its buckets;
//   - the two files' text: file 2's FIRST goose annotation is `-- +goose NO TRANSACTION` and file
//     1 carries no INSERT (the backfill is never in the DDL transaction -- decision 19).
//
// Same database contract as migrations_oncall_weekday_test.go: MIGRATIONS_TEST_DB_URL at a
// DISPOSABLE Postgres whose role may CREATE DATABASE, self-skipping without one,
// MIGRATIONS_TEST_REQUIRED=1 to make the skip a failure. Never the shared dev database.
//
//	MIGRATIONS_TEST_DB_URL='postgres://sentinel:changeme@localhost:55432/sentinel?sslmode=disable' \
//	  GOWORK=off go test ./... -run 'IssueOccurrenceBuckets' -count=1 -v

import (
	"bufio"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
)

const (
	// 1724600000 is BUG-01's index -- the last version before this feature's two files.
	occurrenceBucketsBaseVersion     int64 = 1724600000
	occurrenceBucketsTableVersion    int64 = 1724700000
	occurrenceBucketsBackfillVersion int64 = 1724800000

	occurrenceBucketsTableFile    = "1724700000_add_issue_occurrence_buckets.sql"
	occurrenceBucketsBackfillFile = "1724800000_backfill_issue_occurrence_buckets.sql"
)

// bucketKey names one (issue, UTC hour) row; bucketRows is what the table holds, keyed that way.
type bucketKey struct {
	issueID     string
	bucketStart time.Time
}

type bucketRows map[bucketKey]int

// TestIssueOccurrenceBucketsReplay drives both files up -> down -> up over a database seeded with
// occurrences and asserts the schema and the backfilled rows in each state, then replays the
// directory under two more ledgers and asserts the backfill inserted nothing more.
func TestIssueOccurrenceBucketsReplay(t *testing.T) {
	db, cleanup := freshMigrationDatabase(t, "buckets")
	defer cleanup()

	// One connection, pinned to a +05:30 session zone. goose runs every statement -- the NO
	// TRANSACTION backfill included -- through this same connection, so the backfill's
	// date_trunc runs in a session where a bare date_trunc('hour', timestamptz) would be wrong.
	db.SetMaxOpenConns(1)
	mustExec(t, db, `SET TIME ZONE 'Asia/Kolkata'`)

	ctx := context.Background()
	migrateTo(t, db, occurrenceBucketsBaseVersion)
	// Every ledger is at the base version before the feature lands -- production's shape when
	// the migrate Job runs this release, and the only order the directory permits with issue
	// rows present: 1724400000_add_project_environments.sql refuses to run on a database whose
	// `issues` has rows (its decision 7 guard), so a ledger cannot catch up from zero after the
	// seed below. Each ledger then applies exactly this feature's two files.
	for _, ledger := range []string{"dashboard_migrations", "processor_migrations"} {
		replayUnderLedger(t, db, ledger, occurrenceBucketsBaseVersion)
	}
	assertOccurrenceBucketsShape(t, db, false)

	issueA := seedIssue(t, db, "bucket-a")
	issueB := seedIssue(t, db, "bucket-b")

	kolkata := time.FixedZone("+05:30", 5*3600+30*60)
	occurrences := map[string][]time.Time{
		// Two in one UTC hour, one in the next: (10:00Z, 2) and (11:00Z, 1).
		issueA: {
			time.Date(2026, 3, 1, 10, 15, 0, 0, time.UTC),
			time.Date(2026, 3, 1, 10, 45, 30, 0, time.UTC),
			time.Date(2026, 3, 1, 11, 5, 0, 0, time.UTC),
		},
		// The §3 / §11 case: 15:59:59 at +05:30 is 10:29:59Z, so the UTC hour is 10:00Z. A
		// session-zone truncation would say 15:00+05:30 = 09:30Z, which is not even on the hour.
		issueB: {
			time.Date(2026, 3, 1, 15, 59, 59, 0, kolkata),
		},
	}
	seedOccurrences(t, db, occurrences)

	// Up, both files. File 1 creates the table in its own transaction; file 2 backfills outside
	// any transaction. The ledger must record both.
	migrateTo(t, db, occurrenceBucketsBackfillVersion)
	assertOccurrenceBucketsShape(t, db, true)
	assertLedgerHas(t, db, "schema_migrations", occurrenceBucketsTableVersion, true)
	assertLedgerHas(t, db, "schema_migrations", occurrenceBucketsBackfillVersion, true)

	want := predictBuckets(occurrences)
	if got := readBuckets(t, db); !reflect.DeepEqual(got, want) {
		t.Errorf("backfill rows after first up:\n got %v\nwant %v (the Go rule: CreatedAt.UTC().Truncate(time.Hour))", got, want)
	}
	// The +05:30 row, named: exactly one row for issue B and it is the 10:00Z hour.
	wantB := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	var gotB time.Time
	var countB int
	if err := db.QueryRow(
		`SELECT bucket_start, count FROM issue_occurrence_buckets WHERE issue_id = $1`, issueB,
	).Scan(&gotB, &countB); err != nil {
		t.Fatalf("read issue B's bucket: %v", err)
	}
	if !gotB.Equal(wantB) || countB != 1 {
		t.Errorf("issue B's +05:30 occurrence landed in (%s, %d), want (%s, 1)", gotB.UTC(), countB, wantB)
	}

	// An occurrence stored AFTER the first backfill, in an hour no row covers. The guard that
	// stops the second and third ledgers from re-scanning error_occurrences is WHERE NOT EXISTS,
	// not ON CONFLICT: without it this hour WOULD be inserted on the next replay.
	late := time.Date(2026, 3, 1, 12, 30, 0, 0, time.UTC)
	seedOccurrences(t, db, map[string][]time.Time{issueA: {late}})
	lateKey := bucketKey{issueID: issueA, bucketStart: late.UTC().Truncate(time.Hour)}

	for _, ledger := range []string{"dashboard_migrations", "processor_migrations"} {
		replayUnderLedger(t, db, ledger, occurrenceBucketsBackfillVersion)
		assertLedgerHas(t, db, ledger, occurrenceBucketsTableVersion, true)
		assertLedgerHas(t, db, ledger, occurrenceBucketsBackfillVersion, true)
		got := readBuckets(t, db)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("after the %s replay the backfill changed the table:\n got %v\nwant %v (WHERE NOT EXISTS must skip a table that has rows)", ledger, got, want)
		}
		if _, inserted := got[lateKey]; inserted {
			t.Errorf("after the %s replay the late occurrence's hour %s has a bucket row: WHERE NOT EXISTS is not guarding the scan", ledger, lateKey.bucketStart)
		}
	}
	assertOccurrenceBucketsShape(t, db, true)

	// Live constraints. The PK is §4.3's ON CONFLICT target; the CHECK is the tripwire that a row
	// exists because an occurrence did.
	assertRejectedBy(t, db,
		`INSERT INTO issue_occurrence_buckets (issue_id, bucket_start, count)
		 VALUES ('`+issueB+`', '2026-03-01 10:00:00+00', 1)`,
		"issue_occurrence_buckets_pkey")
	assertRejectedBy(t, db,
		`INSERT INTO issue_occurrence_buckets (issue_id, bucket_start, count)
		 VALUES ('`+issueB+`', '2026-03-01 13:00:00+00', 0)`,
		"issue_occurrence_buckets_count_check")

	// ON DELETE CASCADE: §4.5's orphan rule and a project delete rely on it.
	mustExec(t, db, `DELETE FROM error_occurrences WHERE issue_id = '`+issueB+`'`)
	mustExec(t, db, `DELETE FROM issues WHERE id = '`+issueB+`'`)
	delete(occurrences, issueB)
	if got := readBuckets(t, db); !reflect.DeepEqual(got, predictBuckets(occurrences)) {
		t.Errorf("deleting issue B did not cascade its bucket rows: %v", got)
	}

	// Down, both files, on the primary ledger. File 2's Down is empty; goose still deletes its
	// version row. File 1's Down drops the table and all three indexes.
	goose.SetTableName("schema_migrations")
	if err := goose.DownToContext(ctx, db, migrationsDir(), occurrenceBucketsBaseVersion); err != nil {
		t.Fatalf("goose down to %d failed: %v", occurrenceBucketsBaseVersion, err)
	}
	assertOccurrenceBucketsShape(t, db, false)
	assertLedgerHas(t, db, "schema_migrations", occurrenceBucketsTableVersion, false)
	assertLedgerHas(t, db, "schema_migrations", occurrenceBucketsBackfillVersion, false)
	// The down of this feature removes ONLY its own objects; the affected-users indexes stay.
	assertAffectedRollupShape(t, db, true)
	assertAffectedUsersProjectIndexShape(t, db, true)

	// Up again: the table is empty, so WHERE NOT EXISTS is true and the backfill scans again --
	// this time the late occurrence is in the source and gets its hour.
	occurrences[issueA] = append(occurrences[issueA], late)
	migrateTo(t, db, occurrenceBucketsBackfillVersion)
	assertOccurrenceBucketsShape(t, db, true)
	if got, want := readBuckets(t, db), predictBuckets(occurrences); !reflect.DeepEqual(got, want) {
		t.Errorf("backfill rows after the second up:\n got %v\nwant %v", got, want)
	}
}

// TestIssueOccurrenceBucketsFilesAreSplitAsDecided pins the two properties of the files' TEXT that
// decision 19 turns on and that a passing replay cannot see: the backfill file's first goose
// annotation is NO TRANSACTION (so goose runs it outside a transaction -- the annotation is
// position-independent to goose, so the test pins the contract's stronger "first"), and the table
// file carries no INSERT (the backfill is never in the DDL transaction).
func TestIssueOccurrenceBucketsFilesAreSplitAsDecided(t *testing.T) {
	first := firstGooseAnnotation(t, occurrenceBucketsBackfillFile)
	if first != "NO TRANSACTION" {
		t.Errorf("%s: first goose annotation is %q, want \"NO TRANSACTION\" (decision 19)", occurrenceBucketsBackfillFile, first)
	}
	if first := firstGooseAnnotation(t, occurrenceBucketsTableFile); first != "Up" {
		t.Errorf("%s: first goose annotation is %q, want \"Up\" -- the table file runs in a transaction", occurrenceBucketsTableFile, first)
	}
	for _, line := range sqlLines(t, occurrenceBucketsTableFile) {
		if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(line)), "INSERT") {
			t.Errorf("%s carries an INSERT (%q): the backfill belongs in %s, outside the DDL transaction (decision 19)",
				occurrenceBucketsTableFile, strings.TrimSpace(line), occurrenceBucketsBackfillFile)
		}
	}
	// And the backfill IS an INSERT with both guards, so the two assertions above are not
	// vacuous.
	var hasInsert, hasNotExists, hasOnConflict bool
	for _, line := range sqlLines(t, occurrenceBucketsBackfillFile) {
		u := strings.ToUpper(line)
		hasInsert = hasInsert || strings.Contains(u, "INSERT INTO ISSUE_OCCURRENCE_BUCKETS")
		hasNotExists = hasNotExists || strings.Contains(u, "WHERE NOT EXISTS (SELECT 1 FROM ISSUE_OCCURRENCE_BUCKETS)")
		hasOnConflict = hasOnConflict || strings.Contains(u, "ON CONFLICT (ISSUE_ID, BUCKET_START) DO NOTHING")
	}
	if !hasInsert || !hasNotExists || !hasOnConflict {
		t.Errorf("%s: insert=%v, WHERE NOT EXISTS=%v, ON CONFLICT DO NOTHING=%v; all three must be present (decision 19)",
			occurrenceBucketsBackfillFile, hasInsert, hasNotExists, hasOnConflict)
	}
}

// assertOccurrenceBucketsShape checks every object §4.6 creates is present (want=true) or absent
// (want=false): the table with its three columns, the composite PK, the CHECK, and the three
// indexes with their exact key orders.
func assertOccurrenceBucketsShape(t *testing.T, db *sql.DB, want bool) {
	t.Helper()

	assertTableExists(t, db, "issue_occurrence_buckets", want)
	for _, index := range []string{
		"idx_issue_occurrence_buckets_bucket_start",
		"idx_issues_project_count",
		"idx_issues_project_first_seen",
	} {
		assertIndexExists(t, db, index, want)
	}
	if !want {
		return
	}

	assertColumnShape(t, db, "issue_occurrence_buckets", "issue_id", "uuid", "NO", "")
	// TIMESTAMPTZ, so `< cutoff` is the same comparison retention makes on
	// error_occurrences.created_at; INTEGER NOT NULL with no default -- a row is written with its
	// count or not at all.
	assertColumnShape(t, db, "issue_occurrence_buckets", "bucket_start", "timestamp with time zone", "NO", "")
	assertColumnShape(t, db, "issue_occurrence_buckets", "count", "integer", "NO", "")
	assertConstraint(t, db, "issue_occurrence_buckets", "issue_occurrence_buckets_pkey", "PRIMARY KEY (issue_id, bucket_start)")
	assertConstraint(t, db, "issue_occurrence_buckets", "issue_occurrence_buckets_count_check", "CHECK ((count > 0))")
	assertConstraint(t, db, "issue_occurrence_buckets", "issue_occurrence_buckets_issue_id_fkey",
		"FOREIGN KEY (issue_id) REFERENCES issues(id) ON DELETE CASCADE")
	// The key orders ARE §4.1's two ORDER BYs, `id DESC` last (§3's tiebreak rule) and
	// `first_seen DESC NULLS LAST` explicit -- a reordered index would still exist and still be
	// named right, so the definitions are what is pinned.
	assertIndexDefinition(t, db, "idx_issue_occurrence_buckets_bucket_start", "(bucket_start)")
	assertIndexDefinition(t, db, "idx_issues_project_count", "(project_id, count DESC, last_seen DESC, id DESC)")
	assertIndexDefinition(t, db, "idx_issues_project_first_seen", "(project_id, first_seen DESC NULLS LAST, id DESC)")
}

// seedOccurrences inserts one error_occurrences row per time for each issue, passing the
// time.Time as a parameter so the value in the database is exactly the instant Go holds.
func seedOccurrences(t *testing.T, db *sql.DB, byIssue map[string][]time.Time) {
	t.Helper()

	for issueID, times := range byIssue {
		for _, ts := range times {
			if _, err := db.Exec(
				`INSERT INTO error_occurrences (issue_id, environment, platform, created_at) VALUES ($1, 'production', 'go', $2)`,
				issueID, ts,
			); err != nil {
				t.Fatalf("seed occurrence for %s at %s: %v", issueID, ts, err)
			}
		}
	}
}

// predictBuckets is §3's rule applied in Go: one row per (issue, CreatedAt.UTC().Truncate(hour))
// with the number of occurrences that truncate there.
func predictBuckets(byIssue map[string][]time.Time) bucketRows {
	want := bucketRows{}
	for issueID, times := range byIssue {
		for _, ts := range times {
			want[bucketKey{issueID: issueID, bucketStart: ts.UTC().Truncate(time.Hour)}]++
		}
	}
	return want
}

// readBuckets reads the whole table back, normalising bucket_start to UTC so a session-zone
// rendering cannot make two equal instants compare unequal.
func readBuckets(t *testing.T, db *sql.DB) bucketRows {
	t.Helper()

	rows, err := db.Query(`SELECT issue_id, bucket_start, count FROM issue_occurrence_buckets`)
	if err != nil {
		t.Fatalf("read issue_occurrence_buckets: %v", err)
	}
	defer rows.Close()
	got := bucketRows{}
	for rows.Next() {
		var issueID string
		var start time.Time
		var count int
		if err := rows.Scan(&issueID, &start, &count); err != nil {
			t.Fatalf("scan issue_occurrence_buckets: %v", err)
		}
		got[bucketKey{issueID: issueID, bucketStart: start.UTC()}] = count
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate issue_occurrence_buckets: %v", err)
	}
	return got
}

// replayUnderLedger applies the whole directory up to version under another goose ledger table
// on the same database -- what the second and third targets do in production -- and restores the
// primary ledger name afterwards.
func replayUnderLedger(t *testing.T, db *sql.DB, ledger string, version int64) {
	t.Helper()

	goose.SetTableName(ledger)
	defer goose.SetTableName("schema_migrations")
	if err := goose.UpToContext(context.Background(), db, migrationsDir(), version); err != nil {
		t.Fatalf("replaying the directory under the %s ledger failed -- an unguarded statement in %s or %s: %v",
			ledger, occurrenceBucketsTableFile, occurrenceBucketsBackfillFile, err)
	}
}

func assertLedgerHas(t *testing.T, db *sql.DB, ledger string, version int64, want bool) {
	t.Helper()

	var applied int
	if err := db.QueryRow(
		`SELECT count(*) FROM `+ledger+` WHERE version_id = $1 AND is_applied`, version,
	).Scan(&applied); err != nil {
		t.Fatalf("read the %s ledger: %v", ledger, err)
	}
	if (applied == 1) != want {
		t.Errorf("%s records %d applied rows for %d, want present=%v", ledger, applied, version, want)
	}
}

// firstGooseAnnotation returns the annotation of the first `-- +goose <annotation>` line in the
// named migration file.
func firstGooseAnnotation(t *testing.T, file string) string {
	t.Helper()

	for _, line := range fileLines(t, file) {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "--") && strings.Contains(trimmed, "+goose") {
			_, annotation, _ := strings.Cut(trimmed, "+goose")
			return strings.TrimSpace(annotation)
		}
	}
	t.Fatalf("%s has no goose annotation", file)
	return ""
}

// sqlLines returns the file's non-comment lines -- the text goose would hand to Postgres.
func sqlLines(t *testing.T, file string) []string {
	t.Helper()

	var out []string
	for _, line := range fileLines(t, file) {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "--") {
			continue
		}
		out = append(out, line)
	}
	return out
}

func fileLines(t *testing.T, file string) []string {
	t.Helper()

	f, err := os.Open(filepath.Join(migrationsDir(), file))
	if err != nil {
		t.Fatalf("open %s: %v", file, err)
	}
	defer f.Close()
	var lines []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	return lines
}
