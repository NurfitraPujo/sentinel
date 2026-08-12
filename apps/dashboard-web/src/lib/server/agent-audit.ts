import { db } from '$lib/server/db';
import { auditLogs } from '$lib/db/schema';
import type { AgentAuthContext } from '$lib/server/agent-auth';
import { log } from '$lib/server/observability/log';

/**
 * Manual Issues M5 stage 2 (design §6, Q5): "every agent action is auditable" -- every
 * `/api/agent/*` mutation writes an `audit_logs` row IN ADDITION to its own `issue_activity` row.
 * `issue_activity` is the product timeline (what IssueTimeline.svelte renders); `audit_logs` is
 * the compliance trail (retention/queryability independent of the issue itself, same table
 * api-key create/revoke already writes to, per agents.ts's header comment). They are written
 * together but serve different readers.
 *
 * Deliberately a plain `db` insert, not threaded through the mutation's own `tx` -- unlike
 * `issue_activity` (which must be atomic with the mutation, D18), the audit row's job is to record
 * that the call was MADE; making it depend on the same transaction succeeding would be circular
 * (D18 already guarantees the mutation's own atomicity) and would couple an audit-trail write to
 * application rollback semantics it doesn't need. A failure here is logged, never thrown -- an
 * audit-write hiccup must not turn an already-committed mutation into a 500 to the calling agent.
 */
export async function writeAgentAuditLog(
	ctx: AgentAuthContext,
	action: string,
	resourceType: string,
	resourceId: string,
	metadata: Record<string, unknown> = {}
): Promise<void> {
	try {
		await db.insert(auditLogs).values({
			action,
			resourceType,
			resourceId,
			actorId: ctx.agentId,
			metadata: {
				agentName: ctx.agentName,
				keyPrefix: ctx.keyPrefixForAudit,
				organizationId: ctx.organizationId,
				...metadata,
			},
		});
	} catch (err) {
		log.error('agent.audit_write_failed', { action, resourceType, resourceId, agentId: ctx.agentId, error: err });
	}
}
