import { describe, it, expect, beforeAll, afterAll, vi } from 'vitest';
import postgres from 'postgres';
import { randomUUID } from 'node:crypto';

// email.ts reads EMAIL_SERVER fresh from `$env/dynamic/private` on every call (see
// email.test.ts's comment) rather than from `process.env` directly, so the smtp://debug
// jsonTransport assertion in step 7 below needs this mock rather than a plain
// `process.env.EMAIL_SERVER = ...` mutation.
const mockEnv: Record<string, string | undefined> = { EMAIL_SERVER: 'smtp://debug' };
vi.mock('$env/dynamic/private', () => ({ env: mockEnv }));

// Manual issues M4 (docs/plans/MANUAL_ISSUES_DESIGN.md §8; CLAUDE.md's "Final M4 stage"):
// end-to-end proof of the notification/subscription fan-out, driven through the SAME
// query-layer functions the session-authenticated API routes call (reports.ts's claimIssue,
// comments.ts's createComment, issues.ts's updateIssueStatus, notifications.ts's read side,
// subscriptions.ts's unsubscribe), executed against a REAL, freshly migrated Postgres. Mirrors
// reports.e2e-flow.integration.test.ts's (M1) and comments.threads.flow.integration.test.ts's
// (M3) shape and skip/require convention.
//
// This must run against a DISPOSABLE Postgres, never the shared dev database (CLAUDE.md). Point
// DATABASE_URL at one before running, e.g.:
//
//   docker run -d --name sentinel-m4-disposable-pg -p 15432:5432 \
//     -e POSTGRES_USER=sentinel -e POSTGRES_PASSWORD=changeme -e POSTGRES_DB=sentinel postgres:15-alpine
//   cd packages/db-migrations && DB_URL_DASHBOARD=postgres://sentinel:changeme@localhost:15432/sentinel?sslmode=disable \
//     go run ./cmd/migrate up -target=dashboard
//   cd apps/dashboard-web && DATABASE_URL=postgres://sentinel:changeme@localhost:15432/sentinel \
//     M4_NOTIFICATIONS_INTEGRATION_REQUIRED=1 \
//     pnpm vitest run src/lib/db/queries/notifications.flow.integration.test.ts
//
// If nothing answers on DATABASE_URL the suite is skipped (top-level await, mirroring
// issues.integration.test.ts's D02 guard) rather than failed. Set
// M4_NOTIFICATIONS_INTEGRATION_REQUIRED=1 to turn "no reachable database" into a hard failure,
// matching M1_FLOW_INTEGRATION_REQUIRED / SEARCH_INTEGRATION_REQUIRED / SCHEMA_DRIFT_REQUIRED.
const connectionString =
	process.env.DATABASE_URL ?? 'postgres://sentinel:changeme@localhost:5432/sentinel';
const required = process.env.M4_NOTIFICATIONS_INTEGRATION_REQUIRED === '1';

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
		`M4_NOTIFICATIONS_INTEGRATION_REQUIRED=1 but no database answered at ${connectionString}. ` +
			'This test is the committed, repeatable proof that the M4 notification/subscription flow ' +
			'runs end-to-end against a real migrated Postgres; skipping it silently would leave that ' +
			'claim unverified.'
	);
}

const suffix = randomUUID().slice(0, 8);
const orgId = randomUUID();
const projectId = randomUUID();
const reporterId = randomUUID();
const userBId = randomUUID();

