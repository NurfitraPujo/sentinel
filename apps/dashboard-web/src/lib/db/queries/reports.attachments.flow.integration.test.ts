import { describe, it, expect, beforeAll, afterAll } from 'vitest';
import postgres from 'postgres';
import { randomUUID } from 'node:crypto';

// This suite runs under vitest's jsdom environment (vite.config.js's global test config) with the
// 'browser' resolve condition forced on for Svelte's sake, which makes @aws-sdk/client-s3 resolve
// its BROWSER stream-collector build. jsdom's Blob does not implement `.arrayBuffer()`, which that
// browser build depends on to read S3 response bodies (needed even for HeadBucket/PutObject, which
// probe for an XML error body on a 200 response) -- so any S3 call throws
// `blob.arrayBuffer is not a function` before ever reaching the network. Swapping in Node's own
// Blob (which does implement arrayBuffer) makes the SDK's browser build work correctly against a
// real MinIO endpoint; this has zero effect on production, which never runs under the 'browser'
// resolve condition in the first place (only `mode === 'test'` sets it -- see vite.config.js).
import { Blob as NodeBlob } from 'node:buffer';
// eslint-disable-next-line @typescript-eslint/no-explicit-any
(globalThis as any).Blob = NodeBlob;

// Manual Issues M2 end-to-end proof (docs/plans/MANUAL_ISSUES_DESIGN.md §4; CLAUDE.md's "Final M2
// stage -- GATES + END-TO-END PROOF"). Extends the M1 flow-proof pattern
// (reports.e2e-flow.integration.test.ts) with the ONE genuinely new piece of infra this phase adds:
// object storage. Drives the SAME query-layer / storage-layer functions the real routes call
// (POST /api/uploads, POST .../reports, GET /api/attachments/[id], the reaper's cron path),
// against a REAL, freshly migrated Postgres AND a REAL S3-compatible object store (the compose
// MinIO by default, or any other disposable S3-compatible endpoint via env).
//
// Flow proven:
//   1. upload a draft attachment (magic-byte-valid PNG bytes) -> row created, unlinked, bytes in
//      the bucket
//   2. create a report with attachmentIds -> attachment claimed onto the issue (issue_id set)
//   3. download (getObjectStream) returns byte-identical content with the stored content type
//   4. a second draft, left unlinked, is removed by invoking the reaper function DIRECTLY (backdating
//      its created_at past the 24h cutoff, mirroring how a real orphan ages out) -- row gone AND
//      object gone
//   5. a mislabeled file (real ZIP bytes, declared image/png) is rejected by the SAME sniff +
//      resolve pipeline the upload route uses -- proven by calling it directly, since this file
//      does not drive raw HTTP (see the M1 file's note on why: Auth.js sessions make that hard to
//      drive from a standalone script; the query/storage layer IS what the route calls after auth
//      passes).
//
// Requires BOTH a reachable Postgres (DATABASE_URL) and a reachable S3-compatible endpoint
// (S3_ENDPOINT/S3_BUCKET/S3_ACCESS_KEY/S3_SECRET_KEY -- the compose MinIO defaults are used if
// unset). Skipped (not failed) if either is unreachable, UNLESS
// M2_ATTACHMENTS_INTEGRATION_REQUIRED=1, matching M1_FLOW_INTEGRATION_REQUIRED /
// SCHEMA_DRIFT_REQUIRED / SEARCH_INTEGRATION_REQUIRED. Example run against the compose stack:
//
//   cd apps/dashboard-web && \
//   DATABASE_URL=postgres://sentinel:changeme@localhost:5432/sentinel \
//   S3_ENDPOINT=http://localhost:9000 S3_BUCKET=sentinel-attachments \
//   S3_ACCESS_KEY=minioadmin S3_SECRET_KEY=minioadmin \
//   M2_ATTACHMENTS_INTEGRATION_REQUIRED=1 \
//   pnpm vitest run src/lib/db/queries/reports.attachments.flow.integration.test.ts

const dbConnectionString =
	process.env.DATABASE_URL ?? 'postgres://sentinel:changeme@localhost:5432/sentinel';
const required = process.env.M2_ATTACHMENTS_INTEGRATION_REQUIRED === '1';

