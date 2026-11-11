import { it, expect, vi, beforeEach, describe } from 'vitest';

// Addressing-style guard: the client used to hard-code `forcePathStyle: true` ("MinIO needs it,
// harmless on real S3") and the first production sourcemap upload failed 4/4 against K8s OBS
// with "VirtualHostDomainRequired: Virtual host domain is required while accessing a specific
// bucket" (sentinel-production, 2026-09-17). The style is now derived from the endpoint host, with
// S3_FORCE_PATH_STYLE as an explicit override. Pinned here without MinIO: the pure rule is tested
// directly, the presign client through the URL it signs (path-style puts the bucket in the path,
// virtual-hosted puts it in the host), and the server-side client through the config it is built
// with, because a wrong style there only shows up as a remote 4xx at runtime.

const mockEnv: Record<string, string | undefined> = {};
vi.mock('$env/dynamic/private', () => ({ env: mockEnv }));

// Wrap the real S3Client so we can see what it was constructed with. `send` is stubbed so no
// server-side call ever leaves the process; getSignedUrl does not go through `send`, so presigning
// still exercises the real SDK endpoint/bucket resolution.
const constructed: Array<Record<string, unknown>> = [];
vi.mock('@aws-sdk/client-s3', async (importOriginal) => {
	const actual = await importOriginal<typeof import('@aws-sdk/client-s3')>();
	class RecordingS3Client extends actual.S3Client {
		constructor(config: import('@aws-sdk/client-s3').S3ClientConfig) {
			super(config);
			constructed.push(config as Record<string, unknown>);
		}
		send = vi.fn(async () => ({}));
	}
	return { ...actual, S3Client: RecordingS3Client };
});

const OBS = 'https://obs.example.com';
const MINIO = 'http://minio:9000';

beforeEach(() => {
	constructed.length = 0;
	mockEnv.S3_BUCKET = 'sentinel-attachments';
	mockEnv.S3_ACCESS_KEY = 'minioadmin';
	mockEnv.S3_SECRET_KEY = 'minioadmin';
	mockEnv.S3_PUBLIC_ENDPOINT = undefined;
	mockEnv.S3_FORCE_PATH_STYLE = undefined;
});

describe('usePathStyle', () => {
	it('is path-style for a bare hostname, localhost or an IP literal (MinIO in compose/dev/tests)', async () => {
		const { usePathStyle } = await import('$lib/server/storage');
		expect(usePathStyle(MINIO)).toBe(true);
		expect(usePathStyle('http://localhost:9000')).toBe(true);
		expect(usePathStyle('http://127.0.0.1:9000')).toBe(true);
		expect(usePathStyle('http://[::1]:9000')).toBe(true);
	});

	it('is virtual-hosted for a dotted hostname (K8s OBS, AWS S3, R2)', async () => {
		const { usePathStyle } = await import('$lib/server/storage');
		expect(usePathStyle(OBS)).toBe(false);
		expect(usePathStyle('https://s3.us-east-1.amazonaws.com')).toBe(false);
		expect(usePathStyle('https://uploads.example.test:9000')).toBe(false);
	});

	it('honours S3_FORCE_PATH_STYLE in both directions and rejects a value that is neither', async () => {
		const { usePathStyle } = await import('$lib/server/storage');
		expect(usePathStyle(OBS, 'true')).toBe(true);
		expect(usePathStyle(OBS, '1')).toBe(true);
		expect(usePathStyle(MINIO, 'false')).toBe(false);
		expect(usePathStyle(MINIO, ' FALSE ')).toBe(false);
		expect(usePathStyle(MINIO, '')).toBe(true); // empty is unset, not an error
		expect(() => usePathStyle(MINIO, 'yes')).toThrow(/S3_FORCE_PATH_STYLE/);
	});

	it('rejects an endpoint that is not a URL', async () => {
		const { usePathStyle } = await import('$lib/server/storage');
		expect(() => usePathStyle('minio:9000')).toThrow(/S3_ENDPOINT/); // no scheme -> not a host
	});
});

describe('server-side client', () => {
	it('is built path-style against minio:9000', async () => {
		mockEnv.S3_ENDPOINT = MINIO;
		const { putObject } = await import('$lib/server/storage');
		await putObject('org/o/k', new Uint8Array([1]), 'application/octet-stream');
		expect(constructed.at(-1)).toMatchObject({ endpoint: MINIO, forcePathStyle: true });
	});

	it('is built virtual-hosted against obs.example.com', async () => {
		mockEnv.S3_ENDPOINT = OBS;
		const { putObject } = await import('$lib/server/storage');
		await putObject('org/o/k', new Uint8Array([1]), 'application/octet-stream');
		expect(constructed.at(-1)).toMatchObject({ endpoint: OBS, forcePathStyle: false });
	});

	it('S3_FORCE_PATH_STYLE overrides the host rule both ways', async () => {
		const { putObject } = await import('$lib/server/storage');

		mockEnv.S3_ENDPOINT = OBS;
		mockEnv.S3_FORCE_PATH_STYLE = 'true';
		await putObject('org/o/k', new Uint8Array([1]), 'application/octet-stream');
		expect(constructed.at(-1)).toMatchObject({ endpoint: OBS, forcePathStyle: true });

		mockEnv.S3_ENDPOINT = MINIO;
		mockEnv.S3_FORCE_PATH_STYLE = 'false';
		await putObject('org/o/k', new Uint8Array([1]), 'application/octet-stream');
		expect(constructed.at(-1)).toMatchObject({ endpoint: MINIO, forcePathStyle: false });
	});
});

describe('presigned PUT URLs', () => {
	it('put the bucket in the path against MinIO', async () => {
		mockEnv.S3_ENDPOINT = MINIO;
		const { createPresignedPutUrl } = await import('$lib/server/storage');
		const url = await createPresignedPutUrl('org/o/k', 900);
		expect(url.startsWith('http://minio:9000/sentinel-attachments/org/o/k?')).toBe(true);
	});

	it('put the bucket in the host against OBS (virtual-hosted), following the public endpoint', async () => {
		mockEnv.S3_ENDPOINT = MINIO; // internal endpoint is irrelevant to the signed URL
		mockEnv.S3_PUBLIC_ENDPOINT = OBS;
		const { createPresignedPutUrl } = await import('$lib/server/storage');
		const url = await createPresignedPutUrl('org/o/k', 900);
		expect(
			url.startsWith('https://sentinel-attachments.obs.example.com/org/o/k?')
		).toBe(true);
		expect(url).toContain('X-Amz-Signature');
	});

	it('follow S3_FORCE_PATH_STYLE=true even against a dotted host', async () => {
		mockEnv.S3_ENDPOINT = OBS;
		mockEnv.S3_FORCE_PATH_STYLE = 'true';
		const { createPresignedPutUrl } = await import('$lib/server/storage');
		const url = await createPresignedPutUrl('org/o/k', 900);
		expect(
			url.startsWith('https://obs.example.com/sentinel-attachments/org/o/k?')
		).toBe(true);
	});
});
