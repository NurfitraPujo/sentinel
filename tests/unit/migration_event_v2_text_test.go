package unit

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sentinel-event-schema-v2 §5.4: the text of migration 1725300000, pinned where no database is
// needed. The live half (replay, shape, Down, a second ledger) is V4a,
// tests/integration/db_migrations_test.go TestEventSchemaV2MigrationUpDownReplay.

const eventV2MigrationPath = "../../packages/db-migrations/migrations/1725300000_add_error_occurrences_event_v2_fields.sql"

// §5.2 in order: name and SQL type as the file writes them.
var eventV2MigrationColumns = [][2]string{
	{"level", "VARCHAR(16)"}, {"message", "TEXT"}, {"error_class", "VARCHAR(255)"}, {"logger", "VARCHAR(64)"},
	{"server_name", "VARCHAR(256)"}, {"transaction", "VARCHAR(200)"}, {"dist", "VARCHAR(64)"}, {"source", "VARCHAR(64)"},
	{"trace_flags", "BIGINT"}, {"received_at", "TIMESTAMPTZ"}, {"sdk", "JSONB"}, {"event_user", "JSONB"},
	{"tags", "JSONB"}, {"contexts", "JSONB"}, {"request", "JSONB"}, {"breadcrumbs", "JSONB"}, {"exception", "JSONB"},
}

func TestEventSchemaV2MigrationText(t *testing.T) {
	raw, err := os.ReadFile(filepath.FromSlash(eventV2MigrationPath))
	require.NoError(t, err)
	text := string(raw)
	lines := strings.Split(text, "\n")

	t.Run("the no-transaction directive is line 1 and nowhere else", func(t *testing.T) {
		require.Equal(t, "-- +goose NO TRANSACTION", lines[0])
		assert.Equal(t, 1, strings.Count(text, "NO TRANSACTION"))
	})

	up, down, ok := strings.Cut(text, "\n-- +goose Down\n")
	require.True(t, ok)

	t.Run("one ALTER adds the 17 columns in §5.2 order, every one IF NOT EXISTS, nullable, no default", func(t *testing.T) {
		require.Equal(t, 1, strings.Count(up, "\nALTER TABLE error_occurrences"), "one ALTER: one catalog-only lock")
		alter := up[strings.Index(up, "\nALTER TABLE error_occurrences"):]
		alter = alter[:strings.Index(alter, ";")+1]
		got := regexp.MustCompile(`ADD COLUMN IF NOT EXISTS (\w+) ([A-Z]+(?:\(\d+\))?)[,;]`).FindAllStringSubmatch(alter, -1)
		var cols [][2]string
		for _, m := range got {
			cols = append(cols, [2]string{m[1], m[2]})
		}
		assert.Equal(t, eventV2MigrationColumns, cols)
		assert.Equal(t, strings.Count(alter, "ADD COLUMN "), strings.Count(alter, "ADD COLUMN IF NOT EXISTS "), "every ADD COLUMN is IF NOT EXISTS")
		assert.NotContains(t, alter, "NOT NULL")
		assert.NotContains(t, alter, "DEFAULT")
		assert.NotContains(t, alter, "CHECK")
	})

	t.Run("the GIN index is partial and built concurrently, with lock_timeout released first", func(t *testing.T) {
		build := "CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_error_occurrences_tags\n    ON error_occurrences USING GIN (tags)\n    WHERE tags IS NOT NULL;"
		at := strings.Index(up, build)
		require.GreaterOrEqual(t, at, 0)
		release := strings.LastIndex(up[:at], "SET lock_timeout = 0;")
		assert.Greater(t, release, strings.LastIndex(up[:at], "SET lock_timeout = '5s';"), "lock_timeout is 0 for the build")
		assert.Contains(t, up, "AND NOT indisvalid", "drop-if-invalid")
		assert.Contains(t, up, "RAISE EXCEPTION '1725300000_add_error_occurrences_event_v2_fields", "the indisvalid check names the file")
	})

	t.Run("every statement is wrapped in StatementBegin/StatementEnd", func(t *testing.T) {
		assert.Equal(t, strings.Count(text, "-- +goose StatementBegin"), strings.Count(text, "-- +goose StatementEnd"))
		depth := 0
		for i, l := range lines {
			switch strings.TrimSpace(l) {
			case "-- +goose StatementBegin":
				depth++
				require.Equal(t, 1, depth, "line %d: nested StatementBegin", i+1)
			case "-- +goose StatementEnd":
				depth--
			default:
				trimmed := strings.TrimSpace(l)
				if depth == 0 && trimmed != "" && !strings.HasPrefix(trimmed, "--") {
					t.Errorf("line %d is SQL outside a StatementBegin/End pair: %q", i+1, l)
				}
			}
		}
	})

	t.Run("Down sets lock_timeout before its ALTER and drops all 17 columns in reverse order", func(t *testing.T) {
		lockAt, alterAt := strings.Index(down, "SET lock_timeout = '5s';"), strings.Index(down, "ALTER TABLE error_occurrences")
		require.GreaterOrEqual(t, alterAt, 0)
		assert.True(t, lockAt >= 0 && lockAt < alterAt, "T18: the DROP COLUMNs take ACCESS EXCLUSIVE on the hot table")
		got := regexp.MustCompile(`DROP COLUMN IF EXISTS (\w+)[,;]`).FindAllStringSubmatch(down, -1)
		require.Len(t, got, len(eventV2MigrationColumns))
		for i, m := range got {
			assert.Equal(t, eventV2MigrationColumns[len(eventV2MigrationColumns)-1-i][0], m[1])
		}
		assert.Contains(t, down, "DROP INDEX CONCURRENTLY IF EXISTS idx_error_occurrences_tags;")
	})

	t.Run("the header states the deploy and rollback order", func(t *testing.T) {
		assert.Contains(t, up, "DEPLOY ORDER")
		assert.Contains(t, up, "Down runs LAST")
	})
}
