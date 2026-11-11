import { describe, it, expect, beforeAll, afterAll } from 'vitest';
import postgres from 'postgres';
import { randomUUID } from 'node:crypto';

// See reports.attachments.flow.integration.test.ts for why Node's Blob is swapped in: under
// vitest's jsdom env with the 'browser' resolve condition, @aws-sdk/client-s3 uses its browser
// stream-collector build, whose response reads call `blob.arrayBuffer()` -- absent on jsdom's Blob.
// Node's Blob implements it; production never runs under the 'browser' condition so this is test-only.
import { Blob as NodeBlob } from 'node:buffer';
// eslint-disable-next-line @typescript-eslint/no-explicit-any
(globalThis as any).Blob = NodeBlob;

// M6 Feature A end-to-end proof (docs/plans/M6_PRESIGNED_UPLOADS_AND_TOOLBAR_PLAN.md §Feature A).
// Drives the SAME query/storage-layer functions the presign + finalize routes call
// (createPresignedAttachment, a real PUT to the returned presigned URL, finalizePresignedAttachment,
// then createManualIssue's claimDraftAttachments), against a REAL migrated Postgres AND a REAL
// S3-compatible store (compose MinIO by default). Proves, against real infra:
//   1. presign -> a 'pending' row + a working direct-to-bucket PUT URL
//   2. PUT bytes straight to the bucket, then finalize -> row flips to 'ready' with the SNIFFED
//      content type and the REAL object size (declared values at presign time are untrusted)
//   3. a finalized ('ready') attachment claims onto a report
//   4. LOAD-BEARING NEGATIVE: a presigned-but-never-finalized ('pending') attachment, whose bytes
//      exist in the bucket, is NOT linkable via report creation -- the status gate holds against a
//      real DB, not just a mock
//   5. an upload whose bytes match no allowed signature is rejected at finalize (415) AND its
//      object is deleted from the bucket AND its row removed
//
// Requires BOTH a reachable Postgres (DATABASE_URL) and a reachable S3-compatible endpoint
// (S3_ENDPOINT/S3_BUCKET/S3_ACCESS_KEY/S3_SECRET_KEY). Skipped (not failed) if either is
// unreachable, UNLESS M6_PRESIGN_INTEGRATION_REQUIRED=1. Example against the compose stack:
//
//   cd apps/dashboard-web && \
//   DATABASE_URL=postgres://sentinel:changeme@localhost:5432/sentinel \
//   S3_ENDPOINT=http://localhost:9000 S3_BUCKET=sentinel-attachments \
//   S3_ACCESS_KEY=minioadmin S3_SECRET_KEY=minioadmin \
//   M6_PRESIGN_INTEGRATION_REQUIRED=1 \
//   pnpm vitest run src/lib/db/queries/reports.presign.flow.integration.test.ts

const dbConnectionString =
	process.env.DATABASE_URL ?? 'postgres://sentinel:changeme@localhost:5432/sentinel';
const required = process.env.M6_PRESIGN_INTEGRATION_REQUIRED === '1';

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
		`M6_PRESIGN_INTEGRATION_REQUIRED=1 but a dependency was unreachable ` +
			`(db reachable=${dbReachable}, storage reachable=${storageReachable}). ` +
			'This test is the committed, repeatable proof that the M6 presigned-upload flow runs ' +
			'end-to-end against a real migrated Postgres and a real S3-compatible store.'
	);
}

const PNG_BYTES = Buffer.concat([
	Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
	Buffer.from('presigned upload body that must survive a direct PUT + ranged-GET sniff round trip'),
]);

// Unrecognized/corrupt bytes: no known signature AND a NUL byte (so the text-by-exclusion path
// also rejects it) -> sniffContentType returns null -> finalize must 415. (A real ZIP declared as
// png would NOT 415 -- it resolves to its true application/zip, an allowed type; the sniffer stores
// the truth rather than the client's lie. Rejection requires bytes that match nothing allowed.)
const GARBAGE_BYTES = Buffer.from([0x00, 0x01, 0x02, 0xff, 0xfe, 0x00, 0x13, 0x37, 0x00, 0x42]);

