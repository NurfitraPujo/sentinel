import { db } from '../../server/db';
import { projectApiKeys, auditLogs } from '../schema';
import { eq } from 'drizzle-orm';
import crypto from 'crypto';

export async function getOrganizationApiKeys(orgId: string) {
	return await db
		.select({
			id: projectApiKeys.id,
			organizationId: projectApiKeys.organizationId,
			projectId: projectApiKeys.projectId,
			name: projectApiKeys.name,
			keyPrefix: projectApiKeys.keyPrefix,
			scope: projectApiKeys.scope,
			status: projectApiKeys.status,
			rateLimitRpm: projectApiKeys.rateLimitRpm,
			expiresAt: projectApiKeys.expiresAt,
			revokedAt: projectApiKeys.revokedAt,
			createdBy: projectApiKeys.createdBy,
			createdAt: projectApiKeys.createdAt,
		})
		.from(projectApiKeys)
		.where(eq(projectApiKeys.organizationId, orgId));
}

export async function createApiKey(
	userId: string,
	data: {
		organizationId: string;
		projectId?: string | null;
		name: string;
		scope: 'ingest' | 'read' | 'admin';
		rateLimitRpm?: number;
	}
) {
	const rawBytes = crypto.randomBytes(32).toString('hex');
	const prefix = data.projectId ? 'sent_live_' : 'sent_org_';
	const secretToken = `${prefix}${rawBytes}`;
	const keyHash = crypto.createHash('sha256').update(secretToken).digest('hex');

	const [newKey] = await db
		.insert(projectApiKeys)
		.values({
			organizationId: data.organizationId,
			projectId: data.projectId || null,
			name: data.name,
			keyPrefix: prefix,
			keyHash,
			scope: data.scope,
			rateLimitRpm: data.rateLimitRpm ?? 5000,
			createdBy: userId,
			status: 'active',
		})
		.returning();

	await db.insert(auditLogs).values({
		action: 'api_key.created',
		resourceType: 'api_key',
		resourceId: newKey.id,
		actorId: userId,
		metadata: { name: newKey.name, scope: newKey.scope },
	});

	return { apiKey: newKey, secretToken };
}

export async function rotateApiKey(
	userId: string,
	keyId: string,
	gracePeriodDuration: string = '24h'
) {
	const [existingKey] = await db
		.select()
		.from(projectApiKeys)
		.where(eq(projectApiKeys.id, keyId));

	if (!existingKey) {
		throw new Error('API key not found');
	}

	let hours = 24;
	const match = gracePeriodDuration.match(/^(\d+)h$/);
	if (match) {
		hours = parseInt(match[1], 10);
	}
	const expiresAt = new Date(Date.now() + hours * 60 * 60 * 1000);

	await db
		.update(projectApiKeys)
		.set({ expiresAt })
		.where(eq(projectApiKeys.id, keyId));

	const { apiKey: newKey, secretToken } = await createApiKey(userId, {
		organizationId: existingKey.organizationId,
		projectId: existingKey.projectId,
		name: existingKey.name,
		scope: existingKey.scope as 'ingest' | 'read' | 'admin',
		rateLimitRpm: existingKey.rateLimitRpm,
	});

	await db.insert(auditLogs).values({
		action: 'api_key.rotated',
		resourceType: 'api_key',
		resourceId: existingKey.id,
		actorId: userId,
		metadata: { newKeyId: newKey.id },
	});

	return { apiKey: newKey, secretToken };
}

export interface NatsPublisher {
	publish(subject: string, data: any): Promise<void>;
}

export async function revokeApiKey(userId: string, keyId: string, publisher?: NatsPublisher) {
	const [revokedKey] = await db
		.update(projectApiKeys)
		.set({
			status: 'revoked',
			revokedAt: new Date(),
		})
		.where(eq(projectApiKeys.id, keyId))
		.returning();

	if (!revokedKey) {
		throw new Error('API key not found');
	}

	await db.insert(auditLogs).values({
		action: 'api_key.revoked',
		resourceType: 'api_key',
		resourceId: keyId,
		actorId: userId,
		metadata: {},
	});

	if (publisher) {
		await publisher.publish('api_key.invalidated', { keyId });
	}

	return revokedKey;
}
