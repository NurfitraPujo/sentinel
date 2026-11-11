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
 * around MinIO (dev/prod-compose) or real S3/R2/OBS in a hosted deployment. Addressing style is
 * decided per endpoint by `usePathStyle` (see it for the rule and the `S3_FORCE_PATH_STYLE`
 * override): MinIO has no virtual-host DNS and needs path-style, while K8s OBS REJECTS
 * path-style ("VirtualHostDomainRequired") -- it was hard-coded to path-style until the first
 * production sourcemap upload failed 4/4 on exactly that. Config comes entirely from env,
 * mirroring email.ts's EMAIL_SERVER pattern:
 * `isStorageConfigured()` is the same "feature present or not" guard as email.ts's
 * `if (!emailServer)` check, just promoted to a named export since two call sites (uploads,
 * attachments) both need to ask the question.
 */

let cachedClient: S3Client | null = null;
let cachedEndpoint: string | null = null; // a clientCacheKey, not a bare endpoint
let cachedPresignClient: S3Client | null = null;
let cachedPresignEndpoint: string | null = null; // likewise

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
	/**
	 * Raw `S3_FORCE_PATH_STYLE` value, if set: an explicit addressing-style override that wins over
	 * the host-shape rule of `usePathStyle`. Kept raw (not parsed) so `isStorageConfigured()` never
	 * throws on a typo; the client builder parses it and names the variable when it cannot.
	 */
	forcePathStyle?: string;
}

/**
 * Whether an endpoint should be addressed path-style (`http://host/bucket/key`) or virtual-hosted
 * (`https://bucket.host/key`). The rule is by host shape, so nothing in a deployment has to say
 * which flavour of store it is:
 *
 * - a bare hostname with no dots (`minio`, `localhost`), or an IP literal (`127.0.0.1`, `[::1]`)
 *   -> path-style. That is MinIO in compose/dev/tests, where `bucket.minio` resolves nowhere.
 * - anything with a dot (`obs.example.com`, `s3.us-east-1.amazonaws.com`)
 *   -> virtual-hosted, which every hosted S3-compatible store supports and K8s OBS requires.
 *
 * `override` is `S3_FORCE_PATH_STYLE` and wins when set: `true`/`1` forces path-style,
 * `false`/`0` forces virtual-hosted. Any other non-empty value is an error naming the variable
 * rather than a silent fallback. Exported for its unit test; apps/processor-go/symbolicate's
 * `UsePathStyle` is the same rule for the read side and the two must not drift (the processor
 * reads what this module wrote, through the same endpoint).
 */
export function usePathStyle(endpoint: string, override?: string): boolean {
	const raw = override?.trim().toLowerCase();
	if (raw) {
		if (raw === 'true' || raw === '1') return true;
		if (raw === 'false' || raw === '0') return false;
		throw new Error(`S3_FORCE_PATH_STYLE must be true or false, got ${JSON.stringify(override)}`);
	}
	let hostname = '';
	try {
		hostname = new URL(endpoint).hostname; // `minio:9000` parses as scheme `minio:` with NO host
	} catch {
		// fall through to the empty-hostname error below
	}
	if (!hostname) {
		throw new Error(`S3_ENDPOINT must be a URL with a scheme and host, got ${JSON.stringify(endpoint)}`);
	}
	// URL.hostname keeps IPv6 brackets (`[::1]`), and IPv4 is "dotted" but not a DNS name.
	if (hostname.startsWith('[') || /^\d{1,3}(\.\d{1,3}){3}$/.test(hostname)) return true;
	return !hostname.includes('.');
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
	const forcePathStyle = env.S3_FORCE_PATH_STYLE;

	return { endpoint, publicEndpoint, bucket, accessKeyId, secretAccessKey, forcePathStyle };
}

/** Whether object storage is configured at all -- mirrors email.ts's isSmtpConfigured shape. */
export function isStorageConfigured(): boolean {
	return readConfig() !== null;
}

/**
 * Identity of a built client for the two caches below: the endpoint AND the addressing style it
 * was built with, so a changed `S3_FORCE_PATH_STYLE` (tests vary it per case; a process reads it
 * once) can never keep serving a client built for the other style.
 */
function clientCacheKey(config: StorageConfig, endpoint: string): string {
	return `${endpoint}|${usePathStyle(endpoint, config.forcePathStyle) ? 'path' : 'virtual-hosted'}`;
}

function buildClient(config: StorageConfig, endpoint: string): S3Client {
	return new S3Client({
		endpoint,
		region: 'us-east-1', // MinIO ignores this; required by the SDK's client shape regardless.
		// Decided per endpoint, so the presign client (public endpoint) and the server-side client
		// (internal endpoint) can each land on the style THEIR host needs.
		forcePathStyle: usePathStyle(endpoint, config.forcePathStyle),
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
	const key = clientCacheKey(config, config.endpoint);
	if (!cachedClient || cachedEndpoint !== key) {
		cachedClient = buildClient(config, config.endpoint);
		cachedEndpoint = key;
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
	const key = clientCacheKey(config, config.publicEndpoint);
	if (!cachedPresignClient || cachedPresignEndpoint !== key) {
		cachedPresignClient = buildClient(config, config.publicEndpoint);
		cachedPresignEndpoint = key;
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

// sentinel-issue-tag-facets §5 rule 3: `signal` (the retention run's deadline) aborts the request.
export async function deleteObject(key: string, signal?: AbortSignal): Promise<void> {
	const config = requireConfig();
	const client = getClient(config);

	await client.send(
		new DeleteObjectCommand({
			Bucket: config.bucket,
			Key: key,
		}),
		signal ? { abortSignal: signal } : undefined
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
