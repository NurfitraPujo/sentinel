import { describe, it, expect } from 'vitest';
import { readdirSync, readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';
// The bare 'drizzle-orm/pg-core' entry, not the '/utils' subpath (see tests/schema-drift.test.ts).
import { getTableConfig } from 'drizzle-orm/pg-core';
import { errorOccurrences } from '../src/lib/db/schema';

// sentinel-event-schema-v2 §5.5 / §9.2 V7d: the hand-maintained Drizzle mirror declares exactly the
// columns migration 1725300000 adds, with the same SQL types, and needs NO database -- unlike
// schema-drift.test.ts, which skips when no Postgres answers, this runs under every `pnpm test`.
// `trace_flags` must be bigint (the proto's uint32 does not fit INTEGER), in 'number' mode.

const migrationsDir = join(dirname(fileURLToPath(import.meta.url)), '../../../packages/db-migrations/migrations');

function migrationColumns(): Array<{ name: string; type: string }> {
	const file = readdirSync(migrationsDir).find((f) => f.startsWith('1725300000_'));
	expect(file, 'migration 1725300000 exists').toBeDefined();
	const sql = readFileSync(join(migrationsDir, file!), 'utf8');
	const up = sql.slice(0, sql.indexOf('-- +goose Down'));
	return [...up.matchAll(/ADD COLUMN IF NOT EXISTS (\w+) ([A-Z]+(?:\(\d+\))?)[,;]/g)].map((m) => ({
		name: m[1],
		type: m[2],
	}));
}

// The migration's spelling -> the SQL type Drizzle reports for the column (getSQLType()).
function drizzleSpelling(migrationType: string): string {
	const t = migrationType.toLowerCase();
	if (t === 'timestamptz') return 'timestamp with time zone';
	return t;
}

describe('errorOccurrences mirrors migration 1725300000 (V7d)', () => {
	const cols = migrationColumns();
	const drizzle = new Map(getTableConfig(errorOccurrences).columns.map((c) => [c.name, c]));

	it('the migration adds the 17 §5.2 columns', () => {
		expect(cols.map((c) => c.name)).toEqual([
			'level',
			'message',
			'error_class',
			'logger',
			'server_name',
			'transaction',
			'dist',
			'source',
			'trace_flags',
			'received_at',
			'sdk',
			'event_user',
			'tags',
			'contexts',
			'request',
			'breadcrumbs',
			'exception',
		]);
	});

	it('Drizzle declares every one with the same SQL type, nullable, no default', () => {
		for (const { name, type } of cols) {
			const col = drizzle.get(name);
			expect(col, `errorOccurrences declares ${name}`).toBeDefined();
			expect(col!.getSQLType(), name).toBe(drizzleSpelling(type));
			expect(col!.notNull, `${name} is nullable`).toBe(false);
			expect(col!.hasDefault, `${name} has no default`).toBe(false);
		}
	});

	it('trace_flags is bigint in number mode, so the agent schema\'s z.number() holds', () => {
		const col = drizzle.get('trace_flags')!;
		expect(col.getSQLType()).toBe('bigint');
		expect(col.dataType).toBe('number');
	});

	it('the user column is event_user, never the reserved word user', () => {
		expect(drizzle.has('event_user')).toBe(true);
		expect(drizzle.has('user')).toBe(false);
	});
});
