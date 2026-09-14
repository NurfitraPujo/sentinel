package dbmigrations

// sentinel-releases-snooze §4.5 (TASK-01, criterion 1; contract §11's db-migrations row). Replays
// the two migrations of the feature against a real Postgres in the style
// migrations_issue_occurrence_buckets_test.go established for this package:
//
//	migrations/1724900000_add_releases_and_issue_snooze.sql                 the `releases` table and
//	                                                                        its index, the four
//	                                                                        `issues` columns, three
//	                                                                        CHECKs (NOT VALID then
//	                                                                        VALIDATEd), two partial
//	                                                                        indexes -- DDL only
//	migrations/1725000000_backfill_releases_and_first_release_version.sql  the one-time backfill,
//	                                                                        NO TRANSACTION, WHERE NOT
//	                                                                        EXISTS on both statements,
//	                                                                        empty Down
//
// up -> down -> up, then the whole directory again under two more goose ledgers on the SAME
// database -- the situation hazard sentinel_goose_target names: one flat directory, three
// ledgers, one physical database, so every statement runs again on a schema that already has
// every object it creates and every row the first pass inserted.
//
// What is asserted is the SHAPE the contract paints (the composite PK, `version <> ''`, the four
// nullable no-default columns, the three named CHECKs, the three indexes with their exact
// definitions) and the behaviours only a live database can prove:
//
//   - after file 1 all three CHECKs read convalidated = true (decisions 31/48: added NOT VALID,
//     then VALIDATEd inside the same transaction) and reject what they must: a target of 0, a
//     target without its count_at baseline, and a snoozed_until on a `resolved` row;
//   - the backfill's four §11 cases: the expected `releases` rows from seeded error_occurrences
//     (per (project, version): earliest, latest, count -- keyed per PROJECT, so the same version
//     string in two projects is two rows); first_release_version from the EARLIEST retained
//     occurrence regardless of version; NULL when that earliest occurrence carried no version
//     (NULL or '' -- decision 31, never attributed to a later versioned one); and NOTHING
//     inserted or rewritten on the second and third ledgers (the WHERE NOT EXISTS guards);
//   - decision 50's clamp: a retained occurrence whose client clock ran AHEAD backfills a release
//     whose first_seen and last_seen are LEAST(…, now()) -- never in the future -- while a
//     release with only past occurrences keeps its exact instants;
//   - Down removes the two partial indexes, the three constraints, the four columns, the table
//     and both ledger rows (file 2's Down is empty -- goose still deletes the version), and the
//     second Up backfills again from scratch, this time seeing what was stored in between;
//   - the PK rejects a duplicate (project, version), `version <> ''` rejects the empty string,
//     and deleting a project cascades its releases;
//   - the two files' text: file 2's FIRST goose annotation is `-- +goose NO TRANSACTION`, file 1
//     carries no INSERT and no UPDATE (the backfill is never in the DDL transaction -- decision
//     22), and file 1 adds each CHECK NOT VALID and VALIDATEs it (decision 31).
//
// Same database contract as migrations_oncall_weekday_test.go: MIGRATIONS_TEST_DB_URL at a
// DISPOSABLE Postgres whose role may CREATE DATABASE, self-skipping without one,
// MIGRATIONS_TEST_REQUIRED=1 to make the skip a failure. Never the shared dev database.
//
//	MIGRATIONS_TEST_DB_URL='postgres://sentinel:changeme@localhost:55432/sentinel?sslmode=disable' \
//	  GOWORK=off go test ./... -run 'ReleasesSnooze' -count=1 -v

import (
	"context"
	"database/sql"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
)

const (
	// 1724800000 is the issue-trend backfill -- the last version before this feature's two files.
	releasesSnoozeBaseVersion     int64 = 1724800000
	releasesSnoozeDDLVersion      int64 = 1724900000
	releasesSnoozeBackfillVersion int64 = 1725000000

	releasesSnoozeDDLFile      = "1724900000_add_releases_and_issue_snooze.sql"
	releasesSnoozeBackfillFile = "1725000000_backfill_releases_and_first_release_version.sql"
)

// releaseKey names one (project, version) row; releaseRows is what `releases` holds, keyed that
// way, with the timestamps normalised to UTC so a session-zone rendering cannot make two equal
// instants compare unequal.
type releaseKey struct {
	projectID string
	version   string
}

type releaseRow struct {
	firstSeen       time.Time
	lastSeen        time.Time
	occurrenceCount int64
}

type releaseRows map[releaseKey]releaseRow

// seededOccurrence is one error_occurrences row the test writes: its issue, its instant and its
// release_version -- nil for a NULL version, "" for the empty string the SDK's empty-version rule
// can still let through on old rows.
type seededOccurrence struct {
	issueID string
	at      time.Time
	version *string
}

