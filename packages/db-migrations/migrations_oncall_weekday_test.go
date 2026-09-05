package dbmigrations

// BUG-06 (sentinel-user-report-issue). The regression test the review found missing for
// migrations/1724200000_add_oncall_rotation_anchor.sql: this package carried only
// migrations_api_keys_test.go (an os.Stat on a filename) and cmd/migrate/dsn_sanitize_test.go,
// and NEITHER executed a migration, let alone a Down. That absence is why an ordering defect --
// the handoff_weekday re-encode running BEFORE the CHECK swap in both directions, so each UPDATE
// wrote a value the constraint still in force forbade -- shipped and was only caught by reading.
//
// What is asserted here is the ORDER OF EFFECTS inside the two DO blocks, which is observable
// only by running them against a real Postgres:
//
//   TestOncallWeekdayReEncodeReplay          up -> down -> up over a database seeded with one
//                                            kind='weekly' schedule at EVERY weekday value, on
//                                            the CURRENT (BUG-03, 0-7) shape of 1724100000.
//                                            Down is the direction that is unconditionally
//                                            ordering-dependent: it writes 0 while this file's
//                                            own `BETWEEN 1 AND 7` CHECK would be in force.
//   TestOncallWeekdayReEncodeOverLegacyCheck the Up direction over a database that still carries
//                                            1724100000's PRE-BUG-03 `BETWEEN 0 AND 6` CHECK --
//                                            the database the 0 -> 7 UPDATE exists to migrate,
//                                            and the only one where Up's ordering is load-bearing
//                                            (goose will not re-run an applied version to widen
//                                            a constraint, so such a database is reachable in
//                                            production and unreachable by a fresh replay).
//
// Both fail against the pre-fix file, with `new row for relation "oncall_schedules" violates
// check constraint "oncall_schedules_handoff_weekday_check"`.
//
// DATABASE. These tests need a real, DISPOSABLE Postgres and self-skip without one, per the
// convention apps/dashboard-web/tests/schema-drift.test.ts established: point
// MIGRATIONS_TEST_DB_URL at a database you are prepared to lose and whose role may CREATE
// DATABASE (each test provisions and drops its own, so no two runs and no two tests share a
// schema or a goose ledger). Set MIGRATIONS_TEST_REQUIRED=1 to turn "no database" from a skip
// into a failure. NEVER point it at the shared dev Postgres: one flat migration directory serves
// several goose ledgers against the same physical database (A1), so a Down here can drop what a
// different ledger still believes is applied.
//
//	docker run -d --rm --name pg -e POSTGRES_PASSWORD=changeme -e POSTGRES_USER=sentinel \
//	  -e POSTGRES_DB=sentinel -p 55432:5432 postgres:15-alpine
//	MIGRATIONS_TEST_DB_URL='postgres://sentinel:changeme@localhost:55432/sentinel?sslmode=disable' \
//	  GOWORK=off go test ./... -run Oncall -count=1 -v
//
// CI's `go-migrations` job declares no postgres service, so both tests skip there today; wiring
// one in is a CI-config change, which is not an implementer's to make.

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
)

const (
	// The two versions this test drives between: 1724100000 installs oncall_schedules and the
	// weekday CHECK, 1724200000 re-encodes 0 -> 7 and narrows the CHECK to ISO 1-7.
	oncallBaseVersion   int64 = 1724100000
	oncallAnchorVersion int64 = 1724200000

	weekdayCheckName = "oncall_schedules_handoff_weekday_check"
)

