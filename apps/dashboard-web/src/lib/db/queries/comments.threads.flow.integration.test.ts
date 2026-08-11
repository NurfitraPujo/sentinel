import { describe, it, expect, beforeAll, afterAll } from 'vitest';
import postgres from 'postgres';
import { randomUUID } from 'node:crypto';

// This suite runs under vitest's jsdom environment with the 'browser' resolve condition forced on
// (vite.config.js, mode === 'test'), which makes @aws-sdk/client-s3 resolve its BROWSER
// stream-collector build -- jsdom's Blob has no `.arrayBuffer()`, which that build needs even for
// PutObject/HeadBucket. Swapping in Node's Blob fixes it against a real MinIO endpoint; see the M2
// flow test (reports.attachments.flow.integration.test.ts) for the full rationale. Zero effect on
// production.
import { Blob as NodeBlob } from 'node:buffer';
// eslint-disable-next-line @typescript-eslint/no-explicit-any
(globalThis as any).Blob = NodeBlob;

// Manual Issues M3 end-to-end proof (docs/plans/MANUAL_ISSUES_DESIGN.md §5, plus the
// comment-attachment paths §4; CLAUDE.md's "Final M3 stage -- GATES + END-TO-END PROOF"). Extends
// the M1/M2 flow-proof pattern with the thread mechanics §5 adds: a root comment with an
// attachment, a reply, a reply-to-a-reply resolving to the SAME parent, waiting_on
// set-then-cleared by a human reply (Q11 groundwork), the `after` polling filter, and comment
// deletion cascading both the DB rows and the underlying MinIO object.
//
// Drives the SAME query-layer functions the session-authenticated API routes call
// (src/lib/db/queries/comments.ts, called from api/issues/[issueId]/comments/{,[commentId]}),
// against a REAL, freshly migrated Postgres AND a REAL S3-compatible object store (the compose
// MinIO by default). Auth.js sessions make raw HTTP hard to drive from a standalone script (see
// the M1 file's note) -- the query layer IS what those routes call after
// `requireReportAccess`/`requireIssueAccess` pass.
//
// Requires BOTH a reachable Postgres (DATABASE_URL) and a reachable S3-compatible endpoint
// (S3_ENDPOINT/S3_BUCKET/S3_ACCESS_KEY/S3_SECRET_KEY). Skipped (not failed) if either is
// unreachable, UNLESS M3_THREADS_INTEGRATION_REQUIRED=1, matching the M1/M2 pattern. Example run
// against the compose stack:
//
//   cd apps/dashboard-web && \
//   DATABASE_URL=postgres://sentinel:changeme@localhost:5432/sentinel \
//   S3_ENDPOINT=http://localhost:9000 S3_BUCKET=sentinel-attachments \
//   S3_ACCESS_KEY=minioadmin S3_SECRET_KEY=minioadmin \
//   M3_THREADS_INTEGRATION_REQUIRED=1 \
//   pnpm vitest run src/lib/db/queries/comments.threads.flow.integration.test.ts

const dbConnectionString =
	process.env.DATABASE_URL ?? 'postgres://sentinel:changeme@localhost:5432/sentinel';
const required = process.env.M3_THREADS_INTEGRATION_REQUIRED === '1';

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
		`M3_THREADS_INTEGRATION_REQUIRED=1 but a dependency was unreachable ` +
			`(db reachable=${dbReachable}, storage reachable=${storageReachable}). ` +
			'This test is the committed, repeatable proof that the M3 threads flow runs end-to-end ' +
			'against a real migrated Postgres and a real S3-compatible store; skipping it silently would ' +
			'leave that claim unverified.'
	);
}

// Valid PNG signature + a little body -- enough for sniffContentType to positively identify it.
const PNG_BYTES = Buffer.concat([
	Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
	Buffer.from('m3 thread attachment round trip body'),
]);

const suffix = randomUUID().slice(0, 8);
const orgId = randomUUID();
const projectId = randomUUID();
const reporterId = randomUUID();
const responderId = randomUUID();

