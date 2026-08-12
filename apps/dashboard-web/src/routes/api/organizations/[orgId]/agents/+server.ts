import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { listAgents, createAgent } from '$lib/db/queries/agents';
import { hasPermission } from '$lib/rbac';
import { requireOrgMembership } from '../keys/_shared';

const VALID_KINDS = ['ai', 'bot'] as const;

// GET/POST /api/organizations/[orgId]/agents (M5 §7, §9): agent management, owner/admin only
// per the design's permission matrix. Tenant scope (orgId) is always the membership row looked
// up from the session's own user id (requireOrgMembership), never trusted from the URL alone
// (B7) -- the URL's orgId is only used to SCOPE the query, and every write is additionally
// scoped to that membership's organizationId.
export const GET: RequestHandler = async ({ params, locals }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}

	const { orgId } = params;
	const membership = await requireOrgMembership(session.user.id, orgId!);
	if (!membership) {
		throw error(403, 'Forbidden: not a member of this organization');
	}
	if (!hasPermission(membership.role, 'manage_agents')) {
		throw error(403, 'Forbidden: only owners and admins can manage agents');
	}

	const agentRows = await listAgents(orgId!);
	return json({ agents: agentRows });
};

export const POST: RequestHandler = async ({ params, request, locals }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}

	const { orgId } = params;
	const membership = await requireOrgMembership(session.user.id, orgId!);
	if (!membership) {
		throw error(403, 'Forbidden: not a member of this organization');
	}
	if (!hasPermission(membership.role, 'manage_agents')) {
		throw error(403, 'Forbidden: only owners and admins can manage agents');
	}

	const body = await request.json().catch(() => ({}) as any);
	if (!body?.name || typeof body.name !== 'string' || !body.name.trim()) {
		throw error(400, 'name is required');
	}
	if (body.name.length > 255) {
		throw error(400, 'name must be at most 255 characters');
	}
	if (!body?.kind || !VALID_KINDS.includes(body.kind)) {
		throw error(400, `kind is required and must be one of ${VALID_KINDS.join(', ')}`);
	}

	const newAgent = await createAgent(session.user.id, {
		orgId: orgId!,
		name: body.name.trim(),
		kind: body.kind,
	});

	return json({ agent: newAgent }, { status: 201 });
};
