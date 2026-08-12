import { describe, it, expect, beforeAll, afterAll } from 'vitest';
import postgres from 'postgres';
import { randomUUID } from 'node:crypto';

// This suite runs under vitest's jsdom environment with the 'browser' resolve condition forced
// on for Svelte's sake, which makes @aws-sdk/client-s3 resolve its BROWSER stream-collector
// build; jsdom's Blob does not implement `.arrayBuffer()`, which that build depends on. Swapping
// in Node's own Blob makes the SDK work against a real MinIO endpoint -- see
// reports.attachments.flow.integration.test.ts for the full rationale. Zero effect on production.
import { Blob as NodeBlob } from 'node:buffer';
// eslint-disable-next-line @typescript-eslint/no-explicit-any
(globalThis as any).Blob = NodeBlob;

// PR #13 review remediation (docs/plans/PR13_REVIEW_REMEDIATION_PLAN.md) -- Stage D.7 targeted
// integration proofs for R1, R2, R6, R11, run against a REAL, freshly migrated Postgres (and, for
// R6, a REAL S3-compatible object store -- the compose MinIO by default). Each finding also has
// unit coverage (issues.notify.test.ts, notify.test.ts, members.test.ts for R1; reports.test.ts
// for R2; retention.attachments.test.ts for R6; reports.edit-delete.test.ts for R11) that proves
// the individual code path red-first against a mocked db/tx. This file proves the same fixes
// hold end-to-end against real infrastructure, driving the same query-layer functions the routes
// call (mirrors the M1/M2/M4 flow-proof pattern; Auth.js sessions make raw HTTP hard to drive
// from a standalone script).
//
// Requires a reachable Postgres (DATABASE_URL); R6 additionally requires a reachable
// S3-compatible endpoint (S3_ENDPOINT/S3_BUCKET/S3_ACCESS_KEY/S3_SECRET_KEY -- the compose MinIO
// defaults are used if unset). Skipped (not failed) unless
// PR13_REMEDIATION_INTEGRATION_REQUIRED=1, matching M1_FLOW_INTEGRATION_REQUIRED /
// M2_ATTACHMENTS_INTEGRATION_REQUIRED / M4_NOTIFICATIONS_INTEGRATION_REQUIRED /
// SCHEMA_DRIFT_REQUIRED. Example run against the compose stack:
//
//   cd apps/dashboard-web && \
//   DATABASE_URL=postgres://sentinel:changeme@localhost:5432/sentinel \
//   S3_ENDPOINT=http://localhost:9000 S3_BUCKET=sentinel-attachments \
//   S3_ACCESS_KEY=minioadmin S3_SECRET_KEY=minioadmin \
//   PR13_REMEDIATION_INTEGRATION_REQUIRED=1 \
//   pnpm vitest run src/lib/db/queries/reports.pr13-remediation.flow.integration.test.ts

const dbConnectionString =
	process.env.DATABASE_URL ?? 'postgres://sentinel:changeme@localhost:5432/sentinel';
const required = process.env.PR13_REMEDIATION_INTEGRATION_REQUIRED === '1';

let dbReachable = false;
const probeSql = postgres(dbConnectionString, { connect_timeout: 3, max: 1 });
try {
	await probeSql`select 1`;
	dbReachable = true;
} catch {
	dbReachable = false;
} finally {
	await probeSql.end({ timeout: 1 }).catch(() => {});
}

const s3EnvComplete = Boolean(
	process.env.S3_ENDPOINT &&
		process.env.S3_BUCKET &&
		process.env.S3_ACCESS_KEY &&
		process.env.S3_SECRET_KEY
);

let storageReachable = false;
try {
	if (!s3EnvComplete) {
		throw new Error('S3_ENDPOINT/S3_BUCKET/S3_ACCESS_KEY/S3_SECRET_KEY not fully set');
	}
	const { S3Client, HeadBucketCommand } = await import('@aws-sdk/client-s3');
	const client = new S3Client({
		endpoint: process.env.S3_ENDPOINT,
		region: 'us-east-1',
		forcePathStyle: true,
		credentials: {
			accessKeyId: process.env.S3_ACCESS_KEY!,
			secretAccessKey: process.env.S3_SECRET_KEY!,
		},
	});
	await client.send(new HeadBucketCommand({ Bucket: process.env.S3_BUCKET }));
	storageReachable = true;
} catch {
	storageReachable = false;
}

const ready = dbReachable && storageReachable;

