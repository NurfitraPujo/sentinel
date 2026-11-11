package testcontainers

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// GooseNoTransactionDirective is goose's annotation for "run this file's statements one at a time,
// outside any transaction". goose honours it only on the FIRST line of a file, and so do the raw
// runners here. It must not appear anywhere else in a migration, not even quoted in a comment:
// goose scans every comment line for its annotation marker and fails the whole file with
// "invalid annotation" (1722200000_add_error_occurrences_event_id.sql's header records shipping
// that way once).
const GooseNoTransactionDirective = "-- +goose NO TRANSACTION"

// HasGooseNoTransaction reports whether content's first line is the no-transaction directive.
// Only line 1 counts, matching goose: the same text on any later line is an error in goose and
// is deliberately NOT honoured here, so the two runners cannot disagree with it.
func HasGooseNoTransaction(content string) bool {
	first, _, _ := strings.Cut(content, "\n")
	return strings.TrimSpace(first) == GooseNoTransactionDirective
}

// SplitGooseUp returns a goose migration's Up section as the blocks a raw runner executes.
//
// Without the directive the result is exactly one block — today's behaviour: the Up section with
// the StatementBegin/StatementEnd markers stripped, run as one multi-statement Exec, which
// Postgres wraps in an implicit transaction. (The block may be blank when the Up section is
// empty; callers skip it.)
//
// With the directive on line 1 the Up section is split into one block per
// StatementBegin/StatementEnd pair, in file order, so each can be an Exec of its own in
// autocommit — which is what CREATE INDEX CONCURRENTLY, DROP INDEX CONCURRENTLY and a DO block
// that COMMITs per batch all require ("cannot run inside a transaction block" otherwise). Comment
// and blank lines between pairs are ignored. Any other text outside a pair is an error naming the
// line: goose would split it on semicolons, but this runner does not parse SQL, and a silent guess
// about statement boundaries in a file that runs outside a transaction is worse than a loud
// refusal. Every directive file in packages/db-migrations/migrations wraps every Up statement.
func SplitGooseUp(content string) ([]string, error) {
	if !HasGooseNoTransaction(content) {
		return []string{extractGooseUp(content)}, nil
	}

	var (
		blocks  []string
		current []string
		inUp    bool
		inBlock bool
	)
	for i, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "-- +goose Up"):
			inUp = true
			continue
		case strings.HasPrefix(trimmed, "-- +goose Down"):
			if inBlock {
				return nil, fmt.Errorf("line %d: -- +goose Down inside an unterminated StatementBegin", i+1)
			}
			inUp = false
			continue
		case strings.HasPrefix(trimmed, "-- +goose StatementBegin"):
			if !inUp {
				continue
			}
			if inBlock {
				return nil, fmt.Errorf("line %d: nested -- +goose StatementBegin", i+1)
			}
			inBlock = true
			current = current[:0]
			continue
		case strings.HasPrefix(trimmed, "-- +goose StatementEnd"):
			if !inUp {
				continue
			}
			if !inBlock {
				return nil, fmt.Errorf("line %d: -- +goose StatementEnd without a StatementBegin", i+1)
			}
			inBlock = false
			blocks = append(blocks, strings.Join(current, "\n"))
			continue
		}
		if !inUp {
			continue
		}
		if inBlock {
			current = append(current, line)
			continue
		}
		if trimmed == "" || strings.HasPrefix(trimmed, "--") {
			continue
		}
		return nil, fmt.Errorf("line %d: statement outside a StatementBegin/StatementEnd pair in a %s file; wrap it so the runner knows where the statement ends", i+1, GooseNoTransactionDirective)
	}
	if inBlock {
		return nil, fmt.Errorf("unterminated -- +goose StatementBegin at end of file")
	}
	return blocks, nil
}

// ApplyGooseUp applies one migration file's Up section to the database behind pool, the way the
// raw integration runners (tests/integration/setup_test.go's runMigrations and this package's
// runInitSQLMigrations) bootstrap a disposable container without a goose ledger.
//
// A file without the directive is one pool.Exec, as before. A file with it is executed block by
// block on ONE connection acquired for the file and released after its last block: separate
// pool.Exec calls can land on different pooled connections, and outside a transaction a SET is
// session-scoped, so the session guards a directive file's first block sets (statement_timeout,
// lock_timeout — 1725200000's gate (c) asserts current_setting('statement_timeout') by name) would
// otherwise be silently lost between blocks. goose gives the same guarantee by handing one
// sequential caller its single idle database/sql connection back for every block.
func ApplyGooseUp(ctx context.Context, pool *pgxpool.Pool, name, content string) error {
	blocks, err := SplitGooseUp(content)
	if err != nil {
		return fmt.Errorf("failed to parse migration %s: %w", name, err)
	}

	if !HasGooseNoTransaction(content) {
		up := blocks[0]
		if strings.TrimSpace(up) == "" {
			return nil
		}
		if _, err := pool.Exec(ctx, up); err != nil {
			return fmt.Errorf("failed to apply migration %s: %w", name, err)
		}
		return nil
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("failed to acquire a connection for migration %s: %w", name, err)
	}
	defer conn.Release()

	for i, block := range blocks {
		if strings.TrimSpace(block) == "" {
			continue
		}
		if _, err := conn.Exec(ctx, block); err != nil {
			return fmt.Errorf("failed to apply migration %s block %d of %d: %w", name, i+1, len(blocks), err)
		}
	}
	return nil
}