func versionPtr(v string) *string { return &v }

// TestReleasesSnoozeReplay drives both files up -> down -> up over a database seeded with
// occurrences and asserts the schema, the three CHECKs and the backfilled rows in each state,
// then replays the directory under two more ledgers and asserts the backfill inserted and
// rewrote nothing more.
//
// The backfill case, decision 42 (§4.5, §12): file 2's UPDATE takes a row lock on EVERY issue it
// rewrites -- every issue with a retained versioned occurrence -- while the old-tag processor
// pods, still running at ArgoCD wave 1 (the migrate Job runs before any pod rolls, §8), take the
// same rows at StoreEvent step 1. Unlike the 1724800000 precedent, which inserted into a side
// table and took only FOR KEY SHARE on `issues`. Under the file's lock_timeout = '5s' a busy row
// fails the statement: the migrate Job fails, the sync stalls at wave 1, and the old pods keep
// running -- safe. Re-syncing re-runs the file, and because each statement is its own transaction
// the NOT EXISTS guards skip what the first attempt finished (a failed UPDATE set nothing, so its
// guard still passes) -- which is the idempotence the second- and third-ledger replays below prove
// on the same guards. This test cannot stage a concurrent old-tag pod; it pins the guards the
// re-sync relies on.
func TestReleasesSnoozeReplay(t *testing.T) {
	db, cleanup := freshMigrationDatabase(t, "relsnooze")
	defer cleanup()

	// One connection, pinned to a +05:30 session zone, so MIN/MAX(created_at) and the DISTINCT ON
	// ordering are proven on instants, not on a session-zone rendering.
	db.SetMaxOpenConns(1)
	mustExec(t, db, `SET TIME ZONE 'Asia/Kolkata'`)

	ctx := context.Background()
	migrateTo(t, db, releasesSnoozeBaseVersion)
	// Every ledger is at the base version before the feature lands -- production's shape when
	// the migrate Job runs this release, and the only order the directory permits with issue
	// rows present (1724400000_add_project_environments.sql refuses to run on a database whose
	// `issues` has rows). Each ledger then applies exactly this feature's two files.
	for _, ledger := range []string{"dashboard_migrations", "processor_migrations"} {
		replayUnderLedger(t, db, ledger, releasesSnoozeBaseVersion)
	}
	assertReleasesSnoozeShape(t, db, false)

	// Project P: issue A (versioned from its first occurrence), issue B (its EARLIEST occurrence
	// has a NULL version), issue C (its earliest has the EMPTY STRING). Project Q: issue D, in the
	// same version string as A's first, so (project, version) keying is what is proved.
	projectP, issueA := seedProjectIssue(t, db, "relsnooze-p", "issue-a")
	issueB := seedIssueInProject(t, db, projectP, "issue-b")
	issueC := seedIssueInProject(t, db, projectP, "issue-c")
	projectQ, issueD := seedProjectIssue(t, db, "relsnooze-q", "issue-d")

	kolkata := time.FixedZone("+05:30", 5*3600+30*60)
	seeded := []seededOccurrence{
		// A: v1.0.0 at 10:15Z and 10:45:30Z, then v1.1.0 at 11:05Z -> first_release_version v1.0.0.
		{issueA, time.Date(2026, 3, 1, 10, 15, 0, 0, time.UTC), versionPtr("v1.0.0")},
		{issueA, time.Date(2026, 3, 1, 10, 45, 30, 0, time.UTC), versionPtr("v1.0.0")},
		{issueA, time.Date(2026, 3, 1, 11, 5, 0, 0, time.UTC), versionPtr("v1.1.0")},
		// B: the earliest (09:00Z) carries NO version; a later one carries v1.1.0. Decision 31:
		// first_release_version stays NULL -- never attributed to the first VERSIONED occurrence.
		{issueB, time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC), nil},
		{issueB, time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC), versionPtr("v1.1.0")},
		// C: the earliest carries the EMPTY STRING (NULLIF(release_version, '') is what makes it
		// "no version"); the later one, stamped at a +05:30 wall clock, carries v2.0.0.
		{issueC, time.Date(2026, 3, 1, 8, 30, 0, 0, time.UTC), versionPtr("")},
		{issueC, time.Date(2026, 3, 1, 19, 59, 59, 0, kolkata), versionPtr("v2.0.0")},
		// D, in project Q: the same version string as A's first, one occurrence.
		{issueD, time.Date(2026, 3, 1, 13, 0, 0, 0, time.UTC), versionPtr("v1.0.0")},
	}
	seedVersionedOccurrences(t, db, seeded)

	// Decision 50 (`release-first-seen-clock-guard`): two occurrences of D whose client clock ran
	// one and two years AHEAD, in a version of their own. Without the backfill's LEAST(…, now())
	// they would seed Q/v9.9.9 with first_seen in 2027 -- the permanent "newest" release that
	// rewrites every `next` resolution of the project and reads every genuine later release as
	// older. Kept out of `seeded` so predictReleases (unclamped, MIN/MAX in Go) stays the oracle
	// for the past rows; the clamped row is asserted on its own against the database's clock.
	// In project Q so the cascade below removes it with D.
	future := []seededOccurrence{
		{issueD, time.Now().UTC().AddDate(1, 0, 0), versionPtr("v9.9.9")},
		{issueD, time.Now().UTC().AddDate(2, 0, 0), versionPtr("v9.9.9")},
	}
	seedVersionedOccurrences(t, db, future)
	futureKey := releaseKey{projectQ, "v9.9.9"}

	// Up, both files. File 1 creates everything in its own transaction; file 2 backfills outside
	// any transaction. The ledger must record both.
	notBefore := databaseNow(t, db)
	migrateTo(t, db, releasesSnoozeBackfillVersion)
	notAfter := databaseNow(t, db)
	assertReleasesSnoozeShape(t, db, true)
	assertLedgerHas(t, db, "schema_migrations", releasesSnoozeDDLVersion, true)
	assertLedgerHas(t, db, "schema_migrations", releasesSnoozeBackfillVersion, true)

	// §11 case 1: the expected `releases` rows -- one per (project, version), earliest/latest/count
	// over the versioned occurrences only. The '' occurrence creates no release named "".
	want := predictReleases(t, db, seeded)
	if len(want) != 4 {
		t.Fatalf("the seed predicts %d releases, want 4 (P/v1.0.0, P/v1.1.0, P/v2.0.0, Q/v1.0.0)", len(want))
	}
	// Decision 50: Q/v9.9.9 exists, counts both future occurrences, and its first_seen and
	// last_seen are the backfill's now() -- inside [notBefore, notAfter] -- not 2027 and 2028.
	// Everything else is exact: the clamp fires only on the future side.
	got := takeClampedRelease(t, readReleases(t, db), futureKey, notBefore, notAfter, 2)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("releases after first up:\n got %v\nwant %v", got, want)
	}
	// Named: A's two v1.0.0 occurrences and D's one are two rows, not one, and P/v1.1.0 spans A and B.
	if got := readReleases(t, db)[releaseKey{projectP, "v1.0.0"}]; got.occurrenceCount != 2 {
		t.Errorf("P/v1.0.0 counts %d occurrences, want 2 (issue D's is project Q's row)", got.occurrenceCount)
	}
	if got := readReleases(t, db)[releaseKey{projectP, "v1.1.0"}]; got.occurrenceCount != 2 ||
		!got.firstSeen.Equal(time.Date(2026, 3, 1, 11, 5, 0, 0, time.UTC)) ||
		!got.lastSeen.Equal(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("P/v1.1.0 is %+v, want count 2, first_seen 11:05Z (issue A), last_seen 12:00Z (issue B)", got)
	}

	// §11 cases 2 and 3: first_release_version from the EARLIEST occurrence regardless of version,
	// NULL when that one carried none (NULL for B, '' for C).
	assertFirstReleaseVersion(t, db, map[string]*string{
		issueA: versionPtr("v1.0.0"),
		issueB: nil,
		issueC: nil,
		issueD: versionPtr("v1.0.0"),
	})

	// The three CHECKs are VALIDATED -- convalidated = true, and no `NOT VALID` in the rendered
	// definition -- and reject what they must (§11). Every rejected statement is a plain UPDATE of
	// the kind a write path that forgot one column would issue.
	assertSnoozeChecksValidated(t, db)
	assertRejectedBy(t, db,
		`UPDATE issues SET status = 'ignored', snooze_occurrence_target = 0, snooze_count_at = 0 WHERE id = '`+issueA+`'`,
		"check_snooze_target_positive")
	assertRejectedBy(t, db,
		`UPDATE issues SET status = 'ignored', snooze_occurrence_target = 100 WHERE id = '`+issueA+`'`,
		"check_snooze_pair")
	assertRejectedBy(t, db,
		`UPDATE issues SET status = 'resolved', snoozed_until = now() + interval '1 day' WHERE id = '`+issueA+`'`,
		"check_snooze_only_when_ignored")
	// And the shape every §4.2 write produces is accepted: an ignore with both conditions, then
	// the resolve that nulls them IN THE SAME STATEMENT.
	mustExec(t, db, `UPDATE issues SET status = 'ignored', snoozed_until = now() + interval '1 day',
		snooze_occurrence_target = 100, snooze_count_at = count WHERE id = '`+issueA+`'`)
	mustExec(t, db, `UPDATE issues SET status = 'resolved', snoozed_until = NULL,
		snooze_occurrence_target = NULL, snooze_count_at = NULL WHERE id = '`+issueA+`'`)
	// The `releases` tripwires: the PK and `version <> ''`.
	assertRejectedBy(t, db,
		`INSERT INTO releases (project_id, version, first_seen, last_seen, occurrence_count)
		 VALUES ('`+projectP+`', 'v1.0.0', now(), now(), 1)`,
		"releases_pkey")
	assertRejectedBy(t, db,
		`INSERT INTO releases (project_id, version, first_seen, last_seen, occurrence_count)
		 VALUES ('`+projectP+`', '', now(), now(), 1)`,
		"releases_version_check")

	// Occurrences stored AFTER the first backfill, by old-tag pods in the rollout window: a new
	// version on A, and a brand-new issue E whose only occurrence is versioned. The guards that
	// stop the second and third ledgers from re-scanning error_occurrences are the two WHERE NOT
	// EXISTS, not ON CONFLICT: without them v3.0.0 WOULD be inserted and E WOULD be rewritten.
	issueE := seedIssueInProject(t, db, projectP, "issue-e")
	late := []seededOccurrence{
		{issueA, time.Date(2026, 3, 1, 14, 0, 0, 0, time.UTC), versionPtr("v3.0.0")},
		{issueE, time.Date(2026, 3, 1, 14, 30, 0, 0, time.UTC), versionPtr("v3.0.0")},
	}
	seedVersionedOccurrences(t, db, late)
	lateKey := releaseKey{projectP, "v3.0.0"}

	// §11 case 4: nothing inserted, nothing rewritten, on the second and third ledgers.
	for _, ledger := range []string{"dashboard_migrations", "processor_migrations"} {
		replayUnderLedger(t, db, ledger, releasesSnoozeBackfillVersion)
		assertLedgerHas(t, db, ledger, releasesSnoozeDDLVersion, true)
		assertLedgerHas(t, db, ledger, releasesSnoozeBackfillVersion, true)
		got := takeClampedRelease(t, readReleases(t, db), futureKey, notBefore, notAfter, 2)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("after the %s replay the backfill changed releases:\n got %v\nwant %v (WHERE NOT EXISTS must skip a table that has rows)", ledger, got, want)
		}
		if _, inserted := got[lateKey]; inserted {
			t.Errorf("after the %s replay the late version %s has a releases row: WHERE NOT EXISTS is not guarding the INSERT", ledger, lateKey.version)
		}
		assertFirstReleaseVersion(t, db, map[string]*string{
			issueA: versionPtr("v1.0.0"),
			issueB: nil,
			issueC: nil,
			issueD: versionPtr("v1.0.0"),
			// E was created after the first backfill; the second guard must leave it NULL.
			issueE: nil,
		})
	}
	assertReleasesSnoozeShape(t, db, true)
	assertSnoozeChecksValidated(t, db)

	// ON DELETE CASCADE from projects: a project delete takes its releases (§4.5, §12). Nothing
	// cascades from issues: deleting D first leaves Q/v1.0.0 in place.
	mustExec(t, db, `DELETE FROM error_occurrences WHERE issue_id = '`+issueD+`'`)
	mustExec(t, db, `DELETE FROM issues WHERE id = '`+issueD+`'`)
	if _, still := readReleases(t, db)[releaseKey{projectQ, "v1.0.0"}]; !still {
		t.Errorf("deleting issue D removed Q/v1.0.0: a release must outlive every occurrence that carried it")
	}
	mustExec(t, db, `DELETE FROM projects WHERE id = '`+projectQ+`'`)
	delete(want, releaseKey{projectQ, "v1.0.0"})
	// The clamped row cascades with Q too, so from here every release is exact and readReleases
	// compares whole.
	if got := readReleases(t, db); !reflect.DeepEqual(got, want) {
		t.Errorf("deleting project Q did not cascade its releases (v1.0.0 and the clamped v9.9.9): %v", got)
	}

	// Down, both files, on the primary ledger. File 2's Down is empty; goose still deletes its
	// version row. File 1's Down drops the two partial indexes, the three constraints, the four
	// columns and the table.
	goose.SetTableName("schema_migrations")
	if err := goose.DownToContext(ctx, db, migrationsDir(), releasesSnoozeBaseVersion); err != nil {
		t.Fatalf("goose down to %d failed: %v", releasesSnoozeBaseVersion, err)
	}
	assertReleasesSnoozeShape(t, db, false)
	assertLedgerHas(t, db, "schema_migrations", releasesSnoozeDDLVersion, false)
	assertLedgerHas(t, db, "schema_migrations", releasesSnoozeBackfillVersion, false)
	// The down of this feature removes ONLY its own objects; the trend's and the affected-users'
	// objects stay.
	assertOccurrenceBucketsShape(t, db, true)
	assertAffectedRollupShape(t, db, true)

	// Up again: the table is empty and no issue carries a first_release_version, so both guards
	// are true and the backfill scans again -- this time the late occurrences are in the source:
	// v3.0.0 gets its row and E its version, while A keeps v1.0.0 (its earliest).
	migrateTo(t, db, releasesSnoozeBackfillVersion)
	assertReleasesSnoozeShape(t, db, true)
	assertSnoozeChecksValidated(t, db)
	remaining := append([]seededOccurrence{}, late...)
	for _, o := range seeded {
		if o.issueID != issueD {
			remaining = append(remaining, o)
		}
	}
	if got, want := readReleases(t, db), predictReleases(t, db, remaining); !reflect.DeepEqual(got, want) {
		t.Errorf("releases after the second up:\n got %v\nwant %v", got, want)
	}
	assertFirstReleaseVersion(t, db, map[string]*string{
		issueA: versionPtr("v1.0.0"),
		issueB: nil,
		issueC: nil,
		issueE: versionPtr("v3.0.0"),
	})
}

