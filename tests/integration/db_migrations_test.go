package integration

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

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

func TestMigrationStatus(t *testing.T) {
	dsn := getDSN()
	if dsn == "" {
		t.Skip("PostgreSQL not available")
	}

	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	defer db.Close()

	err = db.Ping()
	require.NoError(t, err)

	absDir, err := filepath.Abs("../../packages/db-migrations/migrations")
	require.NoError(t, err)

	opts := dbmigrations.MigrationOptions{
		TableName: "schema_migrations",
		Directory: absDir,
	}

	err = dbmigrations.RunMigrations(context.Background(), db, "up", opts)
	require.NoError(t, err)

	err = dbmigrations.GetStatus(context.Background(), db, opts)
	require.NoError(t, err)
}

func TestSequentialMigrations(t *testing.T) {
	dsn := getDSN()
	if dsn == "" {
		t.Skip("PostgreSQL not available")
	}

	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	defer db.Close()

	err = db.Ping()
	require.NoError(t, err)

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
	dsn := getDSN()
	if dsn == "" {
		t.Skip("PostgreSQL not available")
	}

	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	defer db.Close()

	err = db.Ping()
	require.NoError(t, err)

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
	dsn := getDSN()
	if dsn == "" {
		t.Skip("PostgreSQL not available")
	}

	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	defer db.Close()

	err = db.Ping()
	require.NoError(t, err)

	absDir, err := filepath.Abs("../../packages/db-migrations/migrations")
	require.NoError(t, err)

	opts := dbmigrations.MigrationOptions{
		TableName: "baseline_test_migrations",
		Directory: absDir,
	}

	err = dbmigrations.RunMigrations(context.Background(), db, "up", opts)
	require.NoError(t, err)

	initialVersion := int64(1716508800)
	err = dbmigrations.BaselineVersion(context.Background(), db, initialVersion, opts)
	require.NoError(t, err)

	err = dbmigrations.GetStatus(context.Background(), db, opts)
	require.NoError(t, err)
}
