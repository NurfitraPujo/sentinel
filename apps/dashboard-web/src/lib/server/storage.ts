import {
	S3Client,
	PutObjectCommand,
	GetObjectCommand,
	DeleteObjectCommand,
	HeadObjectCommand,
} from '@aws-sdk/client-s3';
import type { GetObjectCommandOutput } from '@aws-sdk/client-s3';
import { getSignedUrl } from '@aws-sdk/s3-request-presigner';
import { env } from '$env/dynamic/private';

/**
 * Manual Issues M2 (docs/plans/MANUAL_ISSUES_DESIGN.md §4): a thin S3-compatible client wrapper
 * around MinIO (dev/prod-compose) or real S3/R2 in a hosted deployment -- `forcePathStyle` is
 * required for MinIO's virtual-host-style addressing not being set up, and is harmless against
 * real S3. Config comes entirely from env, mirroring email.ts's EMAIL_SERVER pattern:
 * `isStorageConfigured()` is the same "feature present or not" guard as email.ts's
 * `if (!emailServer)` check, just promoted to a named export since two call sites (uploads,
 * attachments) both need to ask the question.
 */

let cachedClient: S3Client | null = null;
let cachedEndpoint: string | null = null;
let cachedPresignClient: S3Client | null = null;
let cachedPresignEndpoint: string | null = null;

export interface StorageConfig {
	/** Server-side endpoint: where THIS process reaches the bucket (e.g. `http://minio:9000`). */
	endpoint: string;
	/**
	 * Browser-reachable endpoint used to SIGN presigned URLs. A presigned URL is handed to the
	 * client, so it must resolve from the browser -- which the internal `endpoint` (a docker-network
	 * hostname or a private VPC S3 endpoint) generally does not. Falls back to `endpoint` for the
	 * dev/host case where the two are the same. The SigV4 signature covers the host, so this must be
	 * baked into the signing client's endpoint -- rewriting the host after signing would invalidate
	 * the signature.
	 */
	publicEndpoint: string;
	bucket: string;
	accessKeyId: string;
	secretAccessKey: string;
}

function readConfig(): StorageConfig | null {
	const endpoint = env.S3_ENDPOINT;
	const bucket = env.S3_BUCKET;
	const accessKeyId = env.S3_ACCESS_KEY;
	const secretAccessKey = env.S3_SECRET_KEY;

	if (!endpoint || !bucket || !accessKeyId || !secretAccessKey) {
		return null;
	}

	const publicEndpoint = env.S3_PUBLIC_ENDPOINT || endpoint;

	return { endpoint, publicEndpoint, bucket, accessKeyId, secretAccessKey };
}

/** Whether object storage is configured at all -- mirrors email.ts's isSmtpConfigured shape. */
export function isStorageConfigured(): boolean {
	return readConfig() !== null;
}

function buildClient(config: StorageConfig, endpoint: string): S3Client {
	return new S3Client({
		endpoint,
		region: 'us-east-1', // MinIO ignores this; required by the SDK's client shape regardless.
		forcePathStyle: true,
		// AWS SDK v3 (>= ~3.729) injects a default integrity checksum
		// (`x-amz-sdk-checksum-algorithm` + `x-amz-checksum-crc32`) into write requests. On a
		// presigned PUT those become signed expectations that MinIO rejects as "headers present
		// in the request which were not signed" when the browser PUTs the raw bytes. Scoping
		// checksums to WHEN_REQUIRED keeps normal server-side puts/gets working while leaving the
		// presigned PUT signature to just host+key -- validation is by magic bytes at finalize.
		requestChecksumCalculation: 'WHEN_REQUIRED',
		responseChecksumValidation: 'WHEN_REQUIRED',
		credentials: {
			accessKeyId: config.accessKeyId,
			secretAccessKey: config.secretAccessKey,
		},
	});
}

/** Server-side client (internal `endpoint`), for puts/gets/heads/deletes this process makes. */
function getClient(config: StorageConfig): S3Client {
	if (!cachedClient || cachedEndpoint !== config.endpoint) {
		cachedClient = buildClient(config, config.endpoint);
		cachedEndpoint = config.endpoint;
	}
	return cachedClient;
}

/**
 * Signing client for presigned URLs, pinned to the browser-reachable `publicEndpoint`. Kept
 * separate from the server-side client so a presigned URL never embeds the internal endpoint the
 * browser cannot reach. When the two endpoints are equal (dev/host) this is just a second client
 * with identical config.
 */
function getPresignClient(config: StorageConfig): S3Client {
	if (!cachedPresignClient || cachedPresignEndpoint !== config.publicEndpoint) {
		cachedPresignClient = buildClient(config, config.publicEndpoint);
		cachedPresignEndpoint = config.publicEndpoint;
	}
	return cachedPresignClient;
}