// TestReleasesSnoozeFilesAreShapedAsDecided pins the properties of the files' TEXT that decisions
// 22 and 31 turn on and that a passing replay cannot see: the backfill file's first goose
// annotation is NO TRANSACTION, the DDL file carries no INSERT and no UPDATE, the DDL file adds
// each of the three CHECKs NOT VALID and VALIDATEs each, and the backfill carries both statements
// with their guards.
func TestReleasesSnoozeFilesAreShapedAsDecided(t *testing.T) {
	if first := firstGooseAnnotation(t, releasesSnoozeBackfillFile); first != "NO TRANSACTION" {
		t.Errorf("%s: first goose annotation is %q, want \"NO TRANSACTION\" (decision 22)", releasesSnoozeBackfillFile, first)
	}
	if first := firstGooseAnnotation(t, releasesSnoozeDDLFile); first != "Up" {
		t.Errorf("%s: first goose annotation is %q, want \"Up\" -- the DDL file runs in a transaction", releasesSnoozeDDLFile, first)
	}

	ddl := sqlLines(t, releasesSnoozeDDLFile)
	for _, line := range ddl {
		u := strings.ToUpper(strings.TrimSpace(line))
		if strings.HasPrefix(u, "INSERT") || strings.HasPrefix(u, "UPDATE") {
			t.Errorf("%s carries a data statement (%q): the backfill belongs in %s, outside the DDL transaction (decision 22)",
				releasesSnoozeDDLFile, strings.TrimSpace(line), releasesSnoozeBackfillFile)
		}
	}
	// Decision 31: each CHECK is added NOT VALID and then VALIDATEd, in this file.
	ddlText := strings.ToUpper(strings.Join(ddl, "\n"))
	for _, name := range []string{"CHECK_SNOOZE_TARGET_POSITIVE", "CHECK_SNOOZE_PAIR", "CHECK_SNOOZE_ONLY_WHEN_IGNORED"} {
		if !strings.Contains(ddlText, "ADD CONSTRAINT "+name) {
			t.Errorf("%s does not ADD CONSTRAINT %s", releasesSnoozeDDLFile, name)
		}
		if !strings.Contains(ddlText, "VALIDATE CONSTRAINT "+name) {
			t.Errorf("%s does not VALIDATE CONSTRAINT %s (decision 31)", releasesSnoozeDDLFile, name)
		}
	}
	if got := strings.Count(ddlText, "NOT VALID"); got != 3 {
		t.Errorf("%s adds %d constraints NOT VALID, want 3 (decision 31)", releasesSnoozeDDLFile, got)
	}

	// And the backfill IS the two statements with both guards, so the assertions above are not
	// vacuous.
	var hasInsert, hasInsertGuard, hasOnConflict, hasUpdate, hasUpdateGuard, hasNullIf bool
	var hasFirstSeenClamp, hasLastSeenClamp bool
	for _, line := range sqlLines(t, releasesSnoozeBackfillFile) {
		u := strings.ToUpper(strings.Join(strings.Fields(line), " "))
		hasInsert = hasInsert || strings.Contains(u, "INSERT INTO RELEASES")
		hasFirstSeenClamp = hasFirstSeenClamp || strings.Contains(u, "LEAST(MIN(O.CREATED_AT), NOW())")
		hasLastSeenClamp = hasLastSeenClamp || strings.Contains(u, "LEAST(MAX(O.CREATED_AT), NOW())")
		hasInsertGuard = hasInsertGuard || strings.Contains(u, "NOT EXISTS (SELECT 1 FROM RELEASES)")
		hasOnConflict = hasOnConflict || strings.Contains(u, "ON CONFLICT (PROJECT_ID, VERSION) DO NOTHING")
		hasUpdate = hasUpdate || strings.Contains(u, "UPDATE ISSUES I SET FIRST_RELEASE_VERSION")
		hasUpdateGuard = hasUpdateGuard || strings.Contains(u, "NOT EXISTS (SELECT 1 FROM ISSUES WHERE FIRST_RELEASE_VERSION IS NOT NULL)")
		hasNullIf = hasNullIf || strings.Contains(u, "NULLIF(RELEASE_VERSION, '')")
	}
	if !hasInsert || !hasInsertGuard || !hasOnConflict || !hasUpdate || !hasUpdateGuard || !hasNullIf {
		t.Errorf("%s: insert=%v, its NOT EXISTS=%v, ON CONFLICT DO NOTHING=%v, update=%v, its NOT EXISTS=%v, NULLIF=%v; all six must be present (decisions 22, 31)",
			releasesSnoozeBackfillFile, hasInsert, hasInsertGuard, hasOnConflict, hasUpdate, hasUpdateGuard, hasNullIf)
	}
	// Decision 50's backfill half: both instants clamped to now(), verbatim from the contract.
	if !hasFirstSeenClamp || !hasLastSeenClamp {
		t.Errorf("%s: LEAST(MIN(o.created_at), now())=%v, LEAST(MAX(o.created_at), now())=%v; the INSERT must clamp both first_seen and last_seen (decision 50)",
			releasesSnoozeBackfillFile, hasFirstSeenClamp, hasLastSeenClamp)
	}
}

