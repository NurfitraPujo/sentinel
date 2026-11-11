package testcontainers

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The splitter behind both raw migration runners (setup_test.go runMigrations and
// runInitSQLMigrations). sentinel-occurrence-dedup-project TASK-02 / flows M10: a file whose line 1
// is `-- +goose NO TRANSACTION` is executed one StatementBegin/StatementEnd block at a time on one
// acquired connection; every other file keeps its single whole-Up Exec.

const directiveFixture = `-- +goose NO TRANSACTION
-- header comment; the directive above is line 1 and appears nowhere else

-- +goose Up
-- +goose StatementBegin
SET statement_timeout = '15min';
SET lock_timeout = '5s';
-- +goose StatementEnd

-- a comment between blocks is not a statement
-- +goose StatementBegin
ALTER TABLE t ADD COLUMN IF NOT EXISTS c UUID;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_t_c
    ON t (c)
    WHERE c IS NOT NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX CONCURRENTLY IF EXISTS uq_t_c;
-- +goose StatementEnd
`

const plainFixture = `-- an ordinary file: no directive on line 1
-- +goose Up
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS t (id serial PRIMARY KEY);
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE t ADD COLUMN IF NOT EXISTS c UUID;
-- +goose StatementEnd

-- +goose Down
DROP TABLE IF EXISTS t;
`

func TestSplitGooseUp_DirectiveYieldsOneBlockPerPairInOrder(t *testing.T) {
	blocks, err := SplitGooseUp(directiveFixture)
	if err != nil {
		t.Fatalf("SplitGooseUp: %v", err)
	}
	if len(blocks) != 3 {
		t.Fatalf("got %d blocks, want 3: %q", len(blocks), blocks)
	}
	wantStarts := []string{
		"SET statement_timeout = '15min';\nSET lock_timeout = '5s';",
		"ALTER TABLE t ADD COLUMN IF NOT EXISTS c UUID;",
		"CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_t_c",
	}
	for i, want := range wantStarts {
		if !strings.HasPrefix(strings.TrimSpace(blocks[i]), want) {
			t.Errorf("block %d = %q, want it to start with %q", i+1, blocks[i], want)
		}
	}
	joined := strings.Join(blocks, "\n")
	for _, marker := range []string{"+goose", "DROP INDEX", "header comment"} {
		if strings.Contains(joined, marker) {
			t.Errorf("blocks must not carry %q (Down section, directives and text outside a pair are dropped):\n%s", marker, joined)
		}
	}
}

func TestSplitGooseUp_WithoutDirectiveYieldsOneBlock(t *testing.T) {
	for name, content := range map[string]string{
		"no directive at all": plainFixture,
		// goose honours the directive on line 1 only; a later mention — the way a header comment
		// might quote it — must not switch the runner into block mode.
		"directive not on line 1": "-- header\n" + directiveFixture,
	} {
		t.Run(name, func(t *testing.T) {
			blocks, err := SplitGooseUp(content)
			if err != nil {
				t.Fatalf("SplitGooseUp: %v", err)
			}
			if len(blocks) != 1 {
				t.Fatalf("got %d blocks, want exactly 1: %q", len(blocks), blocks)
			}
			up := blocks[0]
			if strings.Contains(up, "+goose") {
				t.Errorf("markers must be stripped from the single block:\n%s", up)
			}
			if !strings.Contains(up, "ALTER TABLE t ADD COLUMN IF NOT EXISTS c UUID;") {
				t.Errorf("the single block must carry the whole Up section:\n%s", up)
			}
			if strings.Contains(up, "DROP") {
				t.Errorf("the Down section must not be in the block:\n%s", up)
			}
		})
	}
}

func TestSplitGooseUp_DirectiveRejectsStatementOutsideAPair(t *testing.T) {
	content := GooseNoTransactionDirective + `
-- +goose Up
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd
SELECT 2;
-- +goose Down
`
	blocks, err := SplitGooseUp(content)
	if err == nil {
		t.Fatalf("want an error for a bare statement in a directive file, got blocks %q", blocks)
	}
	if !strings.Contains(err.Error(), "line 6") {
		t.Errorf("error must name the offending line 6: %v", err)
	}
}

// probePool returns a pool on the test container whose released connections are destroyed rather
// than returned to the idle list. That makes the connection-affinity assertion in the test below
// discriminating: a runner that Exec'd each block through the pool would get a FRESH session per
// block and lose block 1's SET; only a runner that holds one acquired connection for the whole
// file sees it in block 2.
func probePool(t *testing.T, ctx context.Context, dsn string) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	cfg.AfterRelease = func(*pgx.Conn) bool { return false }
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func startProbePostgres(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)

	pg, err := StartPostgreSQL(ctx)
	if err != nil {
		t.Fatalf("StartPostgreSQL: %v", err)
	}
	t.Cleanup(func() {
		c, cc := context.WithTimeout(context.Background(), 10*time.Second)
		defer cc()
		_ = pg.Terminate(c)
	})
	return ctx, probePool(t, ctx, pg.DSN())
}

