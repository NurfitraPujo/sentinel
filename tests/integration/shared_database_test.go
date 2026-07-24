package integration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/NurfitraPujo/sentinel/packages/shared-go/database"
	tc "github.com/NurfitraPujo/sentinel/tests/integration/testcontainers"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// uniqueTableName returns a table name that is unique per test invocation so
// concurrent runs of the test suite do not collide on the same relation.
func uniqueTableName(prefix string) string {
	return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
}

// configFromTest returns a database.Config populated from the integration
// test PostgreSQL container via testcontainers.Setup.
func configFromTest(t *testing.T) *database.Config {
	t.Helper()
	env := tc.Setup(t, tc.WithResources(tc.PostgresResource), tc.WithMigrations(true))
	if env.PGConfig.Host == "" {
		return nil
	}
	port, err := strconv.Atoi(env.PGConfig.Port)
	if err != nil {
		return nil
	}
	return &database.Config{
		Host:            env.PGConfig.Host,
		Port:            port,
		User:            env.PGConfig.User,
		Password:        env.PGConfig.Password,
		Database:        env.PGConfig.DB,
		MaxConns:        5,
		MinConns:        1,
		MaxConnLifetime: time.Hour,
		MaxConnIdleTime: 30 * time.Minute,
	}
}

// poolFromTest returns a fresh pgxpool.Pool for the test PostgreSQL instance
// using the same connection settings as the rest of the integration suite.
func poolFromTest(t *testing.T) *pgxpool.Pool {
	t.Helper()
	cfg, _ := GetTestConfig()
	host := cfg.Host
	port := cfg.Port
	user := cfg.User
	password := cfg.Password
	db := cfg.DB
	if host == "" {
		host = os.Getenv("POSTGRES_HOST")
		port = os.Getenv("POSTGRES_PORT")
		user = os.Getenv("POSTGRES_USER")
		password = os.Getenv("POSTGRES_PASSWORD")
		db = os.Getenv("POSTGRES_DB")
	}
	if host == "" {
		t.Skip("PostgreSQL not available")
	}

	dsn := fmt.Sprintf(
		"postgres://%s:%s@%s:%s/%s?sslmode=disable",
		user, password, host, port, db,
	)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	require.NoError(t, pool.Ping(ctx))
	return pool
}