describe.skipIf(!dbReachable)('Manual issues M4 notifications flow (integration, real Postgres)', () => {
	let sql: ReturnType<typeof postgres>;
	let issueId: string;

	beforeAll(async () => {
		sql = postgres(connectionString, { connect_timeout: 5, max: 1 });

		await sql`
			insert into organizations (id, name, slug)
			values (${orgId}, ${'m4-flow-' + suffix}, ${'m4-flow-' + suffix})
		`;
		await sql`
			insert into projects (id, name, organization_id, api_key, api_key_hash)
			values (${projectId}, ${'m4-flow-' + suffix}, ${orgId},
			        ${'k_' + suffix}, ${'h_' + suffix})
		`;
		await sql`
			insert into "user" (id, name, email)
			values (${reporterId}, ${'Reporter ' + suffix}, ${'reporter-' + suffix + '@example.test'})
		`;
		await sql`
			insert into "user" (id, name, email)
			values (${userBId}, ${'User B ' + suffix}, ${'user-b-' + suffix + '@example.test'})
		`;
		// R1 (docs/plans/PR13_REVIEW_REMEDIATION_PLAN.md): notifyIssueEvent now re-checks CURRENT
		// org membership before notifying a subscriber, joined through the issue's project -> org.
		// Without membership rows here, every fan-out in this flow would be silently filtered to
		// empty, which is correct production behavior but breaks this test's ability to assert on
		// notified lists unless both actors are actually members of `orgId`.
		await sql`
			insert into organization_members (organization_id, user_id, role)
			values (${orgId}, ${reporterId}, 'viewer'), (${orgId}, ${userBId}, 'viewer')
		`;
	});

	afterAll(async () => {
		if (!sql) return;
		try {
			await sql`delete from notifications where issue_id in (select id from issues where project_id = ${projectId})`;
			await sql`delete from issue_subscriptions where issue_id in (select id from issues where project_id = ${projectId})`;
			await sql`delete from issue_activity where issue_id in (select id from issues where project_id = ${projectId})`;
			await sql`delete from issue_comments where issue_id in (select id from issues where project_id = ${projectId})`;
			await sql`delete from manual_issue_reports where issue_id in (select id from issues where project_id = ${projectId})`;
			await sql`delete from issues where project_id = ${projectId}`;
			await sql`delete from projects where organization_id = ${orgId}`;
			await sql`delete from organization_members where organization_id = ${orgId}`;
			await sql`delete from "user" where id in (${reporterId}, ${userBId})`;
			await sql`delete from organizations where id = ${orgId}`;
		} finally {
			await sql.end({ timeout: 5 }).catch(() => {});
		}
	});

	it('runs create -> claim -> comment -> read -> resolve -> unsubscribe end-to-end', async () => {
		const { createManualIssue } = await import('./reports');
		const { claimIssue } = await import('./reports');
		const { createComment } = await import('./comments');
		const { updateIssueStatus } = await import('./issues');
		const {
			listNotifications,
			getUnreadNotificationCount,
			markNotificationRead,
		} = await import('./notifications');
		const { unsubscribe, isSubscribed } = await import('./subscriptions');
		const { sendIssueNotificationEmail } = await import('$lib/server/email');

		// 1. CREATE by the reporter -- auto-subscribes the reporter (reason 'reporter').
		const created = await createManualIssue({
			organizationId: orgId,
			projectId,
			reporterId,
			title: `M4 flow report ${suffix}`,
			bodyMd: 'Notification flow smoke test.',
			severity: 'medium',
		});
		issueId = created.issue.id;
		expect(await isSubscribed(issueId, 'user', reporterId)).toBe(true);

		// 2. CLAIM by user B: reporter gets a 'claimed' notification; B does NOT self-notify.
		const claimResult = await claimIssue(issueId, 'user', userBId);
		expect(claimResult.notified.map((n) => n.userId)).toEqual([reporterId]);
		expect(claimResult.notified.every((n) => n.userId !== userBId)).toBe(true);

		let reporterNotifications = await listNotifications({ userId: reporterId });
		expect(reporterNotifications.some((n) => n.issueId === issueId && n.kind === 'claimed')).toBe(
			true
		);
		let reporterUnread = await getUnreadNotificationCount(reporterId);
		expect(reporterUnread).toBeGreaterThanOrEqual(1);

		let bNotifications = await listNotifications({ userId: userBId });
		expect(bNotifications.some((n) => n.issueId === issueId)).toBe(false);

		// 3. B COMMENTS: reporter notified 'commented'; B auto-subscribed as 'participant'.
		const commentResult = await createComment({
			issueId,
			authorType: 'user',
			authorId: userBId,
			bodyMd: 'Looking into this now.',
		});
		expect(commentResult.notified.map((n) => n.userId)).toEqual([reporterId]);
		expect(await isSubscribed(issueId, 'user', userBId)).toBe(true);

		reporterNotifications = await listNotifications({ userId: reporterId });
		const commentedNotification = reporterNotifications.find(
			(n) => n.issueId === issueId && n.kind === 'commented'
		);
		expect(commentedNotification).toBeTruthy();

		// 4. REPORTER MARKS READ: read_at set, unread count drops.
		const unreadBefore = await getUnreadNotificationCount(reporterId);
		expect(unreadBefore).toBeGreaterThanOrEqual(2); // claimed + commented
		const marked = await markNotificationRead(commentedNotification!.id, reporterId);
		expect(marked).toBe(true);
		const unreadAfter = await getUnreadNotificationCount(reporterId);
		expect(unreadAfter).toBe(unreadBefore - 1);

		const refetched = await listNotifications({ userId: reporterId });
		const refetchedCommented = refetched.find((n) => n.id === commentedNotification!.id);
		expect(refetchedCommented?.readAt).not.toBeNull();

		// 5. STATUS -> resolved by B: reporter notified 'resolved'.
		const resolveNotified = await updateIssueStatus(issueId, 'resolved', undefined, 'user', userBId);
		expect(resolveNotified.map((n) => n.userId)).toEqual([reporterId]);
		reporterNotifications = await listNotifications({ userId: reporterId });
		expect(reporterNotifications.some((n) => n.issueId === issueId && n.kind === 'resolved')).toBe(
			true
		);

		// 6. UNSUBSCRIBE then another event: no new notification for the reporter.
		await unsubscribe({ issueId, subscriberType: 'user', subscriberId: reporterId });
		expect(await isSubscribed(issueId, 'user', reporterId)).toBe(false);

		const notifiedAfterUnsub = await updateIssueStatus(
			issueId,
			'unresolved',
			undefined,
			'user',
			userBId
		);
		expect(notifiedAfterUnsub.every((n) => n.userId !== reporterId)).toBe(true);

		const finalReporterNotifications = await listNotifications({ userId: reporterId });
		const resolvedCount = finalReporterNotifications.filter(
			(n) => n.issueId === issueId && n.kind === 'resolved'
		).length;
		expect(resolvedCount).toBe(1); // still just the one from step 5, no new row after unsubscribe

		// 7. EMAIL: use the smtp://debug jsonTransport path (mocked $env/dynamic/private above)
		// to assert the composer runs and reports delivered, without any real SMTP server.
		const delivered = await sendIssueNotificationEmail(
			'reporter@example.test',
			`http://localhost:13000/${'m4-flow-' + suffix}/reports/${issueId}`,
			'resolved',
			created.issue.message
		);
		expect(delivered).toBe(true);
	});
});