// TestOncallWeekdayReEncodeReplay replays 1724200000 up -> down -> up over a database holding one
// weekly schedule at every weekday value, and asserts the re-encode landed in both directions.
func TestOncallWeekdayReEncodeReplay(t *testing.T) {
	db, cleanup := freshMigrationDatabase(t, "replay")
	defer cleanup()

	ctx := context.Background()
	migrateTo(t, db, oncallBaseVersion)

	// Every value the pre-BUG-03 encoding could write (0 = Sunday .. 6 = Saturday) plus ISO 7,
	// which 1724100000's widened CHECK accepts and the shipped dashboard PUT actually writes.
	// One schedule per organization: idx_oncall_schedules_one_active permits a single ACTIVE
	// schedule per org, so sharing one org would silently seed a single row.
	for weekday := 0; weekday <= 7; weekday++ {
		seedWeeklySchedule(t, db, weekday)
	}
	// A daily schedule with a NULL weekday: the other arm of the CHECK, and the row that proves
	// the UPDATE's WHERE clause does not sweep NULLs into 7.
	seedDailySchedule(t, db, "daily-null")

	migrateTo(t, db, oncallAnchorVersion)

	// Up: only Sunday moves. 0 -> 7; 1..6 mean the same day in both encodings; an existing 7
	// stays 7 (the UPDATE is idempotent by its own WHERE clause).
	assertWeekday(t, db, map[string]any{
		"weekly-0": 7, "weekly-1": 1, "weekly-2": 2, "weekly-3": 3,
		"weekly-4": 4, "weekly-5": 5, "weekly-6": 6, "weekly-7": 7,
		"daily-null": nil,
	})
	assertColumnExists(t, db, "oncall_schedules", "rotation_anchor_at", true)
	// The ISO constraint is the one now judging writes: 0 is rejected, 7 accepted.
	assertWeekdayRejected(t, db, 0)

	if err := goose.DownToContext(ctx, db, migrationsDir(), oncallBaseVersion); err != nil {
		t.Fatalf("goose down to %d failed -- this is the direction whose UPDATE writes 0 under this file's own ISO 1-7 CHECK: %v", oncallBaseVersion, err)
	}

	// Down: the mirror image. Every ISO Sunday goes back to 0, including the row that was
	// already 7 before Up ran -- Down restores the encoding, not the pre-migration rows.
	assertWeekday(t, db, map[string]any{
		"weekly-0": 0, "weekly-1": 1, "weekly-2": 2, "weekly-3": 3,
		"weekly-4": 4, "weekly-5": 5, "weekly-6": 6, "weekly-7": 0,
		"daily-null": nil,
	})
	assertColumnExists(t, db, "oncall_schedules", "rotation_anchor_at", false)
	// Down lands on 1724100000's constraint, which accepts the union of both encodings.
	assertWeekdayAccepted(t, db, 0)
	assertWeekdayAccepted(t, db, 7)

	migrateTo(t, db, oncallAnchorVersion)

	assertWeekday(t, db, map[string]any{
		"weekly-0": 7, "weekly-1": 1, "weekly-2": 2, "weekly-3": 3,
		"weekly-4": 4, "weekly-5": 5, "weekly-6": 6, "weekly-7": 7,
		"daily-null": nil,
		// Written while the union CHECK was live, between Down and this second Up: it proves the
		// re-encode is not a one-shot that only fires on the first application.
		"accepted-0": 7, "accepted-7": 7,
	})
	assertColumnExists(t, db, "oncall_schedules", "rotation_anchor_at", true)
	assertWeekdayRejected(t, db, 0)
}

// TestOncallWeekdayReEncodeOverLegacyCheck runs 1724200000's Up against a database that still
// carries 1724100000's PRE-BUG-03 `BETWEEN 0 AND 6` CHECK. goose will not re-run an applied
// version to widen a constraint, so this is the shape of every database that applied 1724100000
// before BUG-03 landed -- and it is the only shape in which the Up direction's ordering can fail.
func TestOncallWeekdayReEncodeOverLegacyCheck(t *testing.T) {
	db, cleanup := freshMigrationDatabase(t, "legacy")
	defer cleanup()

	migrateTo(t, db, oncallBaseVersion)

	// Re-narrow to exactly what 1724100000 installed before BUG-03 widened it.
	mustExec(t, db, `ALTER TABLE oncall_schedules DROP CONSTRAINT `+weekdayCheckName)
	mustExec(t, db, `ALTER TABLE oncall_schedules ADD CONSTRAINT `+weekdayCheckName+` CHECK (
		(kind = 'weekly' AND handoff_weekday IS NOT NULL AND handoff_weekday BETWEEN 0 AND 6)
		OR (kind = 'daily' AND handoff_weekday IS NULL)
	)`)

	for weekday := 0; weekday <= 6; weekday++ {
		seedWeeklySchedule(t, db, weekday)
	}
	// 7 is unwritable here, which is the whole point: this database cannot store Sunday.
	assertWeekdayRejected(t, db, 7)

	migrateTo(t, db, oncallAnchorVersion)

	assertWeekday(t, db, map[string]any{
		"weekly-0": 7, "weekly-1": 1, "weekly-2": 2, "weekly-3": 3,
		"weekly-4": 4, "weekly-5": 5, "weekly-6": 6,
	})
	assertWeekdayRejected(t, db, 0)
}