describe.skipIf(!ready)('Manual issues M3 threads flow (integration, real Postgres + S3)', () => {
	let sql: ReturnType<typeof postgres>;

	beforeAll(async () => {
		sql = postgres(dbConnectionString, { connect_timeout: 5, max: 1 });

		await sql`
			insert into organizations (id, name, slug)
			values (${orgId}, ${'m3-threads-' + suffix}, ${'m3-threads-' + suffix})
		`;
		await sql`
			insert into projects (id, name, organization_id, api_key, api_key_hash)
			values (${projectId}, ${'m3-threads-' + suffix}, ${orgId}, ${'k_' + suffix}, ${'h_' + suffix})
		`;
		await sql`
			insert into "user" (id, name, email)
			values
				(${reporterId}, ${'Reporter ' + suffix}, ${'reporter-' + suffix + '@example.test'}),
				(${responderId}, ${'Responder ' + suffix}, ${'responder-' + suffix + '@example.test'})
		`;
	});

	afterAll(async () => {
		if (!sql) return;
		try {
			await sql`delete from attachments where org_id = ${orgId}`;
			await sql`delete from issue_activity where issue_id in (select id from issues where project_id = ${projectId})`;
			await sql`delete from issue_comments where issue_id in (select id from issues where project_id = ${projectId})`;
			await sql`delete from manual_issue_reports where issue_id in (select id from issues where project_id = ${projectId})`;
			await sql`delete from issues where project_id = ${projectId}`;
			await sql`delete from projects where organization_id = ${orgId}`;
			await sql`delete from "user" where id in (${reporterId}, ${responderId})`;
			await sql`delete from organizations where id = ${orgId}`;
		} finally {
			await sql.end({ timeout: 5 }).catch(() => {});
		}
	});

	it(
		'creates a report, threads root/reply/reply-to-reply with an attachment, clears ' +
			'waiting_on on a human reply, filters by `after`, and cascades a comment delete',
		async () => {
			const { db } = await import('$lib/server/db');
			const { attachments, issues } = await import('$lib/db/schema');
			const { putObject, getObjectStream, isStorageConfigured } = await import('$lib/server/storage');
			const { sniffContentType, resolveContentType } = await import('$lib/server/attachment-sniff');
			const { createManualIssue } = await import('./reports');
			const { createComment, listComments, deleteComment } = await import('./comments');
			const { eq } = await import('drizzle-orm');

			expect(isStorageConfigured()).toBe(true);

			// --- Setup: a report to thread on. ---
			const created = await createManualIssue({
				organizationId: orgId,
				projectId,
				reporterId,
				title: `M3 threads flow ${suffix}`,
				bodyMd: 'Need help with this.',
				severity: 'medium',
			});
			const issueId = created.issue.id;

			// --- Step 1: upload a draft attachment, then a root comment claiming it. ---
			const detected = sniffContentType(PNG_BYTES);
			expect(detected).toBe('image/png');
			const resolved = resolveContentType(detected, 'image/png');
			expect(resolved).toBe('image/png');

			const storageKey = `org/${orgId}/${randomUUID()}`;
			await putObject(storageKey, PNG_BYTES, resolved!);

			const [draftAttachment] = await db
				.insert(attachments)
				.values({
					orgId,
					issueId: null,
					commentId: null,
					uploaderType: 'user',
					uploaderId: reporterId,
					filename: 'screenshot.png',
					contentType: resolved!,
					sizeBytes: PNG_BYTES.length,
					storageKey,
				})
				.returning();
			expect(draftAttachment).toBeTruthy();

			const rootComment = await createComment({
				issueId,
				authorType: 'user',
				authorId: reporterId,
				bodyMd: 'Here is a screenshot of the bug.',
				attachmentIds: [draftAttachment.id],
			});
			expect(rootComment.parentId).toBeNull();

			const [linkedAttachment] = await sql`
				select id, comment_id from attachments where id = ${draftAttachment.id}
			`;
			expect(linkedAttachment.comment_id).toBe(rootComment.id);

			// Downloadable via the flipped access path (getObjectStream, what GET /api/attachments/[id]
			// calls after its access check passes).
			const downloaded = await getObjectStream(storageKey);
			expect(downloaded.ContentType).toBe('image/png');
			const chunks: Uint8Array[] = [];
			for await (const chunk of downloaded.Body as unknown as AsyncIterable<Uint8Array>) {
				chunks.push(chunk);
			}
			expect(Buffer.concat(chunks).equals(PNG_BYTES)).toBe(true);

			// --- Step 2: a reply. ---
			const reply = await createComment({
				issueId,
				authorType: 'user',
				authorId: responderId,
				bodyMd: 'Can you also share your browser version?',
				parentId: rootComment.id,
			});
			expect(reply.parentId).toBe(rootComment.id);

			// --- Step 3: a reply-to-the-reply resolves to the SAME parent (root), not nested deeper. ---
			const replyToReply = await createComment({
				issueId,
				authorType: 'user',
				authorId: reporterId,
				bodyMd: 'Chrome 128.',
				parentId: reply.id,
			});
			expect(replyToReply.parentId).toBe(rootComment.id);

			// --- Step 4: set waiting_on='reporter' directly in SQL (simulating an agent's blocking
			// question, which is M5's job to drive -- this test only needs the CLEARING side). ---
			await sql`update issues set waiting_on = 'reporter' where id = ${issueId}`;
			const [beforeReply] = await sql`select waiting_on from issues where id = ${issueId}`;
			expect(beforeReply.waiting_on).toBe('reporter');

			// Cutoff is read from the DATABASE's own clock (not the test process's), since
			// issue_comments.created_at is DEFAULT NOW() evaluated by Postgres -- an app-clock
			// timestamp is vulnerable to skew between the test host and a containerized Postgres,
			// which made every comment's created_at compare as "before" an app-clock cutoff captured
			// microseconds earlier.
			const [{ now: afterFilterCutoff }] = await sql<{ now: Date }[]>`select now() as now`;

			const answerComment = await createComment({
				issueId,
				authorType: 'user',
				authorId: reporterId,
				bodyMd: 'Still happening as of today.',
			});

			const [afterAnswerIssue] = await db
				.select({ waitingOn: issues.waitingOn })
				.from(issues)
				.where(eq(issues.id, issueId));
			expect(afterAnswerIssue.waitingOn).toBeNull();

			const activityTypes = await sql`
				select event_type from issue_activity where issue_id = ${issueId} order by created_at asc
			`;
			expect(activityTypes.map((r) => r.event_type)).toContain('question_answered');

			// --- Step 5: `after`-filter returns only the new (answer) root comment. ---
			const afterFiltered = await listComments(issueId, { after: afterFilterCutoff });
			const afterFilteredIds = afterFiltered.map((c) => c.id);
			expect(afterFilteredIds).toContain(answerComment.id);
			expect(afterFilteredIds).not.toContain(rootComment.id);

			const allComments = await listComments(issueId);
			const rootFromList = allComments.find((c) => c.id === rootComment.id);
			expect(rootFromList).toBeTruthy();
			expect(rootFromList!.replies.map((r) => r.id).sort()).toEqual(
				[reply.id, replyToReply.id].sort()
			);
			expect(rootFromList!.attachments.map((a) => a.id)).toContain(draftAttachment.id);

			// --- Step 6: delete a comment with an attachment -> row cascade + MinIO object gone. ---
			await deleteComment(rootComment.id);

			const [rootAfterDelete] = await sql`select id from issue_comments where id = ${rootComment.id}`;
			expect(rootAfterDelete).toBeUndefined();
			// Replies cascade via parent_id FK.
			const [replyAfterDelete] = await sql`select id from issue_comments where id = ${reply.id}`;
			expect(replyAfterDelete).toBeUndefined();
			const [replyToReplyAfterDelete] = await sql`
				select id from issue_comments where id = ${replyToReply.id}
			`;
			expect(replyToReplyAfterDelete).toBeUndefined();

			const [attachmentRowAfterDelete] = await sql`
				select id from attachments where id = ${draftAttachment.id}
			`;
			expect(attachmentRowAfterDelete).toBeUndefined();

			await expect(getObjectStream(storageKey)).rejects.toBeTruthy();

			// The unrelated answer comment must survive the deletion of a different root.
			const [answerAfterDelete] = await sql`
				select id from issue_comments where id = ${answerComment.id}
			`;
			expect(answerAfterDelete).toBeTruthy();
		}
	);
});
