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
 * If no database is reachable this suite SKIPS locally (with a loud console warning) — but in CI,
 * SCHEMA_DRIFT_REQUIRED=1 (set by the `dashboard` job in .github/workflows/ci.yml, which now also starts a
 * real postgres service and runs the real migrations against it before this test runs) turns that same
 * "no database" condition into a hard failure instead. A guard that silently skips and still reports the
 * overall suite green is worse than no guard — it teaches people to trust a check that never ran. Locally,
 * without SCHEMA_DRIFT_REQUIRED set, the skip remains a skip so `pnpm test` stays usable without a database.
 *
 * This guard checks three things per declared table, in both directions:
 *   1. Forward (schema.ts -> DB): every column schema.ts declares exists in the migrated table, with a
 *      compatible type family AND length, and NOT NULL-with-no-default is caught (the created_by_type/
 *      created_by bug).
 *   2. Reverse (DB -> schema.ts): every column the migrated table actually has is declared in schema.ts,
 *      except columns explicitly listed in IGNORED_DB_COLUMNS below. Without this direction, a column added
 *      to the database that schema.ts never learns about is invisible — which is exactly what happened to
 *      issueRelations.created_by_type/created_by: they were ABSENT from schema.ts, not merely mistyped.
 *   3. CHECK constraints: for every column with a literal (non-function) default declared in schema.ts,
 *      cross-check that literal against every single-column CHECK constraint on that column (introspected via
 *      pg_constraint). This is the alertConfigs-era `.default('related')` bug: issueRelations.relationType's
 *      CHECK only allows 'linked_to'/'caused_by'/'duplicate_of', so a default of 'related' would violate it on
 *      every insert that omits the column — svelte-check has no way to know that, since it never runs the
 *      constraint.
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

// Set by the `dashboard` CI job (.github/workflows/ci.yml) alongside the postgres service it now starts.
// When set, an unreachable database is a hard failure instead of a per-table skip — mirrors SENTINEL_E2E's
// role for tests/integration. Left unset locally so `pnpm test` still runs without a database on hand.
const REQUIRED = process.env.SCHEMA_DRIFT_REQUIRED === '1';

// Columns that exist in the migrated database but are intentionally not modeled in schema.ts. Keep this
// narrow and named per-table/per-column, never a blanket opt-out — an entry here should be added only when
// a column is a deliberate, reviewed omission (e.g. a column only ever written by a different service's own
// migration-adjacent tooling), not as a way to silence a real reverse-drift finding.
const IGNORED_DB_COLUMNS: Record<string, string[]> = {};

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

interface PgCheckConstraint {
	conname: string;
	definition: string;
}

