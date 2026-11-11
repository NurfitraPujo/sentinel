package integration

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NurfitraPujo/sentinel/packages/db-migrations"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func getDSN() string {
	cfg, _ := GetTestConfig()
	if cfg.Host == "" {
		return ""
	}
	return "host=" + cfg.Host + " port=" + cfg.Port + " user=" + cfg.User + " password=" + cfg.Password + " dbname=" + cfg.DB + " sslmode=disable"
}

// Disposable, migration-isolated databases for the migration tests.
//
// WHY THIS EXISTS. These tests run goose `up` AND `down` against the real migration files. They used
// to share the dev database and isolate themselves only by goose ledger TABLE name
// (status_test_migrations, seq_migrations, processor_migrations, dashboard_migrations,
// baseline_test_migrations). A ledger table records which versions THAT ledger applied — it does not
// scope the DDL. So a `down` run under one ledger dropped tables the main `schema_migrations` ledger
// still believed existed, leaving the shared database reporting
// `max(version_id) = 1722000000` while `project_api_keys` did not exist at all.
//
// That corrupted the dev database three separate times during this project, and each time it
// surfaced as a wave of unrelated integration failures that read like a code regression. Isolating by
// DATABASE rather than by ledger table makes it structurally impossible.
//
// HOW. A template database is migrated ONCE per process, then each test gets its own database via
// `CREATE DATABASE ... TEMPLATE ...`, which is a file copy rather than a re-run of the whole
// migration chain. Pattern borrowed from works/example-core/test/setup.go, including the parts that are
// easy to omit and painful to debug: a pg_advisory_lock so concurrent test processes cannot race to
// create the template, a retry around "is being accessed by other users" (cloning fails if anything
// is still connected to the template), and pruning of orphans left behind by killed runs.
const (
	migTemplateDB   = "sentinel_migtest_template"
	migTemplateLock = 90210
	migClonePrefix  = "sentinel_migtest_"
)

var migTemplateOnce sync.Once
var migPruneOnce sync.Once

// migPruneOrphans drops clone databases older than an hour. A killed run leaves its clone behind, and
// without this they accumulate until the server hits max connections or disk.
func migPruneOrphans(admin *sql.DB, t *testing.T) {
	rows, err := admin.Query("SELECT datname FROM pg_database WHERE datname LIKE $1", migClonePrefix+"%")
	if err != nil {
		return
	}
	var names []string
	for rows.Next() {
		var n string
		if rows.Scan(&n) == nil {
			names = append(names, n)
		}
	}
	rows.Close()

	now := time.Now().Unix()
	for _, n := range names {
		if n == migTemplateDB {
			continue
		}
		parts := strings.Split(strings.TrimPrefix(n, migClonePrefix), "_")
		if len(parts) < 2 {
			continue
		}
		ts, convErr := strconv.ParseInt(parts[len(parts)-1], 10, 64)
		if convErr != nil || now-ts <= 3600 {
			continue
		}
		t.Logf("housekeeping: dropping orphaned migration-test database %s", n)
		_, _ = admin.Exec(fmt.Sprintf("DROP DATABASE IF EXISTS %q WITH (FORCE)", n))
	}
}

