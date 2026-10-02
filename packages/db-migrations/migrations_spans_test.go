package dbmigrations

// sentinel-span-ingest D1/D9: 1725600000 adds the day-partitioned `spans` table, the partition maintenance
// functions the retention cron calls, and `span_pull_queue`. Needs a DISPOSABLE Postgres
// (MIGRATIONS_TEST_DB_URL, see migrations_oncall_weekday_test.go's header) and self-skips without one.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
)

const spansVersion int64 = 1725600000

func TestSpansMigrationPartitionsAndRetention(t *testing.T) {
	db, cleanup := freshMigrationDatabase(t, "spans")
	defer cleanup()
	migrateTo(t, db, spansVersion)

	today := time.Now().UTC().Truncate(24 * time.Hour)
	// Up created yesterday, today and the two days ahead.
	if got := spanPartitions(t, db); len(got) != 4 {
		t.Fatalf("initial partitions = %v, want 4 (today-1 .. today+2)", got)
	}
	if n := spansEnsure(t, db); n != 0 {
		t.Fatalf("a second ensure created %d partitions, want 0 (idempotent)", n)
	}

	org := seedRelayOrg(t, db)
	insertSpan(t, db, org, "a1", today.Add(time.Hour), nil)
	// Same (trace_id, span_id, start_ts): the dedupe key. ON CONFLICT DO NOTHING stores one row.
	insertSpan(t, db, org, "a1", today.Add(time.Hour), nil)
	if n := countSpans(t, db); n != 1 {
		t.Fatalf("duplicate span stored %d rows, want 1", n)
	}

	// Dedupe is per organization (repair m1): the same trace/span/start under another org is a different row, so one
	// org can neither block another's span nor make a second org pulling the same trace store nothing.
	org2 := seedRelayOrgNamed(t, db, "spans-org-2")
	insertSpan(t, db, org2, "a1", today.Add(time.Hour), nil)
	if n := countSpans(t, db); n != 2 {
		t.Fatalf("a second org's identical span stored %d rows in total, want 2", n)
	}

	// D1: a span outside the created window is refused by the database, never routed into a default partition.
	if err := insertSpanErr(db, org, "a2", today.AddDate(0, 0, -20).Add(time.Hour)); err == nil {
		t.Fatal("a span 20 days back must have no partition (no DEFAULT partition exists)")
	}
	if err := insertSpanErr(db, org, "a3", today.AddDate(0, 0, 10)); err == nil {
		t.Fatal("a span 10 days ahead must have no partition")
	}

	// Retention: partitions for day 13 (kept) and day 15, 20 (expired) back, each holding a row.
	for _, back := range []int{13, 15, 20} {
		day := today.AddDate(0, 0, -back)
		createDayPartition(t, db, day)
		insertSpan(t, db, org, fmt.Sprintf("b%d", back), day.Add(time.Hour), nil)
	}
	if n := spansDropExpired(t, db, 14); n != 2 {
		t.Fatalf("dropped %d partitions, want 2 (the 15- and 20-day-old ones)", n)
	}
	for _, p := range spanPartitions(t, db) {
		if p == "spans_p"+today.AddDate(0, 0, -15).Format("20060102") || p == "spans_p"+today.AddDate(0, 0, -20).Format("20060102") {
			t.Fatalf("expired partition %s survived", p)
		}
	}
	if n := countSpans(t, db); n != 3 { // a1 (two orgs) and b13
		t.Fatalf("rows after retention = %d, want 3 (today's two and the 13-day-old span)", n)
	}
	if n := spansDropExpired(t, db, 14); n != 0 {
		t.Fatalf("a second drop removed %d partitions, want 0", n)
	}

	// Replay (one flat directory serves several goose ledgers): down then up again over the same database.
	if err := goose.DownToContext(context.Background(), db, migrationsDir(), spansVersion-1); err != nil {
		t.Fatalf("goose down: %v", err)
	}
	migrateTo(t, db, spansVersion)
	if got := spanPartitions(t, db); len(got) != 4 {
		t.Fatalf("partitions after down/up = %v, want 4", got)
	}
}

func spansEnsure(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT spans_ensure_partitions(1, 2)`).Scan(&n); err != nil {
		t.Fatalf("spans_ensure_partitions: %v", err)
	}
	return n
}

func spansDropExpired(t *testing.T, db *sql.DB, keep int) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT spans_drop_expired($1)`, keep).Scan(&n); err != nil {
		t.Fatalf("spans_drop_expired: %v", err)
	}
	return n
}

func spanPartitions(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT c.relname FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
	                        WHERE i.inhparent = 'spans'::regclass ORDER BY 1`)
	if err != nil {
		t.Fatalf("list partitions: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func createDayPartition(t *testing.T, db *sql.DB, day time.Time) {
	t.Helper()
	q := fmt.Sprintf(`CREATE TABLE spans_p%s PARTITION OF spans FOR VALUES FROM ('%s 00:00:00+00') TO ('%s 00:00:00+00')`,
		day.Format("20060102"), day.Format("2006-01-02"), day.AddDate(0, 0, 1).Format("2006-01-02"))
	if _, err := db.Exec(q); err != nil {
		t.Fatalf("create partition: %v", err)
	}
}

func insertSpanErr(db *sql.DB, org, tag string, ts time.Time) error {
	_, err := db.Exec(`INSERT INTO spans (org_id, source, trace_id, span_id, start_ts)
	                    VALUES ($1, 'mobile', $2, $3, $4) ON CONFLICT DO NOTHING`,
		org, strings.Repeat("a", 32), strings.Repeat("0", 14)+tag[:2], ts)
	return err
}

func insertSpan(t *testing.T, db *sql.DB, org, tag string, ts time.Time, _ any) {
	t.Helper()
	if err := insertSpanErr(db, org, tag, ts); err != nil {
		t.Fatalf("insert span %s: %v", tag, err)
	}
}

func countSpans(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM spans`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func seedRelayOrgNamed(t *testing.T, db *sql.DB, name string) string {
	t.Helper()
	var id string
	if err := db.QueryRow(`INSERT INTO organizations (name, slug) VALUES ($1, $1) RETURNING id`, name).Scan(&id); err != nil {
		t.Fatalf("seed organization: %v", err)
	}
	return id
}