function requireConfig(): StorageConfig {
	const config = readConfig();
	if (!config) {
		throw new Error('Object storage is not configured (S3_ENDPOINT/BUCKET/ACCESS_KEY/SECRET_KEY)');
	}
	return config;
}

export async function putObject(
	key: string,
	body: Uint8Array | Buffer,
	contentType: string
): Promise<void> {
	const config = requireConfig();
	const client = getClient(config);

	await client.send(
		new PutObjectCommand({
			Bucket: config.bucket,
			Key: key,
			Body: body,
			ContentType: contentType,
		})
	);
}

/**
 * Returns the raw S3 GetObject response so callers can stream `.Body` straight into a SvelteKit
 * `Response` instead of buffering the whole object in memory twice.
 */
export async function getObjectStream(key: string): Promise<GetObjectCommandOutput> {
	const config = requireConfig();
	const client = getClient(config);

	return await client.send(
		new GetObjectCommand({
			Bucket: config.bucket,
			Key: key,
		})
	);
}

export async function deleteObject(key: string): Promise<void> {
	const config = requireConfig();
	const client = getClient(config);

	await client.send(
		new DeleteObjectCommand({
			Bucket: config.bucket,
			Key: key,
		})
	);
}

/**
 * M6 Feature A (docs/plans/M6_PRESIGNED_UPLOADS_AND_TOOLBAR_PLAN.md §Feature A): presigned PUT
 * URL for the direct-to-bucket large-upload path. The client PUTs bytes straight to the bucket
 * with this URL; the server never sees them until finalize's ranged GET, so nothing here can
 * substitute for the sniff-and-validate step that happens at finalize.
 *
 * Content-Type is deliberately NOT part of the signed command: signing it forces the client to
 * echo it back byte-for-byte and makes any extra request header an S3 SigV4 "headers present …
 * which were not signed" rejection (MinIO enforces this strictly). It would buy nothing here --
 * the stored object's content type is never trusted or served; finalize overwrites the DB
 * `content_type` with the magic-byte-sniffed value, and the download route serves THAT. So only
 * the bucket + key are signed, letting the browser PUT with any headers.
 */
export async function createPresignedPutUrl(
	key: string,
	expiresSeconds: number
): Promise<string> {
	const config = requireConfig();
	const client = getPresignClient(config); // browser-reachable endpoint, NOT the internal one

	return await getSignedUrl(
		client,
		new PutObjectCommand({
			Bucket: config.bucket,
			Key: key,
		}),
		{ expiresIn: expiresSeconds }
	);
}

/**
 * Real object size as reported by the bucket, used to re-check the cap after a direct-to-bucket
 * PUT (the client's declared `sizeBytes` at presign time is untrusted).
 */
export async function headObject(key: string): Promise<{ contentLength: number }> {
	const config = requireConfig();
	const client = getClient(config);

	const result = await client.send(
		new HeadObjectCommand({
			Bucket: config.bucket,
			Key: key,
		})
	);

	return { contentLength: result.ContentLength ?? 0 };
}

/**
 * First `length` bytes of an object, buffered, for magic-byte sniffing at finalize -- mirrors the
 * proxy path's `sniffContentType(buffer)` without downloading the whole object.
 *
 * Deliberately a FULL GetObject whose stream we stop after the first `length` bytes, NOT an HTTP
 * `Range` request. A ranged GetObject is mis-signed by the SDK's *browser* build against MinIO
 * ("headers present … which were not signed"), and vitest forces that browser build on us (the
 * 'browser' resolve condition for Svelte's sake -- see the attachment flow integration test). A
 * plain GetObject signs correctly under both builds; we simply break out of the stream once we
 * have enough bytes and destroy it, so only the first chunk actually crosses the wire regardless
 * of how large the object is.
 */
export async function getObjectRangeBytes(key: string, length: number): Promise<Buffer> {
	const config = requireConfig();
	const client = getClient(config);

	const result = await client.send(
		new GetObjectCommand({
			Bucket: config.bucket,
			Key: key,
		})
	);

	const chunks: Uint8Array[] = [];
	let total = 0;
	const body = result.Body as unknown as AsyncIterable<Uint8Array>;
	for await (const chunk of body) {
		chunks.push(chunk);
		total += chunk.length;
		if (total >= length) break;
	}
	// Stop the remaining transfer (Node streams expose destroy(); other body shapes may not).
	try {
		(result.Body as unknown as { destroy?: () => void }).destroy?.();
	} catch {
		// best-effort -- the request completes on its own if it cannot be aborted
	}
	return Buffer.concat(chunks).subarray(0, length);
}
