import { it, expect, vi, beforeEach } from 'vitest';

// M6 Feature A correctness guard: a presigned PUT URL is handed to the BROWSER, so it must be
// signed against the browser-reachable `S3_PUBLIC_ENDPOINT`, never the internal `S3_ENDPOINT` the
// server uses (a docker-network hostname / private VPC endpoint the browser cannot resolve). This
// bug shipped once because the integration test ran on the host where both endpoints happen to be
// localhost -- so it is pinned here where they are deliberately different. getSignedUrl makes no
// network call, so this needs no MinIO.
//
// storage.ts reads config fresh from `$env/dynamic/private` on every call, so (mirroring
// email.test.ts) we mock that module with a mutable object and vary it per test. The `mock`-prefixed
// name is what lets it be referenced inside the hoisted vi.mock factory.
const mockEnv: Record<string, string | undefined> = {};
vi.mock('$env/dynamic/private', () => ({ env: mockEnv }));

beforeEach(() => {
	mockEnv.S3_ENDPOINT = 'http://minio-internal:9000';
	mockEnv.S3_PUBLIC_ENDPOINT = 'http://uploads.example.test:9000';
	mockEnv.S3_BUCKET = 'sentinel-attachments';
	mockEnv.S3_ACCESS_KEY = 'minioadmin';
	mockEnv.S3_SECRET_KEY = 'minioadmin';
});

it('signs presigned PUT URLs against S3_PUBLIC_ENDPOINT, not the internal S3_ENDPOINT', async () => {
	const { createPresignedPutUrl } = await import('$lib/server/storage');

	const url = await createPresignedPutUrl('org/org-1/some-key', 900);

	expect(url).toContain('uploads.example.test:9000'); // browser-reachable public host
	expect(url).not.toContain('minio-internal'); // never the in-cluster host
	expect(url).toContain('X-Amz-Signature'); // it really is a signed URL
});

it('falls back to S3_ENDPOINT when S3_PUBLIC_ENDPOINT is unset (dev/host case)', async () => {
	mockEnv.S3_PUBLIC_ENDPOINT = undefined;
	// readConfig re-reads env on every call and the presign client re-caches whenever the resolved
	// public endpoint changes, so no module reset is needed -- the cache key simply changes.
	const { createPresignedPutUrl } = await import('$lib/server/storage');
	const url = await createPresignedPutUrl('org/org-1/some-key', 900);

	expect(url).toContain('minio-internal:9000'); // falls back to the server-side endpoint
});