const suffix = randomUUID().slice(0, 8);
const orgId = randomUUID();
const projectId = randomUUID();
const uploaderId = randomUUID();

/**
 * PUT bytes straight to a presigned URL, exactly as the browser does. Deliberately sends NO
 * Content-Type header: the presigned URL signs only host+key (see storage.ts), so any extra
 * signed-class header (content-type) present but not in the signature is a MinIO "headers present
 * … which were not signed" rejection. The browser UploadZone likewise PUTs a type-stripped Blob.
 */
async function putToPresignedUrl(url: string, bytes: Buffer): Promise<void> {
	// A type-stripped Blob (empty type) so fetch sends no Content-Type header -- mirrors the
	// browser UploadZone, which PUTs `new Blob([file])` for the same signing reason.
	const res = await fetch(url, { method: 'PUT', body: new Blob([new Uint8Array(bytes)]) });
	if (!res.ok) {
		throw new Error(`presigned PUT failed: ${res.status} ${await res.text().catch(() => '')}`);
	}
}

describe.skipIf(!ready)('Manual issues M6 presigned upload flow (integration, real Postgres + S3)', () => {
	let sql: ReturnType<typeof postgres>;

	beforeAll(async () => {
		sql = postgres(dbConnectionString, { connect_timeout: 5, max: 1 });

		await sql`
			insert into organizations (id, name, slug)
			values (${orgId}, ${'m6-presign-' + suffix}, ${'m6-presign-' + suffix})
		`;
		await sql`
			insert into projects (id, name, organization_id, api_key, api_key_hash)
			values (${projectId}, ${'m6-presign-' + suffix}, ${orgId}, ${'k_' + suffix}, ${'h_' + suffix})
		`;
		await sql`
			insert into "user" (id, name, email)
			values (${uploaderId}, ${'Uploader ' + suffix}, ${'presign-' + suffix + '@example.test'})
		`;
	});

	afterAll(async () => {
		if (!sql) return;
		try {
			await sql`delete from attachments where org_id = ${orgId}`;
			await sql`delete from issue_activity where issue_id in (select id from issues where project_id = ${projectId})`;
			await sql`delete from manual_issue_reports where issue_id in (select id from issues where project_id = ${projectId})`;
			await sql`delete from issues where project_id = ${projectId}`;
			await sql`delete from projects where organization_id = ${orgId}`;
			await sql`delete from "user" where id = ${uploaderId}`;
			await sql`delete from organizations where id = ${orgId}`;
		} finally {
			await sql.end({ timeout: 5 }).catch(() => {});
		}
	});

	it('presign -> direct PUT -> finalize -> ready -> claim, with the pending-cannot-link gate held', async () => {
		const { createPresignedAttachment, finalizePresignedAttachment } = await import(
			'$lib/server/upload-core'
		);
		const { createManualIssue } = await import('./reports');
		const { isStorageConfigured } = await import('$lib/server/storage');

		expect(isStorageConfigured()).toBe(true);

		// --- Step 1: presign creates a 'pending' row + a working PUT URL. ---
		const presigned = await createPresignedAttachment({
			organizationId: orgId,
			uploaderId,
			filename: 'big-video.png', // PNG bytes here; the point is the presign->finalize path, not the codec
			declaredContentType: 'image/png',
			sizeBytes: PNG_BYTES.length,
		});
		expect(presigned.attachmentId).toBeTruthy();
		expect(presigned.uploadUrl).toContain('http');

		const [pendingRow] = await sql`
			select status, issue_id, comment_id from attachments where id = ${presigned.attachmentId}
		`;
		expect(pendingRow.status).toBe('pending');
		expect(pendingRow.issue_id).toBeNull();

		// --- Step 2: PUT the bytes straight to the bucket, then finalize. ---
		await putToPresignedUrl(presigned.uploadUrl, PNG_BYTES);

		const finalized = await finalizePresignedAttachment({
			attachmentId: presigned.attachmentId,
			organizationId: orgId,
			uploaderId,
		});
		expect(finalized.contentType).toBe('image/png');
		expect(finalized.sizeBytes).toBe(PNG_BYTES.length); // REAL object size, not the declared one

		const [readyRow] = await sql`
			select status from attachments where id = ${presigned.attachmentId}
		`;
		expect(readyRow.status).toBe('ready');

		// --- Step 3: a 'ready' attachment claims onto a report. ---
		const created = await createManualIssue({
			organizationId: orgId,
			projectId,
			reporterId: uploaderId,
			title: `M6 presign flow ${suffix}`,
			bodyMd: 'See the attached large upload.',
			severity: 'medium',
			attachmentIds: [presigned.attachmentId],
		});
		const [linkedRow] = await sql`
			select issue_id from attachments where id = ${presigned.attachmentId}
		`;
		expect(linkedRow.issue_id).toBe(created.issue.id);

		// --- Step 4: LOAD-BEARING NEGATIVE -- a pending (never-finalized) attachment, whose bytes
		//     really are in the bucket, must NOT be linkable via report creation. ---
		const pending2 = await createPresignedAttachment({
			organizationId: orgId,
			uploaderId,
			filename: 'never-finalized.png',
			declaredContentType: 'image/png',
			sizeBytes: PNG_BYTES.length,
		});
		await putToPresignedUrl(pending2.uploadUrl, PNG_BYTES); // bytes exist, but no finalize

		const created2 = await createManualIssue({
			organizationId: orgId,
			projectId,
			reporterId: uploaderId,
			title: `M6 presign gate ${suffix}`,
			bodyMd: 'This attachment id is still pending and must be refused.',
			severity: 'low',
			attachmentIds: [pending2.attachmentId],
		});
		const [stillPending] = await sql`
			select status, issue_id from attachments where id = ${pending2.attachmentId}
		`;
		expect(stillPending.status).toBe('pending');
		expect(stillPending.issue_id).toBeNull(); // NOT linked onto created2's issue

		const gateActivity = await sql`
			select event_type from issue_activity
			where issue_id = ${created2.issue.id} and event_type = 'attachment_added'
		`;
		expect(gateActivity.length).toBe(0); // no attachment_added activity for a refused pending draft
	});

	it('rejects unrecognized bytes at finalize (415) and deletes the bucket object + row', async () => {
		const { createPresignedAttachment, finalizePresignedAttachment } = await import(
			'$lib/server/upload-core'
		);
		const { getObjectStream } = await import('$lib/server/storage');

		const presigned = await createPresignedAttachment({
			organizationId: orgId,
			uploaderId,
			filename: 'liar.png',
			declaredContentType: 'image/png',
			sizeBytes: GARBAGE_BYTES.length,
		});
		const [row] = await sql`select storage_key from attachments where id = ${presigned.attachmentId}`;
		const storageKey = row.storage_key as string;

		await putToPresignedUrl(presigned.uploadUrl, GARBAGE_BYTES);

		// finalize sniffs the REAL bytes (a ZIP), which do not resolve to the declared image/png,
		// so it must throw a 415 and clean up.
		await expect(
			finalizePresignedAttachment({
				attachmentId: presigned.attachmentId,
				organizationId: orgId,
				uploaderId,
			})
		).rejects.toMatchObject({ status: 415 });

		// Row deleted...
		const remaining = await sql`select id from attachments where id = ${presigned.attachmentId}`;
		expect(remaining.length).toBe(0);

		// ...and the object gone from the bucket (getObjectStream now 404s).
		await expect(getObjectStream(storageKey)).rejects.toBeTruthy();
	});
});
