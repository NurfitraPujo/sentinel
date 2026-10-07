import { describe, it, expect, beforeAll, afterAll } from 'vitest';
import postgres from 'postgres';
import { randomUUID } from 'node:crypto';

// sentinel-agent-claim-next-issue: `claimNextAgentIssue` is one UPDATE whose candidate comes from a
// `SELECT ... FOR UPDATE SKIP LOCKED` subquery, and its whole point is what happens under
// concurrency -- which a chainable-mock test cannot exercise (a mock that returns itself for
// every call cannot tell "picked a distinct row" from "picked the same row twice", B10). So this
// is the committed proof, against a REAL Postgres: it CREATES the state it asserts on (an org,
// a project, a queue of issues), fires N claim-next calls in parallel for N distinct agents, and
// asserts every one of them got a DIFFERENT issue and the loser of an empty queue got null.
//
// Same disposable-database contract as reports.e2e-flow.integration.test.ts and
// events.claim-state.integration.test.ts: point DATABASE_URL at a throwaway Postgres, never the
// shared dev database. If nothing answers, the suite skips (set CLAIM_NEXT_INTEGRATION_REQUIRED=1
// to make "no reachable database" a hard failure instead).
const connectionString =
	process.env.DATABASE_URL ?? 'postgres://sentinel:changeme@localhost:5432/sentinel';
const required = process.env.CLAIM_NEXT_INTEGRATION_REQUIRED === '1';

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
		`CLAIM_NEXT_INTEGRATION_REQUIRED=1 but no database answered at ${connectionString}. ` +
			'This test is the committed proof that claim-next is atomic under concurrency; ' +
			'skipping it silently would leave that claim unverified.'
	);
}

const suffix = randomUUID().slice(0, 8);
const orgId = randomUUID();
const otherOrgId = randomUUID();
const projectId = randomUUID();
const otherOrgProjectId = randomUUID();

