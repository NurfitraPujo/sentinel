package dbmigrations

// sentinel-aws-error-reporting TASK-02: 1725700000 adds span_pull_issue_window, the per-issue throttle on span pulls
// (apps/processor-go/spanpull.Queue.MaybeEnqueue). Needs a DISPOSABLE Postgres (MIGRATIONS_TEST_DB_URL, see
// migrations_oncall_weekday_test.go's header) and self-skips without one.

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
)

const spanPullIssueWindowVersion int64 = 1725700000

// The processor's statement, verbatim but for the placeholders' casts: a row back means "queue a pull".
const issueWindowClaim = `INSERT INTO span_pull_issue_window (issue_id, last_enqueued_at) VALUES ($1, $2)
	ON CONFLICT (issue_id) DO UPDATE SET last_enqueued_at = EXCLUDED.last_enqueued_at
	 WHERE span_pull_issue_window.last_enqueued_at <= $3
	RETURNING issue_id`

func TestSpanPullIssueWindowMigration(t *testing.T) {
	db, cleanup := freshMigrationDatabase(t, "pullwin")
	defer cleanup()
	migrateTo(t, db, spanPullIssueWindowVersion)

	org := seedRelayOrg(t, db)
	project := seedRelayProject(t, db, org)
	issue := seedWindowIssue(t, db, project, "fp-window")

	start := time.Date(2026, 10, 6, 1, 0, 0, 0, time.UTC)
	window := 10 * time.Minute
	if !claimWindow(t, db, issue, start, window) {
		t.Fatal("the issue's first claim must be allowed")
	}
	if claimWindow(t, db, issue, start.Add(time.Minute), window) {
		t.Fatal("a claim inside the window must be refused")
	}
	if !claimWindow(t, db, issue, start.Add(window), window) {
		t.Fatal("a claim once the window has elapsed must be allowed")
	}

	// The row goes with its issue.
	if _, err := db.Exec(`DELETE FROM issues WHERE id = $1`, issue); err != nil {
		t.Fatalf("delete issue: %v", err)
	}
	if n := countWindowRows(t, db); n != 0 {
		t.Fatalf("window rows after the issue was deleted = %d, want 0 (ON DELETE CASCADE)", n)
	}
	// And cannot exist without one.
	if _, err := db.Exec(`INSERT INTO span_pull_issue_window (issue_id, last_enqueued_at) VALUES (gen_random_uuid(), now())`); err == nil {
		t.Fatal("a window row for a nonexistent issue must violate the foreign key")
	}

	// Replay (one flat directory serves several goose ledgers): the Up runs again over the existing table, keeping
	// its rows.
	kept := seedWindowIssue(t, db, project, "fp-kept")
	claimWindow(t, db, kept, start, window)
	if _, err := db.Exec(`DELETE FROM schema_migrations WHERE version_id = $1`, spanPullIssueWindowVersion); err != nil {
		t.Fatalf("forget the version: %v", err)
	}
	migrateTo(t, db, spanPullIssueWindowVersion)
	if n := countWindowRows(t, db); n != 1 {
		t.Fatalf("window rows after a replay = %d, want 1", n)
	}

	// Down drops it; up again recreates it.
	if err := goose.DownToContext(context.Background(), db, migrationsDir(), spanPullIssueWindowVersion-1); err != nil {
		t.Fatalf("goose down: %v", err)
	}
	var reg sql.NullString
	if err := db.QueryRow(`SELECT to_regclass('span_pull_issue_window')::text`).Scan(&reg); err != nil || reg.Valid {
		t.Fatalf("span_pull_issue_window after down: %v, %v; want dropped", reg, err)
	}
	migrateTo(t, db, spanPullIssueWindowVersion)
	if n := countWindowRows(t, db); n != 0 {
		t.Fatalf("window rows after down/up = %d, want 0", n)
	}
}

func seedWindowIssue(t *testing.T, db *sql.DB, project, fingerprint string) string {
	t.Helper()
	var id string
	if err := db.QueryRow(`INSERT INTO issues (project_id, environment, fingerprint, message, error_class)
		VALUES ($1, 'production', $2, 'boom', 'Error') RETURNING id`, project, fingerprint).Scan(&id); err != nil {
		t.Fatalf("seed issue: %v", err)
	}
	return id
}

func claimWindow(t *testing.T, db *sql.DB, issue string, now time.Time, window time.Duration) bool {
	t.Helper()
	var got string
	err := db.QueryRow(issueWindowClaim, issue, now, now.Add(-window)).Scan(&got)
	if errors.Is(err, sql.ErrNoRows) {
		return false
	}
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	return true
}

func countWindowRows(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM span_pull_issue_window`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
