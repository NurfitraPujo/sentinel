# Database Migrations

Shared migration system using `goose` for PostgreSQL. Located at `packages/db-migrations/`.

## Structure

```
packages/db-migrations/
├── cmd/migrate/main.go    # CLI entrypoint
├── migrations/            # SQL migration files (Unix timestamp versioned)
├── driver.go               # pgx database connection
└── goose.go                # goose wrapper functions
```

## Usage

```bash
# Check status
go run cmd/migrate/main.go status -target processor

# Apply migrations
go run cmd/migrate/main.go up -target processor

# Rollback last migration
go run cmd/migrate/main.go down -target processor

# Baseline existing database
go run cmd/migrate/main.go baseline -target processor -version 1716508800
```

## Taskfile Commands

```bash
task db:processor:status
task db:processor:up
task db:processor:down
task db:dashboard:status
task db:dashboard:up
task db:ingestor:status
task db:ingestor:up
```

## Recovery Procedures

### Partial Migration Failure

When a migration fails mid-execution, the database may be left in an inconsistent state.

**Symptoms:**
- `goose` reports error during migration
- Some tables/columns exist, others don't
- Subsequent migrations fail with dependency errors

**Recovery Steps:**

1. Identify the last successful migration version:
   ```sql
   SELECT * FROM schema_migrations ORDER BY version DESC LIMIT 5;
   ```

2. If using custom table name, replace `schema_migrations` with your table name.

3. Determine if partial changes need rollback:
   ```sql
   -- Check current state
   SELECT * FROM schema_migrations;
   ```

4. Rollback the failed migration manually:
   ```sql
   -- If migration 1716550000 partially ran, roll it back
   DROP TABLE IF EXISTS project_members;  -- from 1716550000_add_project_members.sql

   -- Update migration table to reflect true state
   DELETE FROM schema_migrations WHERE version = 1716550000;
   ```

5. Re-run migrations:
   ```bash
   task db:processor:up
   ```

### Migration Table Out of Sync

When `schema_migrations` table doesn't match actual database state.

**Symptoms:**
- Migrations report "already applied" but tables don't exist
- Version conflicts between migration files and table records

**Recovery Steps:**

1. Compare migration table with actual migrations:
   ```sql
   -- Get recorded migrations
   SELECT version, is_applied FROM schema_migrations ORDER BY version;

   -- Check for missing tables
   SELECT tablename FROM pg_tables WHERE schemaname = 'public';
   ```

2. Force sync to a known good state:
   ```sql
   -- Option A: Set to specific version (migrations below will re-run)
   INSERT INTO schema_migrations (version, is_applied) VALUES (1716508800, true) ON CONFLICT DO NOTHING;

   -- Option B: Clear and re-baseline
   TRUNCATE schema_migrations;
   INSERT INTO schema_migrations (version, is_applied) VALUES (1716508800, true);
   ```

3. Run migrations again:
   ```bash
   task db:processor:up
   ```

### Baseline Required (Existing Database)

When introducing migrations to a database that already has a schema.

**Symptoms:**
- New migrations fail with "relation already exists"
- Migration table is empty but tables exist

**Recovery Steps:**

1. Identify the baseline version (earliest migration that matches current schema):
   ```bash
   # Review your migration files and determine which version matches current state
   ls migrations/
   ```

2. Run baseline command:
   ```bash
   task db:processor:baseline VERSION=1716508800
   ```

3. Verify:
   ```sql
   SELECT * FROM schema_migrations;
   ```

### Concurrent Migration Prevention

Goose uses advisory locks to prevent concurrent migrations. If a migration appears hung:

1. Check for stuck connections:
   ```sql
   SELECT * FROM pg_stat_activity WHERE state = 'active' AND query LIKE '%goose%';
   ```

2. Terminate stuck connections (replace `<pid>`):
   ```sql
   SELECT pg_terminate_backend(<pid>);
   ```

### Destructive Operations (Reset)

**WARNING: These operations destroy data.**

The `db:processor:reset` task is protected by:
- Interactive confirmation prompt
- Production environment check (`ENVIRONMENT != prod`)

To manually reset:
```bash
# Drop and recreate schema
psql "postgres://user:pass@host/db" -c "DROP SCHEMA public CASCADE; CREATE SCHEMA public;"

# Re-run migrations
task db:processor:up
```

## Security

- Connection strings passed via environment variables (`DB_URL_<TARGET>`)
- CLI sanitizes DSN in output (passwords redacted)
- Production destructive commands require confirmation