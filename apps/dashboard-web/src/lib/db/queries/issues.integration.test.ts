import { describe, it, expect } from 'vitest';
import postgres from 'postgres';

// D02 — REPRODUCED against live sentinel-postgres before the fix:
//   select id from issues where id ilike '%abc%'
//   -> ERROR: operator does not exist: uuid ~~* unknown
//
// Every search with query.length >= 2 threw a 500, and search is the ONLY way to pick a link
// target for the relations UI — so the whole link/duplicate flow was unusable. A mock-chained
// Drizzle unit test cannot catch this: the mock has no notion of Postgres operator typing, which is
// exactly what let the bug ship. This test runs the REAL query against a REAL Postgres.
//
// This is a read-only query against whatever database DATABASE_URL points at (default:
// postgres://sentinel:changeme@localhost:5432/sentinel, i.e. the sentinel-postgres dev container).
// It performs no writes, so it is safe to run against the shared dev database — unlike
// tests/integration's Go suite, nothing here can corrupt it. If nothing answers on that connection,
// the suite is skipped (via a top-level await, so the decision is made before `describe` runs)
// rather than failed, so this file does not become a spurious CI failure on a machine with no
// Postgres running.
const connectionString =
	process.env.DATABASE_URL ?? 'postgres://sentinel:changeme@localhost:5432/sentinel';

let dbReachable = false;
let seedRow: { id: string; orgId: string } | undefined;

const probeSql = postgres(connectionString, { connect_timeout: 3, max: 1 });
try {
	const [row] = await probeSql<{ id: string; orgId: string }[]>`
		select i.id, p.organization_id as "orgId"
		from issues i
		join projects p on p.id = i.project_id
		limit 1
	`;
	seedRow = row;
	dbReachable = true;
} catch {
	dbReachable = false;
} finally {
	await probeSql.end({ timeout: 1 }).catch(() => {});
}

describe.skipIf(!dbReachable)('searchIssuesInOrg (integration, real Postgres)', () => {
	it('finds an existing issue by a substring of its UUID id, without throwing', async () => {
		expect(seedRow, 'expected at least one issue row in the connected database').toBeTruthy();
		if (!seedRow) return;

		const { searchIssuesInOrg } = await import('./issues');

		const idSubstring = seedRow.id.slice(4, 12); // an arbitrary middle chunk of the UUID

		// Before the D02 fix, this line throws:
		//   error: operator does not exist: uuid ~~* unknown
		const results = await searchIssuesInOrg(seedRow.orgId, idSubstring);

		expect(Array.isArray(results)).toBe(true);
		expect(results.some((r) => r.id === seedRow!.id)).toBe(true);
	});
});
