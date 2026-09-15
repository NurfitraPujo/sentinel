/**
 * sentinel-sourcemaps §4.5 (TASK-01, criterion 1): the Drizzle DECLARATION of the feature's
 * schema, pinned without a database.
 *
 * tests/schema-drift.test.ts proves `schema.ts ⊆ database` in the direction that matters at runtime
 * (a declared column that does not exist, a NOT NULL the database does not satisfy) -- but it cannot
 * see an index the declaration forgot, or a `.default()` the database does NOT have: Drizzle applies
 * a declared default on the INSERT side only. §4.5 says the table's identity is
 * UNIQUE (project_id, release_version, minified_path) -- the upload route's upsert target and the
 * processor's lookup -- and that storage_key is UNIQUE on its own, so both are asserted here on the
 * table config itself, beside the column bounds the ingest path already enforces on the same
 * strings (VARCHAR(100) / VARCHAR(512)) and the two defaults the migration carries (id, uploaded_at).
 */
import { describe, it, expect } from 'vitest';
import { getTableColumns } from 'drizzle-orm';
import { getTableConfig } from 'drizzle-orm/pg-core';
import { sourcemaps } from './schema';

describe('sentinel-sourcemaps §4.5: the Drizzle declaration', () => {
	const columns = getTableColumns(sourcemaps);
	const config = getTableConfig(sourcemaps);

	it('sourcemaps declares the eight migrated columns and nothing else', () => {
		expect(config.name).toBe('sourcemaps');
		expect(Object.keys(columns).sort()).toEqual([
			'id',
			'minifiedPath',
			'projectId',
			'releaseVersion',
			'sha256',
			'sizeBytes',
			'storageKey',
			'uploadedAt',
		]);
	});

	it.each([
		['id', 'id'],
		['projectId', 'project_id'],
		['releaseVersion', 'release_version'],
		['minifiedPath', 'minified_path'],
		['storageKey', 'storage_key'],
		['sizeBytes', 'size_bytes'],
		['sha256', 'sha256'],
		['uploadedAt', 'uploaded_at'],
	] as const)('sourcemaps.%s is NOT NULL (mapped to %s)', (key, column) => {
		const col = columns[key];
		expect(col).toBeDefined();
		expect(col.name).toBe(column);
		expect(col.notNull).toBe(true);
	});

	it('id is the only primary key, defaulted; uploadedAt is defaulted; nothing else has a default', () => {
		expect(columns.id.primary).toBe(true);
		expect(columns.id.hasDefault).toBe(true);
		expect(columns.uploadedAt.hasDefault).toBe(true);
		expect(config.primaryKeys).toHaveLength(0);
		for (const key of ['projectId', 'releaseVersion', 'minifiedPath', 'storageKey', 'sizeBytes', 'sha256'] as const) {
			expect(columns[key].primary, key).toBe(false);
			expect(columns[key].hasDefault, key).toBe(false);
			expect(columns[key].default, key).toBeUndefined();
		}
	});

	it('the string bounds are the ingest path\'s: release_version 100, minified_path 512, storage_key 1024, sha256 64', () => {
		expect(columns.releaseVersion.getSQLType()).toBe('varchar(100)');
		expect(columns.minifiedPath.getSQLType()).toBe('varchar(512)');
		expect(columns.storageKey.getSQLType()).toBe('varchar(1024)');
		expect(columns.sha256.getSQLType()).toBe('varchar(64)');
		expect(columns.sizeBytes.getSQLType()).toBe('bigint');
		expect(columns.uploadedAt.getSQLType()).toBe('timestamp with time zone');
	});

	it('UNIQUE (project_id, release_version, minified_path) is the identity, storage_key is UNIQUE, and (project_id, release_version) is indexed', () => {
		const byName = new Map(config.indexes.map((i) => [i.config.name, i.config]));
		expect([...byName.keys()].sort()).toEqual([
			'idx_sourcemaps_project_release',
			'idx_sourcemaps_storage_key',
			'sourcemaps_project_id_release_version_minified_path_key',
		]);

		const identity = byName.get('sourcemaps_project_id_release_version_minified_path_key')!;
		expect(identity.unique).toBe(true);
		expect(identity.columns.map((c) => (c as { name: string }).name)).toEqual([
			'project_id',
			'release_version',
			'minified_path',
		]);

		const storageKey = byName.get('idx_sourcemaps_storage_key')!;
		expect(storageKey.unique).toBe(true);
		expect(storageKey.columns.map((c) => (c as { name: string }).name)).toEqual(['storage_key']);

		const projectRelease = byName.get('idx_sourcemaps_project_release')!;
		expect(projectRelease.unique).toBe(false);
		expect(projectRelease.columns.map((c) => (c as { name: string }).name)).toEqual(['project_id', 'release_version']);
	});

	it('the only foreign key is project_id -> projects.id ON DELETE CASCADE (no FK to releases)', () => {
		expect(config.foreignKeys).toHaveLength(1);
		const fk = config.foreignKeys[0];
		expect(fk.onDelete).toBe('cascade');
		const ref = fk.reference();
		expect(ref.columns.map((c) => c.name)).toEqual(['project_id']);
		expect(getTableConfig(ref.foreignTable).name).toBe('projects');
		expect(ref.foreignColumns.map((c) => c.name)).toEqual(['id']);
	});
});
