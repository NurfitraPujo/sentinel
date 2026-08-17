import { describe, it, expect, beforeAll, afterAll } from 'vitest';
import postgres from 'postgres';
import { randomUUID } from 'node:crypto';

// N8 (docs/audits/AGENT_AUTOMATION_AUDIT_2026-08-14.md A04, DECISIONS.md D20) -- proves the full
// retention -> tombstone -> agent events feed path against a REAL, migrated Postgres. A mock-chained
// Drizzle test cannot prove the tombstone's `seq` really comes from issue_activity's IDENTITY
// sequence, that the FK-free row actually survives its issue's ON DELETE CASCADE, or that
// `listOrgActivity`'s UNION surfaces it in seq order -- all three are properties of the schema and
// live SQL, not of any value a mock returns.
//
// The suite seeds its own org/project/issue, deletes the issue through the real
// `cleanupRetainedData` path, and asserts a matching tombstone appears on the feed. `deleted_at` is
// backdated via raw SQL after the delete so the feed's 2s lag guard (EVENTS_LAG_GUARD_INTERVAL)
// does not hide a just-written row -- the same technique retention.claims.integration.test.ts uses
// to control `now()`-relative comparisons. All rows are suffix-scoped and torn down in afterAll so a
// shared dev database's ambient rows can neither satisfy nor mask the assertions.
//
// Skipped (not failed) unless a Postgres answers; set TOMBSTONE_INTEGRATION_REQUIRED=1 to turn "no
// reachable database" into a hard failure so CI cannot silently skip the only proof this holds
// against real infrastructure.
//
//   cd apps/dashboard-web && \
//   DATABASE_URL=postgres://sentinel:changeme@localhost:5432/sentinel \
//   TOMBSTONE_INTEGRATION_REQUIRED=1 \
//   pnpm vitest run src/lib/server/retention.tombstones.integration.test.ts

const connectionString =
	process.env.DATABASE_URL ?? 'postgres://sentinel:changeme@localhost:5432/sentinel';
const required = process.env.TOMBSTONE_INTEGRATION_REQUIRED === '1';

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
		`TOMBSTONE_INTEGRATION_REQUIRED=1 but no database answered at ${connectionString}. ` +
			'This test is the only coverage that proves retention writes an issue_deleted tombstone ' +
			'that survives the FK cascade and surfaces on the agent events feed; skipping it silently ' +
			'would leave N8/D20 unguarded.'
	);
}

const suffix = randomUUID().slice(0, 8);
const orgId = randomUUID();
const projectId = randomUUID();

function daysAgo(d: number): Date {
	const date = new Date();
	date.setDate(date.getDate() - d);
	return date;
}

