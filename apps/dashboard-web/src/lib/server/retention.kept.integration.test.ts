import { describe, it, expect, beforeAll, afterAll } from 'vitest';
import postgres from 'postgres';
import { randomUUID } from 'node:crypto';

// sentinel-retention-resolved-ignored-kept contract §5.4 (a-h): after a sweep with every occurrence
// aged out, issues carrying triage state (resolved, ignored, assigned, commented) keep their rows;
// only an old, quiet, unresolved, unassigned, comment-free orphan is deleted. Real Postgres, same
// shape as retention.claims.integration.test.ts (shared advisory lock, suffix-scoped seed data).
//
//   cd apps/dashboard-web && DATABASE_URL=postgres://... \
//   KEPT_RETENTION_INTEGRATION_REQUIRED=1 pnpm vitest run src/lib/server/retention.kept.integration.test.ts

const connectionString =
	process.env.DATABASE_URL ?? 'postgres://sentinel:changeme@localhost:5432/sentinel';
const required = process.env.KEPT_RETENTION_INTEGRATION_REQUIRED === '1';

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
		`KEPT_RETENTION_INTEGRATION_REQUIRED=1 but no database answered at ${connectionString}.`
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

describe.skipIf(!dbReachable)('retention keeps triage state (integration, real Postgres)', () => {
	let sql: ReturnType<typeof postgres>;

	beforeAll(async () => {
		sql = postgres(connectionString, { connect_timeout: 5, max: 1 });
		await sql`select pg_advisory_lock(hashtext('sentinel.retention.integration'))`;
		await sql`
			insert into organizations (id, name, slug)
			values (${orgId}, ${'kept-retention-it-' + suffix}, ${'kept-retention-it-' + suffix})
		`;
		await sql`
			insert into projects (id, name, organization_id, api_key, api_key_hash)
			values (${projectId}, ${'kept-retention-it-' + suffix}, ${orgId},
			        ${'k_' + suffix}, ${'h_' + suffix})
		`;
	}, 120_000);

	afterAll(async () => {
		if (!sql) return;
		try {
			await sql`delete from issue_comments where issue_id in (select id from issues where project_id = ${projectId})`;
			await sql`delete from error_occurrences where issue_id in (select id from issues where project_id = ${projectId})`;
			await sql`delete from issues where project_id = ${projectId}`;
			await sql`delete from projects where id = ${projectId}`;
			await sql`delete from organizations where id = ${orgId}`;
		} finally {
			await sql`select pg_advisory_unlock_all()`.catch(() => {});
			await sql.end({ timeout: 5 }).catch(() => {});
		}
	});

	async function seedIssue(o: {
		key: string;
		status?: string;
		assigneeType?: string | null;
		assignedTo?: string | null;
		resolvedInVersion?: string | null;
		snoozedUntil?: Date | null;
		snoozeTarget?: number | null;
		firstSeen?: Date;
		count?: number;
	}) {
		const id = randomUUID();
		await sql`
			insert into issues (
				id, project_id, environment, fingerprint, message, error_class, status, issue_type,
				assignee_type, assigned_to, resolved_in_version, snoozed_until,
				snooze_occurrence_target, snooze_count_at, first_seen, count
			) values (
				${id}, ${projectId}, 'production', ${o.key + '-' + suffix}, 'seed', 'SeedError',
				${o.status ?? 'unresolved'}, 'system_error',
				${o.assigneeType ?? null}, ${o.assignedTo ?? null}, ${o.resolvedInVersion ?? null},
				${o.snoozedUntil ?? null}, ${o.snoozeTarget ?? null},
				${o.snoozeTarget ? 5 : null}, ${o.firstSeen ?? daysAgo(100)}, ${o.count ?? 7}
			)
		`;
		return id;
	}

	async function exists(id: string) {
		const rows = await sql`select id from issues where id = ${id}`;
		return rows.length === 1;
	}

	it('keeps resolved, ignored (date and N-more-events), assigned and commented issues; deletes only the quiet unresolved orphan', async () => {
		const { cleanupRetainedData } = await import('./retention');

		const resolvedId = await seedIssue({
			key: 'resolved',
			status: 'resolved',
			resolvedInVersion: '1.2.3',
		});
		const ignoredId = await seedIssue({ key: 'ignored', status: 'ignored' });
		const snoozedDateId = await seedIssue({
			key: 'snoozed-date',
			status: 'ignored',
			snoozedUntil: new Date(Date.now() + 7 * 86_400_000),
		});
		const snoozedCountId = await seedIssue({
			key: 'snoozed-count',
			status: 'ignored',
			snoozeTarget: 50,
			count: 10,
		});
		const humanId = await seedIssue({
			key: 'human',
			assigneeType: 'user',
			assignedTo: 'user-' + suffix,
		});
		const agentId = await seedIssue({
			key: 'agent',
			assigneeType: 'agent',
			assignedTo: 'agent-' + suffix,
		});
		const commentedId = await seedIssue({ key: 'commented' });
		await sql`
			insert into issue_comments (issue_id, author_type, author_id, body_md)
			values (${commentedId}, 'user', ${'u-' + suffix}, 'investigating')
		`;
		const resolvedCommentedId = await seedIssue({ key: 'resolved-commented', status: 'resolved' });
		await sql`
			insert into issue_comments (issue_id, author_type, author_id, body_md)
			values (${resolvedCommentedId}, 'user', ${'u-' + suffix}, 'fixed in 1.2.3')
		`;
		const quietId = await seedIssue({ key: 'quiet' });
		const youngId = await seedIssue({ key: 'young', firstSeen: daysAgo(1) });

		const kept = [
			resolvedId,
			ignoredId,
			snoozedDateId,
			snoozedCountId,
			humanId,
			agentId,
			commentedId,
			resolvedCommentedId,
			youngId,
		];

		// Run twice: the second sweep also exercises count / bucket recompute over kept issues with
		// no occurrences and no buckets (h).
		await cleanupRetainedData(30, 365);
		await cleanupRetainedData(30, 365);

		for (const id of kept) expect(await exists(id), id).toBe(true);
		expect(await exists(quietId)).toBe(false);

		const [resolved] = await sql`
			select status, resolved_in_version from issues where id = ${resolvedId}
		`;
		expect(resolved.status).toBe('resolved');
		expect(resolved.resolved_in_version).toBe('1.2.3');

		const [snoozedCount] = await sql`
			select status, snooze_occurrence_target from issues where id = ${snoozedCountId}
		`;
		expect(snoozedCount.status).toBe('ignored');
		expect(Number(snoozedCount.snooze_occurrence_target)).toBe(50);

		const comments = await sql`
			select id from issue_comments where issue_id in (${commentedId}, ${resolvedCommentedId})
		`;
		expect(comments).toHaveLength(2);
	});
});
