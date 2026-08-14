import { db } from '../../server/db';
import { projectApiKeys, auditLogs } from '../schema';
import { eq } from 'drizzle-orm';
import crypto from 'crypto';
import net from 'net';
import { log } from '../../server/observability/log';

// Fixed, greppable event name for a failed 'api_key.invalidated' NATS publish (both call sites below).
// This failure was previously an unstructured `console.error` that nobody alerted on for the entire
// life of the feature (docs/memory/VERIFIED_STATE.md, API-key revocation entry) — a fixed `event` value
// is what makes it finally alertable-by-grep instead of relying on someone reading raw stdout.
const EVENT_API_KEY_INVALIDATED_PUBLISH_FAILED = 'api_key.invalidated.publish_failed';

// getApiKeyById fetches a single key row, including the columns callers need to enforce
// organization scoping (organizationId) BEFORE acting on a keyId that came from the URL — the
// same class of check VERIFIED_STATE.md S6 was missing on the ingest path. A caller MUST verify
// `row.organizationId === <the authenticated caller's org>` itself; this function does not scope
// by organization, so it must never be exposed to a route that skips that check.
export async function getApiKeyById(id: string) {
	const [row] = await db
		.select({
			id: projectApiKeys.id,
			organizationId: projectApiKeys.organizationId,
			projectId: projectApiKeys.projectId,
			name: projectApiKeys.name,
			keyPrefix: projectApiKeys.keyPrefix,
			keyHash: projectApiKeys.keyHash,
			scope: projectApiKeys.scope,
			status: projectApiKeys.status,
			rateLimitRpm: projectApiKeys.rateLimitRpm,
			expiresAt: projectApiKeys.expiresAt,
			revokedAt: projectApiKeys.revokedAt,
			createdBy: projectApiKeys.createdBy,
			createdAt: projectApiKeys.createdAt,
			agentId: projectApiKeys.agentId,
		})
		.from(projectApiKeys)
		.where(eq(projectApiKeys.id, id));
	return row;
}

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
			agentId: projectApiKeys.agentId,
		})
		.from(projectApiKeys)
		.where(eq(projectApiKeys.organizationId, orgId));
}

// Agent keys (scope='agent') for one agent, newest first — used by the agents settings page to
// list/issue/revoke an agent's own keys without pulling the whole org's key inventory.
export async function getAgentApiKeys(agentId: string) {
	return await db
		.select({
			id: projectApiKeys.id,
			organizationId: projectApiKeys.organizationId,
			name: projectApiKeys.name,
			keyPrefix: projectApiKeys.keyPrefix,
			scope: projectApiKeys.scope,
			status: projectApiKeys.status,
			rateLimitRpm: projectApiKeys.rateLimitRpm,
			expiresAt: projectApiKeys.expiresAt,
			revokedAt: projectApiKeys.revokedAt,
			createdBy: projectApiKeys.createdBy,
			createdAt: projectApiKeys.createdAt,
			agentId: projectApiKeys.agentId,
		})
		.from(projectApiKeys)
		.where(eq(projectApiKeys.agentId, agentId));
}

export async function createApiKey(
	userId: string,
	data: {
		organizationId: string;
		projectId?: string | null;
		name: string;
		scope: 'ingest' | 'read' | 'admin' | 'agent';
		rateLimitRpm?: number;
		// M5 §7: set only for scope='agent'. Agent keys are always org-scoped (projectId stays
		// null regardless of what the caller passes) — an agent works across every project in
		// the org, so binding its key to a single project would be wrong, not just unused.
		agentId?: string | null;
	}
) {
	const rawBytes = crypto.randomBytes(32).toString('hex');
	const isAgentKey = data.scope === 'agent';
	const prefix = isAgentKey ? 'sent_agent_' : data.projectId ? 'sent_live_' : 'sent_org_';
	const secretToken = `${prefix}${rawBytes}`;
	const keyHash = crypto.createHash('sha256').update(secretToken).digest('hex');

	const [newKey] = await db
		.insert(projectApiKeys)
		.values({
			organizationId: data.organizationId,
			projectId: isAgentKey ? null : data.projectId || null,
			name: data.name,
			keyPrefix: prefix,
			keyHash,
			scope: data.scope,
			rateLimitRpm: data.rateLimitRpm ?? 5000,
			createdBy: userId,
			status: 'active',
			agentId: isAgentKey ? data.agentId ?? null : null,
		})
		.returning({
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
			agentId: projectApiKeys.agentId,
		});

	await db.insert(auditLogs).values({
		action: 'api_key.created',
		resourceType: 'api_key',
		resourceId: newKey.id,
		actorId: userId,
		metadata: { name: newKey.name, scope: newKey.scope, agentId: newKey.agentId ?? undefined },
	});

	return { apiKey: newKey, secretToken };
}