if (required && !ready) {
	throw new Error(
		`PR13_REMEDIATION_INTEGRATION_REQUIRED=1 but a dependency was unreachable ` +
			`(db reachable=${dbReachable}, storage reachable=${storageReachable}). ` +
			'This test is the committed, repeatable proof that R1/R2/R6/R11 hold end-to-end against a ' +
			'real migrated Postgres and a real S3-compatible store; skipping it silently would leave ' +
			'that claim unverified.'
	);
}

const PNG_BYTES = Buffer.concat([
	Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
	Buffer.from('pr13 remediation R6/R11 round trip bytes'),
]);

const suffix = randomUUID().slice(0, 8);
const orgId = randomUUID();
const projectId = randomUUID();
const reporterId = randomUUID();
const ownerId = randomUUID();
const removedMemberId = randomUUID();

describe.skipIf(!ready)('PR #13 remediation flow (integration, real Postgres + S3)', () => {
	let sql: ReturnType<typeof postgres>;

	beforeAll(async () => {
		sql = postgres(dbConnectionString, { connect_timeout: 5, max: 1 });

		await sql`
			insert into organizations (id, name, slug)
			values (${orgId}, ${'pr13-remed-' + suffix}, ${'pr13-remed-' + suffix})
		`;
		await sql`
			insert into projects (id, name, organization_id, api_key, api_key_hash)
			values (${projectId}, ${'pr13-remed-' + suffix}, ${orgId}, ${'k_' + suffix}, ${'h_' + suffix})
		`;
		await sql`
			insert into "user" (id, name, email)
			values
				(${reporterId}, ${'Reporter ' + suffix}, ${'reporter-' + suffix + '@example.test'}),
				(${ownerId}, ${'Owner ' + suffix}, ${'owner-' + suffix + '@example.test'}),
				(${removedMemberId}, ${'Removed ' + suffix}, ${'removed-' + suffix + '@example.test'})
		`;
		await sql`
			insert into organization_members (organization_id, user_id, role)
			values
				(${orgId}, ${ownerId}, 'owner'),
				(${orgId}, ${reporterId}, 'viewer'),
				(${orgId}, ${removedMemberId}, 'viewer')
		`;
	});

	afterAll(async () => {
		if (!sql) return;
		try {
			await sql`delete from attachments where org_id = ${orgId}`;
			await sql`delete from issue_activity where issue_id in (select id from issues where project_id = ${projectId})`;
			await sql`delete from issue_subscriptions where issue_id in (select id from issues where project_id = ${projectId})`;
			await sql`delete from manual_issue_reports where issue_id in (select id from issues where project_id = ${projectId})`;
			await sql`delete from issues where project_id = ${projectId}`;
			await sql`delete from projects where organization_id = ${orgId}`;
			await sql`delete from organization_members where organization_id = ${orgId}`;
			await sql`delete from "user" where id in (${reporterId}, ${ownerId}, ${removedMemberId})`;
			await sql`delete from organizations where id = ${orgId}`;
		} finally {
			await sql.end({ timeout: 5 }).catch(() => {});
		}
	});

	it('R1: a member removed from the org receives no notification even though still subscribed', async () => {
		const { db } = await import('$lib/server/db');
		const { notifications, issueSubscriptions, organizationMembers } = await import('$lib/db/schema');
		const { eq, and } = await import('drizzle-orm');
		const { createManualIssue } = await import('./reports');
		const { subscribe } = await import('./subscriptions');
		const { notifyIssueEvent } = await import('$lib/server/notify');

		const created = await createManualIssue({
			organizationId: orgId,
			projectId,
			reporterId,
			title: `R1 flow ${suffix}`,
			bodyMd: 'R1 body',
			severity: 'medium',
		});
		const issueId = created.issue.id;

		// The removed member subscribes WHILE still a member -- a real subscription row that
		// must outlive their membership only in the sense of "still exists", never "still fires".
		await subscribe({ issueId, subscriberType: 'user', subscriberId: removedMemberId, reason: 'manual' });

		// Simulate the DELETE /organizations/[orgId]/members/[memberId] route's removal (same two
		// statements, same transaction shape) rather than driving raw HTTP through Auth.js.
		await db.transaction(async (tx) => {
			await tx
				.delete(organizationMembers)
				.where(
					and(
						eq(organizationMembers.organizationId, orgId),
						eq(organizationMembers.userId, removedMemberId)
					)
				);
		});

		// The subscription row is untouched by this simulated removal (only the route's own
		// transaction deletes it) -- so the fan-out re-check, not row absence, is what R1 proves.
		const [stillSubscribed] = await sql`
			select 1 from issue_subscriptions
			where issue_id = ${issueId} and subscriber_id = ${removedMemberId}
		`;
		expect(stillSubscribed).toBeTruthy();

		await db.transaction(async (tx) => {
			await notifyIssueEvent(tx, {
				issueId,
				kind: 'status_changed',
				actorType: 'user',
				actorId: ownerId,
				payload: { from: 'unresolved', to: 'resolved' },
			});
		});

		const notifiedRemoved = await db
			.select()
			.from(notifications)
			.where(and(eq(notifications.issueId, issueId), eq(notifications.userId, removedMemberId)));
		expect(notifiedRemoved).toHaveLength(0);

		const notifiedReporter = await db
			.select()
			.from(notifications)
			.where(and(eq(notifications.issueId, issueId), eq(notifications.userId, reporterId)));
		expect(notifiedReporter.length).toBeGreaterThanOrEqual(1);
	});

	it('R2: two concurrent no-project creates in the same org produce exactly ONE Triage project', async () => {
		const { createManualIssue } = await import('./reports');

		const [createdA, createdB] = await Promise.all([
			createManualIssue({
				organizationId: orgId,
				projectId: null,
				reporterId,
				title: `R2 flow A ${suffix}`,
				bodyMd: 'R2 body A',
				severity: 'low',
			}),
			createManualIssue({
				organizationId: orgId,
				projectId: null,
				reporterId,
				title: `R2 flow B ${suffix}`,
				bodyMd: 'R2 body B',
				severity: 'low',
			}),
		]);

		expect(createdA.issue.projectId).toBe(createdB.issue.projectId);

		const inboxRows = await sql`
			select id from projects where organization_id = ${orgId} and is_inbox = true
		`;
		expect(inboxRows).toHaveLength(1);
	});

	it('R6 + R11: author edit writes report_edited; author delete removes rows and the MinIO object', async () => {
		const { putObject, getObjectStream, isStorageConfigured } = await import('$lib/server/storage');
		const { attachments } = await import('$lib/db/schema');
		const { db } = await import('$lib/server/db');
		const { createManualIssue, updateManualIssueReport, deleteManualIssue } = await import('./reports');
		const { bestEffortDeleteObjects } = await import('$lib/server/retention');

		expect(isStorageConfigured()).toBe(true);

		const storageKey = `org/${orgId}/${randomUUID()}`;
		await putObject(storageKey, PNG_BYTES, 'image/png');

		const [attachmentRow] = await db
			.insert(attachments)
			.values({
				orgId,
				issueId: null,
				commentId: null,
				uploaderType: 'user',
				uploaderId: reporterId,
				filename: 'r6-r11.png',
				contentType: 'image/png',
				sizeBytes: PNG_BYTES.length,
				storageKey,
			})
			.returning();

		const created = await createManualIssue({
			organizationId: orgId,
			projectId,
			reporterId,
			title: `R11 flow ${suffix}`,
			bodyMd: 'Original body',
			severity: 'medium',
			attachmentIds: [attachmentRow.id],
		});
		const issueId = created.issue.id;

		// --- R11 edit: author edits their own report -> `report_edited` activity, not `report_created`. ---
		await updateManualIssueReport({
			issueId,
			actorId: reporterId,
			bodyMd: 'Edited body',
			severity: 'high',
		});

		const activityTypes = await sql`
			select event_type from issue_activity where issue_id = ${issueId} order by created_at asc
		`;
		expect(activityTypes.map((r) => r.event_type)).toContain('report_created');
		expect(activityTypes.map((r) => r.event_type)).toContain('report_edited');

		const [editedReport] = await sql`
			select body_md, severity from manual_issue_reports where issue_id = ${issueId}
		`;
		expect(editedReport.body_md).toBe('Edited body');
		expect(editedReport.severity).toBe('high');

		// Object still present before delete.
		await expect(getObjectStream(storageKey)).resolves.toBeTruthy();

		// --- R6/R11 delete: collects storage keys before the cascade, then best-effort deletes them. ---
		const { storageKeys } = await deleteManualIssue(issueId);
		expect(storageKeys).toContain(storageKey);

		await bestEffortDeleteObjects(storageKeys, 'test.r6_r11_delete');

		const [issueAfter] = await sql`select id from issues where id = ${issueId}`;
		expect(issueAfter).toBeUndefined();
		const [reportAfter] = await sql`select issue_id from manual_issue_reports where issue_id = ${issueId}`;
		expect(reportAfter).toBeUndefined();
		const [attachmentAfter] = await sql`select id from attachments where id = ${attachmentRow.id}`;
		expect(attachmentAfter).toBeUndefined();

		await expect(getObjectStream(storageKey)).rejects.toBeTruthy();
	});
});
