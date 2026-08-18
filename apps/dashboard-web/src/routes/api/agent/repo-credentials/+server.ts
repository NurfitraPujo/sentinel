import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { eq } from 'drizzle-orm';
import { db } from '$lib/server/db';
import { agents } from '$lib/db/schema';
import { authenticateAgentRequest } from '$lib/server/agent-auth';
import { writeAgentAuditLog } from '$lib/server/agent-audit';
import { fetchDecryptedCredentialsForAgent } from '$lib/db/queries/repo-credentials';
import { isEncryptionKeyAvailable } from '$lib/server/repo-credential-crypto';

/**
 * N10 part 2 (docs/plans/AGENT_WORKER_PLAN.md §4.5): the scoped delivery endpoint -- the ONLY
 * place decrypted git credentials ever leave the server. Defense layers, in order:
 *
 *  1. Normal agent-key auth (B7: org scope comes off the credential).
 *  2. `agents.can_access_repo_credentials` -- an admin-set grant, default false. A plain agent
 *     key gets 403 here; possession of a valid key is deliberately NOT enough to receive
 *     write-capable git tokens.
 *  3. SENTINEL_ENCRYPTION_KEY must be present -- without it the server refuses (503) rather
 *     than serving anything from a degraded configuration.
 *  4. Every credential actually served is audited (agent id, credential id, timestamp).
 *
 * Responses are `Cache-Control: no-store`; the worker holds fetched tokens in memory only.
 */
export const GET: RequestHandler = async ({ request }) => {
	const ctx = await authenticateAgentRequest(request);

	// Re-read the flag per request rather than caching it on the auth context: revoking access
	// must take effect immediately, not at next key rotation.
	const [agentRow] = await db
		.select({ canAccessRepoCredentials: agents.canAccessRepoCredentials })
		.from(agents)
		.where(eq(agents.id, ctx.agentId));

	if (!agentRow?.canAccessRepoCredentials) {
		throw error(403, 'This agent is not authorized to access repo credentials');
	}

	if (!isEncryptionKeyAvailable()) {
		throw error(503, 'Server encryption key is not configured; refusing to serve credentials');
	}

	const decrypted = await fetchDecryptedCredentialsForAgent(ctx.organizationId);

	for (const cred of decrypted) {
		await writeAgentAuditLog(ctx, 'agent.repo_credential_fetched', 'repo_credential', cred.id, {
			provider: cred.provider,
			fetchedAt: new Date().toISOString(),
		});
	}

	return json(
		{
			credentials: decrypted.map((c) => ({
				id: c.id,
				provider: c.provider,
				label: c.label,
				secret: c.secret,
			})),
		},
		{ headers: { 'Cache-Control': 'no-store' } }
	);
};
