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
