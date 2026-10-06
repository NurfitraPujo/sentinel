package dbmigrations

// sentinel-occurrence-spans TASK-02: 1725800000 adds the on-demand columns + terminal outcome to span_pull_queue and the
// span_pull_requests ledger (audit + per-organization rate limit). Needs a DISPOSABLE Postgres (MIGRATIONS_TEST_DB_URL,
// see migrations_oncall_weekday_test.go's header) and self-skips without one.

import (
	"context"
	"database/sql"
	"testing"

	"github.com/pressly/goose/v3"
)

const spanPullOnDemandVersion int64 = 1725800000

func TestSpanPullOnDemandMigration(t *testing.T) {
	db, cleanup := freshMigrationDatabase(t, "pullod")
	defer cleanup()
	migrateTo(t, db, spanPullOnDemandVersion-1)

	// A row queued BEFORE the migration (the automatic path) must keep working and pick up the defaults.
	const org = "11111111-1111-1111-1111-111111111111"
	const trace = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err := db.Exec(`INSERT INTO span_pull_queue (org_id, trace_id, next_fetch_at) VALUES ($1, $2, now())`, org, trace); err != nil {
		t.Fatalf("seed queue row: %v", err)
	}
	migrateTo(t, db, spanPullOnDemandVersion)

	var onDemand bool
	var by, kind, outcome string
	var at sql.NullTime
	if err := db.QueryRow(`SELECT on_demand, requested_by, requested_by_kind, requested_at, outcome FROM span_pull_queue WHERE org_id = $1`, org).
		Scan(&onDemand, &by, &kind, &at, &outcome); err != nil {
		t.Fatalf("read defaults: %v", err)
	}
	if onDemand || by != "" || kind != "" || at.Valid || outcome != "" {
		t.Fatalf("defaults = %v %q %q %v %q, want false, empty, empty, NULL, empty", onDemand, by, kind, at, outcome)
	}

	// The ledger takes a request and refuses a malformed trace id or an unknown requester kind.
	insert := `INSERT INTO span_pull_requests (org_id, trace_id, action, requested_by, requested_by_kind) VALUES ($1, $2, 'queued', 'u1', $3)`
	if _, err := db.Exec(insert, org, trace, "user"); err != nil {
		t.Fatalf("insert request: %v", err)
	}
	if _, err := db.Exec(insert, org, "not-a-trace", "user"); err == nil {
		t.Fatal("a malformed trace id must violate the CHECK")
	}
	if _, err := db.Exec(insert, org, trace, "robot"); err == nil {
		t.Fatal("an unknown requester kind must violate the CHECK")
	}

	// Replay: the Up runs again over the existing columns and table, keeping their rows.
	if _, err := db.Exec(`DELETE FROM schema_migrations WHERE version_id = $1`, spanPullOnDemandVersion); err != nil {
		t.Fatalf("forget the version: %v", err)
	}
	migrateTo(t, db, spanPullOnDemandVersion)
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM span_pull_requests`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("ledger rows after a replay = %d (%v), want 1", n, err)
	}

	// Down removes both; up again recreates them.
	if err := goose.DownToContext(context.Background(), db, migrationsDir(), spanPullOnDemandVersion-1); err != nil {
		t.Fatalf("goose down: %v", err)
	}
	var reg sql.NullString
	if err := db.QueryRow(`SELECT to_regclass('span_pull_requests')::text`).Scan(&reg); err != nil || reg.Valid {
		t.Fatalf("span_pull_requests after down: %v, %v; want dropped", reg, err)
	}
	var cols int
	if err := db.QueryRow(`SELECT count(*) FROM information_schema.columns WHERE table_name = 'span_pull_queue' AND column_name IN ('on_demand','requested_by','requested_by_kind','requested_at','outcome')`).Scan(&cols); err != nil || cols != 0 {
		t.Fatalf("on-demand columns after down = %d (%v), want 0", cols, err)
	}
	migrateTo(t, db, spanPullOnDemandVersion)
}
