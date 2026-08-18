import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { db } from '$lib/server/db';
import { projects, auditLogs } from '$lib/db/schema';
import { eq, and } from 'drizzle-orm';
import { hasPermission } from '$lib/rbac';
import { requireOrgMembership } from '../../../keys/_shared';
import {
	getRepoConnection,
	upsertRepoConnection,
	deleteRepoConnection,
	AgentSettingsValidationError,
} from '$lib/db/queries/agent-settings';

// PUT/DELETE /api/organizations/[orgId]/projects/[projectId]/repo-connection (N10 part 1,
// docs/plans/AGENT_WORKER_PLAN.md rev 4 SS4.5): one repo connection per project (v1). Same RBAC
// mechanism as the sibling agent-settings route in this directory tree -- see that file's header
// comment for the shared rationale (B7, audit trail).
async function requireProjectInOrg(orgId: string, projectId: string) {
	const [project] = await db
		.select({ id: projects.id })
		.from(projects)
		.where(and(eq(projects.id, projectId), eq(projects.organizationId, orgId)));
	return project;
}

async function requireManageAgents(orgId: string, userId: string) {
	const membership = await requireOrgMembership(userId, orgId);
	if (!membership) {
		throw error(403, 'Forbidden: not a member of this organization');
	}
	if (!hasPermission(membership.role, 'manage_agents')) {
		throw error(403, 'Forbidden: only owners and admins can manage agent settings');
	}
}

export const GET: RequestHandler = async ({ params, locals }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}

	const { orgId, projectId } = params;
	await requireManageAgents(orgId!, session.user.id);

	const project = await requireProjectInOrg(orgId!, projectId!);
	if (!project) {
		throw error(404, 'Project not found in this organization');
	}

	const connection = await getRepoConnection(projectId!);
	return json({ connection });
};

export const PUT: RequestHandler = async ({ params, request, locals }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}

	const { orgId, projectId } = params;
	await requireManageAgents(orgId!, session.user.id);

	const project = await requireProjectInOrg(orgId!, projectId!);
	if (!project) {
		throw error(404, 'Project not found in this organization');
	}

	const body = await request.json().catch(() => ({}) as any);
	const before = await getRepoConnection(projectId!);

	let updated;
	try {
		updated = await upsertRepoConnection(projectId!, {
			provider: body?.provider,
			owner: body?.owner,
			repo: body?.repo,
			defaultBranch: body?.defaultBranch,
			testCmd: body?.testCmd,
			agentCmd: body?.agentCmd ?? null,
			cloneDepth:
				body?.cloneDepth === undefined || body?.cloneDepth === null || body?.cloneDepth === ''
					? null
					: Number(body.cloneDepth),
		});
	} catch (err) {
		if (err instanceof AgentSettingsValidationError) {
			throw error(400, err.message);
		}
		throw err;
	}

	await db.insert(auditLogs).values({
		action: before ? 'agent_repo_connection.updated' : 'agent_repo_connection.created',
		resourceType: 'project_repo_connection',
		resourceId: projectId!,
		actorId: session.user.id,
		metadata: {
			orgId,
			before: before
				? { provider: before.provider, owner: before.owner, repo: before.repo, defaultBranch: before.defaultBranch }
				: null,
			after: {
				provider: updated.provider,
				owner: updated.owner,
				repo: updated.repo,
				defaultBranch: updated.defaultBranch,
			},
		},
	});

	return json({ connection: updated });
};

export const DELETE: RequestHandler = async ({ params, locals }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}

	const { orgId, projectId } = params;
	await requireManageAgents(orgId!, session.user.id);

	const project = await requireProjectInOrg(orgId!, projectId!);
	if (!project) {
		throw error(404, 'Project not found in this organization');
	}

	const before = await getRepoConnection(projectId!);
	if (!before) {
		throw error(404, 'No repo connection for this project');
	}

	await deleteRepoConnection(projectId!);

	await db.insert(auditLogs).values({
		action: 'agent_repo_connection.deleted',
		resourceType: 'project_repo_connection',
		resourceId: projectId!,
		actorId: session.user.id,
		metadata: {
			orgId,
			before: { provider: before.provider, owner: before.owner, repo: before.repo, defaultBranch: before.defaultBranch },
		},
	});

	return json({ ok: true });
};
