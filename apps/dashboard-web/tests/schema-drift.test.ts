/**
 * Schema-drift guard (P6-3 / P6-1).
 *
 * src/lib/db/schema.ts is a hand-maintained description of a database whose actual shape is defined by
 * packages/db-migrations/migrations/*.sql — the same "two hand-maintained descriptions of one contract,
 * nothing checking they agree" pattern as the SDK<->ingestor seam (B5 in docs/memory/BUGS.md). That drift
 * was real and shipped: issueActivity declared a `metadata` column that doesn't exist (the migrated table
 * has old_value/new_value), issueRelations was missing NOT NULL created_by_type/created_by and defaulted
 * relationType to a value the CHECK constraint rejects, and alertConfigs declared channel_target/
 * window_seconds where the migrated table has channel_config (JSONB) / frequency_window_seconds. Every one
 * of those made the corresponding route 500 on every real request while `pnpm check` stayed green, because
 * TypeScript has no way to know the migrated database disagrees with schema.ts.
 *
 * This test introspects a REAL, migrated Postgres (information_schema) and asserts every table/column
 * schema.ts declares actually exists with a compatible type family. It is the guard that makes this class
 * of drift loud instead of silent.
 *
 * Requires a reachable, migrated Postgres. Point it at one with DATABASE_URL, e.g.:
 *   docker run --rm -d -p 15533:5432 -e POSTGRES_USER=sentinel -e POSTGRES_PASSWORD=changeme \
 *     -e POSTGRES_DB=sentinel postgres:15-alpine
 *   cd packages/db-migrations && DB_URL_DASHBOARD="postgres://sentinel:changeme@localhost:15533/sentinel?sslmode=disable" \
 *     go run ./cmd/migrate up -target dashboard
 *   DATABASE_URL="postgres://sentinel:changeme@localhost:15533/sentinel" pnpm test
 *
 * If no database is reachable this suite SKIPS (with a loud console warning) rather than failing — the
 * `dashboard` CI job (.github/workflows/ci.yml) does not currently start a postgres service for the
 * `pnpm test` step, so a hard failure here would be a false red for every push until that job is given one.
 * That CI gap is itself worth closing (see docs/memory/VERIFIED_STATE.md / this feature's write-up); it is
 * out of this file's scope to fix (outside apps/dashboard-web).
 */
import { describe, it, expect, beforeAll, afterAll } from 'vitest';
import postgres from 'postgres';
// NOTE: import getTableConfig from the bare 'drizzle-orm/pg-core' package, not the
// 'drizzle-orm/pg-core/utils' subpath — in this installed version that subpath's package.json "exports"
// entry resolves to pg-core/utils/index.js (unrelated array-literal helpers: makePgArray etc.), not the
// flat pg-core/utils.js that actually defines getTableConfig. The flat file is only reachable through the
// bare 'drizzle-orm/pg-core' entry point, which re-exports both via `export *`.
import { getTableConfig } from 'drizzle-orm/pg-core';
import { isTable } from 'drizzle-orm';
import type { AnyPgColumn } from 'drizzle-orm/pg-core';
import * as schema from '../src/lib/db/schema';

// Deliberately opt-in only (SCHEMA_DRIFT_DATABASE_URL, falling back to DATABASE_URL) — NOT defaulted to
// localhost:5432 the way src/lib/server/db.ts is. That address is the shared dev compose stack's
// `sentinel-postgres`, and docs/memory/VERIFIED_STATE.md documents it as observed, more than once, in an
// inconsistent, partially-migrated state (independent goose ledgers stepping on each other — see its "shared
// database corruption" hazard note). A schema-drift guard that opportunistically binds to whatever is
// listening on 5432 would flap based on unrelated concurrent activity, not on whether schema.ts itself is
// correct. Point it explicitly at a database you know was freshly migrated (see the header comment above).
const DATABASE_URL = process.env.SCHEMA_DRIFT_DATABASE_URL ?? process.env.DATABASE_URL;

interface PgColumnInfo {
	column_name: string;
	data_type: string;
	character_maximum_length: number | null;
	is_nullable: 'YES' | 'NO';
}

interface TypeFamily {
	family: string;
	length: number | null;
}

// Maps a Drizzle column's getSQLType() output to the "family" information_schema.columns.data_type would
// report, plus a declared length where relevant. Deliberately loose on things that don't cause the bug
// class this test guards against (e.g. timestamp vs timestamptz precision) and strict on things that do
// (e.g. a JSONB column declared as varchar — exactly the alertConfigs drift found alongside this test).
function normalizeDrizzleSqlType(sqlType: string): TypeFamily {
	const varcharMatch = sqlType.match(/^varchar(?:\((\d+)\))?$/);
	if (varcharMatch) {
		return { family: 'string', length: varcharMatch[1] ? Number(varcharMatch[1]) : null };
	}
	if (sqlType === 'text') return { family: 'string', length: null };
	if (sqlType === 'uuid') return { family: 'uuid', length: null };
	if (sqlType === 'jsonb') return { family: 'json', length: null };
	if (sqlType === 'json') return { family: 'json', length: null };
	if (sqlType === 'boolean') return { family: 'boolean', length: null };
	if (sqlType === 'integer') return { family: 'integer', length: null };
	if (sqlType === 'bigint') return { family: 'integer', length: null };
	if (sqlType.startsWith('timestamp')) return { family: 'timestamp', length: null };
	// Unknown type: return it verbatim as its own family so the comparison below fails loudly instead of
	// silently passing — a new column type added to schema.ts must extend this map, not be ignored by it.
	return { family: `unknown:${sqlType}`, length: null };
}

