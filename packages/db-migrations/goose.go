package dbmigrations

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/pressly/goose/v3"

	_ "github.com/jackc/pgx/v5/stdlib"
)

type MigrationOptions struct {
	TableName string
	Directory string
}

func DefaultOptions() MigrationOptions {
	return MigrationOptions{
		TableName: "schema_migrations",
	}
}

func RunMigrations(ctx context.Context, db *sql.DB, cmd string, opts MigrationOptions) error {
	if opts.TableName == "" {
		opts.TableName = "schema_migrations"
	}

	if opts.Directory == "" {
		return fmt.Errorf("migration directory is required")
	}

	absDir, err := filepath.Abs(opts.Directory)
	if err != nil {
		return fmt.Errorf("failed to resolve migration directory: %w", err)
	}

	if _, err := os.Stat(absDir); os.IsNotExist(err) {
		return fmt.Errorf("migration directory does not exist: %s", absDir)
	}

	goose.SetTableName(opts.TableName)

	if err := goose.RunContext(ctx, cmd, db, absDir); err != nil {
		return fmt.Errorf("migration failed (%s): %w", cmd, err)
	}

	return nil
}

func GetStatus(ctx context.Context, db *sql.DB, opts MigrationOptions) error {
	if opts.TableName == "" {
		opts.TableName = "schema_migrations"
	}

	if opts.Directory == "" {
		return fmt.Errorf("migration directory is required")
	}

	goose.SetTableName(opts.TableName)

	if err := goose.RunContext(ctx, "status", db, opts.Directory); err != nil {
		return fmt.Errorf("status check failed: %w", err)
	}

	return nil
}

func BaselineVersion(ctx context.Context, db *sql.DB, version int64, opts MigrationOptions) error {
	if opts.TableName == "" {
		opts.TableName = "schema_migrations"
	}

	goose.SetTableName(opts.TableName)

	if err := goose.RunContext(ctx, "baseline", db, opts.Directory, strconv.FormatInt(version, 10)); err != nil {
		return fmt.Errorf("baseline failed: %w", err)
	}

	return nil
}
