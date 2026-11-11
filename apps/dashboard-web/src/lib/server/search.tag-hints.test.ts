import { describe, it, expect } from 'vitest';
import { readdirSync, readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';
import { TAG_SEARCH_HINTS, CUSTOM_TAG_HINT_INDEX } from './search';

// sentinel-issue-tag-facets spec §3.2 / §6.4 (TASK-03, "hint drift"): the derived-key list lives in ONE
// place, the migration's issue_tag_derived. Search's index-hint map is the one TypeScript structure keyed
// by derived-key names, an optimisation and never a definition: every entry must name a real derived
// key, and the hints the bound relies on (user.id, device.model, custom tags) must name an index the
// migration builds.

const migrationsDir = join(dirname(fileURLToPath(import.meta.url)), '../../../../../packages/db-migrations/migrations');
const files = readdirSync(migrationsDir).filter((f) => f.endsWith('_add_issue_tag_values.sql'));
const migration = readFileSync(join(migrationsDir, files[0]), 'utf8');
const up = migration.slice(0, migration.indexOf('-- +goose Down'));

function derivedKeys(): string[] {
	const start = up.indexOf('CREATE OR REPLACE FUNCTION issue_tag_derived(');
	const body = up.slice(start, up.indexOf('$$;', start));
	return [...body.matchAll(/\(\s*\d+\s*,\s*'([^']+)'/g)].map((m) => m[1]);
}

describe('the search hint map against issue_tag_derived', () => {
	it('finds exactly one tag-facets migration and seven derived keys in it', () => {
		expect(files).toHaveLength(1);
		expect(derivedKeys()).toEqual(['release', 'environment', 'os.name', 'client_os.name', 'device.model', 'browser.name', 'user.id']);
	});

	it('every hint entry is a derived key', () => {
		const keys = derivedKeys();
		for (const k of Object.keys(TAG_SEARCH_HINTS)) expect(keys, k).toContain(k);
	});

	it('user.id, device.model and custom tags have hints, each on an index the migration builds', () => {
		for (const k of ['user.id', 'device.model']) {
			const index = TAG_SEARCH_HINTS[k]?.index;
			expect(index, `${k} has an index hint`).toBeTruthy();
			expect(up).toContain(`CREATE INDEX CONCURRENTLY IF NOT EXISTS ${index}`);
		}
		expect(up).toContain(`CREATE INDEX CONCURRENTLY IF NOT EXISTS ${CUSTOM_TAG_HINT_INDEX}`);
		expect(TAG_SEARCH_HINTS.release?.index).toBe('idx_occurrences_issue_release');
	});
});