// migTestDB returns a connection to a fresh, disposable database cloned from a migrated template.
// The database is dropped on cleanup.
func migTestDB(t *testing.T, label string) *sql.DB {
	t.Helper()

	adminDSN := getDSN()
	var dsnErr error
	if adminDSN == "" {
		dsnErr = fmt.Errorf("POSTGRES_HOST not configured")
	}
	requireInfra(t, dsnErr, "postgres DSN")

	admin, err := sql.Open("pgx", adminDSN)
	require.NoError(t, err)
	defer admin.Close()
	require.NoError(t, admin.Ping())

	migPruneOnce.Do(func() { migPruneOrphans(admin, t) })

	// The advisory lock is held only while creating/migrating the template. Two `go test` processes
	// against the same server would otherwise both find the template missing and both try to build it.
	_, err = admin.Exec("SELECT pg_advisory_lock($1)", migTemplateLock)
	require.NoError(t, err)

	var exists bool
	err = admin.QueryRow("SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)", migTemplateDB).Scan(&exists)
	if err == nil && !exists {
		if _, cErr := admin.Exec(fmt.Sprintf("CREATE DATABASE %q", migTemplateDB)); cErr != nil &&
			!strings.Contains(cErr.Error(), "already exists") {
			_, _ = admin.Exec("SELECT pg_advisory_unlock($1)", migTemplateLock)
			require.NoError(t, cErr)
		}
	}

	absDir, err := filepath.Abs("../../packages/db-migrations/migrations")
	if err != nil {
		_, _ = admin.Exec("SELECT pg_advisory_unlock($1)", migTemplateLock)
		require.NoError(t, err)
	}

	var migErr error
	migTemplateOnce.Do(func() {
		tdb, oErr := sql.Open("pgx", replaceDBName(adminDSN, migTemplateDB))
		if oErr != nil {
			migErr = oErr
			return
		}
		defer tdb.Close()
		migErr = dbmigrations.RunMigrations(context.Background(), tdb, "up", dbmigrations.MigrationOptions{
			TableName: "migtest_template_migrations",
			Directory: absDir,
		})
	})
	_, _ = admin.Exec("SELECT pg_advisory_unlock($1)", migTemplateLock)
	require.NoError(t, migErr, "could not migrate the migration-test template database")

	dbName := fmt.Sprintf("%s%s_%d", migClonePrefix, label, time.Now().Unix())
	var cloneErr error
	for attempt := 1; attempt <= 10; attempt++ {
		_, cloneErr = admin.Exec(fmt.Sprintf("CREATE DATABASE %q TEMPLATE %q", dbName, migTemplateDB))
		if cloneErr == nil {
			break
		}
		// CREATE DATABASE ... TEMPLATE refuses while anything is connected to the template.
		if strings.Contains(cloneErr.Error(), "being accessed by other users") ||
			strings.Contains(cloneErr.Error(), "already exists") {
			dbName = fmt.Sprintf("%s%s_%d_%d", migClonePrefix, label, attempt, time.Now().UnixNano())
			time.Sleep(150 * time.Millisecond)
			continue
		}
		break
	}
	require.NoError(t, cloneErr, "could not clone the migration-test template database")

	t.Cleanup(func() {
		cleanup, cErr := sql.Open("pgx", adminDSN)
		if cErr != nil {
			return
		}
		defer cleanup.Close()
		_, _ = cleanup.Exec(fmt.Sprintf("DROP DATABASE IF EXISTS %q WITH (FORCE)", dbName))
	})

	db, err := sql.Open("pgx", replaceDBName(adminDSN, dbName))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, db.Ping())
	return db
}

// replaceDBName swaps the dbname in a keyword/value DSN (the form getDSN builds).
func replaceDBName(dsn, dbName string) string {
	parts := strings.Fields(dsn)
	for i, p := range parts {
		if strings.HasPrefix(p, "dbname=") {
			parts[i] = "dbname=" + dbName
		}
	}
	return strings.Join(parts, " ")
}

func TestMigrationStatus(t *testing.T) {
	db := migTestDB(t, "status")
	var err error

	absDir, err := filepath.Abs("../../packages/db-migrations/migrations")
	require.NoError(t, err)

	opts := dbmigrations.MigrationOptions{
		TableName: "status_test_migrations",
		Directory: absDir,
	}

	err = dbmigrations.RunMigrations(context.Background(), db, "up", opts)
	require.NoError(t, err)

	err = dbmigrations.GetStatus(context.Background(), db, opts)
	require.NoError(t, err)
}