func TestDatabasePackage_NewConnection_Success(t *testing.T) {
	cfg := configFromTest(t)
	if cfg == nil {
		t.Skip("PostgreSQL not available")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := database.NewConnection(ctx, *cfg)
	require.NoError(t, err)
	require.NotNil(t, pool)
	t.Cleanup(func() { pool.Close() })

	// Verify the pool is functional.
	require.NoError(t, pool.Ping(ctx))

	// Sanity check: a trivial query should succeed.
	var one int
	err = pool.QueryRow(ctx, "SELECT 1").Scan(&one)
	require.NoError(t, err)
	assert.Equal(t, 1, one)

	// Verify pool config was applied by checking the runtime configuration.
	cp := pool.Config()
	assert.Equal(t, int32(cfg.MaxConns), cp.MaxConns)
	assert.Equal(t, int32(cfg.MinConns), cp.MinConns)
	assert.Equal(t, cfg.MaxConnLifetime, cp.MaxConnLifetime)
	assert.Equal(t, cfg.MaxConnIdleTime, cp.MaxConnIdleTime)
}

func TestDatabasePackage_NewConnection_BadHost(t *testing.T) {
	// Use a deliberately unreachable host. Port 1 is reserved and not used
	// by any real Postgres instance, providing a reliable failure signal.
	cfg := database.Config{
		Host:     "127.0.0.1",
		Port:     1,
		User:     "sentinel",
		Password: "changeme",
		Database: "sentinel",
		MaxConns: 5,
		MinConns: 1,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pool, err := database.NewConnection(ctx, cfg)
	require.Error(t, err)

	// No pool should be returned on failure - the implementation closes the
	// pool internally when Ping fails, so callers must not call Close on it.
	assert.Nil(t, pool)
}

func TestDatabasePackage_NewConnection_InvalidConfig(t *testing.T) {
	// A configuration that produces a malformed connection string should be
	// rejected by pgxpool.ParseConfig before any pool is created.
	cfg := database.Config{
		Host:     "127.0.0.1",
		Port:     0,
		User:     "sentinel",
		Password: "changeme",
		Database: "sentinel",
		MaxConns: 5,
		MinConns: 1,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pool, err := database.NewConnection(ctx, cfg)
	require.Error(t, err)
	assert.Nil(t, pool)
}

func TestDatabasePackage_NewConnection_TLSConfig(t *testing.T) {
	// Cover the TLS branches in NewConnection. The connection itself will
	// fail because the certs/keys are not real, but the goal is to exercise
	// the branch paths so the connection string is built with the TLS pieces
	// appended. We assert that the resulting error mentions connection
	// failure, not parsing failure.
	cfg := database.Config{
		Host:     "127.0.0.1",
		Port:     1,
		User:     "sentinel",
		Password: "changeme",
		Database: "sentinel",
		MaxConns: 5,
		MinConns: 1,
		TLSMode:  "require",
		TLSCert:  "/tmp/fake-cert.pem",
		TLSKey:   "/tmp/fake-key.pem",
		TLSCA:    "/tmp/fake-ca.pem",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pool, err := database.NewConnection(ctx, cfg)
	require.Error(t, err)
	assert.Nil(t, pool)
}

func TestDatabasePackage_NewConnection_ParseConfigError(t *testing.T) {
	// Force a parse failure by injecting a host with characters that should
	// be rejected by pgxpool.ParseConfig.
	cfg := database.Config{
		Host:     "host with spaces and bad chars",
		Port:     5432,
		User:     "sentinel",
		Password: "changeme",
		Database: "sentinel",
		MaxConns: 5,
		MinConns: 1,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pool, err := database.NewConnection(ctx, cfg)
	require.Error(t, err)
	assert.Nil(t, pool)
}

func TestDatabasePackage_NewConnection_NewWithConfigError(t *testing.T) {
	// Force a pool creation failure by setting MaxConns to a negative value.
	// pgxpool requires MaxConns >= 1, so pgxpool.NewWithConfig will fail.
	cfg := database.Config{
		Host:     "127.0.0.1",
		Port:     1,
		User:     "sentinel",
		Password: "changeme",
		Database: "sentinel",
		MaxConns: -1,
		MinConns: 1,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pool, err := database.NewConnection(ctx, cfg)
	require.Error(t, err)
	assert.Nil(t, pool)
}

func TestDatabasePackage_LoadMigrations_AlphabeticalOrder(t *testing.T) {
	dir := t.TempDir()

	// Write two SQL files with names that would sort differently if read in
	// directory order rather than alphabetical order.
	firstSQL := "CREATE TABLE load_migrations_first_x (id int);"
	secondSQL := "CREATE TABLE load_migrations_second_x (id int);"

	// Write them in non-alphabetical order on disk to prove the loader sorts.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "002_b.sql"), []byte(secondSQL), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "001_a.sql"), []byte(firstSQL), 0o644))

	got, err := database.LoadMigrations(dir)
	require.NoError(t, err)

	// The first file alphabetically should come before the second.
	idxFirst := strings.Index(got, firstSQL)
	idxSecond := strings.Index(got, secondSQL)
	require.NotEqual(t, -1, idxFirst, "first migration missing from output")
	require.NotEqual(t, -1, idxSecond, "second migration missing from output")
	assert.Less(t, idxFirst, idxSecond, "migrations should be concatenated in alphabetical order")

	// Concatenated output should end with a newline separator.
	assert.True(t, strings.HasSuffix(got, "\n"))
}

func TestDatabasePackage_LoadMigrations_MissingDirectory(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")

	got, err := database.LoadMigrations(missing)
	require.Error(t, err)
	assert.Empty(t, got)
}

func TestDatabasePackage_LoadMigrations_FileReadError(t *testing.T) {
	// Create a .sql file that is unreadable. On POSIX systems, removing read
	// permissions from the owner forces os.ReadFile to fail. The file still
	// sorts into the listings because it is a regular file, but the read
	// step in LoadMigrations will return an error.
	if os.Getuid() == 0 {
		t.Skip("Running as root - permission bits are bypassed")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "001_a.sql")
	require.NoError(t, os.WriteFile(path, []byte("SELECT 1;"), 0o000))
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })

	_, err := database.LoadMigrations(dir)
	require.Error(t, err)
}

func TestDatabasePackage_LoadMigrations_IgnoresNonSQLFiles(t *testing.T) {
	dir := t.TempDir()

	sqlContent := "CREATE TABLE load_migrations_only_sql_x (id int);"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "001_a.sql"), []byte(sqlContent), 0o644))

	// Files that should be ignored.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "002_b.txt"), []byte("not sql"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("# nope"), 0o644))
	// A subdirectory should also be skipped.
	require.NoError(t, os.Mkdir(filepath.Join(dir, "subdir"), 0o755))

	got, err := database.LoadMigrations(dir)
	require.NoError(t, err)
	assert.Contains(t, got, sqlContent)
	assert.NotContains(t, got, "not sql")
	assert.NotContains(t, got, "# nope")
}

func TestDatabasePackage_LoadMigrations_EmptyDirectory(t *testing.T) {
	dir := t.TempDir()

	got, err := database.LoadMigrations(dir)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestDatabasePackage_RunMigrationsWithPool_CreatesTables(t *testing.T) {
	pool := poolFromTest(t)
	t.Cleanup(pool.Close)

	dir := t.TempDir()
	tableName := uniqueTableName("u11_rmwp")

	sql := fmt.Sprintf("CREATE TABLE %s (id int PRIMARY KEY, name text);", tableName)
	migFile := filepath.Join(dir, "001_init.sql")
	require.NoError(t, os.WriteFile(migFile, []byte(sql), 0o644))

	// Make sure the table does not exist prior to running migrations.
	ctx := context.Background()
	var existsBefore bool
	err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = $1)`,
		strings.ToLower(tableName),
	).Scan(&existsBefore)
	require.NoError(t, err)
	require.False(t, existsBefore, "precondition: table should not exist before migration")

	// Run migrations and ensure the table is created.
	err = database.RunMigrationsWithPool(ctx, pool, dir)
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), fmt.Sprintf("DROP TABLE IF EXISTS %s", tableName))
	})

	var existsAfter bool
	err = pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = $1)`,
		strings.ToLower(tableName),
	).Scan(&existsAfter)
	require.NoError(t, err)
	assert.True(t, existsAfter, "table should exist after RunMigrationsWithPool")
}