// assertReleasesSnoozeShape checks every object §4.5 creates is present (want=true) or absent
// (want=false): the table with its five columns, the composite PK, the CHECK and the FK; the four
// `issues` columns, nullable with no default; the three named CHECKs; the three indexes with their
// exact definitions.
func assertReleasesSnoozeShape(t *testing.T, db *sql.DB, want bool) {
	t.Helper()

	assertTableExists(t, db, "releases", want)
	for _, column := range []string{"snoozed_until", "snooze_occurrence_target", "snooze_count_at", "first_release_version"} {
		assertColumnExists(t, db, "issues", column, want)
	}
	for _, index := range []string{
		"idx_releases_project_first_seen",
		"idx_issues_project_first_release",
		"idx_issues_project_resolved_next",
	} {
		assertIndexExists(t, db, index, want)
	}
	for _, name := range []string{"check_snooze_target_positive", "check_snooze_pair", "check_snooze_only_when_ignored"} {
		var exists bool
		if err := db.QueryRow(
			`SELECT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = $1 AND conrelid = 'issues'::regclass)`, name,
		).Scan(&exists); err != nil {
			t.Fatalf("introspect constraint %s: %v", name, err)
		}
		if exists != want {
			t.Errorf("constraint %s on issues exists = %v, want %v", name, exists, want)
		}
	}
	if !want {
		return
	}

	assertColumnShape(t, db, "releases", "project_id", "uuid", "NO", "")
	assertColumnShape(t, db, "releases", "version", "character varying", "NO", "")
	assertColumnShape(t, db, "releases", "first_seen", "timestamp with time zone", "NO", "")
	assertColumnShape(t, db, "releases", "last_seen", "timestamp with time zone", "NO", "")
	assertColumnShape(t, db, "releases", "occurrence_count", "bigint", "NO", "0")
	assertConstraint(t, db, "releases", "releases_pkey", "PRIMARY KEY (project_id, version)")
	assertConstraint(t, db, "releases", "releases_version_check", "CHECK (((version)::text <> ''::text))")
	assertConstraint(t, db, "releases", "releases_project_id_fkey",
		"FOREIGN KEY (project_id) REFERENCES projects(id) ON DELETE CASCADE")

	// The four columns: nullable, NO DEFAULT (§4.5 -- NULL is the correct value for every
	// pre-feature row and every write path that does not know about them).
	assertColumnShape(t, db, "issues", "snoozed_until", "timestamp with time zone", "YES", "")
	assertColumnShape(t, db, "issues", "snooze_occurrence_target", "bigint", "YES", "")
	assertColumnShape(t, db, "issues", "snooze_count_at", "bigint", "YES", "")
	assertColumnShape(t, db, "issues", "first_release_version", "character varying", "YES", "")
	assertConstraint(t, db, "issues", "check_snooze_target_positive", "CHECK ((snooze_occurrence_target > 0))")
	assertConstraint(t, db, "issues", "check_snooze_pair", "CHECK (((snooze_occurrence_target IS NULL) = (snooze_count_at IS NULL)))")
	assertConstraint(t, db, "issues", "check_snooze_only_when_ignored",
		"CHECK ((((status)::text = 'ignored'::text) OR ((snoozed_until IS NULL) AND (snooze_occurrence_target IS NULL))))")
	// check_status is untouched: no fourth status (decision 2).
	assertConstraint(t, db, "issues", "check_status",
		"CHECK (((status)::text = ANY ((ARRAY['unresolved'::character varying, 'resolved'::character varying, 'ignored'::character varying])::text[])))")

	// The definitions ARE the readers' predicates: the picker's/page's ORDER BY first_seen DESC
	// under project_id, and the two partial predicates.
	assertIndexDefinition(t, db, "idx_releases_project_first_seen", "(project_id, first_seen DESC)")
	assertIndexDefinition(t, db, "idx_issues_project_first_release", "(project_id, first_release_version) WHERE (first_release_version IS NOT NULL)")
	assertIndexDefinition(t, db, "idx_issues_project_resolved_next", "(project_id) WHERE ((resolved_in_version)::text = 'next'::text)")
}