func TestSequentialMigrations(t *testing.T) {
	db := migTestDB(t, "sequential")
	var err error

	absDir, err := filepath.Abs("../../packages/db-migrations/migrations")
	require.NoError(t, err)

	opts := dbmigrations.MigrationOptions{
		TableName: "seq_migrations",
		Directory: absDir,
	}

	err = dbmigrations.RunMigrations(context.Background(), db, "up", opts)
	require.NoError(t, err)

	var count int
	err = db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM issues").Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count)

	err = dbmigrations.RunMigrations(context.Background(), db, "down", opts)
	require.NoError(t, err)
}

func TestTargetIsolation(t *testing.T) {
	db := migTestDB(t, "targetiso")
	var err error

	absDir, err := filepath.Abs("../../packages/db-migrations/migrations")
	require.NoError(t, err)

	opts1 := dbmigrations.MigrationOptions{
		TableName: "processor_migrations",
		Directory: absDir,
	}

	err = dbmigrations.RunMigrations(context.Background(), db, "up", opts1)
	require.NoError(t, err)

	var processorCount int
	err = db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM processor_migrations").Scan(&processorCount)
	require.NoError(t, err)
	assert.Greater(t, processorCount, 0)

	opts2 := dbmigrations.MigrationOptions{
		TableName: "dashboard_migrations",
		Directory: absDir,
	}

	err = dbmigrations.RunMigrations(context.Background(), db, "up", opts2)
	require.NoError(t, err)

	var dashboardCount int
	err = db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM dashboard_migrations").Scan(&dashboardCount)
	require.NoError(t, err)
	assert.Equal(t, processorCount, dashboardCount)
}

func TestBaselineCommand(t *testing.T) {
	db := migTestDB(t, "baseline")
	var err error

	absDir, err := filepath.Abs("../../packages/db-migrations/migrations")
	require.NoError(t, err)

	opts := dbmigrations.MigrationOptions{
		TableName: "baseline_test_migrations",
		Directory: absDir,
	}

	err = dbmigrations.RunMigrations(context.Background(), db, "up", opts)
	require.NoError(t, err)

	initialVersion := int64(1716508800)
	_ = dbmigrations.BaselineVersion(context.Background(), db, initialVersion, opts)

	err = dbmigrations.GetStatus(context.Background(), db, opts)
	require.NoError(t, err)
}

