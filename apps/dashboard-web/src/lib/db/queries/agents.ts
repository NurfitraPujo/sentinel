import { db } from '$lib/server/db';
import { agents, auditLogs } from '$lib/db/schema';
import { eq, and, desc } from 'drizzle-orm';

/**
 * Manual Issues M5 stage 1 (docs/plans/MANUAL_ISSUES_DESIGN.md §7, Q5): agent identity.
 * Every mutation here writes an `audit_logs` row in the same shape as
 * src/lib/db/queries/apikeys.ts's `api_key.*` actions, per Q5 ("every agent action is
 * auditable"). Tenant scope (`orgId`) MUST always come from the caller's authenticated
 * membership, never from the request body (B7) -- every function below takes orgId as an
 * explicit parameter and callers are responsible for having derived it from the credential.
 */

export type AgentKind = 'ai' | 'bot';
export type AgentStatus = 'active' | 'disabled';

export interface AgentRow {
	id: string;
	orgId: string;
	name: string;
	kind: AgentKind;
	status: AgentStatus;
	createdBy: string;
	createdAt: Date | null;
}

export async function listAgents(orgId: string): Promise<AgentRow[]> {
	const rows = await db
		.select()
		.from(agents)
		.where(eq(agents.orgId, orgId))
		.orderBy(desc(agents.createdAt));
	return rows as AgentRow[];
}

// getAgentById intentionally does NOT filter by orgId -- exactly like apikeys.ts's
// getApiKeyById, callers reading a caller-supplied agentId out of the URL MUST verify
// `row.orgId === <the authenticated caller's org>` themselves before acting on it.
export async function getAgentById(id: string): Promise<AgentRow | undefined> {
	const [row] = await db.select().from(agents).where(eq(agents.id, id));
	return row as AgentRow | undefined;
}

export async function createAgent(
	actorUserId: string,
	data: { orgId: string; name: string; kind: AgentKind }
): Promise<AgentRow> {
	const [newAgent] = await db
		.insert(agents)
		.values({
			orgId: data.orgId,
			name: data.name,
			kind: data.kind,
			status: 'active',
			createdBy: actorUserId,
		})
		.returning();

	await db.insert(auditLogs).values({
		action: 'agent.created',
		resourceType: 'agent',
		resourceId: newAgent.id,
		actorId: actorUserId,
		metadata: { name: newAgent.name, kind: newAgent.kind, orgId: data.orgId },
	});

	return newAgent as AgentRow;
}

export async function setAgentStatus(
	actorUserId: string,
	orgId: string,
	agentId: string,
	status: AgentStatus
): Promise<AgentRow> {
	const [updated] = await db
		.update(agents)
		.set({ status })
		.where(and(eq(agents.id, agentId), eq(agents.orgId, orgId)))
		.returning();

	if (!updated) {
		throw new Error('Agent not found');
	}

	await db.insert(auditLogs).values({
		action: 'agent.status_changed',
		resourceType: 'agent',
		resourceId: agentId,
		actorId: actorUserId,
		metadata: { status },
	});

	return updated as AgentRow;
}
