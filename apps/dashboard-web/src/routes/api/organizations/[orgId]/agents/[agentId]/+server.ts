import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { getAgentById, setAgentStatus, setAgentRepoCredentialAccess } from '$lib/db/queries/agents';
import { hasPermission } from '$lib/rbac';
import { requireOrgMembership } from '../../keys/_shared';

const VALID_STATUSES = ['active', 'disabled'] as const;

// PATCH /api/organizations/[orgId]/agents/[agentId]: status changes only (M5 §7). Owner/admin
// only, same gate as agent creation.
export const PATCH: RequestHandler = async ({ params, request, locals }) => {
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

	const existing = await getAgentById(agentId!);
	// An agent belonging to a different organization must 404 exactly like one that does not
	// exist at all -- same reasoning as the keys routes' organizationId comparison (S6's class
	// of bug): the URL's orgId is never the authority, only this membership-scoped comparison
	// is, and the two cases must be indistinguishable to the caller.
	if (!existing || existing.orgId !== orgId) {
		throw error(404, 'Agent not found');
	}

	const body = await request.json().catch(() => ({}) as any);

	// N10: the repo-credentials delivery gate is toggled here too, as its own audited mutation.
	// Exactly one of {status, canAccessRepoCredentials} per request keeps each audit row a single
	// unambiguous change.
	if (typeof body?.canAccessRepoCredentials === 'boolean') {
		if (body.status !== undefined) {
			throw error(400, 'send either status or canAccessRepoCredentials, not both');
		}
		const updated = await setAgentRepoCredentialAccess(
			session.user.id,
			orgId!,
			agentId!,
			body.canAccessRepoCredentials
		);
		return json({ agent: updated });
	}

	if (!body?.status || !VALID_STATUSES.includes(body.status)) {
		throw error(400, `status is required and must be one of ${VALID_STATUSES.join(', ')}`);
	}

	const updated = await setAgentStatus(session.user.id, orgId!, agentId!, body.status);
	return json({ agent: updated });
};
