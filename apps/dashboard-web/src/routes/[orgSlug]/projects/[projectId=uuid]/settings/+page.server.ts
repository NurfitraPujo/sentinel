import { error } from '@sveltejs/kit';
import type { PageServerLoad } from './$types';
import { db } from '$lib/server/db';
import { projects, organizationMembers } from '$lib/db/schema';
import { eq, and } from 'drizzle-orm';
import { hasPermission } from '$lib/rbac';

// N10 part 1 (docs/plans/AGENT_WORKER_PLAN.md rev 4 SS4.5): project settings root, currently just
// the "Agent automation" section. D39: authenticate BEFORE any org/project lookup, matching every
// sibling loader under this route tree (settings/keys/+page.server.ts).
//
// RBAC: the whole section is manage_agents-tier, same permission and same mechanism (role read
// off the caller's own membership row) as settings/agents/+page.server.ts and
// /api/organizations/[orgId]/agents. `canManageAgents` is returned so the page can hide the
// section entirely for non-permitted users -- the API routes underneath additionally reject the
// actions server-side regardless of what this flag says, so hiding the UI is a UX nicety, not the
// actual gate (B7's point: authority never comes from what the client claims).
export const load: PageServerLoad = async ({ params, fetch, locals }) => {
	const { orgSlug, projectId } = params;

	const session = await locals.auth();
	if (!session?.user?.email || !session.user.id) {
		throw error(401, 'Unauthorized');
	}

	const currentOrg = locals.currentOrg;
	if (!currentOrg || currentOrg.slug !== orgSlug) {
		throw error(403, 'Forbidden: Unauthorized access to organization');
	}

	const org = { id: currentOrg.id, name: currentOrg.name };

	const [project] = await db
		.select({ id: projects.id, name: projects.name })
		.from(projects)
		.where(and(eq(projects.id, projectId), eq(projects.organizationId, org.id)));

	if (!project) {
		throw error(404, 'Project not found in this organization');
	}

	const [membership] = await db
		.select({ role: organizationMembers.role })
		.from(organizationMembers)
		.where(and(eq(organizationMembers.organizationId, org.id), eq(organizationMembers.userId, session.user.id)));

	const canManageAgents = !!membership && hasPermission(membership.role as any, 'manage_agents');

	let agentSettings: { fixEnabled: boolean; maxPrsPerDay: number | null } = {
		fixEnabled: false,
		maxPrsPerDay: null,
	};
	let repoConnection: {
		provider: string;
		owner: string;
		repo: string;
		defaultBranch: string;
		testCmd: string;
		agentCmd: string | null;
		cloneDepth: number | null;
	} | null = null;

	if (canManageAgents) {
		const res = await fetch(`/api/organizations/${org.id}/projects/${projectId}/agent-settings`);
		if (res.ok) {
			const body = await res.json();
			if (body.settings) {
				agentSettings = { fixEnabled: body.settings.fixEnabled, maxPrsPerDay: body.settings.maxPrsPerDay };
			}
		}

		const repoRes = await fetch(`/api/organizations/${org.id}/projects/${projectId}/repo-connection`);
		if (repoRes.ok) {
			const body = await repoRes.json();
			repoConnection = body.connection ?? null;
		}
	}

	return {
		orgId: org.id,
		orgSlug,
		projectId,
		projectName: project.name,
		canManageAgents,
		agentSettings,
		repoConnection,
	};
};