// TestOncallHandoffWeekdayReEncodeReplay is BUG-01's regression test
// (sentinel-user-report-issue/BUG-01).
//
// 1724200000_add_oncall_rotation_anchor.sql re-encodes the pre-existing `handoff_weekday = 0`
// (old encoding: Sunday) to ISO 7, and swaps 1724100000's `BETWEEN 0 AND 6` CHECK for
// `BETWEEN 1 AND 7`. Its first revision ran that UPDATE BEFORE the swap in both directions, which
// is exactly backwards: the constraint that judges a write is the one in force at the moment of
// the write, i.e. the OLD one. Up wrote 7 under `BETWEEN 0 AND 6`; Down wrote 0 under
// `BETWEEN 1 AND 7`. Both aborted with
// `new row for relation "oncall_schedules" violates check constraint
// "oncall_schedules_handoff_weekday_check"`, and nothing in the repo replayed the file over
// non-empty data, so it went unnoticed.
//
// This test seeds one kind='weekly' schedule at EACH weekday value the old encoding allows and
// then replays the file up/down/up. Delete the UPDATE from either direction of the migration, or
// move it back outside the DO block, and this test goes red.
func TestOncallHandoffWeekdayReEncodeReplay(t *testing.T) {
	db := migTestDB(t, "weekday")
	ctx := context.Background()

	absDir, err := filepath.Abs("../../packages/db-migrations/migrations")
	require.NoError(t, err)

	opts := dbmigrations.MigrationOptions{
		TableName: "weekday_replay_migrations",
		Directory: absDir,
	}

	// BaselineVersion is goose's UpTo. Stopping one migration short of the file under test also
	// puts the physical schema back on the old encoding whatever the clone template applied:
	// 1724100000's CHECK swap is an unconditional DROP + ADD of `BETWEEN 0 AND 6`.
	require.NoError(t, dbmigrations.BaselineVersion(ctx, db, 1724100000, opts),
		"could not migrate up to 1724100000")

	// One weekly schedule per old-encoding weekday. Seven organizations, because
	// idx_oncall_schedules_one_active allows at most one ACTIVE schedule per organization.
	for wd := 0; wd <= 6; wd++ {
		var orgID string
		require.NoError(t, db.QueryRowContext(ctx,
			`INSERT INTO organizations (name, slug) VALUES ($1, $2) RETURNING id`,
			fmt.Sprintf("bug01 org %d", wd),
			fmt.Sprintf("bug01-org-%d-%d", wd, time.Now().UnixNano()),
		).Scan(&orgID))

		_, insErr := db.ExecContext(ctx,
			`INSERT INTO oncall_schedules (organization_id, name, kind, handoff_time, handoff_weekday, created_by)
			 VALUES ($1, $2, 'weekly', '09:00', $3, 'bug01')`,
			orgID, fmt.Sprintf("bug01-weekly-%d", wd), wd)
		require.NoError(t, insErr, "seeding handoff_weekday=%d under the 0-6 CHECK", wd)
	}

	weekdays := func() []int {
		rows, qErr := db.QueryContext(ctx,
			`SELECT handoff_weekday FROM oncall_schedules WHERE created_by = 'bug01' ORDER BY handoff_weekday`)
		require.NoError(t, qErr)
		defer rows.Close()
		var out []int
		for rows.Next() {
			var wd int
			require.NoError(t, rows.Scan(&wd))
			out = append(out, wd)
		}
		require.NoError(t, rows.Err())
		return out
	}

	hasAnchorColumn := func() bool {
		var n int
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM information_schema.columns
			 WHERE table_name = 'oncall_schedules' AND column_name = 'rotation_anchor_at'`).Scan(&n))
		return n > 0
	}

	require.Equal(t, []int{0, 1, 2, 3, 4, 5, 6}, weekdays(), "seed did not land as written")

	// UP: the statement that used to abort. Only 0 moves; 1-6 mean the same day in both encodings.
	require.NoError(t, dbmigrations.BaselineVersion(ctx, db, 1724200000, opts),
		"up over seeded rows must not violate the outgoing 0-6 CHECK")
	assert.Equal(t, []int{1, 2, 3, 4, 5, 6, 7}, weekdays())
	assert.True(t, hasAnchorColumn(), "up must add rotation_anchor_at")

	// DOWN: the mirror-image statement, which used to abort against this file's own 1-7 CHECK.
	require.NoError(t, dbmigrations.RunMigrations(ctx, db, "down", opts),
		"down over ISO rows must not violate the outgoing 1-7 CHECK")
	assert.Equal(t, []int{0, 1, 2, 3, 4, 5, 6}, weekdays())
	assert.False(t, hasAnchorColumn(), "down must drop rotation_anchor_at")

	// UP again: the replay the A1 flat-directory ledgers make routine.
	require.NoError(t, dbmigrations.BaselineVersion(ctx, db, 1724200000, opts),
		"replaying up must be as safe as the first up")
	assert.Equal(t, []int{1, 2, 3, 4, 5, 6, 7}, weekdays())
	assert.True(t, hasAnchorColumn(), "replayed up must re-add rotation_anchor_at")
}

// TestOncallWeekdayCheckReplayOverIsoRow is BUG-03's regression test
// (sentinel-user-report-issue/BUG-03).
//
// BUG-01 (TestOncallHandoffWeekdayReEncodeReplay above) proved 1724200000's own two directions
// against rows seeded under the OLD 0-6 encoding. It could not see the rest of the finding,
// because it never replays 1724100000 over a row in the NEW encoding.
//
// A1: one flat migration directory serves several goose ledgers against the SAME physical
// database, and 1724100000's header promises "every statement below is a no-op on a schema that
// already has it applied". Its weekday CHECK swap was not: an unconditional DROP + ADD of
// `BETWEEN 0 AND 6`, replayed by a second target after 1724200000 had re-encoded Sunday to ISO 7,
// re-narrows a constraint 1724200000 already superseded, and Postgres validates ADD CONSTRAINT
// against existing rows -- `check constraint "oncall_schedules_handoff_weekday_check" of relation
// "oncall_schedules" is violated by some row`. The offending row is not hypothetical: the
// dashboard's PUT /oncall validates with isIsoWeekday (1..7) and writes 7 for Sunday.
//
// So this test seeds the row an operator creates through the shipped UI -- kind='weekly',
// handoff_weekday=7 -- and then replays the whole directory under a fresh ledger, exactly as a
// second migration target does. Restore `BETWEEN 0 AND 6` in 1724100000 and this test goes red on
// the first replay.
func TestOncallWeekdayCheckReplayOverIsoRow(t *testing.T) {
	db := migTestDB(t, "bug03weekday")
	ctx := context.Background()

	absDir, err := filepath.Abs("../../packages/db-migrations/migrations")
	require.NoError(t, err)

	// A ledger of its own, against a database the template already migrated to head under
	// migtest_template_migrations. That is the A1 situation verbatim: a second target replaying
	// every file over a schema another ledger already applied.
	opts := dbmigrations.MigrationOptions{
		TableName: "bug03_replay_migrations",
		Directory: absDir,
	}

	var orgID string
	require.NoError(t, db.QueryRowContext(ctx,
		`INSERT INTO organizations (name, slug) VALUES ($1, $2) RETURNING id`,
		"bug03 org", fmt.Sprintf("bug03-org-%d", time.Now().UnixNano()),
	).Scan(&orgID))

	// The row the shipped UI writes for Sunday. It must be accepted by the constraint in force at
	// head -- if this INSERT fails, the ISO swap itself never happened.
	_, err = db.ExecContext(ctx,
		`INSERT INTO oncall_schedules (organization_id, name, kind, handoff_time, handoff_weekday, created_by)
		 VALUES ($1, 'bug03-sunday', 'weekly', '09:00', 7, 'bug03')`, orgID)
	require.NoError(t, err, "handoff_weekday=7 must be storable at head; that is the ISO encoding SR-4 settles")

	weekday := func() int {
		var wd int
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT handoff_weekday FROM oncall_schedules WHERE created_by = 'bug03'`).Scan(&wd))
		return wd
	}

	// rejectsZero reports whether the live constraint still refuses the OLD encoding's Sunday.
	// The fix widens 1724100000's swap; it must not leave the SCHEMA widened, because 1724200000
	// runs after it in the same directory and narrows it back to ISO 1-7.
	rejectsZero := func() bool {
		var probeOrg string
		require.NoError(t, db.QueryRowContext(ctx,
			`INSERT INTO organizations (name, slug) VALUES ($1, $2) RETURNING id`,
			"bug03 probe", fmt.Sprintf("bug03-probe-%d", time.Now().UnixNano()),
		).Scan(&probeOrg))
		_, insErr := db.ExecContext(ctx,
			`INSERT INTO oncall_schedules (organization_id, name, kind, handoff_time, handoff_weekday, created_by)
			 VALUES ($1, 'bug03-probe', 'weekly', '09:00', 0, 'bug03probe')`, probeOrg)
		if insErr == nil {
			_, _ = db.ExecContext(ctx, `DELETE FROM oncall_schedules WHERE created_by = 'bug03probe'`)
			return false
		}
		return strings.Contains(insErr.Error(), "oncall_schedules_handoff_weekday_check")
	}

	require.Equal(t, 7, weekday(), "seed did not land as written")

	// REPLAY. This is the statement that used to abort with "is violated by some row".
	// Up to 1724200000, not the whole directory: goose down is one step, and the step below must be
	// 1724200000's no matter which migrations are added after it.
	require.NoError(t, dbmigrations.BaselineVersion(ctx, db, 1724200000, opts),
		"replaying the directory under a second ledger must not re-narrow the weekday CHECK onto a live ISO row")
	assert.Equal(t, 7, weekday(), "the replay must leave the ISO row untouched")
	assert.True(t, rejectsZero(), "after the replay the live CHECK must still be ISO 1-7, not the widened union")

	// DOWN then UP, over the same ISO row -- the up/down/up the item asks for, on data rather than
	// on an empty table. Down rolls back 1724200000 alone (goose down is one step): Sunday goes
	// back to 0 and the anchor column goes away.
	require.NoError(t, dbmigrations.RunMigrations(ctx, db, "down", opts),
		"down over an ISO row must not violate the outgoing 1-7 CHECK")
	assert.Equal(t, 0, weekday(), "down must re-encode ISO Sunday back to 0")

	require.NoError(t, dbmigrations.BaselineVersion(ctx, db, 1724200000, opts),
		"up again must re-encode 0 back to ISO 7")
	assert.Equal(t, 7, weekday())
	assert.True(t, rejectsZero(), "the replayed up must reinstate the ISO 1-7 CHECK")

	var anchorCols int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.columns
		 WHERE table_name = 'oncall_schedules' AND column_name = 'rotation_anchor_at'`).Scan(&anchorCols))
	assert.Equal(t, 1, anchorCols, "up must leave rotation_anchor_at in place")
}

// ---- sentinel-event-schema-v2 §5 (TASK-04): 1725300000_add_error_occurrences_event_v2_fields.sql.

const (
	eventV2MigrationVersion int64 = 1725300000
	eventV2MigrationFile          = "1725300000_add_error_occurrences_event_v2_fields.sql"
	eventV2TagsIndex              = "idx_error_occurrences_tags"
)

// eventV2Columns is spec §5.2 in order, as information_schema renders each type.
var eventV2Columns = []struct{ name, dataType string }{
	{"level", "character varying(16)"},
	{"message", "text"},
	{"error_class", "character varying(255)"},
	{"logger", "character varying(64)"},
	{"server_name", "character varying(256)"},
	{"transaction", "character varying(200)"},
	{"dist", "character varying(64)"},
	{"source", "character varying(64)"},
	{"trace_flags", "bigint"},
	{"received_at", "timestamp with time zone"},
	{"sdk", "jsonb"},
	{"event_user", "jsonb"},
	{"tags", "jsonb"},
	{"contexts", "jsonb"},
	{"request", "jsonb"},
	{"breadcrumbs", "jsonb"},
	{"exception", "jsonb"},
}

// eventV2ColumnShape returns name -> "type nullable default" for every §5.2 column that exists.
func eventV2ColumnShape(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), `
		SELECT column_name,
		       CASE WHEN character_maximum_length IS NOT NULL
		            THEN data_type || '(' || character_maximum_length || ')'
		            ELSE data_type END,
		       is_nullable, COALESCE(column_default, '')
		  FROM information_schema.columns
		 WHERE table_schema = 'public' AND table_name = 'error_occurrences'`)
	require.NoError(t, err)
	defer rows.Close()
	want := map[string]bool{}
	for _, c := range eventV2Columns {
		want[c.name] = true
	}
	out := map[string]string{}
	for rows.Next() {
		var name, typ, nullable, def string
		require.NoError(t, rows.Scan(&name, &typ, &nullable, &def))
		if want[name] {
			out[name] = fmt.Sprintf("%s nullable=%s default=%q", typ, nullable, def)
		}
	}
	require.NoError(t, rows.Err())
	return out
}

