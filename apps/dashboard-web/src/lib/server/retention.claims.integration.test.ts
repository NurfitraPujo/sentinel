import { describe, it, expect, beforeAll, afterAll } from 'vitest';
import postgres from 'postgres';
import { randomUUID } from 'node:crypto';

// N7c (docs/plans/AGENT_AUTOMATION_REMEDIATION_PLAN.md, A03/A04) -- runs `reapStaleClaims` and
// the A04 manual-issue retention guards against a REAL, migrated Postgres (the compose stack's
// dev database by default). Both features are time-based conditional UPDATE/DELETEs whose
// correctness depends on real `timestamptz` comparisons and a real `issue_activity` table; a
// mock-chained Drizzle unit test cannot distinguish "the WHERE clause is correct" from "the mock
// returned whatever we told it to" (this repo's own retention.attachments.test.ts is exactly that
// style, for a different, non-time-based code path).
//
// This file seeds its own organization/project/issues (and issue_activity rows, with `created_at`
// backdated via raw SQL -- `now()` cannot be controlled any other way against a real Postgres) and
// deletes them again in `afterAll`, scoped by suffix so a shared dev database's ambient rows can
// neither satisfy nor mask the assertions (see issues.integration.test.ts's rationale for the
// same pattern).
//
// Skipped (not failed) unless a Postgres answers; set CLAIM_RETENTION_INTEGRATION_REQUIRED=1 to
// turn "no reachable database" into a hard failure so CI cannot silently skip the only proof that
// A03/A04 hold against real infrastructure.
//
//   cd apps/dashboard-web && \
//   DATABASE_URL=postgres://sentinel:changeme@localhost:5432/sentinel \
//   CLAIM_RETENTION_INTEGRATION_REQUIRED=1 \
//   pnpm vitest run src/lib/server/retention.claims.integration.test.ts

const connectionString =
	process.env.DATABASE_URL ?? 'postgres://sentinel:changeme@localhost:5432/sentinel';
const required = process.env.CLAIM_RETENTION_INTEGRATION_REQUIRED === '1';

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
		`CLAIM_RETENTION_INTEGRATION_REQUIRED=1 but no database answered at ${connectionString}. ` +
			'This test is the only coverage that runs reapStaleClaims and the A04 manual-issue ' +
			'retention guards against real Postgres; skipping it silently would leave A03/A04 unguarded.'
	);
}

const suffix = randomUUID().slice(0, 8);
const orgId = randomUUID();
const projectId = randomUUID();

function hoursAgo(h: number): Date {
	const d = new Date();
	d.setHours(d.getHours() - h);
	return d;
}

function daysAgo(d: number): Date {
	const date = new Date();
	date.setDate(date.getDate() - d);
	return date;
}