describe.skipIf(!dbReachable)('claimNextAgentIssue (integration, real Postgres)', () => {
	let sql: ReturnType<typeof postgres>;
	// Seeded oldest -> newest by last_seen. The queue's default order is last_seen DESC, so the
	// expected claim order is the reverse of this array.
	const queue: string[] = [];
	let resolvedIssueId: string;
	let preClaimedIssueId: string;
	let foreignIssueId: string;

	async function seedIssue(project: string, overrides: { status?: string; assignedTo?: string; lastSeenOffsetSec: number }) {
		const [row] = await sql`
			insert into issues (project_id, environment, fingerprint, message, error_class, issue_type, status,
				assignee_type, assigned_to, last_seen)
			values (${project}, ${'production'}, ${'fp_' + randomUUID().slice(0, 8)}, ${'claim-next ' + suffix},
				${'TypeError'}, ${'system_error'}, ${overrides.status ?? 'unresolved'},
				${overrides.assignedTo ? 'agent' : null}, ${overrides.assignedTo ?? null},
				now() - make_interval(secs => ${overrides.lastSeenOffsetSec}))
			returning id
		`;
		return row.id as string;
	}

	beforeAll(async () => {
		sql = postgres(connectionString, { connect_timeout: 5, max: 10 });

		for (const [id, name] of [
			[orgId, 'claim-next-' + suffix],
			[otherOrgId, 'claim-next-other-' + suffix],
		]) {
			await sql`insert into organizations (id, name, slug) values (${id}, ${name}, ${name})`;
		}
		await sql`
			insert into projects (id, name, organization_id, api_key, api_key_hash)
			values (${projectId}, ${'claim-next-' + suffix}, ${orgId}, ${'k_' + suffix}, ${'h_' + suffix})
		`;
		await sql`
			insert into projects (id, name, organization_id, api_key, api_key_hash)
			values (${otherOrgProjectId}, ${'claim-next-other-' + suffix}, ${otherOrgId}, ${'ko_' + suffix}, ${'ho_' + suffix})
		`;

		// Four eligible issues, distinct last_seen so the order is fully determined.
		for (let i = 0; i < 4; i++) {
			queue.push(await seedIssue(projectId, { lastSeenOffsetSec: 400 - i * 100 }));
		}
		// Ineligible rows that would sort FIRST if the predicates were missing: a resolved one,
		// one already claimed by someone else, and one in another org.
		resolvedIssueId = await seedIssue(projectId, { status: 'resolved', lastSeenOffsetSec: 1 });
		preClaimedIssueId = await seedIssue(projectId, { assignedTo: 'someone-else', lastSeenOffsetSec: 2 });
		foreignIssueId = await seedIssue(otherOrgProjectId, { lastSeenOffsetSec: 3 });
	});

	afterAll(async () => {
		if (!sql) return;
		try {
			await sql`delete from issue_activity where issue_id in (select id from issues where project_id in (${projectId}, ${otherOrgProjectId}))`;
			await sql`delete from issue_subscriptions where issue_id in (select id from issues where project_id in (${projectId}, ${otherOrgProjectId}))`;
			await sql`delete from issues where project_id in (${projectId}, ${otherOrgProjectId})`;
			await sql`delete from projects where organization_id in (${orgId}, ${otherOrgId})`;
			await sql`delete from organizations where id in (${orgId}, ${otherOrgId})`;
		} finally {
			await sql.end({ timeout: 5 }).catch(() => {});
		}
	});

	it('hands N concurrent callers N distinct issues, newest last_seen first, then null when drained', async () => {
		const { claimNextAgentIssue } = await import('./agent-work');
		// beforeAll's four, always queue's first four: the other cases push (and claim) their own rows
		// after them, and under --sequence.shuffle (B13) they may already have run.
		const seeded = queue.slice(0, 4);

		const agents = ['agent-a-' + suffix, 'agent-b-' + suffix, 'agent-c-' + suffix, 'agent-d-' + suffix];
		const results = await Promise.all(
			agents.map((agentId) => claimNextAgentIssue({ organizationId: orgId, agentId }))
		);

		// Every caller got an issue, and no two got the same one -- the SKIP LOCKED property.
		const claimedIds = results.map((r) => r?.issue.id);
		expect(claimedIds.every(Boolean)).toBe(true);
		expect(new Set(claimedIds).size).toBe(agents.length);
		expect(new Set(claimedIds)).toEqual(new Set(seeded));

		// Each row records the caller that won it, as an agent claim.
		for (let i = 0; i < agents.length; i++) {
			expect(results[i]!.issue.assignedTo).toBe(agents[i]);
			expect(results[i]!.issue.assigneeType).toBe('agent');
			expect(results[i]!.issue.claimedAt).toBeInstanceOf(Date);
		}

		// The ineligible rows were never touched, even though they sort ahead of the seeded rows.
		expect(claimedIds).not.toContain(resolvedIssueId);
		expect(claimedIds).not.toContain(preClaimedIssueId);
		expect(claimedIds).not.toContain(foreignIssueId);
		const [foreign] = await sql`select assigned_to from issues where id = ${foreignIssueId}`;
		expect(foreign.assigned_to).toBeNull();

		// Queue drained: a further call for the org gets nothing, and nothing changes.
		const drained = await claimNextAgentIssue({ organizationId: orgId, agentId: 'agent-e-' + suffix });
		expect(drained).toBeNull();

		// The claim is recorded exactly as a single-issue claim is: one `claimed` activity row and
		// one claimant subscription per issue, attributed to the winning agent.
		const activity = await sql`
			select issue_id, actor_id from issue_activity
			where event_type = 'claimed' and actor_type = 'agent' and issue_id in ${sql(seeded)}
		`;
		expect(activity.length).toBe(seeded.length);
		for (let i = 0; i < agents.length; i++) {
			expect(activity.find((a) => a.issue_id === claimedIds[i])?.actor_id).toBe(agents[i]);
		}
		const subs = await sql`
			select count(*)::int as n from issue_subscriptions
			where subscriber_type = 'agent' and reason = 'claimant' and issue_id in ${sql(seeded)}
		`;
		expect(subs[0].n).toBe(seeded.length);
	});

	it('serial calls walk the queue in last_seen DESC order (the list route\'s order)', async () => {
		const { claimNextAgentIssue } = await import('./agent-work');
		// Fresh queue: two more eligible rows, newest first.
		const older = await seedIssue(projectId, { lastSeenOffsetSec: 50 });
		const newer = await seedIssue(projectId, { lastSeenOffsetSec: 40 });
		queue.push(older, newer);

		const first = await claimNextAgentIssue({ organizationId: orgId, agentId: 'agent-serial-' + suffix });
		const second = await claimNextAgentIssue({ organizationId: orgId, agentId: 'agent-serial-' + suffix });
		expect(first?.issue.id).toBe(newer);
		expect(second?.issue.id).toBe(older);

		// sort: 'firstSeen' is honoured (all rows share first_seen = now(), so id DESC breaks the tie
		// and the point is only that the option is accepted and still returns an eligible row).
		const third = await seedIssue(projectId, { lastSeenOffsetSec: 30 });
		queue.push(third);
		const bySort = await claimNextAgentIssue({ organizationId: orgId, agentId: 'agent-serial-' + suffix, sort: 'firstSeen' });
		expect(bySort?.issue.id).toBe(third);
	});

	it('honours the type / project / environment / source filters (no match -> null, nothing claimed)', async () => {
		const { claimNextAgentIssue } = await import('./agent-work');
		const eligible = await seedIssue(projectId, { lastSeenOffsetSec: 20 });
		queue.push(eligible);

		expect(await claimNextAgentIssue({ organizationId: orgId, agentId: 'f', type: 'user_report' })).toBeNull();
		expect(await claimNextAgentIssue({ organizationId: orgId, agentId: 'f', projectId: randomUUID() })).toBeNull();
		expect(await claimNextAgentIssue({ organizationId: orgId, agentId: 'f', environment: 'staging' })).toBeNull();
		expect(await claimNextAgentIssue({ organizationId: orgId, agentId: 'f', source: 'user_report_api' })).toBeNull();
		const [still] = await sql`select assigned_to from issues where id = ${eligible}`;
		expect(still.assigned_to).toBeNull();

		const matched = await claimNextAgentIssue({
			organizationId: orgId,
			agentId: 'agent-filter-' + suffix,
			type: 'system_error',
			projectId,
			environment: 'production',
			source: 'ingestion_sdk',
		});
		expect(matched?.issue.id).toBe(eligible);
	});
});