// --- harness ---------------------------------------------------------------------------------

func migrationsDir() string { return "migrations" }

// freshMigrationDatabase provisions a database of its own on the server MIGRATIONS_TEST_DB_URL
// points at, so no two tests share a goose ledger or a schema, and drops it on cleanup.
func freshMigrationDatabase(t *testing.T, label string) (*sql.DB, func()) {
	t.Helper()

	dsn := strings.TrimSpace(os.Getenv("MIGRATIONS_TEST_DB_URL"))
	if dsn == "" {
		msg := "MIGRATIONS_TEST_DB_URL is unset: point it at a DISPOSABLE Postgres (see this file's header). Never the shared dev database."
		if os.Getenv("MIGRATIONS_TEST_REQUIRED") == "1" {
			t.Fatal(msg)
		}
		t.Skip(msg)
	}

	admin, err := Connect(dsn)
	if err != nil {
		msg := fmt.Sprintf("MIGRATIONS_TEST_DB_URL is unreachable: %v", err)
		if os.Getenv("MIGRATIONS_TEST_REQUIRED") == "1" {
			t.Fatal(msg)
		}
		t.Skip(msg)
	}

	name := fmt.Sprintf("dbmig_%s_%d", label, time.Now().UnixNano())
	if _, err := admin.Exec(`CREATE DATABASE ` + name); err != nil {
		admin.Close()
		t.Fatalf("CREATE DATABASE %s failed (the role needs CREATEDB; use a disposable server): %v", name, err)
	}

	target, err := Connect(withDatabase(t, dsn, name))
	if err != nil {
		dropDatabase(admin, name)
		admin.Close()
		t.Fatalf("failed to connect to the freshly created %s: %v", name, err)
	}

	return target, func() {
		target.Close()
		dropDatabase(admin, name)
		admin.Close()
	}
}

func dropDatabase(admin *sql.DB, name string) {
	_, _ = admin.Exec(`DROP DATABASE IF EXISTS ` + name + ` WITH (FORCE)`)
}

