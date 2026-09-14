/**
 * sentinel-releases-snooze §4.5 (TASK-01, criterion 2): the Drizzle DECLARATION of the feature's
 * schema, pinned without a database.
 *
 * tests/schema-drift.test.ts proves `schema.ts ⊆ database` in the direction that matters at runtime
 * (a declared column that does not exist, a NOT NULL the database does not satisfy) -- but it cannot
 * see a `.default()` that the database does NOT have: Drizzle applies a declared default on the
 * INSERT side only, so `snoozeCountAt: … .default(0)` would pass the drift guard and quietly write a
 * baseline for a snooze that was never set (check_snooze_pair would then let a target-less write
 * through with count_at = 0). §4.5 says the four columns are nullable with NO default, so that is
 * asserted here on the table config itself, beside the composite primary key `releases` must have
 * for the processor's ON CONFLICT target (§4.3).
 */
import { describe, it, expect } from 'vitest';
import { getTableColumns } from 'drizzle-orm';
import { getTableConfig } from 'drizzle-orm/pg-core';
import { issues, releases } from './schema';

describe('sentinel-releases-snooze §4.5: the Drizzle declaration', () => {
	const issueColumns = getTableColumns(issues);

	it.each([
		['snoozedUntil', 'snoozed_until'],
		['snoozeOccurrenceTarget', 'snooze_occurrence_target'],
		['snoozeCountAt', 'snooze_count_at'],
		['firstReleaseVersion', 'first_release_version'],
	] as const)('issues.%s is nullable with no default (mapped to %s)', (key, column) => {
		const col = issueColumns[key];
		expect(col).toBeDefined();
		expect(col.name).toBe(column);
		expect(col.notNull).toBe(false);
		expect(col.hasDefault).toBe(false);
		expect(col.default).toBeUndefined();
	});

	it('releases has primaryKey({ columns: [projectId, version] }) and no other primary key', () => {
		const config = getTableConfig(releases);
		expect(config.name).toBe('releases');
		expect(config.primaryKeys).toHaveLength(1);
		expect(config.primaryKeys[0].columns.map((c) => c.name)).toEqual(['project_id', 'version']);
		// Neither column is a primary key on its own -- the pair is the identity.
		const columns = getTableColumns(releases);
		expect(columns.projectId.primary).toBe(false);
		expect(columns.version.primary).toBe(false);
	});

	it('releases declares the five migrated columns, occurrenceCount with the migrated DEFAULT 0', () => {
		const columns = getTableColumns(releases);
		expect(Object.keys(columns).sort()).toEqual(['firstSeen', 'lastSeen', 'occurrenceCount', 'projectId', 'version']);
		expect(columns.occurrenceCount.notNull).toBe(true);
		expect(columns.occurrenceCount.hasDefault).toBe(true);
		expect(columns.firstSeen.notNull).toBe(true);
		expect(columns.lastSeen.notNull).toBe(true);
		expect(columns.version.notNull).toBe(true);
	});
});