// assertSnoozeChecksValidated pins decisions 31/48's observable: after file 1 every CHECK reads
// convalidated = true, and its rendered definition carries no `NOT VALID` suffix.
func assertSnoozeChecksValidated(t *testing.T, db *sql.DB) {
	t.Helper()

	for _, name := range []string{"check_snooze_target_positive", "check_snooze_pair", "check_snooze_only_when_ignored"} {
		var validated bool
		var def string
		if err := db.QueryRow(
			`SELECT convalidated, pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = $1 AND conrelid = 'issues'::regclass`, name,
		).Scan(&validated, &def); err != nil {
			t.Fatalf("read convalidated for %s: %v", name, err)
		}
		if !validated || strings.Contains(def, "NOT VALID") {
			t.Errorf("%s reads convalidated = %v, definition %q; want validated (decision 31: added NOT VALID, then VALIDATEd in the same file)", name, validated, def)
		}
	}
}

// seedProjectIssue inserts one organization, one project and one issue, returning the project
// and issue ids -- the sibling of seedIssue that also hands back the project, because `releases`
// is keyed on it.
func seedProjectIssue(t *testing.T, db *sql.DB, name, issueName string) (projectID, issueID string) {
	t.Helper()

	var orgID string
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
	return projectID, seedIssueInProject(t, db, projectID, issueName)
}