func withDatabase(t *testing.T, dsn, name string) string {
	t.Helper()

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("MIGRATIONS_TEST_DB_URL is not a URL: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}

// migrateTo runs the real migration directory up to version, through goose, on a ledger of this
// database's own. It is the project's sanctioned migration path (D1), not a SQL-file shim.
func migrateTo(t *testing.T, db *sql.DB, version int64) {
	t.Helper()

	goose.SetTableName("schema_migrations")
	if err := goose.UpToContext(context.Background(), db, migrationsDir(), version); err != nil {
		t.Fatalf("goose up to %d failed: %v", version, err)
	}
}

// seedWeeklySchedule inserts one organization and one active kind='weekly' schedule at weekday,
// named weekly-<weekday> so every assertion can name the row it means.
func seedWeeklySchedule(t *testing.T, db *sql.DB, weekday int) {
	t.Helper()
	name := fmt.Sprintf("weekly-%d", weekday)
	insertSchedule(t, db, name, "weekly", &weekday)
}

func seedDailySchedule(t *testing.T, db *sql.DB, name string) {
	t.Helper()
	insertSchedule(t, db, name, "daily", nil)
}

func insertSchedule(t *testing.T, db *sql.DB, name, kind string, weekday *int) {
	t.Helper()

	var orgID string
	if err := db.QueryRow(
		`INSERT INTO organizations (name, slug) VALUES ($1, $1) RETURNING id`, name,
	).Scan(&orgID); err != nil {
		t.Fatalf("failed to seed organization %s: %v", name, err)
	}

	var value any
	if weekday != nil {
		value = *weekday
	}
	if _, err := db.Exec(
		`INSERT INTO oncall_schedules (organization_id, name, kind, handoff_time, handoff_weekday, created_by)
		 VALUES ($1, $2, $3, '09:00', $4, 'bug-06-test')`,
		orgID, name, kind, value,
	); err != nil {
		t.Fatalf("failed to seed schedule %s (kind=%s weekday=%v): %v", name, kind, weekday, err)
	}
}

// assertWeekday checks the handoff_weekday of every named schedule, and that the table holds
// exactly those rows -- an assertion that only names rows it expects cannot see a stray one.
func assertWeekday(t *testing.T, db *sql.DB, want map[string]any) {
	t.Helper()

	rows, err := db.Query(`SELECT name, handoff_weekday FROM oncall_schedules ORDER BY name`)
	if err != nil {
		t.Fatalf("failed to read oncall_schedules: %v", err)
	}
	defer rows.Close()

	got := map[string]any{}
	for rows.Next() {
		var name string
		var weekday sql.NullInt64
		if err := rows.Scan(&name, &weekday); err != nil {
			t.Fatalf("failed to scan oncall_schedules: %v", err)
		}
		if weekday.Valid {
			got[name] = int(weekday.Int64)
		} else {
			got[name] = nil
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("failed to iterate oncall_schedules: %v", err)
	}

	if len(got) != len(want) {
		t.Fatalf("oncall_schedules holds %d rows, want %d: got %v, want %v", len(got), len(want), got, want)
	}
	for name, expected := range want {
		actual, ok := got[name]
		if !ok {
			t.Fatalf("schedule %q is missing: got %v", name, got)
		}
		if actual != expected {
			t.Errorf("schedule %q has handoff_weekday %v, want %v", name, actual, expected)
		}
	}
}

// assertWeekdayRejected proves which CHECK is in force by writing through it: the constraint that
// judges a write is the one live at the moment of the write, which is the fact this whole file
// exists to pin down.
func assertWeekdayRejected(t *testing.T, db *sql.DB, weekday int) {
	t.Helper()

	name := fmt.Sprintf("rejected-%d", weekday)
	if err := tryInsertSchedule(db, name, weekday); err == nil {
		t.Fatalf("handoff_weekday %d was accepted; expected %s to reject it", weekday, weekdayCheckName)
	} else if !strings.Contains(err.Error(), weekdayCheckName) {
		t.Fatalf("handoff_weekday %d was rejected by something other than %s: %v", weekday, weekdayCheckName, err)
	}
}

func assertWeekdayAccepted(t *testing.T, db *sql.DB, weekday int) {
	t.Helper()

	name := fmt.Sprintf("accepted-%d", weekday)
	if err := tryInsertSchedule(db, name, weekday); err != nil {
		t.Fatalf("handoff_weekday %d was rejected; the constraint in force should accept it: %v", weekday, err)
	}
}

func tryInsertSchedule(db *sql.DB, name string, weekday int) error {
	var orgID string
	if err := db.QueryRow(
		`INSERT INTO organizations (name, slug) VALUES ($1, $1) RETURNING id`, name,
	).Scan(&orgID); err != nil {
		return err
	}
	_, err := db.Exec(
		`INSERT INTO oncall_schedules (organization_id, name, kind, handoff_time, handoff_weekday, created_by)
		 VALUES ($1, $2, 'weekly', '09:00', $3, 'bug-06-test')`,
		orgID, name, weekday,
	)
	if err != nil {
		// Leave no organization behind for an insert that was meant to fail: assertWeekday
		// counts rows, and a half-seeded probe would show up as a missing schedule.
		_, _ = db.Exec(`DELETE FROM organizations WHERE id = $1`, orgID)
	}
	return err
}

func assertColumnExists(t *testing.T, db *sql.DB, table, column string, want bool) {
	t.Helper()

	var exists bool
	if err := db.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = $1 AND column_name = $2)`,
		table, column,
	).Scan(&exists); err != nil {
		t.Fatalf("failed to introspect %s.%s: %v", table, column, err)
	}
	if exists != want {
		t.Errorf("%s.%s exists = %v, want %v", table, column, exists, want)
	}
}

func mustExec(t *testing.T, db *sql.DB, stmt string) {
	t.Helper()

	if _, err := db.Exec(stmt); err != nil {
		t.Fatalf("failed to execute %q: %v", stmt, err)
	}
}
