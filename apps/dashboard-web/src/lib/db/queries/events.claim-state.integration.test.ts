import { describe, it, expect, beforeAll, afterAll } from 'vitest';
import postgres from 'postgres';
import { randomUUID } from 'node:crypto';

// N9 (AGENT_WORKER_PLAN contract correction C2): the events feed's embedded issue object must
// carry CURRENT claim/waiting state (assigneeType/assignedTo/claimedAt/waitingOn) so an agent
// dispatcher can evaluate "claimed by me" per-event without a follow-up GET /api/agent/issues/:id.
// This is the committed proof that the projection actually reflects a real claim: it CREATES the
// state it asserts on -- seed an issue + activity row, claim the issue through the SAME query-layer
// function the API route calls (claimIssue), set waitingOn, then read the feed via listOrgActivity
// and assert the embedded issue reflects the post-claim state -- against a REAL, freshly migrated
// Postgres.
//
// Same disposable-database contract as reports.e2e-flow.integration.test.ts: point DATABASE_URL at a
// throwaway Postgres, never the shared dev database. If nothing answers, the suite skips (set
// N9_FLOW_INTEGRATION_REQUIRED=1 to make "no reachable database" a hard failure instead).
const connectionString =
	process.env.DATABASE_URL ?? 'postgres://sentinel:changeme@localhost:5432/sentinel';
const required = process.env.N9_FLOW_INTEGRATION_REQUIRED === '1';

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
		`N9_FLOW_INTEGRATION_REQUIRED=1 but no database answered at ${connectionString}. ` +
			'This test is the committed proof that the events feed exposes current claim state; ' +
			'skipping it silently would leave that claim unverified.'
	);
}

const suffix = randomUUID().slice(0, 8);
const orgId = randomUUID();
const projectId = randomUUID();
const agentId = randomUUID();

describe.skipIf(!dbReachable)('events feed exposes current claim state (integration, real Postgres)', () => {
	let sql: ReturnType<typeof postgres>;
	let issueId: string;

	beforeAll(async () => {
		sql = postgres(connectionString, { connect_timeout: 5, max: 1 });

		await sql`
			insert into organizations (id, name, slug)
			values (${orgId}, ${'n9-flow-' + suffix}, ${'n9-flow-' + suffix})
		`;
		await sql`
			insert into projects (id, name, organization_id, api_key, api_key_hash)
			values (${projectId}, ${'n9-flow-' + suffix}, ${orgId}, ${'k_' + suffix}, ${'h_' + suffix})
		`;
		const [issueRow] = await sql`
			insert into issues (project_id, fingerprint, message, error_class, issue_type)
			values (${projectId}, ${'fp_' + suffix}, ${'Something broke ' + suffix}, ${'TypeError'}, ${'system_error'})
			returning id
		`;
		issueId = issueRow.id as string;
	});

	afterAll(async () => {
		if (!sql) return;
		try {
			await sql`delete from issue_activity where issue_id = ${issueId}`;
			await sql`delete from issues where project_id = ${projectId}`;
			await sql`delete from projects where organization_id = ${orgId}`;
			await sql`delete from organizations where id = ${orgId}`;
		} finally {
			await sql.end({ timeout: 5 }).catch(() => {});
		}
	});

	it('projects assigneeType/assignedTo/claimedAt/waitingOn from the CURRENT issue row', async () => {
		const { claimIssue } = await import('./reports');
		const { listOrgActivity } = await import('./events');

		// Seed an activity row so the feed has something to return for this issue. Backdate it past
		// the 2s lag guard so listOrgActivity's `created_at < now() - interval '2 seconds'` includes
		// it immediately rather than making the test sleep.
		await sql`
			insert into issue_activity (issue_id, event_type, actor_type, actor_id, created_at)
			values (${issueId}, ${'commented'}, ${'agent'}, ${agentId}, now() - interval '10 seconds')
		`;

		// Before the claim: the embedded issue reports no claim.
		const before = await listOrgActivity({ organizationId: orgId, after: 0, limit: 50 });
		const beforeEvent = before.events.find((e) => e.issue.id === issueId);
		expect(beforeEvent).toBeTruthy();
		expect(beforeEvent!.issue.assigneeType).toBeNull();
		expect(beforeEvent!.issue.assignedTo).toBeNull();
		expect(beforeEvent!.issue.claimedAt).toBeNull();
		expect(beforeEvent!.issue.waitingOn).toBeNull();

		// CLAIM through the same query-layer function the API route calls, then set waitingOn.
		await claimIssue(issueId, 'agent', agentId);
		await sql`update issues set waiting_on = ${'reporter'} where id = ${issueId}`;

		// After the claim: the SAME feed row now reflects the current claim/waiting state, even
		// though the activity row itself predates the claim (fields are current-at-read-time).
		const after = await listOrgActivity({ organizationId: orgId, after: 0, limit: 50 });
		const afterEvent = after.events.find((e) => e.issue.id === issueId);
		expect(afterEvent).toBeTruthy();
		expect(afterEvent!.issue.assigneeType).toBe('agent');
		expect(afterEvent!.issue.assignedTo).toBe(agentId);
		expect(afterEvent!.issue.claimedAt).toBeInstanceOf(Date);
		expect(afterEvent!.issue.waitingOn).toBe('reporter');
	});
});
