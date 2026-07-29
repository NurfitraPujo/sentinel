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
	// Historical parameter — see the decision note below. Kept (rather than removed) so
	// existing call sites keep compiling, and recorded in the audit log for traceability even
	// though it no longer controls how long the old key stays valid.
	gracePeriodDuration: string = '24h',
	publisher?: NatsPublisher
) {
	const [existingKey] = await db
		.select()
		.from(projectApiKeys)
		.where(eq(projectApiKeys.id, keyId));

	if (!existingKey) {
		throw new Error('API key not found');
	}

	// DECISION (VERIFIED_STATE.md S7 / E2E_RECOVERY_PLAN P3-2): rotation revokes the old key
	// IMMEDIATELY (status='revoked'), not via a timed expires_at grace period.
	//
	// The old code set expires_at on the old key but left status='active'. Combined with the
	// ingestor never checking expires_at at all (the other half of S7, fixed separately in
	// apps/ingestor-go/auth/apikey.go), that made a rotated key valid forever. Even with
	// expires_at now enforced, a grace period is the wrong default here: rotation is the
	// operator saying "this secret should stop being trusted", most often because it leaked or
	// is being retired as a precaution — the same intent as revokeApiKey, not a softer version
	// of it. A multi-hour window where the flagged-as-compromised key still authenticates
	// requests defeats that intent for the exact scenario rotation exists to handle, and it
	// reopens the Redis-cache staleness gap (S7's third break) for the whole window instead of
	// just the cache TTL.
	//
	// A deployment that genuinely needs a soft cutover (e.g. staggered redeploy of many
	// clients) should keep the OLD key active and provision a second, independently-active key
	// for new clients, then explicitly revoke the old one once cutover is confirmed — not lean
	// on a timer attached to a key already flagged for retirement. `revokeApiKey` already
	// serves that explicit-revoke step.
	const revokedAt = new Date();
	const updateResult = await db
		.update(projectApiKeys)
		.set({ status: 'revoked', revokedAt, expiresAt: revokedAt })
		.where(eq(projectApiKeys.id, keyId))
		.returning();
	const oldKeyRow = Array.isArray(updateResult) ? updateResult[0] : undefined;

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
		metadata: { newKeyId: newKey.id, requestedGracePeriod: gracePeriodDuration, revocation: 'immediate' },
	});

	// Same invalidation path as revokeApiKey: without this, the old key's Redis cache entry
	// (apps/ingestor-go/auth/apikey.go) survives up to its own TTL despite status now being
	// 'revoked' in Postgres.
	if (publisher) {
		await publisher.publish('api_key.invalidated', { keyId: existingKey.id, keyHash: oldKeyRow?.keyHash });
	}

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
		// Publish BOTH keyId (for readability/audit-trail correlation) and keyHash. The
		// ingestor's Redis cache (apps/ingestor-go/auth/apikey.go) is keyed by SHA256 hash, not
		// by this row's id, so keyHash is the field that actually lets it delete the cache
		// entry. Publishing only `{ keyId }` (the previous, S7 bug) meant the ingestor's
		// handler — which read `data["key_hash"]` and would still not have matched this
		// object's camelCase key even if the field had existed — could never find anything to
		// delete, and revocation silently waited out the cache TTL instead of being instant.
		await publisher.publish('api_key.invalidated', { keyId, keyHash: revokedKey.keyHash });
	}

	return revokedKey;
}