func seedIssueInProject(t *testing.T, db *sql.DB, projectID, name string) string {
	t.Helper()

	var issueID string
	if err := db.QueryRow(
		`INSERT INTO issues (project_id, environment, fingerprint, message, error_class)
		 VALUES ($1, 'production', $2, $3, 'Error') RETURNING id`,
		projectID, name, name,
	).Scan(&issueID); err != nil {
		t.Fatalf("seed issue %s: %v", name, err)
	}
	return issueID
}

// seedVersionedOccurrences inserts one error_occurrences row per entry, passing the time.Time and
// the version as parameters so the values in the database are exactly what Go holds (a nil
// version is a SQL NULL; "" is the empty string).
func seedVersionedOccurrences(t *testing.T, db *sql.DB, occurrences []seededOccurrence) {
	t.Helper()

	for _, o := range occurrences {
		if _, err := db.Exec(
			`INSERT INTO error_occurrences (issue_id, environment, platform, created_at, release_version)
			 VALUES ($1, 'production', 'go', $2, $3)`,
			o.issueID, o.at, o.version,
		); err != nil {
			t.Fatalf("seed occurrence for %s at %s: %v", o.issueID, o.at, err)
		}
	}
}

// predictReleases is file 2's INSERT applied in Go: one row per (project, non-empty version) with
// the earliest and latest instant and the count, over the given occurrences. The issue -> project
// mapping is read from the database because the seed only holds issue ids.
func predictReleases(t *testing.T, db *sql.DB, occurrences []seededOccurrence) releaseRows {
	t.Helper()

	projectOf := map[string]string{}
	want := releaseRows{}
	for _, o := range occurrences {
		if o.version == nil || *o.version == "" {
			continue
		}
		projectID, ok := projectOf[o.issueID]
		if !ok {
			if err := db.QueryRow(`SELECT project_id FROM issues WHERE id = $1`, o.issueID).Scan(&projectID); err != nil {
				t.Fatalf("read project of issue %s: %v", o.issueID, err)
			}
			projectOf[o.issueID] = projectID
		}
		key := releaseKey{projectID, *o.version}
		at := o.at.UTC()
		row, seen := want[key]
		if !seen {
			want[key] = releaseRow{firstSeen: at, lastSeen: at, occurrenceCount: 1}
			continue
		}
		if at.Before(row.firstSeen) {
			row.firstSeen = at
		}
		if at.After(row.lastSeen) {
			row.lastSeen = at
		}
		row.occurrenceCount++
		want[key] = row
	}
	return want
}