func assertEventV2Shape(t *testing.T, db *sql.DB) {
	t.Helper()
	got := eventV2ColumnShape(t, db)
	want := map[string]string{}
	for _, c := range eventV2Columns {
		want[c.name] = fmt.Sprintf("%s nullable=YES default=%q", c.dataType, "")
	}
	assert.Equal(t, want, got, "the 17 §5.2 columns: exact types, all nullable, no default")

	var valid bool
	var def string
	require.NoError(t, db.QueryRowContext(context.Background(), `
		SELECT i.indisvalid, pg_get_indexdef(i.indexrelid)
		  FROM pg_index i
		 WHERE i.indexrelid = to_regclass($1)`, eventV2TagsIndex).Scan(&valid, &def),
		"%s must exist", eventV2TagsIndex)
	assert.True(t, valid, "%s must be valid (indisvalid)", eventV2TagsIndex)
	assert.Equal(t, "CREATE INDEX idx_error_occurrences_tags ON public.error_occurrences USING gin (tags) WHERE (tags IS NOT NULL)", def)
}

func assertEventV2Gone(t *testing.T, db *sql.DB) {
	t.Helper()
	assert.Empty(t, eventV2ColumnShape(t, db), "Down drops all 17 columns")
	for _, idx := range []string{eventV2TagsIndex, eventV2TagsIndex + "_invalid"} {
		var reg sql.NullString
		require.NoError(t, db.QueryRowContext(context.Background(), `SELECT to_regclass($1)::text`, idx).Scan(&reg))
		assert.False(t, reg.Valid, "Down drops %s", idx)
	}
}

