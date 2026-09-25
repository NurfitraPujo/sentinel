import { describe, it, expect } from 'vitest';
import { readdirSync, readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';
// The bare 'drizzle-orm/pg-core' entry, not the '/utils' subpath (see tests/schema-drift.test.ts).
import { getTableConfig, type PgTable } from 'drizzle-orm/pg-core';
import { errorOccurrences, issueTagValues, issueTagValuesBackfill, issueTagValuesDrift } from '../src/lib/db/schema';

// sentinel-issue-tag-facets spec §8.5: the hand-maintained Drizzle mirror declares exactly the columns the
// tag-facets migration creates, with the same SQL types and nullability, for all three tables and the
// error_occurrences marker column. Like event-v2-mirror.test.ts it parses the migration file and needs no
// database, so it runs under every `pnpm test`. The file is found by its suffix, never its version, so a
// renumbering (plan §6) changes nothing here.

const migrationsDir = join(dirname(fileURLToPath(import.meta.url)), '../../../packages/db-migrations/migrations');

function migrationUp(): string {
	const files = readdirSync(migrationsDir).filter((f) => f.endsWith('_add_issue_tag_values.sql'));
	expect(files, 'exactly one *_add_issue_tag_values.sql').toHaveLength(1);
	const sql = readFileSync(join(migrationsDir, files[0]), 'utf8');
	return sql.slice(0, sql.indexOf('-- +goose Down'));
}

interface MigrationColumn {
	name: string;
	type: string;
	notNull: boolean;
}

function tableColumns(up: string, table: string): MigrationColumn[] {
	const start = up.indexOf(`CREATE TABLE IF NOT EXISTS ${table} (`);
	expect(start, `the migration creates ${table}`).toBeGreaterThan(-1);
	const rest = up.slice(start);
	const body = rest.slice(0, rest.search(/\n\s*\)/));
	const cols: MigrationColumn[] = [];
	for (const line of body.split('\n').slice(1)) {
		const m = line.match(/^\s+(\w+)\s+([A-Z]+(?:\(\d+\))?)(.*)$/);
		if (!m || m[1] === 'PRIMARY') continue;
		cols.push({ name: m[1], type: m[2], notNull: /NOT NULL|PRIMARY KEY/.test(m[3]) });
	}
	return cols;
}

// The migration's spelling -> the SQL type Drizzle reports (getSQLType()).
function drizzleSpelling(migrationType: string): string {
	const t = migrationType.toLowerCase();
	if (t === 'timestamptz') return 'timestamp with time zone';
	return t;
}

function assertMirrors(table: PgTable, cols: MigrationColumn[]) {
	const drizzle = new Map(getTableConfig(table).columns.map((c) => [c.name, c]));
	expect([...drizzle.keys()].sort(), `${getTableConfig(table).name}: the same column set`).toEqual(
		cols.map((c) => c.name).sort()
	);
	for (const { name, type, notNull } of cols) {
		const col = drizzle.get(name)!;
		expect(col.getSQLType(), name).toBe(drizzleSpelling(type));
		expect(col.notNull, `${name} NOT NULL`).toBe(notNull);
	}
}

describe('the tag-facets Drizzle mirror matches the migration (spec §8.5)', () => {
	const up = migrationUp();

	it('issue_tag_values: six columns, the composite primary key', () => {
		const cols = tableColumns(up, 'issue_tag_values');
		expect(cols.map((c) => c.name)).toEqual(['issue_id', 'key', 'value', 'count', 'first_seen', 'last_seen']);
		assertMirrors(issueTagValues, cols);
		const cfg = getTableConfig(issueTagValues);
		expect(cfg.primaryKeys.map((pk) => pk.columns.map((c) => c.name))).toEqual([['issue_id', 'key', 'value']]);
		expect(cfg.indexes.map((i) => i.config.name)).toContain('idx_issue_tag_values_key_value');
	});

	it('issue_tag_values_backfill', () => {
		assertMirrors(issueTagValuesBackfill, tableColumns(up, 'issue_tag_values_backfill'));
	});

	it('issue_tag_values_drift', () => {
		assertMirrors(issueTagValuesDrift, tableColumns(up, 'issue_tag_values_drift'));
	});

	it('error_occurrences.tag_rollup_applied: nullable boolean, no default', () => {
		expect(up).toMatch(/ADD COLUMN tag_rollup_applied BOOLEAN;/);
		const col = getTableConfig(errorOccurrences).columns.find((c) => c.name === 'tag_rollup_applied');
		expect(col, 'errorOccurrences declares tag_rollup_applied').toBeDefined();
		expect(col!.getSQLType()).toBe('boolean');
		expect(col!.notNull).toBe(false);
		expect(col!.hasDefault).toBe(false);
	});
});