// readReleases reads the whole table back, normalising the timestamps to UTC.
func readReleases(t *testing.T, db *sql.DB) releaseRows {
	t.Helper()

	rows, err := db.Query(`SELECT project_id, version, first_seen, last_seen, occurrence_count FROM releases`)
	if err != nil {
		t.Fatalf("read releases: %v", err)
	}
	defer rows.Close()
	got := releaseRows{}
	for rows.Next() {
		var key releaseKey
		var row releaseRow
		if err := rows.Scan(&key.projectID, &key.version, &row.firstSeen, &row.lastSeen, &row.occurrenceCount); err != nil {
			t.Fatalf("scan releases: %v", err)
		}
		row.firstSeen = row.firstSeen.UTC()
		row.lastSeen = row.lastSeen.UTC()
		got[key] = row
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate releases: %v", err)
	}
	return got
}

// databaseNow reads the server's clock, so the clamp is bounded by the same clock the backfill's
// now() came from, not the test process's.
func databaseNow(t *testing.T, db *sql.DB) time.Time {
	t.Helper()

	var now time.Time
	if err := db.QueryRow(`SELECT now()`).Scan(&now); err != nil {
		t.Fatalf("read now(): %v", err)
	}
	return now.UTC()
}

// takeClampedRelease asserts decision 50 on one row and removes it from got so the remaining rows
// compare exactly against the unclamped oracle: the row exists, counts wantCount occurrences,
// and its first_seen and last_seen are the same instant, no earlier than notBefore and no later
// than notAfter -- the backfill's now(), not the seeded future clock.
func takeClampedRelease(t *testing.T, got releaseRows, key releaseKey, notBefore, notAfter time.Time, wantCount int64) releaseRows {
	t.Helper()

	row, ok := got[key]
	if !ok {
		t.Fatalf("release %v was not backfilled: a future-dated occurrence must still seed its release", key)
	}
	if row.occurrenceCount != wantCount {
		t.Errorf("release %v counts %d occurrences, want %d", key, row.occurrenceCount, wantCount)
	}
	if !row.firstSeen.Equal(row.lastSeen) {
		t.Errorf("release %v has first_seen %s and last_seen %s; both future instants clamp to the same now()", key, row.firstSeen, row.lastSeen)
	}
	if row.firstSeen.Before(notBefore) || row.firstSeen.After(notAfter) {
		t.Errorf("release %v has first_seen %s, want within [%s, %s]: the backfill must clamp a future client clock to now() (decision 50)",
			key, row.firstSeen, notBefore, notAfter)
	}
	if row.lastSeen.After(notAfter) {
		t.Errorf("release %v has last_seen %s, want no later than %s (decision 50)", key, row.lastSeen, notAfter)
	}
	delete(got, key)
	return got
}

// assertFirstReleaseVersion reads issues.first_release_version for each named issue and compares
// it with the expectation -- nil meaning SQL NULL.
func assertFirstReleaseVersion(t *testing.T, db *sql.DB, want map[string]*string) {
	t.Helper()

	for issueID, wantVersion := range want {
		var got sql.NullString
		if err := db.QueryRow(`SELECT first_release_version FROM issues WHERE id = $1`, issueID).Scan(&got); err != nil {
			t.Fatalf("read first_release_version of %s: %v", issueID, err)
		}
		switch {
		case wantVersion == nil && got.Valid:
			t.Errorf("issue %s reads first_release_version %q, want NULL (its earliest occurrence carried no version -- decision 31)", issueID, got.String)
		case wantVersion != nil && !got.Valid:
			t.Errorf("issue %s reads first_release_version NULL, want %q (its earliest occurrence)", issueID, *wantVersion)
		case wantVersion != nil && got.String != *wantVersion:
			t.Errorf("issue %s reads first_release_version %q, want %q (its earliest occurrence)", issueID, got.String, *wantVersion)
		}
	}
}