export class AgentKeyRotationError extends Error {
	constructor(message: string) {
		super(message);
		this.name = 'AgentKeyRotationError';
	}
}

/**
 * R1b (docs/plans/AGENT_AUTOMATION_REMEDIATION_PLAN.md N7f): self-service rotation for an
 * AGENT-scoped key, called from `POST /api/agent/key/rotate` with `oldKeyId`/`oldKeyId`'s agent
 * resolved from the caller's OWN authenticated context (B7: an agent can only ever rotate the key
 * it authenticated with -- never one it names in the request body).
 *
 * Deliberately NOT a variant of `rotateApiKey` above: that function's whole point (S7) is
 * IMMEDIATE revocation of the old key, which is correct for a human operator retiring a
 * potentially-compromised credential, but wrong here -- an unattended agent mid-task with the old
 * key cached has no synchronous way to pick up a new one, so an immediate cutover would break it
 * mid-flight. Instead the OLD key's `status` stays `'active'` and only its `expires_at` is pulled
 * forward to `now() + graceHours` (0 = immediate expiry, same effect as `rotateApiKey` if an
 * operator wants that). Expiry IS enforced (S7, `agent-auth.ts`'s `expiresAt` check), so the old
 * key genuinely stops authenticating once the grace window elapses -- this does not reopen S7,
 * it just widens the window deliberately, on a per-rotation, operator-tunable basis
 * (`AGENT_KEY_ROTATION_GRACE_HOURS`).
 *
 * Returns the new secret ONCE, same contract as `createApiKey`/`rotateApiKey` -- callers must
 * never log or persist it beyond handing it back to the caller in the HTTP response body.
 *
 * This function does NOT itself re-check the old key's status/expiry -- the
 * only-a-live-key-can-rotate guarantee is ROUTE-ENFORCED (`authenticateAgentRequest` runs first
 * and rejects revoked/expired keys). During its grace window the old key remains active and can
 * rotate again (same agent, same org -- not an escalation); callers adding a new call site must
 * keep the authenticate-first ordering.
 */
export async function rotateAgentKeyWithGrace(oldKeyId: string, graceHours: number) {
	const [existingKey] = await db.select().from(projectApiKeys).where(eq(projectApiKeys.id, oldKeyId));

	if (!existingKey) {
		throw new AgentKeyRotationError('API key not found');
	}
	if (existingKey.scope !== 'agent' || !existingKey.agentId) {
		throw new AgentKeyRotationError('Only agent-scoped keys support self-rotation with grace');
	}

	const now = Date.now();
	const graceMs = Math.max(0, graceHours) * 60 * 60 * 1000;
	const newExpiresAt = new Date(now + graceMs);

	const [oldKeyRow] = await db
		.update(projectApiKeys)
		.set({ expiresAt: newExpiresAt })
		.where(eq(projectApiKeys.id, oldKeyId))
		.returning();

	const { apiKey: newKey, secretToken } = await createApiKey(existingKey.agentId, {
		organizationId: existingKey.organizationId,
		projectId: existingKey.projectId,
		name: existingKey.name,
		scope: 'agent',
		rateLimitRpm: existingKey.rateLimitRpm,
		agentId: existingKey.agentId,
	});

	return { oldKey: oldKeyRow, newKey, secretToken };
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
		scope: existingKey.scope as 'ingest' | 'read' | 'admin' | 'agent',
		rateLimitRpm: existingKey.rateLimitRpm,
		agentId: existingKey.agentId,
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
	// 'revoked' in Postgres. Best-effort for the same reason as revokeApiKey: the DB update above
	// (status='revoked', expires_at=now) already makes the old key correctly unusable; this only
	// makes that fact propagate fast.
	if (publisher) {
		try {
			await publisher.publish('api_key.invalidated', { keyId: existingKey.id, keyHash: oldKeyRow?.keyHash });
		} catch (err) {
			log.error(EVENT_API_KEY_INVALIDATED_PUBLISH_FAILED, {
				keyId: existingKey.id,
				reason: 'rotate',
				note: 'revoke already committed to DB',
				error: err,
			});
		}
	}

	return { apiKey: newKey, secretToken };
}