// Returns the literal value schema.ts declares as a column's default, if (and only if) it is a plain
// literal (string/number/boolean) rather than a SQL expression (e.g. defaultNow()/defaultRandom(), or a
// $defaultFn() callback) — those aren't something we can textually substitute into a CHECK expression, and
// aren't the class of bug this check targets anyway (CHECK constraints in this schema are enum-style string
// checks, not timestamp/uuid checks). Returns undefined when there is no default, or the default isn't a
// literal we can test.
function literalDefaultOf(column: AnyPgColumn): string | number | boolean | undefined {
	const value = (column as unknown as { default?: unknown }).default;
	if (value === undefined || value === null) return undefined;
	if (typeof value === 'string' || typeof value === 'number' || typeof value === 'boolean') return value;
	return undefined;
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
			const message =
				'[schema-drift.test.ts] Neither SCHEMA_DRIFT_DATABASE_URL nor DATABASE_URL is set. ' +
				(REQUIRED
					? 'SCHEMA_DRIFT_REQUIRED=1 is set, so this is a hard failure rather than a skip — the ' +
						'`dashboard` CI job is expected to provide a real, migrated postgres service (see ' +
						'.github/workflows/ci.yml). If you are running locally, either unset SCHEMA_DRIFT_REQUIRED ' +
						'or point one of those env vars at a freshly-migrated database (see this file\'s header comment).'
					: 'Skipping schema-drift assertions — this guard cannot run without a freshly-migrated database ' +
						'pointed at it explicitly (see the header comment in this file for how to stand one up).');
			// eslint-disable-next-line no-console
			console.warn(message);
			if (REQUIRED) throw new Error(message);
			return;
		}
		sql = postgres(DATABASE_URL, { connect_timeout: 3, max: 1, onnotice: () => {} });
		try {
			await sql`select 1`;
			dbAvailable = true;
		} catch (err) {
			dbAvailable = false;
			const message =
				`[schema-drift.test.ts] No reachable Postgres at ${DATABASE_URL.replace(/:[^:@]*@/, ':***@')} ` +
				`(${(err as Error).message}). ` +
				(REQUIRED
					? 'SCHEMA_DRIFT_REQUIRED=1 is set, so this is a hard failure rather than a skip.'
					: 'Skipping schema-drift assertions.');
			// eslint-disable-next-line no-console
			console.warn(message);
			if (REQUIRED) throw new Error(message);
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
					const message = `[schema-drift.test.ts] skipped (no DB): ${exportName}`;
					if (REQUIRED) throw new Error(`${message} — SCHEMA_DRIFT_REQUIRED=1 forbids a silent skip.`);
					console.warn(message);
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

					// Family alone is not enough: two varchar columns of different declared lengths are the same
					// family but not interchangeable — a migrated varchar(5) silently truncates every insert past 5
					// characters, and normalizeDrizzleSqlType computing a length that is then thrown away would let
					// exactly that pass. Only compare when both sides declare a length (unbounded text/text-family
					// columns report null on both sides and are intentionally not compared here).
					if (declared.length !== null && actual.length !== null) {
						expect(
							declared.length,
							`${dbTableName}.${column.name}: schema.ts declares length ${declared.length} but the ` +
								`migrated column is length ${actual.length} — a shorter DB column silently truncates ` +
								`every write past that length (e.g. created_by shrunk to varchar(5) would still pass a ` +
								`family-only check).`
						).toBe(actual.length);
					}

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

					// Reverse walk (DB -> schema.ts): a column that exists in the migrated table but that
					// schema.ts never declares is invisible to every check above — this is precisely how
					// issueRelations.created_by_type/created_by shipped: they were ABSENT from schema.ts, not
					// merely mistyped, so a forward-only walk (schema.ts -> DB) would never have caught it.
					const declaredNames = new Set((config.columns as AnyPgColumn[]).map((c) => c.name));
					const ignored = new Set(IGNORED_DB_COLUMNS[dbTableName] ?? []);
					for (const row of rows) {
						if (declaredNames.has(row.column_name) || ignored.has(row.column_name)) continue;
						throw new Error(
							`${dbTableName}.${row.column_name} exists in the migrated database but schema.ts ("${exportName}") ` +
								`does not declare it. Either add it to schema.ts, or if it is a deliberate, reviewed omission, ` +
								`add "${dbTableName}": ["${row.column_name}"] to IGNORED_DB_COLUMNS in this file.`
						);
					}

					// CHECK constraint introspection: a literal default in schema.ts that violates the table's own
					// CHECK constraint is the issueRelations `.default('related')` bug — relationType's CHECK only
					// allows 'linked_to'/'caused_by'/'duplicate_of', so every insert that omitted the column would
					// have failed at write time despite schema.ts/svelte-check being fully green. Only single-column
					// CHECK constraints are evaluated (a constraint referencing more than one column identifier is
					// logged and skipped, not silently treated as passing).
					const checkConstraints = (await sql!<PgCheckConstraint[]>`
						SELECT conname, pg_get_constraintdef(oid) AS definition
						FROM pg_constraint
						WHERE conrelid = ${dbTableName}::regclass AND contype = 'c'
					`) as unknown as PgCheckConstraint[];

					if (checkConstraints.length > 0) {
						const allColumnNames = (config.columns as AnyPgColumn[]).map((c) => c.name);
						// 'g' flag: a column referenced more than once in the same CHECK expression (e.g. a range
						// check) needs every occurrence substituted, not just the first — a fresh RegExp instance
						// is created per call so there is no lastIndex state to leak between .test()/.replace() uses.
						const wordBoundary = (name: string) => new RegExp(`\\b${name}\\b`, 'g');

						for (const column of config.columns as AnyPgColumn[]) {
							const literalDefault = literalDefaultOf(column);
							if (literalDefault === undefined) continue;

							for (const constraint of checkConstraints) {
								// pg_get_constraintdef returns the full "CHECK (<expr>)" clause, not just <expr> — strip
								// the keyword so the remainder is a bare boolean expression we can SELECT directly.
								const def = constraint.definition.replace(/^CHECK\s*/i, '');
								if (!wordBoundary(column.name).test(def)) continue;

								const otherColumnsReferenced = allColumnNames.filter(
									(name) => name !== column.name && wordBoundary(name).test(def)
								);
								if (otherColumnsReferenced.length > 0) {
									// eslint-disable-next-line no-console
									console.warn(
										`[schema-drift.test.ts] CHECK constraint "${constraint.conname}" on "${dbTableName}" ` +
											`references multiple columns (${column.name}, ${otherColumnsReferenced.join(', ')}) — ` +
											`default-satisfies-CHECK introspection only supports single-column constraints; skipping.`
									);
									continue;
								}

								const literalSql =
									typeof literalDefault === 'string'
										? `'${literalDefault.replace(/'/g, "''")}'`
										: String(literalDefault);
								const expr = def.replace(wordBoundary(column.name), `(${literalSql})`);

								const [{ ok }] = (await sql!.unsafe(`SELECT (${expr}) AS ok`)) as unknown as {
									ok: boolean;
								}[];

								expect(
									ok,
									`${dbTableName}.${column.name}: schema.ts declares default ${JSON.stringify(literalDefault)} ` +
										`("${exportName}"), but constraint "${constraint.conname}" (${def}) rejects it — every ` +
										`insert that omits this column would violate the table's own CHECK constraint. This is ` +
										`the issueRelations.relationType \`.default('related')\` bug class.`
								).toBe(true);
							}
						}
					}
				});
			});
	}
});