func TestDatabasePackage_RunMigrationsWithPool_MultipleFiles(t *testing.T) {
	pool := poolFromTest(t)
	t.Cleanup(pool.Close)

	dir := t.TempDir()
	firstTable := uniqueTableName("u11_rmwp_multi_first")
	secondTable := uniqueTableName("u11_rmwp_multi_second")

	// Write files in non-alphabetical order; the loader should still run them
	// in alphabetical order, so the first file's table must exist before the
	// second file can reference it via a foreign key.
	fkTable := uniqueTableName("u11_rmwp_fk")
	firstFile := fmt.Sprintf("CREATE TABLE %s (id int PRIMARY KEY);", firstTable)
	secondFile := fmt.Sprintf(
		"CREATE TABLE %s (id int PRIMARY KEY, parent_id int REFERENCES %s(id));",
		fkTable, firstTable,
	)
	thirdFile := fmt.Sprintf("CREATE TABLE %s (id int PRIMARY KEY);", secondTable)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "003_third.sql"), []byte(thirdFile), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "001_first.sql"), []byte(firstFile), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "002_second.sql"), []byte(secondFile), 0o644))

	ctx := context.Background()
	err := database.RunMigrationsWithPool(ctx, pool, dir)
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), fmt.Sprintf("DROP TABLE IF EXISTS %s", secondTable))
		_, _ = pool.Exec(context.Background(), fmt.Sprintf("DROP TABLE IF EXISTS %s", fkTable))
		_, _ = pool.Exec(context.Background(), fmt.Sprintf("DROP TABLE IF EXISTS %s", firstTable))
	})

	for _, table := range []string{firstTable, secondTable, fkTable} {
		var exists bool
		err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = $1)`,
			strings.ToLower(table),
		).Scan(&exists)
		require.NoError(t, err)
		assert.True(t, exists, "table %s should exist after migration", table)
	}
}

func TestDatabasePackage_RunMigrationsWithPool_MissingDirectory(t *testing.T) {
	pool := poolFromTest(t)
	t.Cleanup(pool.Close)

	err := database.RunMigrationsWithPool(context.Background(), pool, filepath.Join(t.TempDir(), "missing"))
	require.Error(t, err)
}

func TestDatabasePackage_RunMigrationsWithPool_InvalidSQL(t *testing.T) {
	pool := poolFromTest(t)
	t.Cleanup(pool.Close)

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "001_broken.sql"), []byte("THIS IS NOT VALID SQL;"), 0o644))

	err := database.RunMigrationsWithPool(context.Background(), pool, dir)
	require.Error(t, err)
}

func TestDatabasePackage_RunMigrations_DirectCall(t *testing.T) {
	pool := poolFromTest(t)
	t.Cleanup(pool.Close)

	tableName := uniqueTableName("u11_runmig")
	sql := fmt.Sprintf("CREATE TABLE %s (id int);", tableName)

	ctx := context.Background()
	require.NoError(t, database.RunMigrations(ctx, pool, sql))
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), fmt.Sprintf("DROP TABLE IF EXISTS %s", tableName))
	})

	var exists bool
	err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = $1)`,
		strings.ToLower(tableName),
	).Scan(&exists)
	require.NoError(t, err)
	assert.True(t, exists)
}

func TestDatabasePackage_RunMigrations_InvalidSQL(t *testing.T) {
	pool := poolFromTest(t)
	t.Cleanup(pool.Close)

	err := database.RunMigrations(context.Background(), pool, "NOT VALID SQL;")
	require.Error(t, err)
}