// toPublicKey strips keyHash (and any other future secret-bearing column) from an API-key row
// before it is serialized to the browser. keyHash is the SHA-256 of the live secret and is the
// exact value the ingestor's Redis cache is keyed on (apps/ingestor-go/auth/apikey.go:53) — every
// route that returns an api-key row to a client MUST go through this, even though create/rotate's
// .returning() above already omits keyHash at the query level, because a future column list change
// (or a query that reverts to a bare .returning()) must not silently reintroduce the leak.
export function toPublicKey<T extends Record<string, unknown>>(row: T): Omit<T, 'keyHash'> {
	const { keyHash: _keyHash, ...publicRow } = row as Record<string, unknown> & { keyHash?: unknown };
	return publicRow as Omit<T, 'keyHash'>;
}

export interface NatsPublisher {
	publish(subject: string, data: any): Promise<void>;
}

export async function revokeApiKey(userId: string, keyId: string, publisher?: NatsPublisher) {
	// expires_at is set to "now" alongside status, not left null. apps/ingestor-go/auth/apikey.go
	// enforces `expires_at IS NULL OR expires_at > now()` in the query itself AND caps any Redis
	// cache entry's TTL at the key's remaining lifetime — so setting this closes the gap for BOTH
	// paths at once: a request that reaches Postgres directly (cache miss) is rejected by the
	// query's own WHERE clause, and a request served from a stale cache entry cannot outlive it
	// either. Without this, correctness rested entirely on the NATS publish below succeeding;
	// with it, a publish failure only costs up to the cache TTL, never longer.
	const revokedAt = new Date();
	const [revokedKey] = await db
		.update(projectApiKeys)
		.set({
			status: 'revoked',
			revokedAt,
			expiresAt: revokedAt,
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
		//
		// Best-effort: the DB write above (status + expires_at) is what makes the revoke
		// correct; this publish only makes it FAST (<100ms vs. up to the cache TTL). A NATS
		// hiccup must not turn an already-committed revoke into a 500 to the caller.
		try {
			await publisher.publish('api_key.invalidated', { keyId, keyHash: revokedKey.keyHash });
		} catch (err) {
			log.error(EVENT_API_KEY_INVALIDATED_PUBLISH_FAILED, {
				keyId,
				reason: 'revoke',
				note: 'revoke already committed to DB',
				error: err,
			});
		}
	}

	return revokedKey;
}

// NatsPublisher implementation using a raw core-NATS TCP connection (no `nats` npm dependency).
// JetStream streams capture any message published to a subject they watch regardless of whether
// the publish went through the JetStream API or plain core NATS PUB — this sends CONNECT+PUB and
// does not wait for a JetStream PubAck reply, which is fine for this fire-and-forget
// invalidation signal: correctness does not depend on it (see revokeApiKey/rotateApiKey's
// expires_at handling above), only latency does.
export function createNatsPublisher(url: string = process.env.NATS_URL || 'nats://localhost:4222'): NatsPublisher {
	return {
		publish(subject: string, data: any): Promise<void> {
			return new Promise((resolve, reject) => {
				let host = 'localhost';
				let port = 4222;
				try {
					const parsed = new URL(url);
					host = parsed.hostname || host;
					port = parsed.port ? Number(parsed.port) : port;
				} catch {
					// keep defaults
				}

				const payload = Buffer.from(JSON.stringify(data));
				let settled = false;
				const socket = net.createConnection({ host, port });

				const timeout = setTimeout(() => {
					if (settled) return;
					settled = true;
					socket.destroy();
					reject(new Error(`NATS publish to ${host}:${port} timed out`));
				}, 2000);

				let buffered = '';
				socket.on('data', (chunk) => {
					if (settled) return;
					buffered += chunk.toString('utf8');
					// Wait for the server's initial INFO line before sending CONNECT/PUB — sending
					// before the connection is fully established/greeted is not part of the protocol.
					if (!buffered.includes('\r\n')) return;

					const connect = `CONNECT {"verbose":false,"pedantic":false,"lang":"node"}\r\n`;
					const pub = `PUB ${subject} ${payload.length}\r\n`;
					socket.write(connect + pub);
					socket.write(payload);
					socket.write('\r\n');

					// Give the write a brief moment to flush to the OS socket buffer before closing;
					// core NATS PUB has no application-level ack, so there is nothing else to wait for.
					setImmediate(() => {
						if (settled) return;
						settled = true;
						clearTimeout(timeout);
						socket.end();
						resolve();
					});
				});

				socket.on('error', (err) => {
					if (settled) return;
					settled = true;
					clearTimeout(timeout);
					reject(err);
				});
			});
		},
	};
}
