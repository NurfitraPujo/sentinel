import { error } from '@sveltejs/kit';
import crypto from 'crypto';
import { eq } from 'drizzle-orm';
import { db } from '$lib/server/db';
import { projectApiKeys, agents } from '$lib/db/schema';

/**
 * Manual Issues M5 stage 2 (docs/plans/MANUAL_ISSUES_DESIGN.md §7, Q5, Q6, B7): key
 * authentication for `/api/agent/*`. Deliberately its own module rather than a variant of the
 * ingestor's `apikey.go` middleware or any session-auth helper -- this is a *third* credential
 * shape (key -> agent identity -> org), and B7's rule ("tenant scope derives from the credential,
 * never the request body") is exactly why `AgentAuthContext.organizationId` below MUST always be
 * read off the key/agent rows, never off anything the caller sends in the URL or body. Every
 * `/api/agent/*` route must call `authenticateAgentRequest` first and use ONLY the returned
 * context for scoping.
 */

// Hash exactly like apikeys.ts's createApiKey computes it: sha256(secretToken) as lowercase hex,
// matched against project_api_keys.key_hash. Any divergence here (different digest, different
// encoding) makes every agent key silently unauthenticatable -- there is deliberately no
// alternate lookup path.
function hashKey(secretToken: string): string {
	return crypto.createHash('sha256').update(secretToken).digest('hex');
}

export interface AgentAuthContext {
	agentId: string;
	organizationId: string;
	agentName: string;
	/** First 12 chars of the raw key hash -- never the raw secret -- for audit metadata (Q5). */
	keyPrefixForAudit: string;
}

/**
 * Authenticates an `Authorization: Bearer <key>` request against `project_api_keys`. Requires
 * scope='agent', status='active', unexpired/unrevoked, and a linked agent whose own status is
 * 'active'. Throws SvelteKit `error(401)` on any failure -- deliberately the SAME status for
 * "missing header", "unknown key", "wrong scope", "revoked/expired key", and "disabled agent", so
 * this endpoint cannot be used to probe which of those is true (mirrors the ingestor's "Invalid
 * API key" / "Forbidden" split, collapsed further here since scope IS the tenancy boundary for
 * this whole route tree).
 *
 * Rate limiting: NOT wired here. `$lib/rate-limit.ts` exists for session-authenticated dashboard
 * routes; a per-key limiter for `/api/agent/*` (mirroring the ingestor's Redis-backed limiter) is
 * left for a follow-up -- noted here rather than silently absent, since an unthrottled,
 * key-authenticated write surface is exactly the kind of gap this repo's history (S-series) warns
 * gets forgotten.
 */
export async function authenticateAgentRequest(request: Request): Promise<AgentAuthContext> {
	const authHeader = request.headers.get('authorization') || request.headers.get('Authorization');
	if (!authHeader || !authHeader.toLowerCase().startsWith('bearer ')) {
		throw error(401, 'Missing or malformed Authorization header');
	}

	const rawKey = authHeader.slice(authHeader.indexOf(' ') + 1).trim();
	if (!rawKey) {
		throw error(401, 'Missing bearer token');
	}

	const keyHash = hashKey(rawKey);

	const [keyRow] = await db
		.select({
			id: projectApiKeys.id,
			organizationId: projectApiKeys.organizationId,
			scope: projectApiKeys.scope,
			status: projectApiKeys.status,
			expiresAt: projectApiKeys.expiresAt,
			revokedAt: projectApiKeys.revokedAt,
			agentId: projectApiKeys.agentId,
		})
		.from(projectApiKeys)
		.where(eq(projectApiKeys.keyHash, keyHash));

	if (!keyRow) {
		throw error(401, 'Invalid API key');
	}

	if (keyRow.scope !== 'agent') {
		throw error(401, 'Invalid API key');
	}

	if (keyRow.status !== 'active' || keyRow.revokedAt) {
		throw error(401, 'Invalid API key');
	}

	if (keyRow.expiresAt && keyRow.expiresAt.getTime() <= Date.now()) {
		throw error(401, 'Invalid API key');
	}

	if (!keyRow.agentId) {
		// Should be unreachable given createApiKey always sets agentId for scope='agent', but a
		// key with no linked agent identity cannot resolve an actor -- fail closed, not open.
		throw error(401, 'Invalid API key');
	}

	const [agentRow] = await db
		.select({
			id: agents.id,
			orgId: agents.orgId,
			name: agents.name,
			status: agents.status,
		})
		.from(agents)
		.where(eq(agents.id, keyRow.agentId));

	if (!agentRow || agentRow.status !== 'active') {
		throw error(401, 'Invalid API key');
	}

	// Belt-and-suspenders: the key's own organizationId and the linked agent's orgId must agree.
	// They can only diverge through direct DB tampering (createApiKey always derives one from the
	// other), but B7 means this function must never trust either alone without checking.
	if (keyRow.organizationId !== agentRow.orgId) {
		throw error(401, 'Invalid API key');
	}

	return {
		agentId: agentRow.id,
		organizationId: keyRow.organizationId,
		agentName: agentRow.name,
		keyPrefixForAudit: keyHash.slice(0, 12),
	};
}