// seedEventV2Rows inserts one project, one issue, one pre-v2 occurrence and one with every v2 column
// populated, and returns the two occurrence ids.
func seedEventV2Rows(t *testing.T, db *sql.DB, label string, createdAt time.Time) (preV2, v2 string) {
	t.Helper()
	ctx := context.Background()
	var projectID, issueID string
	suffix := fmt.Sprintf("%s-%d", label, time.Now().UnixNano())
	require.NoError(t, db.QueryRowContext(ctx,
		`INSERT INTO projects (name, api_key, api_key_hash) VALUES ($1, $2, $3) RETURNING id`,
		"esv2-"+suffix, "k-"+suffix, "h-"+suffix).Scan(&projectID))
	require.NoError(t, db.QueryRowContext(ctx,
		`INSERT INTO issues (project_id, fingerprint, message, error_class, environment)
		 VALUES ($1, $2, 'm', 'E', 'production') RETURNING id`,
		projectID, "fp-"+suffix).Scan(&issueID))
	require.NoError(t, db.QueryRowContext(ctx,
		`INSERT INTO error_occurrences (issue_id, environment, platform, created_at)
		 VALUES ($1, 'production', 'go', $2) RETURNING id`, issueID, createdAt).Scan(&preV2))
	require.NoError(t, db.QueryRowContext(ctx, `
		INSERT INTO error_occurrences (issue_id, environment, platform, created_at,
			level, message, error_class, logger, server_name, transaction, dist, source, trace_flags, received_at,
			sdk, event_user, tags, contexts, request, breadcrumbs, exception)
		VALUES ($1, 'production', 'javascript', $2,
			'warning', 'charge [REDACTED_PAN] failed', 'TypeError', 'checkout', 'web-1', '/checkout/pay', '4127', 'onerror', 1, $2,
			'{"name":"sentinel.javascript","version":"1.8.0"}', '{"id":"u1","email":"a@b.co"}', '{"tenant_id":"acme"}',
			'{"device":{"model":"Pixel 8"}}', '{"method":"POST","headers":{"Cookie":"***REDACTED***"}}',
			'[{"message":"/cart -> /checkout"}]', '{"values":[{"type":"TypeError","value":"x"}]}')
		RETURNING id`, issueID, createdAt).Scan(&v2))
	return preV2, v2
}

