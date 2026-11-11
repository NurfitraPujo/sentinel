import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { db } from '$lib/server/db';
import { projects, auditLogs } from '$lib/db/schema';
import { eq, and } from 'drizzle-orm';
import { hasPermission } from '$lib/rbac';
import { requireOrgMembership } from '../../../keys/_shared';
import {
	getProjectAgentSettings,
	upsertProjectAgentSettings,
	AgentSettingsValidationError,
} from '$lib/db/queries/agent-settings';

// GET/PUT /api/organizations/[orgId]/projects/[projectId]/agent-settings (N10 part 1,
// docs/plans/AGENT_WORKER_PLAN.md rev 4 SS4.5): {fixEnabled, maxPrsPerDay} for one project.
// Same RBAC mechanism as /api/organizations/[orgId]/agents/+server.ts -- 'manage_agents',
// checked against the CALLER's own membership row (requireOrgMembership), never against
// anything in the URL or body (B7). Every mutation writes an audit_logs row via the same
// db.insert(auditLogs) path agents.ts uses (Q5: every agent-adjacent action is auditable).
async function requireProjectInOrg(orgId: string, projectId: string) {
	const [project] = await db
		.select({ id: projects.id })
		.from(projects)
		.where(and(eq(projects.id, projectId), eq(projects.organizationId, orgId)));
	return project;
}

export const GET: RequestHandler = async ({ params, locals }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}

	const { orgId, projectId } = params;
	const membership = await requireOrgMembership(session.user.id, orgId!);
	if (!membership) {
		throw error(403, 'Forbidden: not a member of this organization');
	}
	if (!hasPermission(membership.role, 'manage_agents')) {
		throw error(403, 'Forbidden: only owners and admins can manage agent settings');
	}

	const project = await requireProjectInOrg(orgId!, projectId!);
	if (!project) {
		throw error(404, 'Project not found in this organization');
	}

	const settings = await getProjectAgentSettings(projectId!);
	return json({
		settings: settings ?? { projectId: projectId!, fixEnabled: false, maxPrsPerDay: null },
	});
};

export const PUT: RequestHandler = async ({ params, request, locals }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}

	const { orgId, projectId } = params;
	const membership = await requireOrgMembership(session.user.id, orgId!);
	if (!membership) {
		throw error(403, 'Forbidden: not a member of this organization');
	}
	if (!hasPermission(membership.role, 'manage_agents')) {
		throw error(403, 'Forbidden: only owners and admins can manage agent settings');
	}

	const project = await requireProjectInOrg(orgId!, projectId!);
	if (!project) {
		throw error(404, 'Project not found in this organization');
	}

	const body = await request.json().catch(() => ({}) as any);
	const before = await getProjectAgentSettings(projectId!);

	let updated;
	try {
		updated = await upsertProjectAgentSettings(projectId!, {
			fixEnabled: Boolean(body?.fixEnabled),
			maxPrsPerDay:
				body?.maxPrsPerDay === undefined || body?.maxPrsPerDay === null || body?.maxPrsPerDay === ''
					? null
					: Number(body.maxPrsPerDay),
		});
	} catch (err) {
		if (err instanceof AgentSettingsValidationError) {
			throw error(400, err.message);
		}
		throw err;
	}

	await db.insert(auditLogs).values({
		action: 'agent_settings.updated',
		resourceType: 'project_agent_settings',
		resourceId: projectId!,
		actorId: session.user.id,
		metadata: {
			orgId,
			before: { fixEnabled: before?.fixEnabled ?? false, maxPrsPerDay: before?.maxPrsPerDay ?? null },
			after: { fixEnabled: updated.fixEnabled, maxPrsPerDay: updated.maxPrsPerDay },
		},
	});

	return json({ settings: updated });
};
