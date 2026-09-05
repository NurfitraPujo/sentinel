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
// migration chain. Pattern borrowed from works/daya-core/test/setup.go, including the parts that are
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