function normalizePgDataType(dataType: string, maxLength: number | null): TypeFamily {
	switch (dataType) {
		case 'character varying':
		case 'character':
		case 'text':
			return { family: 'string', length: maxLength };
		case 'uuid':
			return { family: 'uuid', length: null };
		case 'json':
		case 'jsonb':
			return { family: 'json', length: null };
		case 'boolean':
			return { family: 'boolean', length: null };
		case 'integer':
		case 'smallint':
		case 'bigint':
			return { family: 'integer', length: null };
		case 'timestamp without time zone':
		case 'timestamp with time zone':
			return { family: 'timestamp', length: null };
		default:
			return { family: `unknown:${dataType}`, length: null };
	}
}

// Every pgTable export from schema.ts, keyed by its export name (for readable failure messages).
const declaredTables = Object.entries(schema).filter(
	(entry): entry is [string, (typeof schema)[keyof typeof schema]] => isTable(entry[1])
);

describe('Drizzle schema vs migrated database (schema-drift guard)', () => {
	let sql: postgres.Sql | null = null;
	let dbAvailable = false;

	beforeAll(async () => {
		if (!DATABASE_URL) {
			dbAvailable = false;
			// eslint-disable-next-line no-console
			console.warn(
				'[schema-drift.test.ts] Neither SCHEMA_DRIFT_DATABASE_URL nor DATABASE_URL is set. Skipping ' +
					'schema-drift assertions — this guard cannot run without a freshly-migrated database pointed at ' +
					'it explicitly (see the header comment in this file for how to stand one up).'
			);
			return;
		}
		sql = postgres(DATABASE_URL, { connect_timeout: 3, max: 1, onnotice: () => {} });
		try {
			await sql`select 1`;
			dbAvailable = true;
		} catch (err) {
			dbAvailable = false;
			// eslint-disable-next-line no-console
			console.warn(
				`[schema-drift.test.ts] No reachable Postgres at ${DATABASE_URL.replace(/:[^:@]*@/, ':***@')} ` +
					`(${(err as Error).message}). Skipping schema-drift assertions.`
			);
		}
	}, 10_000);

	afterAll(async () => {
		if (sql) await sql.end({ timeout: 1 });
	});

	it('found at least one pgTable export to check (sanity check on this test itself)', () => {
		expect(declaredTables.length).toBeGreaterThan(10);
	});

	for (const [exportName, table] of declaredTables) {
		describe(`table: ${exportName}`, () => {
			it('exists in the migrated database, with every declared column present and type-compatible', async () => {
				if (!dbAvailable) {
					console.warn(`[schema-drift.test.ts] skipped (no DB): ${exportName}`);
					return;
				}

				const config = getTableConfig(table as never);
				const dbTableName = config.name;

				const rows = (await sql!<PgColumnInfo[]>`
					SELECT column_name, data_type, character_maximum_length, is_nullable
					FROM information_schema.columns
					WHERE table_schema = 'public' AND table_name = ${dbTableName}
				`) as unknown as PgColumnInfo[];

				expect(
					rows.length,
					`table "${dbTableName}" (schema.ts export "${exportName}") does not exist in the migrated ` +
						`database at all — is a migration missing, or did the table get renamed on one side only?`
				).toBeGreaterThan(0);

				const byName = new Map(rows.map((r) => [r.column_name, r]));

				for (const column of config.columns as AnyPgColumn[]) {
					const dbColumn = byName.get(column.name);
					expect(
						dbColumn,
						`${dbTableName}.${column.name} (schema.ts "${exportName}.${String((column as any).fieldName ?? column.name)}") ` +
							`does not exist in the migrated database. This is exactly the class of bug that made ` +
							`POST /api/projects/[projectId]/issues/batch and POST /api/issues/[issueId]/relations 500 ` +
							`with SQLSTATE 42703 on every request (see docs/memory/VERIFIED_STATE.md P6-3) — check the ` +
							`migration in packages/db-migrations/migrations/ for the real column name/existence.`
					).toBeDefined();
					if (!dbColumn) continue;

					const declared = normalizeDrizzleSqlType(column.getSQLType());
					const actual = normalizePgDataType(dbColumn.data_type, dbColumn.character_maximum_length);

					expect(
						declared.family,
						`${dbTableName}.${column.name}: schema.ts declares type "${column.getSQLType()}" (family ` +
							`"${declared.family}") but the migrated column is "${dbColumn.data_type}" (family ` +
							`"${actual.family}") — these are not interchangeable. This is the alertConfigs.channelTarget ` +
							`(declared varchar) vs the real channel_config (JSONB) bug class: a type family mismatch ` +
							`silently breaks every read/write, not just a missing column.`
					).toBe(actual.family);

					// A DB column that is NOT NULL with no application-visible default is the exact shape of the
					// issueRelations.created_by_type/created_by bug: schema.ts declared them nullable, every insert
					// omitted them, and the CHECK/NOT NULL constraint rejected every row. A schema.ts column marked
					// notNull() for a DB column that is actually nullable is safe (over-strict, not under-strict), so
					// only the DB-NOT-NULL / schema.ts-nullable direction is checked.
					if (dbColumn.is_nullable === 'NO' && !column.notNull && !column.hasDefault) {
						throw new Error(
							`${dbTableName}.${column.name} is NOT NULL in the migrated database with no default, but ` +
								`schema.ts declares it nullable/optional ("${exportName}"). Every insert that omits this ` +
								`field will fail — mark it .notNull() in schema.ts (and make sure every write path ` +
								`actually supplies it).`
						);
					}
				}
			});
		});
	}
});