// TestEventSchemaV2MigrationUpDownReplay is V4a (spec §9.2): the file replayed over a schema that
// already has it, its exact shape, Down with seeded rows, the Up again, and a second fresh ledger.
func TestEventSchemaV2MigrationUpDownReplay(t *testing.T) {
	db := migTestDB(t, "eventv2")
	ctx := context.Background()
	absDir, err := filepath.Abs("../../packages/db-migrations/migrations")
	require.NoError(t, err)
	opts := dbmigrations.MigrationOptions{TableName: "eventv2_a_migrations", Directory: absDir}

	// The clone is the whole directory applied (prod's schema by construction). A fresh ledger replays
	// every file over it, this one included: the idempotency A1 makes routine.
	require.NoError(t, dbmigrations.BaselineVersion(ctx, db, eventV2MigrationVersion, opts),
		"replaying the directory up to %d over a schema that already has it", eventV2MigrationVersion)
	assertEventV2Shape(t, db)

	preV2, v2 := seedEventV2Rows(t, db, "updown", time.Now())

	// Down one step: the 17 columns and both index names go; both rows stay.
	require.NoError(t, dbmigrations.RunMigrations(ctx, db, "down", opts))
	assertEventV2Gone(t, db)
	var n int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM error_occurrences WHERE id IN ($1, $2)`, preV2, v2).Scan(&n))
	assert.Equal(t, 2, n, "Down drops columns, never rows")

	require.NoError(t, dbmigrations.BaselineVersion(ctx, db, eventV2MigrationVersion, opts), "Up again after Down")
	assertEventV2Shape(t, db)

	// The seeded issue has to go first: 1724400000's decision-7 guard refuses to run over a database
	// that has any issue row, and the fresh ledger below replays it.
	_, err = db.ExecContext(ctx, `
		WITH o AS (DELETE FROM error_occurrences WHERE id IN ($1, $2) RETURNING issue_id)
		DELETE FROM issues WHERE id IN (SELECT issue_id FROM o)`, preV2, v2)
	require.NoError(t, err)

	// A second fresh ledger replays the whole directory (A1: three targets, one physical database).
	require.NoError(t, dbmigrations.RunMigrations(ctx, db, "up",
		dbmigrations.MigrationOptions{TableName: "eventv2_b_migrations", Directory: absDir}))
	assertEventV2Shape(t, db)
}

// TestEventSchemaV2RetentionDeletesWholeRow is V4c: the statement retention.ts issues
// (DELETE FROM error_occurrences WHERE created_at < $1 RETURNING id) removes an old occurrence with
// every v2 column populated, its error_search_index row cascades, and a newer row is intact.
func TestEventSchemaV2RetentionDeletesWholeRow(t *testing.T) {
	db := migTestDB(t, "eventv2ret")
	ctx := context.Background()
	assertEventV2Shape(t, db)

	old := time.Now().Add(-40 * 24 * time.Hour)
	oldPre, oldV2 := seedEventV2Rows(t, db, "retold", old)
	newPre, newV2 := seedEventV2Rows(t, db, "retnew", time.Now())
	_, err := db.ExecContext(ctx, `INSERT INTO error_search_index (occurrence_id, user_id, tenant_id) VALUES ($1, 'u1', 'acme'), ($2, 'u2', 'acme')`, oldV2, newV2)
	require.NoError(t, err)

	cutoff := time.Now().Add(-30 * 24 * time.Hour)
	rows, err := db.QueryContext(ctx, `DELETE FROM error_occurrences WHERE created_at < $1 RETURNING id`, cutoff)
	require.NoError(t, err)
	var deleted []string
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		deleted = append(deleted, id)
	}
	require.NoError(t, rows.Err())
	rows.Close()
	assert.ElementsMatch(t, []string{oldPre, oldV2}, deleted)

	var n int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM error_search_index WHERE occurrence_id = $1`, oldV2).Scan(&n))
	assert.Equal(t, 0, n, "the search index row cascades with the occurrence")
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM error_search_index WHERE occurrence_id = $1`, newV2).Scan(&n))
	assert.Equal(t, 1, n)

	var tags, level string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT tags::text, level FROM error_occurrences WHERE id = $1`, newV2).Scan(&tags, &level))
	assert.JSONEq(t, `{"tenant_id":"acme"}`, tags)
	assert.Equal(t, "warning", level)
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM error_occurrences WHERE id = $1`, newPre).Scan(&n))
	assert.Equal(t, 1, n)
}
