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
//
// sentinel-releases-snooze §11 adds a second suite to this file -- updateIssueStatus /
// batchUpdateIssues against the same real Postgres -- because every one of its §4.2 properties is
// a database property: three CHECK constraints (check_snooze_only_when_ignored, check_snooze_pair,
// check_snooze_target_positive) refuse a write path that forgets to null a column, and a Drizzle
// mock has no notion of a CHECK. That suite is additionally gated by SCHEMA_DRIFT_REQUIRED=1 (CI
// sets both), and it needs the 1724900000 migration applied.
const connectionString =
	process.env.DATABASE_URL ?? 'postgres://sentinel:changeme@localhost:5432/sentinel';
const required =
	process.env.SEARCH_INTEGRATION_REQUIRED === '1' || process.env.SCHEMA_DRIFT_REQUIRED === '1';

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
			insert into issues (id, project_id, environment, fingerprint, message, error_class)
			values (${issueId}, ${projectId}, ${'production'}, ${'fp-' + suffix},
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

// ---------------------------------------------------------------------------------------------
// sentinel-releases-snooze §4.2 / §11: updateIssueStatus and batchUpdateIssues on real Postgres.
//
// Why here and not in issues.test.ts's mock: the three migrated CHECKs (§4.5) are tripwires for a
// write path that forgot to null a column, and the test proves each constraint exists and then
// proves the write paths by NOT hitting it -- a resolve, a reopen and a batch resolve/unresolve of
// a snoozed issue all succeed only because the same UPDATE nulls the snooze. A mock that returns
// itself for every call cannot tell that apart from "did nothing".
//
// Seeds its own organization / project / issues (unique per run) and deletes them again;
// issue_activity and notifications cascade from issues.
// ---------------------------------------------------------------------------------------------
describe.skipIf(!dbReachable)('updateIssueStatus / batchUpdateIssues (sentinel-releases-snooze §4.2, real Postgres)', () => {
	let sql: ReturnType<typeof postgres>;
	const orgId2 = randomUUID();
	const projectId2 = randomUUID();
	const seeded: string[] = [];

	const FUTURE_Z = '2099-09-20T09:00:00Z';
	const FUTURE_OFFSET = '2099-09-20T16:00:00+07:00'; // the same instant as FUTURE_Z
	const LATER_Z = '2099-10-01T00:00:00Z';

	type Row = {
		status: string;
		count: number;
		resolved_in_version: string | null;
		resolved_at: Date | null;
		resolved_by_type: string | null;
		resolved_by: string | null;
		snoozed_until: Date | null;
		snooze_occurrence_target: number | null;
		snooze_count_at: number | null;
	};

	async function seedIssue(opts: { count?: number; status?: string } = {}): Promise<string> {
		const id = randomUUID();
		await sql`
			insert into issues (id, project_id, environment, fingerprint, message, error_class, status, count)
			values (${id}, ${projectId2}, ${'production'}, ${'fp-' + id.slice(0, 8)},
			        ${'snooze integration seed'}, ${'SnoozeSeedError'}, ${opts.status ?? 'unresolved'},
			        ${opts.count ?? 1})
		`;
		seeded.push(id);
		return id;
	}

	async function readIssue(id: string): Promise<Row> {
		const [row] = await sql<Row[]>`
			select status, count::int as count, resolved_in_version, resolved_at, resolved_by_type,
			       resolved_by, snoozed_until, snooze_occurrence_target::int as snooze_occurrence_target,
			       snooze_count_at::int as snooze_count_at
			from issues where id = ${id}
		`;
		return row;
	}

	async function lastActivityNewValue(id: string): Promise<Record<string, unknown>> {
		const [row] = await sql<{ new_value: Record<string, unknown> }[]>`
			select new_value from issue_activity
			where issue_id = ${id} and event_type = 'status_changed'
			order by created_at desc limit 1
		`;
		return row.new_value;
	}

	const expectNoSnooze = (row: Row) => {
		expect(row.snoozed_until).toBeNull();
		expect(row.snooze_occurrence_target).toBeNull();
		expect(row.snooze_count_at).toBeNull();
	};
	const expectNoResolve = (row: Row) => {
		expect(row.resolved_in_version).toBeNull();
		expect(row.resolved_at).toBeNull();
		expect(row.resolved_by_type).toBeNull();
		expect(row.resolved_by).toBeNull();
	};

	beforeAll(async () => {
		sql = postgres(connectionString, { connect_timeout: 5, max: 1 });
		await sql`
			insert into organizations (id, name, slug)
			values (${orgId2}, ${'snooze-it-' + suffix}, ${'snooze-it-' + suffix})
		`;
		await sql`
			insert into projects (id, name, organization_id, api_key, api_key_hash)
			values (${projectId2}, ${'snooze-it-' + suffix}, ${orgId2},
			        ${'k2_' + suffix}, ${'h2_' + suffix})
		`;
	});

	afterAll(async () => {
		if (!sql) return;
		try {
			if (seeded.length > 0) {
				await sql`delete from issues where id in ${sql(seeded)}`;
			}
			await sql`delete from projects where id = ${projectId2}`;
			await sql`delete from organizations where id = ${orgId2}`;
		} finally {
			await sql.end({ timeout: 5 }).catch(() => {});
		}
	});

	// The tripwires must EXIST for the cases below to mean anything: a nulling test against a
	// database without check_snooze_only_when_ignored passes for the wrong reason.
	it('the three §4.5 CHECKs exist and are validated on `issues`', async () => {
		const rows = await sql<{ conname: string; convalidated: boolean }[]>`
			select conname, convalidated from pg_constraint
			where conrelid = 'issues'::regclass
			  and conname in ('check_snooze_only_when_ignored', 'check_snooze_pair', 'check_snooze_target_positive')
			order by conname
		`;
		expect(rows.map((r) => r.conname)).toEqual([
			'check_snooze_only_when_ignored',
			'check_snooze_pair',
			'check_snooze_target_positive',
		]);
		expect(rows.every((r) => r.convalidated)).toBe(true);
	});

	it('ignored + snoozeOccurrences 100 stores target = 100 and count_at = count; snoozed_until stays NULL', async () => {
		const { updateIssueStatus } = await import('./issues');
		const id = await seedIssue({ count: 37 });

		const result = await updateIssueStatus(id, 'ignored', undefined, 'user', 'user-1', { snoozeOccurrences: 100 });
		expect(result.changed).toBe(true);

		const row = await readIssue(id);
		expect(row.status).toBe('ignored');
		expect(row.snooze_occurrence_target).toBe(100);
		expect(row.snooze_count_at).toBe(37);
		expect(row.snoozed_until).toBeNull();
		expectNoResolve(row);

		// The activity payload carries the key ONLY because it was present (decision 40), and not
		// the other one.
		const newValue = await lastActivityNewValue(id);
		expect(newValue.status).toBe('ignored');
		expect(newValue.snoozeOccurrences).toBe(100);
		expect(newValue).not.toHaveProperty('snoozedUntil');
	});

	it('ignored with neither key stores NULLs -- the indefinite ignore, exactly today\'s write, and ends a running snooze', async () => {
		const { updateIssueStatus } = await import('./issues');
		const id = await seedIssue({ count: 5 });

		// Fresh issue: the pre-feature payload is byte-identical (no snooze keys at all).
		await updateIssueStatus(id, 'ignored', undefined, 'user', 'user-1', {});
		let row = await readIssue(id);
		expect(row.status).toBe('ignored');
		expectNoSnooze(row);
		expect(await lastActivityNewValue(id)).toEqual({ status: 'ignored' });

		// A running snooze, then a plain ignore: both conditions cleared.
		await updateIssueStatus(id, 'ignored', undefined, 'user', 'user-1', {
			snoozedUntil: new Date(FUTURE_Z),
			snoozeOccurrences: 3,
		});
		row = await readIssue(id);
		expect(row.snooze_occurrence_target).toBe(3);
		await updateIssueStatus(id, 'ignored', undefined, 'user', 'user-1', {});
		row = await readIssue(id);
		expect(row.status).toBe('ignored');
		expectNoSnooze(row);
	});

	it('resolved + `next` stores `next`, stamps the clock and nulls the snooze (check_snooze_only_when_ignored not tripped)', async () => {
		const { updateIssueStatus } = await import('./issues');
		const id = await seedIssue({ count: 10 });
		await updateIssueStatus(id, 'ignored', undefined, 'user', 'user-1', {
			snoozedUntil: new Date(FUTURE_Z),
			snoozeOccurrences: 100,
		});
		expect((await readIssue(id)).snooze_occurrence_target).toBe(100);

		const before = Date.now();
		const result = await updateIssueStatus(id, 'resolved', 'next', 'user', 'user-1', {});
		expect(result.changed).toBe(true);

		const row = await readIssue(id);
		expect(row.status).toBe('resolved');
		expect(row.resolved_in_version).toBe('next');
		expect(row.resolved_at).not.toBeNull();
		expect(Math.abs(row.resolved_at!.getTime() - before)).toBeLessThan(60_000);
		expect(row.resolved_by_type).toBe('user');
		expect(row.resolved_by).toBe('user-1');
		expectNoSnooze(row);
		expect(await lastActivityNewValue(id)).toEqual({ status: 'resolved', resolvedInVersion: 'next' });
	});

	it('unresolved nulls all seven columns', async () => {
		const { updateIssueStatus } = await import('./issues');
		// Seeded directly into the shape decision 44 says exists: `ignored` with a running snooze AND
		// a stale set of resolve columns (the CHECKs allow it -- only snooze-on-non-ignored is refused).
		const id = await seedIssue({ count: 8 });
		await sql`
			update issues set status = 'ignored', snoozed_until = ${FUTURE_Z}::timestamptz,
			       snooze_occurrence_target = 50, snooze_count_at = 8,
			       resolved_in_version = 'v1.0.0', resolved_at = now(), resolved_by_type = 'agent',
			       resolved_by = 'agent-9'
			where id = ${id}
		`;

		const result = await updateIssueStatus(id, 'unresolved', undefined, 'user', 'user-1', {});
		expect(result.changed).toBe(true);

		const row = await readIssue(id);
		expect(row.status).toBe('unresolved');
		expectNoResolve(row);
		expectNoSnooze(row);
	});

	// Flow A5 / decision 39: the widened no-op guard and the count_at carry-over.
	it('A5: changed:false only on all-equal; a new N re-stamps count_at; the same N with a new date keeps it', async () => {
		const { updateIssueStatus } = await import('./issues');
		const id = await seedIssue({ count: 37 });

		// 1st: a real change.
		expect((await updateIssueStatus(id, 'ignored', undefined, 'user', 'user-1', { snoozeOccurrences: 100 })).changed).toBe(true);
		let row = await readIssue(id);
		expect(row.snooze_occurrence_target).toBe(100);
		expect(row.snooze_count_at).toBe(37);

		// 2nd: the identical call is the no-op -- no second activity row.
		const activityBefore = await sql<{ n: number }[]>`select count(*)::int as n from issue_activity where issue_id = ${id}`;
		expect((await updateIssueStatus(id, 'ignored', undefined, 'user', 'user-1', { snoozeOccurrences: 100 })).changed).toBe(false);
		const activityAfter = await sql<{ n: number }[]>`select count(*)::int as n from issue_activity where issue_id = ${id}`;
		expect(activityAfter[0].n).toBe(activityBefore[0].n);

		// Occurrences arrive in between (the processor's write, simulated).
		await sql`update issues set count = 60 where id = ${id}`;

		// 3rd: a DIFFERENT N is a real change and re-stamps count_at from the current count.
		expect((await updateIssueStatus(id, 'ignored', undefined, 'user', 'user-1', { snoozeOccurrences: 200 })).changed).toBe(true);
		row = await readIssue(id);
		expect(row.snooze_occurrence_target).toBe(200);
		expect(row.snooze_count_at).toBe(60);

		await sql`update issues set count = 75 where id = ${id}`;

		// 4th: the SAME N plus a new date is a real change that leaves count_at alone (carried over).
		expect(
			(
				await updateIssueStatus(id, 'ignored', undefined, 'user', 'user-1', {
					snoozeOccurrences: 200,
					snoozedUntil: new Date(FUTURE_Z),
				})
			).changed
		).toBe(true);
		row = await readIssue(id);
		expect(row.snooze_occurrence_target).toBe(200);
		expect(row.snooze_count_at).toBe(60);
		expect(row.snoozed_until!.getTime()).toBe(Date.parse(FUTURE_Z));

		// 5th: the same instant written with an offset is all-equal -- compared as instants, not
		// as strings (be.md task 6).
		expect(
			(
				await updateIssueStatus(id, 'ignored', undefined, 'user', 'user-1', {
					snoozeOccurrences: 200,
					snoozedUntil: new Date(FUTURE_OFFSET),
				})
			).changed
		).toBe(false);

		// 6th: a different date alone is a change; count_at still carried over.
		expect(
			(
				await updateIssueStatus(id, 'ignored', undefined, 'user', 'user-1', {
					snoozeOccurrences: 200,
					snoozedUntil: new Date(LATER_Z),
				})
			).changed
		).toBe(true);
		row = await readIssue(id);
		expect(row.snooze_count_at).toBe(60);
		expect(row.snoozed_until!.getTime()).toBe(Date.parse(LATER_Z));

		// 7th: dropping the target with the date kept is a change; count_at is NULL with it
		// (check_snooze_pair) and snoozed_until stays.
		expect(
			(await updateIssueStatus(id, 'ignored', undefined, 'user', 'user-1', { snoozedUntil: new Date(LATER_Z) })).changed
		).toBe(true);
		row = await readIssue(id);
		expect(row.snooze_occurrence_target).toBeNull();
		expect(row.snooze_count_at).toBeNull();
		expect(row.snoozed_until!.getTime()).toBe(Date.parse(LATER_Z));
	});

	// Decision 40: `new_value.snoozedUntil` is the normalised UTC instant with `Z` and milliseconds,
	// whatever offset the request carried, and equals the stored instant.
	it('A11 / decision 40: a +07:00 input lands in new_value.snoozedUntil as the Z millisecond string', async () => {
		const { updateIssueStatus } = await import('./issues');
		const id = await seedIssue();

		await updateIssueStatus(id, 'ignored', undefined, 'agent', 'agent-1', { snoozedUntil: new Date(FUTURE_OFFSET) });

		const row = await readIssue(id);
		expect(row.snoozed_until!.getTime()).toBe(Date.parse(FUTURE_Z));
		expectNoResolve(row);
		expect(row.snooze_occurrence_target).toBeNull();
		expect(row.snooze_count_at).toBeNull();

		const newValue = await lastActivityNewValue(id);
		expect(newValue.snoozedUntil).toBe('2099-09-20T09:00:00.000Z');
		expect(String(newValue.snoozedUntil).endsWith('Z')).toBe(true);
		expect(Date.parse(String(newValue.snoozedUntil))).toBe(row.snoozed_until!.getTime());
		expect(newValue).not.toHaveProperty('snoozeOccurrences');
	});

	// Decision 27 / flows A8, A10: the batch resolve stamps the clock and the actor and nulls the
	// snooze; the batch unresolve nulls both sets.
	it('A8/A10: batch resolve with `next` stamps resolved_at (≈ now) and resolved_by*, nulls the snooze; unresolve nulls them', async () => {
		const { updateIssueStatus, batchUpdateIssues } = await import('./issues');
		const snoozed = await seedIssue({ count: 12 });
		const plain = await seedIssue();
		await updateIssueStatus(snoozed, 'ignored', undefined, 'user', 'user-1', {
			snoozedUntil: new Date(FUTURE_Z),
			snoozeOccurrences: 100,
		});
		expect((await readIssue(snoozed)).snooze_count_at).toBe(12);

		const before = Date.now();
		const updated = await batchUpdateIssues(projectId2, 'resolve', [snoozed, plain], {
			resolvedInVersion: 'next',
			actorType: 'user',
			actorId: 'user-7',
			environmentScope: null,
		});
		expect(updated).toBe(2);

		for (const id of [snoozed, plain]) {
			const row = await readIssue(id);
			expect(row.status).toBe('resolved');
			expect(row.resolved_in_version).toBe('next');
			expect(row.resolved_at).not.toBeNull();
			expect(Math.abs(row.resolved_at!.getTime() - before)).toBeLessThan(60_000);
			expect(row.resolved_by_type).toBe('user');
			expect(row.resolved_by).toBe('user-7');
			expectNoSnooze(row);
		}

		// A8's other shape: a typed version on a snoozed issue -- same nulling, same stamping.
		const snoozed2 = await seedIssue({ count: 3 });
		await updateIssueStatus(snoozed2, 'ignored', undefined, 'user', 'user-1', { snoozeOccurrences: 5 });
		expect(
			await batchUpdateIssues(projectId2, 'resolve', [snoozed2], {
				resolvedInVersion: 'v2',
				actorType: 'agent',
				actorId: 'agent-3',
				environmentScope: null,
			})
		).toBe(1);
		const row2 = await readIssue(snoozed2);
		expect(row2.resolved_in_version).toBe('v2');
		expect(row2.resolved_at).not.toBeNull();
		expect(row2.resolved_by_type).toBe('agent');
		expect(row2.resolved_by).toBe('agent-3');
		expectNoSnooze(row2);

		// unresolve: the resolve columns AND the snooze columns are nulled.
		const snoozed3 = await seedIssue({ count: 3 });
		await updateIssueStatus(snoozed3, 'ignored', undefined, 'user', 'user-1', { snoozedUntil: new Date(FUTURE_Z) });
		expect(
			await batchUpdateIssues(projectId2, 'unresolve', [snoozed, snoozed3], {
				actorType: 'user',
				actorId: 'user-7',
				environmentScope: null,
			})
		).toBe(2);
		for (const id of [snoozed, snoozed3]) {
			const row = await readIssue(id);
			expect(row.status).toBe('unresolved');
			expectNoResolve(row);
			expectNoSnooze(row);
		}
	});

	// Decision 44 / flow A12: the batch ignore arm is untouched -- a snoozed issue keeps its snooze,
	// an unresolved one becomes the indefinite ignore.
	it('A12: batch ignore of a snoozed issue leaves all three snooze columns as they were', async () => {
		const { updateIssueStatus, batchUpdateIssues } = await import('./issues');
		const snoozed = await seedIssue({ count: 20 });
		const plain = await seedIssue();
		await updateIssueStatus(snoozed, 'ignored', undefined, 'user', 'user-1', {
			snoozedUntil: new Date(FUTURE_Z),
			snoozeOccurrences: 100,
		});
		const beforeRow = await readIssue(snoozed);
		expect(beforeRow.snooze_count_at).toBe(20);

		expect(
			await batchUpdateIssues(projectId2, 'ignore', [snoozed, plain], {
				actorType: 'user',
				actorId: 'user-7',
				environmentScope: null,
			})
		).toBe(2);

		const afterRow = await readIssue(snoozed);
		expect(afterRow.status).toBe('ignored');
		expect(afterRow.snoozed_until!.getTime()).toBe(beforeRow.snoozed_until!.getTime());
		expect(afterRow.snooze_occurrence_target).toBe(100);
		expect(afterRow.snooze_count_at).toBe(20);

		const plainRow = await readIssue(plain);
		expect(plainRow.status).toBe('ignored');
		expectNoSnooze(plainRow);
	});
});