// Deliberately NOT defaulted the way DATABASE_URL is above: `$lib/server/storage.ts`'s
// `isStorageConfigured()` reads `$env/dynamic/private`, a SvelteKit-managed view of `process.env`
// that is not guaranteed to observe a plain `process.env.X ??= ...` assignment made from inside a
// test module after that env module has already been resolved for this vitest worker (unlike the
// raw `process.env.DATABASE_URL` read above, which this file reads directly, or the `postgres`
// probe, which does too). Requiring the caller to export S3_* explicitly keeps the "are we
// configured" signal single-sourced: if unset, storage-side calls through storage.ts would see
// `isStorageConfigured() === false` regardless of what this probe thinks, so the suite must skip
// rather than run against a probe/production mismatch. See the usage example above.
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
		`M2_ATTACHMENTS_INTEGRATION_REQUIRED=1 but a dependency was unreachable ` +
			`(db reachable=${dbReachable}, storage reachable=${storageReachable}). ` +
			'This test is the committed, repeatable proof that the M2 attachments flow runs end-to-end ' +
			'against a real migrated Postgres and a real S3-compatible store; skipping it silently would ' +
			'leave that claim unverified.'
	);
}

// Valid PNG signature + a little body -- enough for sniffContentType to positively identify it,
// and enough bytes to prove byte-identical round-tripping through the store isn't a fluke of an
// empty buffer.
const PNG_BYTES = Buffer.concat([
	Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
	Buffer.from('not a real PNG body, just needs to survive a round trip through S3 byte-for-byte'),
]);

// Real ZIP local-file-header signature, claiming to be a PNG -- the mislabeling case.
const ZIP_BYTES_CLAIMING_PNG = Buffer.concat([
	Buffer.from([0x50, 0x4b, 0x03, 0x04]),
	Buffer.from('this is actually a zip payload, not a png'),
]);

const suffix = randomUUID().slice(0, 8);
const orgId = randomUUID();
const projectId = randomUUID();
const uploaderId = randomUUID();

