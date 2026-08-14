import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { getAgentById } from '$lib/db/queries/agents';
import { listAgentWebhooks, createAgentWebhook } from '$lib/db/queries/agent-webhooks';
import { validateWebhookUrl } from '$lib/server/agent-webhook-url';
import { AGENT_EVENT_TYPES } from '$lib/server/agent-events';
import { hasPermission } from '$lib/rbac';
import { requireOrgMembership } from '../../../keys/_shared';

// GET/POST /api/organizations/[orgId]/agents/[agentId]/webhooks (N3a): webhook registration,
// owner/admin only, exactly mirroring the agents/+server.ts pattern -- tenant scope (orgId) is
// always the membership row looked up from the session's own user id (requireOrgMembership),
// never trusted from the URL alone (B7), and every write is additionally scoped to that
// membership's organizationId.
export const GET: RequestHandler = async ({ params, locals }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}

	const { orgId, agentId } = params;
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

	// listAgentWebhooks's projection never includes the `secret` column -- only secretPrefix, so
	// the signing secret is never re-exposed after creation.
	const webhooks = await listAgentWebhooks(orgId!, agentId!);
	return json({ webhooks });
};

export const POST: RequestHandler = async ({ params, request, locals }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}

	const { orgId, agentId } = params;
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

	const body = await request.json().catch(() => ({}) as any);
	if (!body?.url || typeof body.url !== 'string') {
		throw error(400, 'url is required');
	}
	const urlCheck = validateWebhookUrl(body.url);
	if (!urlCheck.valid) {
		throw error(400, urlCheck.error || 'url is invalid');
	}

	let eventTypes: string[] = [];
	if (body.eventTypes !== undefined) {
		if (!Array.isArray(body.eventTypes) || !body.eventTypes.every((t: unknown) => typeof t === 'string')) {
			throw error(400, 'eventTypes must be an array of strings');
		}
		const invalid = body.eventTypes.filter((t: string) => !(AGENT_EVENT_TYPES as readonly string[]).includes(t));
		if (invalid.length > 0) {
			throw error(400, `eventTypes contains invalid event type(s): ${invalid.join(', ')}. Valid: ${AGENT_EVENT_TYPES.join(', ')}`);
		}
		eventTypes = body.eventTypes;
	}

	// createAgentWebhook returns the raw secret exactly once -- this response is the only place
	// it is ever sent to a client. It is never re-derivable from secretPrefix or the audit log.
	const { webhook, secret } = await createAgentWebhook(session.user.id, {
		organizationId: orgId!,
		agentId: agentId!,
		url: body.url,
		eventTypes,
	});

	return json({ webhook, secret }, { status: 201 });
};
