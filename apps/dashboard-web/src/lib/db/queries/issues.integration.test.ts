import { describe, it, expect, beforeAll, afterAll } from 'vitest';
import postgres from 'postgres';
import { randomUUID } from 'node:crypto';

// D02 — REPRODUCED against a live Postgres before the fix:
//   select id from issues where id ilike '%abc%'
//   -> ERROR: operator does not exist: uuid ~~* unknown
//
// Every search with query.length >= 2 threw a 500, and search is the ONLY way to pick a link
// target for the relations UI — so the whole link/duplicate flow was unusable. A mock-chained
// Drizzle unit test cannot catch this: the mock has no notion of Postgres operator typing, which is
// exactly what let the bug ship. This test runs the REAL query against a REAL Postgres.
//
// This file SEEDS ITS OWN organization/project/issue and deletes them again. It used to instead
// query for "any issue row that happens to exist" and assert one was found — which passed locally
// against a dev database full of e2e leftovers and FAILED in CI, where the `dashboard` job stands
// up a freshly-migrated, EMPTY Postgres. A test whose result depends on ambient data is not a
// guard. Seeding also lets the assertions be scoped to our own organization, so unrelated rows can
// neither satisfy nor mask them.
//
// If nothing answers on the connection the suite is skipped (decided via top-level await, before
// `describe` runs) rather than failed, so a machine with no Postgres does not see a spurious
// failure. Set SEARCH_INTEGRATION_REQUIRED=1 to turn "no reachable database" into a hard failure —
// mirroring SCHEMA_DRIFT_REQUIRED, so CI cannot silently skip the one test that actually executes
// this SQL.
const connectionString =
	process.env.DATABASE_URL ?? 'postgres://sentinel:changeme@localhost:5432/sentinel';
const required = process.env.SEARCH_INTEGRATION_REQUIRED === '1';

let dbReachable = false;
const probeSql = postgres(connectionString, { connect_timeout: 3, max: 1 });
try {
	await probeSql`select 1`;
	dbReachable = true;
} catch {
	dbReachable = false;
} finally {
	await probeSql.end({ timeout: 1 }).catch(() => {});
}

if (required && !dbReachable) {
	throw new Error(
		`SEARCH_INTEGRATION_REQUIRED=1 but no database answered at ${connectionString}. ` +
			'This test is the only coverage that runs searchIssuesInOrg against real Postgres; ' +
			'skipping it silently would leave the D02 uuid-cast regression unguarded.'
	);
}

// Unique per run so two concurrent runs against one shared database cannot collide.
const suffix = randomUUID().slice(0, 8);
const orgId = randomUUID();
const projectId = randomUUID();
const issueId = randomUUID();

describe.skipIf(!dbReachable)('searchIssuesInOrg (integration, real Postgres)', () => {
	let sql: ReturnType<typeof postgres>;

	beforeAll(async () => {
		sql = postgres(connectionString, { connect_timeout: 5, max: 1 });
		await sql`
			insert into organizations (id, name, slug)
			values (${orgId}, ${'search-it-' + suffix}, ${'search-it-' + suffix})
		`;
		await sql`
			insert into projects (id, name, organization_id, api_key, api_key_hash)
			values (${projectId}, ${'search-it-' + suffix}, ${orgId},
			        ${'k_' + suffix}, ${'h_' + suffix})
		`;
		await sql`
			insert into issues (id, project_id, fingerprint, message, error_class)
			values (${issueId}, ${projectId}, ${'fp-' + suffix},
			        ${'search integration seed'}, ${'SearchIntegrationSeedError'})
		`;
	});

	afterAll(async () => {
		if (!sql) return;
		// Ordered by FK dependency. A cleanup failure must not mask a test failure, but it must also
		// not leave rows behind in a shared dev database.
		try {
			await sql`delete from issues where id = ${issueId}`;
			await sql`delete from projects where id = ${projectId}`;
			await sql`delete from organizations where id = ${orgId}`;
		} finally {
			await sql.end({ timeout: 5 }).catch(() => {});
		}
	});

	it('finds a seeded issue by a substring of its UUID id, without throwing', async () => {
		const { searchIssuesInOrg } = await import('./issues');

		const idSubstring = issueId.slice(4, 12); // an arbitrary middle chunk of the UUID

		// Before the D02 fix this line throws:
		//   error: operator does not exist: uuid ~~* unknown
		const results = await searchIssuesInOrg(orgId, idSubstring);

		expect(Array.isArray(results)).toBe(true);
		expect(results.some((r) => r.id === issueId)).toBe(true);
	});

	it('matches on message text too, and stays scoped to its own organization', async () => {
		const { searchIssuesInOrg } = await import('./issues');

		const results = await searchIssuesInOrg(orgId, 'SearchIntegrationSeedError');
		expect(results.some((r) => r.id === issueId)).toBe(true);

		// A different organization must not see this issue. With a seeded row this can be asserted
		// positively, rather than hoping ambient data happens to prove it.
		const otherOrgResults = await searchIssuesInOrg(randomUUID(), 'SearchIntegrationSeedError');
		expect(otherOrgResults.some((r) => r.id === issueId)).toBe(false);
	});
});
