import { db } from '$lib/server/db';
import { agentWebhooks, auditLogs } from '$lib/db/schema';
import { eq, and, desc } from 'drizzle-orm';
import crypto from 'crypto';

/**
 * N3a: agent webhook registration. Every mutation writes an `audit_logs` row in the same shape
 * as src/lib/db/queries/apikeys.ts's `api_key.*` / agents.ts's `agent.*` actions -- same
 * auditability precedent (M5 §7 Q5, "every agent action is auditable"). Tenant scope
 * (`organizationId`) MUST always come from the caller's authenticated membership, never from the
 * request body (B7) -- every function below takes it as an explicit parameter and callers are
 * responsible for having derived it from the credential/session (see the routes in
 * src/routes/api/organizations/[orgId]/agents/[agentId]/webhooks/).
 */

export type WebhookStatus = 'active' | 'disabled' | 'failed';

export interface AgentWebhookRow {
	id: string;
	organizationId: string;
	agentId: string;
	url: string;
	secretPrefix: string;
	eventTypes: string[];
	status: WebhookStatus;
	lastDeliveredSeq: number;
	consecutiveFailures: number;
	lastAttemptAt: Date | null;
	lastError: string | null;
	createdAt: Date | null;
}

// Public projection used everywhere EXCEPT immediately after creation -- deliberately omits the
// `secret` column so the raw signing secret never round-trips back out of the database once
// written (mirrors apikeys.ts never re-selecting keyHash for display).
const PUBLIC_COLUMNS = {
	id: agentWebhooks.id,
	organizationId: agentWebhooks.organizationId,
	agentId: agentWebhooks.agentId,
	url: agentWebhooks.url,
	secretPrefix: agentWebhooks.secretPrefix,
	eventTypes: agentWebhooks.eventTypes,
	status: agentWebhooks.status,
	lastDeliveredSeq: agentWebhooks.lastDeliveredSeq,
	consecutiveFailures: agentWebhooks.consecutiveFailures,
	lastAttemptAt: agentWebhooks.lastAttemptAt,
	lastError: agentWebhooks.lastError,
	createdAt: agentWebhooks.createdAt,
} as const;

export async function listAgentWebhooks(orgId: string, agentId: string): Promise<AgentWebhookRow[]> {
	const rows = await db
		.select(PUBLIC_COLUMNS)
		.from(agentWebhooks)
		.where(and(eq(agentWebhooks.organizationId, orgId), eq(agentWebhooks.agentId, agentId)))
		.orderBy(desc(agentWebhooks.createdAt));
	return rows as AgentWebhookRow[];
}

// getAgentWebhookById intentionally does NOT filter by orgId/agentId -- exactly like
// agents.ts's getAgentById, callers reading a caller-supplied webhookId out of the URL MUST
// verify `row.organizationId === <the authenticated caller's org>` AND
// `row.agentId === <the URL's agentId>` themselves before acting on it.
export async function getAgentWebhookById(id: string): Promise<AgentWebhookRow | undefined> {
	const [row] = await db.select(PUBLIC_COLUMNS).from(agentWebhooks).where(eq(agentWebhooks.id, id));
	return row as AgentWebhookRow | undefined;
}

export async function createAgentWebhook(
	actorUserId: string,
	data: { organizationId: string; agentId: string; url: string; eventTypes: string[] }
): Promise<{ webhook: AgentWebhookRow; secret: string }> {
	const secret = `whsec_${crypto.randomBytes(32).toString('hex')}`;
	// First 10 chars of the secret (including the 'whsec_' prefix) is enough for a caller to
	// recognize "which webhook is this" in a UI without ever re-exposing the signing secret,
	// same tradeoff as projectApiKeys.keyPrefix.
	const secretPrefix = secret.slice(0, 10);

	const [newWebhook] = await db
		.insert(agentWebhooks)
		.values({
			organizationId: data.organizationId,
			agentId: data.agentId,
			url: data.url,
			secret,
			secretPrefix,
			eventTypes: data.eventTypes,
			status: 'active',
		})
		.returning(PUBLIC_COLUMNS);

	await db.insert(auditLogs).values({
		action: 'agent_webhook.created',
		resourceType: 'agent_webhook',
		resourceId: newWebhook.id,
		actorId: actorUserId,
		metadata: { agentId: data.agentId, url: data.url, eventTypes: data.eventTypes },
	});

	return { webhook: newWebhook as AgentWebhookRow, secret };
}

export interface UpdateAgentWebhookInput {
	url?: string;
	eventTypes?: string[];
	status?: WebhookStatus;
}

// updateAgentWebhook is the single place that applies the "re-enable resumes, it does not jump to
// head" rule: transitioning INTO 'active' FROM 'failed' or 'disabled' clears consecutiveFailures
// and lastError (the failure streak that caused/accompanied the non-active status is stale once an
// operator has acted on it), but deliberately leaves lastDeliveredSeq untouched. The delivery
// worker's cursor is lastDeliveredSeq -- resetting it would either replay every historical event
// (jump to 0) or silently skip everything queued while the webhook was down (jump to current head).
// Neither is what re-enabling a webhook means; it should resume exactly where delivery left off.
export async function updateAgentWebhook(
	actorUserId: string,
	orgId: string,
	agentId: string,
	webhookId: string,
	updates: UpdateAgentWebhookInput
): Promise<AgentWebhookRow> {
	const existing = await getAgentWebhookById(webhookId);
	if (!existing || existing.organizationId !== orgId || existing.agentId !== agentId) {
		throw new Error('Webhook not found');
	}

	const isReenabling =
		updates.status === 'active' && (existing.status === 'failed' || existing.status === 'disabled');

	const setValues: Record<string, unknown> = {};
	if (updates.url !== undefined) setValues.url = updates.url;
	if (updates.eventTypes !== undefined) setValues.eventTypes = updates.eventTypes;
	if (updates.status !== undefined) setValues.status = updates.status;
	if (isReenabling) {
		setValues.consecutiveFailures = 0;
		setValues.lastError = null;
		// lastDeliveredSeq is intentionally absent from setValues: re-enabling must resume
		// delivery from wherever it last succeeded, not reset the cursor.
	}

	const [updated] = await db
		.update(agentWebhooks)
		.set(setValues)
		.where(
			and(
				eq(agentWebhooks.id, webhookId),
				eq(agentWebhooks.organizationId, orgId),
				eq(agentWebhooks.agentId, agentId)
			)
		)
		.returning(PUBLIC_COLUMNS);

	if (!updated) {
		throw new Error('Webhook not found');
	}

	await db.insert(auditLogs).values({
		action: 'agent_webhook.updated',
		resourceType: 'agent_webhook',
		resourceId: webhookId,
		actorId: actorUserId,
		metadata: { ...updates, resumedFromSeq: isReenabling ? existing.lastDeliveredSeq : undefined },
	});

	return updated as AgentWebhookRow;
}

export async function deleteAgentWebhook(
	actorUserId: string,
	orgId: string,
	agentId: string,
	webhookId: string
): Promise<void> {
	const [deleted] = await db
		.delete(agentWebhooks)
		.where(
			and(
				eq(agentWebhooks.id, webhookId),
				eq(agentWebhooks.organizationId, orgId),
				eq(agentWebhooks.agentId, agentId)
			)
		)
		.returning({ id: agentWebhooks.id });

	if (!deleted) {
		throw new Error('Webhook not found');
	}

	await db.insert(auditLogs).values({
		action: 'agent_webhook.deleted',
		resourceType: 'agent_webhook',
		resourceId: webhookId,
		actorId: actorUserId,
		metadata: { agentId },
	});
}