func TestApplyGooseUp_DirectiveBlocksShareOneConnectionOutsideATransaction(t *testing.T) {
	ctx, pool := startProbePostgres(t)

	if _, err := pool.Exec(ctx, `CREATE TABLE t (id serial PRIMARY KEY)`); err != nil {
		t.Fatalf("create table: %v", err)
	}

	// Block 1 SETs a session variable; block 2 reads it back with current_setting and RAISEs by
	// name if it is not there — the shape of 1725200000's gate (c). Block 3 is the statement the
	// whole item exists for: CREATE INDEX CONCURRENTLY, which fails with 25001 inside any
	// transaction block.
	content := GooseNoTransactionDirective + `
-- +goose Up
-- +goose StatementBegin
SET sentinel.probe = 'task-02';
-- +goose StatementEnd
-- +goose StatementBegin
DO $$
BEGIN
    IF current_setting('sentinel.probe', true) IS DISTINCT FROM 'task-02' THEN
        RAISE EXCEPTION 'block 2 runs in a different session from block 1: sentinel.probe = %', current_setting('sentinel.probe', true);
    END IF;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE t ADD COLUMN IF NOT EXISTS c UUID;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_t_c ON t (c) WHERE c IS NOT NULL;
-- +goose StatementEnd
-- +goose Down
`
	if err := ApplyGooseUp(ctx, pool, "probe.sql", content); err != nil {
		t.Fatalf("ApplyGooseUp on a directive file: %v", err)
	}

	var valid bool
	if err := pool.QueryRow(ctx,
		`SELECT indisvalid FROM pg_index WHERE indexrelid = to_regclass('uq_t_c')`).Scan(&valid); err != nil {
		t.Fatalf("the concurrent build left no uq_t_c: %v", err)
	}
	if !valid {
		t.Fatalf("uq_t_c exists but indisvalid = false")
	}

	// Control: the SET was session-scoped, so a different connection must NOT see it — otherwise
	// block 2's check above would have passed vacuously.
	var seen string
	if err := pool.QueryRow(ctx, `SELECT coalesce(current_setting('sentinel.probe', true), '')`).Scan(&seen); err != nil {
		t.Fatalf("read sentinel.probe on a fresh connection: %v", err)
	}
	if seen != "" {
		t.Fatalf("sentinel.probe = %q on a fresh connection; the SET leaked past the file's connection, so the affinity check proves nothing", seen)
	}
}

func TestApplyGooseUp_WithoutDirectiveIsOneImplicitTransaction(t *testing.T) {
	ctx, pool := startProbePostgres(t)

	// Today's behaviour, kept: the whole Up section is one multi-statement Exec, which Postgres
	// runs as an implicit transaction block — so CREATE INDEX CONCURRENTLY is refused with 25001.
	// This pins that a file WITHOUT the directive is not quietly split; the directive is what
	// opts a file into block mode.
	content := `-- +goose Up
CREATE TABLE t (id serial PRIMARY KEY, c UUID);
CREATE UNIQUE INDEX CONCURRENTLY uq_t_c ON t (c);
-- +goose Down
`
	err := ApplyGooseUp(ctx, pool, "plain.sql", content)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "25001" {
		t.Fatalf("want SQLSTATE 25001 (cannot run inside a transaction block) from the single Exec, got %v", err)
	}

	// And the same statements under the directive, one block each, succeed.
	split := GooseNoTransactionDirective + `
-- +goose Up
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS t (id serial PRIMARY KEY, c UUID);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_t_c ON t (c);
-- +goose StatementEnd
-- +goose Down
`
	if err := ApplyGooseUp(ctx, pool, "split.sql", split); err != nil {
		t.Fatalf("ApplyGooseUp on the directive form: %v", err)
	}
}
