import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { getAgentById } from '$lib/db/queries/agents';
import { getAgentWebhookById, updateAgentWebhook, deleteAgentWebhook } from '$lib/db/queries/agent-webhooks';
import { validateWebhookUrl } from '$lib/server/agent-webhook-url';
import { AGENT_EVENT_TYPES } from '$lib/server/agent-events';
import { hasPermission } from '$lib/rbac';
import { requireOrgMembership } from '../../../../keys/_shared';

const VALID_STATUSES = ['active', 'disabled'] as const;

// Shared setup for both handlers below: session -> membership -> manage_agents -> agent scoped
// to orgId -> webhook scoped to (orgId, agentId). A webhook belonging to a different agent or
// organization must 404 exactly like one that does not exist at all (same reasoning as the
// agents/[agentId] route's orgId comparison, S6's class of bug) -- the URL's orgId/agentId are
// never the authority, only these membership- and ownership-scoped comparisons are.
async function authorizeAndScope(
	locals: App.Locals,
	params: Record<string, string | undefined>
): Promise<{ userId: string; orgId: string; agentId: string; webhookId: string }> {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}

	const { orgId, agentId, webhookId } = params;
	const membership = await requireOrgMembership(session.user.id, orgId!);
	if (!membership) {
		throw error(403, 'Forbidden: not a member of this organization');
	}
	if (!hasPermission(membership.role, 'manage_agents')) {
		throw error(403, 'Forbidden: only owners and admins can manage agents');
	}

	const agent = await getAgentById(agentId!);
	if (!agent || agent.orgId !== orgId) {
		throw error(404, 'Agent not found');
	}

	const webhook = await getAgentWebhookById(webhookId!);
	if (!webhook || webhook.organizationId !== orgId || webhook.agentId !== agentId) {
		throw error(404, 'Webhook not found');
	}

	return { userId: session.user.id, orgId: orgId!, agentId: agentId!, webhookId: webhookId! };
}

export const PATCH: RequestHandler = async ({ params, request, locals }) => {
	const { userId, orgId, agentId, webhookId } = await authorizeAndScope(locals, params);

	const body = await request.json().catch(() => ({}) as any);
	const updates: { url?: string; eventTypes?: string[]; status?: 'active' | 'disabled' | 'failed' } = {};

	if (body.url !== undefined) {
		if (typeof body.url !== 'string') {
			throw error(400, 'url must be a string');
		}
		const urlCheck = validateWebhookUrl(body.url);
		if (!urlCheck.valid) {
			throw error(400, urlCheck.error || 'url is invalid');
		}
		updates.url = body.url;
	}

	if (body.eventTypes !== undefined) {
		if (!Array.isArray(body.eventTypes) || !body.eventTypes.every((t: unknown) => typeof t === 'string')) {
			throw error(400, 'eventTypes must be an array of strings');
		}
		const invalid = body.eventTypes.filter((t: string) => !(AGENT_EVENT_TYPES as readonly string[]).includes(t));
		if (invalid.length > 0) {
			throw error(400, `eventTypes contains invalid event type(s): ${invalid.join(', ')}. Valid: ${AGENT_EVENT_TYPES.join(', ')}`);
		}
		updates.eventTypes = body.eventTypes;
	}

	if (body.status !== undefined) {
		// PATCH accepts only 'active'/'disabled' from the UI -- 'failed' is a system-set status
		// the delivery worker transitions into, not something an operator PATCHes to directly.
		if (!VALID_STATUSES.includes(body.status)) {
			throw error(400, `status is required and must be one of ${VALID_STATUSES.join(', ')}`);
		}
		updates.status = body.status;
	}

	if (Object.keys(updates).length === 0) {
		throw error(400, 'at least one of url, eventTypes, status is required');
	}

	const updated = await updateAgentWebhook(userId, orgId, agentId, webhookId, updates);
	return json({ webhook: updated });
};

export const DELETE: RequestHandler = async ({ params, locals }) => {
	const { userId, orgId, agentId, webhookId } = await authorizeAndScope(locals, params);
	await deleteAgentWebhook(userId, orgId, agentId, webhookId);
	return new Response(null, { status: 204 });
};
