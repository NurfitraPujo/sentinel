package dbmigrations

// sentinel-reporting-and-dashboard D18: 1726000000 adds service / tag_source / request_scoped to error_occurrences and
// 1726000100 backfills them from tags in batches of 5,000. Needs a DISPOSABLE Postgres (MIGRATIONS_TEST_DB_URL, see
// migrations_oncall_weekday_test.go's header) and self-skips without one.

import (
	"database/sql"
	"os"
	"strings"
	"testing"
)

const (
	serviceFieldsVersion         int64 = 1726000000
	serviceFieldsBackfillVersion int64 = 1726000100
)

func TestServiceFieldsMigrationAndBackfill(t *testing.T) {
	db, cleanup := freshMigrationDatabase(t, "svcfields")
	defer cleanup()
	migrateTo(t, db, serviceFieldsVersion-1)

	seedServiceFieldRows(t, db)

	migrateTo(t, db, serviceFieldsVersion)

	// Existing rows pick up the defaults; `source` is untouched.
	var svc, tagSrc sql.NullString
	var scoped bool
	if err := db.QueryRow(`SELECT service, tag_source, request_scoped FROM error_occurrences WHERE source = 'sentry-compat' LIMIT 1`).
		Scan(&svc, &tagSrc, &scoped); err != nil {
		t.Fatalf("read defaults: %v", err)
	}
	if svc.Valid || tagSrc.Valid || scoped {
		t.Fatalf("defaults = %v %v %v, want NULL, NULL, false", svc, tagSrc, scoped)
	}
	// More than one batch of 5,000 so the keyset loop is exercised: seedServiceFieldRows wrote 5,003 tagged rows.
	migrateTo(t, db, serviceFieldsBackfillVersion)

	type want struct {
		service, tagSource sql.NullString
		scoped             bool
	}
	check := func(label string, w want) {
		t.Helper()
		var g want
		if err := db.QueryRow(`SELECT service, tag_source, request_scoped FROM error_occurrences WHERE metadata->>'label' = $1`, label).
			Scan(&g.service, &g.tagSource, &g.scoped); err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if g != w {
			t.Fatalf("%s = %+v, want %+v", label, g, w)
		}
	}
	ns := func(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }
	check("http", want{ns("api"), ns("http_handler"), true})
	check("job", want{ns("jobs worker"), ns("job"), false})
	check("logger-with-ctx", want{ns("api"), ns("logger"), true})
	check("logger-bare", want{ns("api"), ns("logger"), false})
	check("blank-service", want{ns(""), ns("job"), false})
	check("untagged", want{ns(""), ns(""), false})
	// Regression: tags without a usable `source` made the request_scoped expression NULL (SQLSTATE 23502).
	check("no-source-service-only", want{ns("api"), ns(""), false})
	check("empty-source", want{ns("api"), ns(""), false})
	check("blank-source", want{ns(""), ns(""), false})
	check("empty-tags", want{ns(""), ns(""), false})
	check("other-tags-only", want{ns(""), ns(""), false})
	check("no-source-next-to-logger", want{ns("api"), ns(""), false})

	var n int
	if err := db.QueryRow(`SELECT count(*) FROM error_occurrences WHERE service = 'batch' AND tag_source = 'worker_handler'`).Scan(&n); err != nil || n != 5000 {
		t.Fatalf("batch rows backfilled = %d (%v), want 5000", n, err)
	}
	// `source` was never written.
	if err := db.QueryRow(`SELECT count(*) FROM error_occurrences WHERE source = 'sentry-compat'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("source rows = %d (%v), want 1 untouched", n, err)
	}

	// Replay both: same values, no error.
	for _, v := range []int64{serviceFieldsVersion, serviceFieldsBackfillVersion} {
		if _, err := db.Exec(`DELETE FROM schema_migrations WHERE version_id = $1`, v); err != nil {
			t.Fatalf("forget %d: %v", v, err)
		}
	}
	migrateTo(t, db, serviceFieldsBackfillVersion)
	check("http", want{ns("api"), ns("http_handler"), true})
}

func seedServiceFieldRows(t *testing.T, db *sql.DB) {
	t.Helper()
	var projectID, issueID string
	must := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := db.QueryRow(`INSERT INTO projects (name, api_key, api_key_hash) VALUES ('p', 'proj_x', 'h') RETURNING id`).Scan(&projectID); err != nil {
		t.Fatalf("project: %v", err)
	}
	if err := db.QueryRow(`INSERT INTO issues (project_id, fingerprint, message, error_class, environment) VALUES ($1, 'fp', 'm', 'E', 'production') RETURNING id`, projectID).Scan(&issueID); err != nil {
		t.Fatalf("issue: %v", err)
	}
	ins := `INSERT INTO error_occurrences (id, issue_id, environment, platform, metadata, tags, transaction, request, source)
	        VALUES (gen_random_uuid(), $1, 'production', 'go', jsonb_build_object('label', $2::text), $3::jsonb, $4, $5::jsonb, $6)`
	must(ins, issueID, "http", `{"service":"api","source":"http_handler"}`, nil, nil, nil)
	must(ins, issueID, "job", `{"service":"  jobs worker ","source":"job"}`, nil, nil, nil)
	must(ins, issueID, "logger-with-ctx", `{"service":"api","source":"logger"}`, "GET /x", `{"url":"https://h/x"}`, nil)
	must(ins, issueID, "logger-bare", `{"service":"api","source":"logger"}`, nil, nil, nil)
	must(ins, issueID, "blank-service", `{"service":"   ","source":"job"}`, nil, nil, nil)
	must(ins, issueID, "untagged", nil, nil, nil, "sentry-compat")
	must(ins, issueID, "no-source-service-only", `{"service":"api"}`, nil, nil, nil)
	must(ins, issueID, "empty-source", `{"service":"api","source":""}`, nil, nil, nil)
	must(ins, issueID, "blank-source", `{"source":"  "}`, nil, nil, nil)
	must(ins, issueID, "empty-tags", `{}`, nil, nil, nil)
	must(ins, issueID, "other-tags-only", `{"release":"1.0"}`, "GET /x", `{"url":"https://h/x"}`, nil)
	// A logger row with request context sits in the same table (and, ids being random uuids, the same batches) as the
	// no-source rows above, and a transaction/url on a no-source row must not make it request-scoped.
	must(ins, issueID, "no-source-next-to-logger", `{"service":"api"}`, "GET /y", `{"url":"https://h/y"}`, nil)
	must(`INSERT INTO error_occurrences (id, issue_id, environment, platform, metadata, tags)
	      SELECT gen_random_uuid(), $1, 'production', 'go', '{}'::jsonb, '{"service":"batch","source":"worker_handler"}'::jsonb FROM generate_series(1, 5000)`, issueID)
}

// The role's statement_timeout (5 minutes in production) applies to the backfill's single DO statement; its internal
// COMMITs do not reset it. The file must lift it before the DO block and restore it after.
func TestServiceFieldsBackfillLiftsStatementTimeout(t *testing.T) {
	b, err := os.ReadFile("migrations/1726000100_backfill_error_occurrences_service_fields.sql")
	if err != nil {
		t.Fatal(err)
	}
	sqlText := string(b)
	lift := strings.Index(sqlText, "SET statement_timeout = 0;")
	do := strings.Index(sqlText, "DO $backfill$")
	reset := strings.Index(sqlText, "RESET statement_timeout;")
	if lift < 0 || do < 0 || reset < 0 || !(lift < do && do < reset) {
		t.Fatalf("want SET statement_timeout = 0 (%d) < DO block (%d) < RESET statement_timeout (%d)", lift, do, reset)
	}
}

// Runs the backfill under a database whose statement_timeout is far shorter than the backfill takes, as the production
// role's 5 minutes is shorter than production's. Without `SET statement_timeout = 0` it fails with SQLSTATE 57014.
func TestServiceFieldsBackfillSurvivesShortStatementTimeout(t *testing.T) {
	db, cleanup := freshMigrationDatabase(t, "svcfieldsto")
	defer cleanup()
	migrateTo(t, db, serviceFieldsVersion-1)
	seedServiceFieldRows(t, db)
	migrateTo(t, db, serviceFieldsVersion)

	var dbName string
	if err := db.QueryRow(`SELECT current_database()`).Scan(&dbName); err != nil {
		t.Fatal(err)
	}
	// New sessions pick this up; the pool is closed and reopened by forcing fresh connections below.
	if _, err := db.Exec(`ALTER DATABASE ` + dbName + ` SET statement_timeout = '30ms'`); err != nil {
		t.Fatalf("alter database: %v", err)
	}
	db.SetMaxIdleConns(0) // close pooled sessions so the migration runs on new ones carrying the 30ms default
	db.SetMaxIdleConns(2) // then allow reuse, as in production, so session-level SETs reach the DO block
	migrateTo(t, db, serviceFieldsBackfillVersion)

	var n int
	if err := db.QueryRow(`SELECT count(*) FROM error_occurrences WHERE service = 'batch' AND tag_source = 'worker_handler'`).Scan(&n); err != nil || n != 5000 {
		t.Fatalf("batch rows backfilled = %d (%v), want 5000", n, err)
	}
}
