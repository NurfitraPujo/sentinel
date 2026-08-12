import {
	S3Client,
	PutObjectCommand,
	GetObjectCommand,
	DeleteObjectCommand,
} from '@aws-sdk/client-s3';
import type { GetObjectCommandOutput } from '@aws-sdk/client-s3';
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

export interface StorageConfig {
	endpoint: string;
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

	return { endpoint, bucket, accessKeyId, secretAccessKey };
}

/** Whether object storage is configured at all -- mirrors email.ts's isSmtpConfigured shape. */
export function isStorageConfigured(): boolean {
	return readConfig() !== null;
}

function getClient(config: StorageConfig): S3Client {
	if (!cachedClient || cachedEndpoint !== config.endpoint) {
		cachedClient = new S3Client({
			endpoint: config.endpoint,
			region: 'us-east-1', // MinIO ignores this; required by the SDK's client shape regardless.
			forcePathStyle: true,
			credentials: {
				accessKeyId: config.accessKeyId,
				secretAccessKey: config.secretAccessKey,
			},
		});
		cachedEndpoint = config.endpoint;
	}
	return cachedClient;
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