describe.skipIf(!dbReachable)('reapStaleClaims + A04 manual retention (integration, real Postgres)', () => {
	let sql: ReturnType<typeof postgres>;

	beforeAll(async () => {
		sql = postgres(connectionString, { connect_timeout: 5, max: 1 });
		await sql`
			insert into organizations (id, name, slug)
			values (${orgId}, ${'claim-retention-it-' + suffix}, ${'claim-retention-it-' + suffix})
		`;
		await sql`
			insert into projects (id, name, organization_id, api_key, api_key_hash)
			values (${projectId}, ${'claim-retention-it-' + suffix}, ${orgId},
			        ${'k_' + suffix}, ${'h_' + suffix})
		`;
	});

	afterAll(async () => {
		if (!sql) return;
		try {
			await sql`delete from issue_activity where issue_id in (select id from issues where project_id = ${projectId})`;
			await sql`delete from manual_issue_reports where issue_id in (select id from issues where project_id = ${projectId})`;
			await sql`delete from issues where project_id = ${projectId}`;
			await sql`delete from projects where id = ${projectId}`;
			await sql`delete from organizations where id = ${orgId}`;
		} finally {
			await sql.end({ timeout: 5 }).catch(() => {});
		}
	});

	async function seedIssue(overrides: {
		fingerprint: string;
		assigneeType?: string | null;
		assignedTo?: string | null;
		claimedAt?: Date | null;
		status?: string;
		issueType?: string;
		firstSeen?: Date;
	}) {
		const id = randomUUID();
		await sql`
			insert into issues (
				id, project_id, fingerprint, message, error_class, status, issue_type,
				assignee_type, assigned_to, claimed_at, first_seen
			) values (
				${id}, ${projectId}, ${overrides.fingerprint}, ${'seed'}, ${'SeedError'},
				${overrides.status ?? 'unresolved'}, ${overrides.issueType ?? 'system_error'},
				${overrides.assigneeType ?? null}, ${overrides.assignedTo ?? null},
				${overrides.claimedAt ?? null}, ${overrides.firstSeen ?? new Date()}
			)
		`;
		return id;
	}

	it('force-releases an agent claim past CLAIM_STALE_HOURS with no recent activity, and logs a system claim_released event', async () => {
		const { reapStaleClaims } = await import('./retention');

		const agentId = 'agent-stale-' + suffix;
		const issueId = await seedIssue({
			fingerprint: 'stale-' + suffix,
			assigneeType: 'agent',
			assignedTo: agentId,
			claimedAt: hoursAgo(48),
		});

		const result = await reapStaleClaims(24);

		expect(result.releasedClaims).toBeGreaterThanOrEqual(1);

		const [issueRow] = await sql`select assigned_to, assignee_type, claimed_at from issues where id = ${issueId}`;
		expect(issueRow.assigned_to).toBeNull();
		expect(issueRow.assignee_type).toBeNull();
		expect(issueRow.claimed_at).toBeNull();

		const events = await sql`
			select actor_type, actor_id, new_value from issue_activity
			where issue_id = ${issueId} and event_type = 'claim_released'
		`;
		expect(events).toHaveLength(1);
		expect(events[0].actor_type).toBe('system');
		expect(events[0].actor_id).toBe('sentinel-claim-reaper');
		expect(events[0].new_value).toMatchObject({ previousAssignee: agentId, reason: 'stale' });
	});

	it('protects a stale-eligible claim when the claimant has recent activity on the issue', async () => {
		const { reapStaleClaims } = await import('./retention');

		const agentId = 'agent-active-' + suffix;
		const issueId = await seedIssue({
			fingerprint: 'active-' + suffix,
			assigneeType: 'agent',
			assignedTo: agentId,
			claimedAt: hoursAgo(48),
		});
		await sql`
			insert into issue_activity (issue_id, event_type, actor_type, actor_id, new_value, created_at)
			values (${issueId}, 'progress_update', 'agent', ${agentId}, ${sql.json({ note: 'still working' })}, ${hoursAgo(1)})
		`;

		await reapStaleClaims(24);

		const [issueRow] = await sql`select assigned_to from issues where id = ${issueId}`;
		expect(issueRow.assigned_to).toBe(agentId);
	});

	it('protects a fresh claim (claimed_at within the window) even with zero activity', async () => {
		const { reapStaleClaims } = await import('./retention');

		const agentId = 'agent-fresh-' + suffix;
		const issueId = await seedIssue({
			fingerprint: 'fresh-' + suffix,
			assigneeType: 'agent',
			assignedTo: agentId,
			claimedAt: hoursAgo(1),
		});

		await reapStaleClaims(24);

		const [issueRow] = await sql`select assigned_to from issues where id = ${issueId}`;
		expect(issueRow.assigned_to).toBe(agentId);
	});

	it('treats a NULL claimed_at (pre-migration claim) as stale-eligible', async () => {
		const { reapStaleClaims } = await import('./retention');

		const agentId = 'agent-null-claimed-at-' + suffix;
		const issueId = await seedIssue({
			fingerprint: 'null-claimed-at-' + suffix,
			assigneeType: 'agent',
			assignedTo: agentId,
			claimedAt: null,
		});

		await reapStaleClaims(24);

		const [issueRow] = await sql`select assigned_to from issues where id = ${issueId}`;
		expect(issueRow.assigned_to).toBeNull();
	});

	it('never touches a human claim, however old', async () => {
		const { reapStaleClaims } = await import('./retention');

		const userId = 'user-old-claim-' + suffix;
		const issueId = await seedIssue({
			fingerprint: 'human-' + suffix,
			assigneeType: 'user',
			assignedTo: userId,
			claimedAt: hoursAgo(1000),
		});

		await reapStaleClaims(24);

		const [issueRow] = await sql`select assigned_to from issues where id = ${issueId}`;
		expect(issueRow.assigned_to).toBe(userId);
	});

	it('A04: an occurrence-less unresolved or claimed issue survives cleanupRetainedData at any age', async () => {
		const { cleanupRetainedData } = await import('./retention');

		const unresolvedId = await seedIssue({
			fingerprint: 'manual-unresolved-' + suffix,
			issueType: 'user_report',
			status: 'unresolved',
			firstSeen: daysAgo(2000),
		});
		const claimedId = await seedIssue({
			fingerprint: 'manual-claimed-' + suffix,
			issueType: 'user_report',
			status: 'resolved',
			assigneeType: 'agent',
			assignedTo: 'agent-holding-claim-' + suffix,
			claimedAt: new Date(),
			firstSeen: daysAgo(2000),
		});

		await cleanupRetainedData(1, 365);

		const survivors = await sql`select id from issues where id in (${unresolvedId}, ${claimedId})`;
		expect(survivors.map((r) => r.id).sort()).toEqual([claimedId, unresolvedId].sort());
	});

	it('A04: a resolved, unclaimed manual issue is deleted only past MANUAL_ISSUE_RETENTION_DAYS', async () => {
		const { cleanupRetainedData } = await import('./retention');

		const recentId = await seedIssue({
			fingerprint: 'manual-recent-' + suffix,
			issueType: 'user_report',
			status: 'resolved',
			firstSeen: daysAgo(10),
		});
		const oldId = await seedIssue({
			fingerprint: 'manual-old-' + suffix,
			issueType: 'user_report',
			status: 'ignored',
			firstSeen: daysAgo(400),
		});

		await cleanupRetainedData(1, 365);

		const recentSurvives = await sql`select id from issues where id = ${recentId}`;
		expect(recentSurvives).toHaveLength(1);

		const oldSurvives = await sql`select id from issues where id = ${oldId}`;
		expect(oldSurvives).toHaveLength(0);
	});
});