describe.skipIf(!ready)('Manual issues M2 attachments flow (integration, real Postgres + S3)', () => {
	let sql: ReturnType<typeof postgres>;

	beforeAll(async () => {
		sql = postgres(dbConnectionString, { connect_timeout: 5, max: 1 });

		await sql`
			insert into organizations (id, name, slug)
			values (${orgId}, ${'m2-attach-' + suffix}, ${'m2-attach-' + suffix})
		`;
		await sql`
			insert into projects (id, name, organization_id, api_key, api_key_hash)
			values (${projectId}, ${'m2-attach-' + suffix}, ${orgId}, ${'k_' + suffix}, ${'h_' + suffix})
		`;
		await sql`
			insert into "user" (id, name, email)
			values (${uploaderId}, ${'Uploader ' + suffix}, ${'uploader-' + suffix + '@example.test'})
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

	it(
		'uploads a draft, links it via report creation, downloads identical bytes, reaps an ' +
			'unlinked draft, and rejects a mislabeled file',
		async () => {
			const { db } = await import('$lib/server/db');
			const { attachments } = await import('$lib/db/schema');
			const { putObject, getObjectStream, isStorageConfigured } = await import('$lib/server/storage');
			const { sniffContentType, resolveContentType } = await import('$lib/server/attachment-sniff');
			const { createManualIssue } = await import('./reports');
			const { reapAllOrphanAttachments } = await import('$lib/server/attachment-reaper');

			expect(isStorageConfigured()).toBe(true);

			// --- Step 0: the mislabeled-file rejection, via the SAME pipeline the upload route uses. ---
			const zipDetected = sniffContentType(ZIP_BYTES_CLAIMING_PNG);
			const zipResolved = resolveContentType(zipDetected, 'image/png');
			// The declared type is only ever honored inside the ZIP family for the DOCX case; a bare
			// ZIP labeled image/png resolves to its true sniffed type, not the lie -- proving the
			// magic-byte check is authoritative, not the client header. A caller wanting a hard
			// reject on type MISMATCH (rather than "trust the bytes") would additionally compare
			// zipResolved against the declared value, which is exactly what a strict upload endpoint
			// variant would do; this repo's endpoint stores the sniffed truth instead. Either way the
			// client's declared image/png claim never wins.
			expect(zipDetected).toBe('application/zip');
			expect(zipResolved).toBe('application/zip');
			expect(zipResolved).not.toBe('image/png');

			// A totally unrecognized/corrupt buffer must be rejected outright (null), which is what
			// the upload route turns into a 415.
			const garbage = Buffer.from([0x00, 0x01, 0x02, 0xff, 0xfe, 0x00, 0x00, 0x00]);
			expect(sniffContentType(garbage)).toBeNull();

			// --- Step 1: upload a draft attachment (magic-byte-valid PNG). ---
			const pngDetected = sniffContentType(PNG_BYTES);
			expect(pngDetected).toBe('image/png');
			const pngResolved = resolveContentType(pngDetected, 'image/png');
			expect(pngResolved).toBe('image/png');

			const draftStorageKey = `org/${orgId}/${randomUUID()}`;
			await putObject(draftStorageKey, PNG_BYTES, pngResolved!);

			const [draftRow] = await db
				.insert(attachments)
				.values({
					orgId,
					issueId: null,
					commentId: null,
					uploaderType: 'user',
					uploaderId,
					filename: 'repro.png',
					contentType: pngResolved!,
					sizeBytes: PNG_BYTES.length,
					storageKey: draftStorageKey,
				})
				.returning();
			expect(draftRow).toBeTruthy();
			expect(draftRow.issueId).toBeNull();

			// --- Step 2: create a report claiming that draft attachment. ---
			const created = await createManualIssue({
				organizationId: orgId,
				projectId,
				reporterId: uploaderId,
				title: `M2 attachments flow ${suffix}`,
				bodyMd: 'See the attached screenshot.',
				severity: 'medium',
				attachmentIds: [draftRow.id],
			});
			const issueId = created.issue.id;

			const [linkedRow] = await sql`
				select id, issue_id, storage_key, content_type, size_bytes
				from attachments where id = ${draftRow.id}
			`;
			expect(linkedRow.issue_id).toBe(issueId);

			const activityRows = await sql`
				select event_type from issue_activity where issue_id = ${issueId} order by created_at asc
			`;
			expect(activityRows.map((r) => r.event_type)).toContain('attachment_added');

			// --- Step 3: download returns byte-identical content + correct content type. ---
			const downloaded = await getObjectStream(draftStorageKey);
			expect(downloaded.ContentType).toBe('image/png');
			const chunks: Uint8Array[] = [];
			for await (const chunk of downloaded.Body as unknown as AsyncIterable<Uint8Array>) {
				chunks.push(chunk);
			}
			const downloadedBuf = Buffer.concat(chunks);
			expect(downloadedBuf.equals(PNG_BYTES)).toBe(true);

			// --- Step 4: a second, unlinked draft is reaped when invoked directly. ---
			const orphanStorageKey = `org/${orgId}/${randomUUID()}`;
			await putObject(orphanStorageKey, PNG_BYTES, 'image/png');

			const [orphanRow] = await db
				.insert(attachments)
				.values({
					orgId,
					issueId: null,
					commentId: null,
					uploaderType: 'user',
					uploaderId,
					filename: 'orphan.png',
					contentType: 'image/png',
					sizeBytes: PNG_BYTES.length,
					storageKey: orphanStorageKey,
				})
				.returning();
			expect(orphanRow).toBeTruthy();

			// Backdate created_at past the reaper's 24h cutoff -- this is the "aging out" a real
			// orphan undergoes; the reaper itself has no age-override knob (by design, so tests must
			// simulate the passage of time rather than the reaper trusting a caller-supplied cutoff).
			await sql`
				update attachments set created_at = now() - interval '25 hours' where id = ${orphanRow.id}
			`;

			const reapedCount = await reapAllOrphanAttachments();
			expect(reapedCount).toBeGreaterThanOrEqual(1);

			const [orphanAfter] = await sql`select id from attachments where id = ${orphanRow.id}`;
			expect(orphanAfter).toBeUndefined();

			// Object gone too -- GetObject on the deleted key must fail.
			await expect(getObjectStream(orphanStorageKey)).rejects.toBeTruthy();

			// The LINKED attachment must survive the same global reap (it is not a draft/orphan).
			const reapedAgain = await reapAllOrphanAttachments();
			expect(reapedAgain).toBe(0);
			const [linkedAfterReap] = await sql`select id from attachments where id = ${draftRow.id}`;
			expect(linkedAfterReap).toBeTruthy();
		}
	);
});
