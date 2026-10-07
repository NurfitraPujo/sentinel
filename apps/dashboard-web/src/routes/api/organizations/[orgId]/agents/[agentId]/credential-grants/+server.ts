import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { db } from '$lib/server/db';
import { agents, projects } from '$lib/db/schema';
import { eq, and, inArray } from 'drizzle-orm';
import { hasPermission } from '$lib/rbac';
import { requireOrgMembership } from '../../../keys/_shared';
import { listAgentCredentialGrants, replaceAgentCredentialGrants } from '$lib/db/queries/repo-credentials';

// 1724300000: which projects an agent may receive repo credentials for. Same owner/admin gate as
// the rest of agent management (`manage_agents`) — this decides who gets write-capable git tokens,
// so it is not an engineer-level control.
async function requireManageAgents(locals: App.Locals, orgId: string) {
	const session = await locals.auth();
	if (!session?.user?.id) {
		error(401, 'Unauthorized');
	}
	const membership = await requireOrgMembership(session.user.id, orgId);
	if (!membership) {
		error(403, 'Forbidden: not a member of this organization');
	}
	if (!hasPermission(membership.role, 'manage_agents')) {
		error(403, 'Forbidden: only owners and admins can manage agent credential scope');
	}
	return session.user.id;
}

// The agent must belong to THIS org. Without this an admin of org A could rewrite the grants of an
// agent in org B by naming its id in the path — the org in the URL is not proof of ownership.
async function requireAgentInOrg(orgId: string, agentId: string) {
	const [agent] = await db
		.select({ id: agents.id, kind: agents.kind })
		.from(agents)
		.where(and(eq(agents.id, agentId), eq(agents.orgId, orgId)));
	if (!agent) {
		error(404, 'Agent not found in this organization');
	}
	// sentinel-cli-browser-login: repo-credential grants are for org agents; a personal agent can never fetch credentials.
	if (agent.kind === 'personal') {
		error(400, 'Personal CLI agents cannot hold repo-credential grants');
	}
}

export const GET: RequestHandler = async ({ params, locals }) => {
	await requireManageAgents(locals, params.orgId!);
	await requireAgentInOrg(params.orgId!, params.agentId!);

	return json({ projectIds: await listAgentCredentialGrants(params.agentId!) });
};

export const PUT: RequestHandler = async ({ params, request, locals }) => {
	const userId = await requireManageAgents(locals, params.orgId!);
	await requireAgentInOrg(params.orgId!, params.agentId!);

	const body = await request.json().catch(() => ({}) as { projectIds?: unknown });
	const raw = body?.projectIds;
	if (!Array.isArray(raw) || raw.some((id) => typeof id !== 'string')) {
		error(400, 'projectIds must be an array of strings');
	}
	const requested = [...new Set(raw as string[])];

	// Every project must belong to this org. replaceAgentCredentialGrants deliberately does not
	// re-derive this, so the check has to be here: a project id from another tenant written into a
	// grant would let this agent receive that tenant's project-scoped credentials.
	if (requested.length > 0) {
		const owned = await db
			.select({ id: projects.id })
			.from(projects)
			.where(and(eq(projects.organizationId, params.orgId!), inArray(projects.id, requested)));
		if (owned.length !== requested.length) {
			error(400, 'every projectId must belong to this organization');
		}
	}

	const projectIds = await replaceAgentCredentialGrants(userId, params.agentId!, requested);
	return json({ projectIds });
};