describe.skipIf(!dbReachable)('retention tombstones (integration, real Postgres)', () => {
	let sql: ReturnType<typeof postgres>;

	beforeAll(async () => {
		sql = postgres(connectionString, { connect_timeout: 5, max: 1 });
		await sql`
			insert into organizations (id, name, slug)
			values (${orgId}, ${'tombstone-it-' + suffix}, ${'tombstone-it-' + suffix})
		`;
		await sql`
			insert into projects (id, name, organization_id, api_key, api_key_hash)
			values (${projectId}, ${'tombstone-it-' + suffix}, ${orgId}, ${'k_' + suffix}, ${'h_' + suffix})
		`;
	});

	afterAll(async () => {
		if (!sql) return;
		try {
			await sql`delete from issue_tombstones where organization_id = ${orgId}`;
			await sql`delete from issue_activity where issue_id in (select id from issues where project_id = ${projectId})`;
			await sql`delete from manual_issue_reports where issue_id in (select id from issues where project_id = ${projectId})`;
			await sql`delete from issues where project_id = ${projectId}`;
			await sql`delete from projects where id = ${projectId}`;
			await sql`delete from organizations where id = ${orgId}`;
		} finally {
			await sql.end({ timeout: 5 }).catch(() => {});
		}
	});

	async function seedOrphanIssue(overrides: {
		fingerprint: string;
		assigneeType?: string | null;
		assignedTo?: string | null;
		firstSeen: Date;
	}) {
		const id = randomUUID();
		await sql`
			insert into issues (
				id, project_id, fingerprint, message, error_class, status, issue_type,
				assignee_type, assigned_to, claimed_at, first_seen
			) values (
				${id}, ${projectId}, ${overrides.fingerprint}, ${'orphan seed ' + overrides.fingerprint},
				${'SeedError'}, ${'unresolved'}, ${'system_error'},
				${overrides.assigneeType ?? null}, ${overrides.assignedTo ?? null}, ${null},
				${overrides.firstSeen}
			)
		`;
		return id;
	}

	it('writes an issue_deleted tombstone that survives deletion and surfaces on the feed in seq order', async () => {
		const { cleanupRetainedData } = await import('./retention');
		const { listOrgActivity } = await import('$lib/db/queries/events');

		const agentId = 'agent-claimer-' + suffix;
		const issueId = await seedOrphanIssue({
			fingerprint: 'tombstone-' + suffix,
			assigneeType: 'agent',
			assignedTo: agentId,
			firstSeen: daysAgo(60),
		});

		const result = await cleanupRetainedData(30, 365, 30);
		expect(result.tombstonesWritten).toBeGreaterThanOrEqual(1);

		// The issue is gone...
		const issueRows = await sql`select id from issues where id = ${issueId}`;
		expect(issueRows).toHaveLength(0);

		// ...but the tombstone survives, carrying the claim snapshot and denormalized org/project.
		const tombstones = await sql`
			select issue_id, organization_id, project_id, assignee_type, assigned_to, reason, seq
			from issue_tombstones where issue_id = ${issueId}
		`;
		expect(tombstones).toHaveLength(1);
		expect(tombstones[0].organization_id).toBe(orgId);
		expect(tombstones[0].project_id).toBe(projectId);
		expect(tombstones[0].assignee_type).toBe('agent');
		expect(tombstones[0].assigned_to).toBe(agentId);
		expect(tombstones[0].reason).toBe('retention');
		expect(Number(tombstones[0].seq)).toBeGreaterThan(0);

		// Backdate past the feed's lag guard so the just-written row is visible.
		await sql`update issue_tombstones set deleted_at = now() - interval '10 seconds' where issue_id = ${issueId}`;

		const feed = await listOrgActivity({ organizationId: orgId, after: 0, limit: 200 });
		const deleted = feed.events.find(
			(e) => e.eventType === 'issue_deleted' && e.issue.id === issueId
		);
		expect(deleted).toBeDefined();
		expect(deleted!.actorType).toBe('system');
		expect(deleted!.actorId).toBe('sentinel-retention');
		expect(deleted!.issue.status).toBe('deleted');
		// Events are returned strictly ascending by seq -- the deletion is ordered, not appended.
		const seqs = feed.events.map((e) => e.seq);
		expect(seqs).toEqual([...seqs].sort((a, b) => a - b));

		// A claim-holding agent finds its deleted issue via ?claimed=me despite the issues row being gone.
		const claimedFeed = await listOrgActivity({
			organizationId: orgId,
			after: 0,
			limit: 200,
			claimedByAgentId: agentId,
		});
		expect(
			claimedFeed.events.some((e) => e.eventType === 'issue_deleted' && e.issue.id === issueId)
		).toBe(true);
	});

	it('prunes tombstones older than tombstoneRetentionDays', async () => {
		const { cleanupRetainedData } = await import('./retention');

		const oldIssueId = randomUUID();
		await sql`
			insert into issue_tombstones (issue_id, organization_id, project_id, reason, deleted_at)
			values (${oldIssueId}, ${orgId}, ${projectId}, ${'retention'}, ${daysAgo(90)})
		`;

		const result = await cleanupRetainedData(30, 365, 30);
		expect(result.deletedTombstones).toBeGreaterThanOrEqual(1);

		const survivors = await sql`select issue_id from issue_tombstones where issue_id = ${oldIssueId}`;
		expect(survivors).toHaveLength(0);
	});
});
