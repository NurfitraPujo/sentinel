import { json } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { authenticateAgentRequest } from '$lib/server/agent-auth';
import { listAgentProjects } from '$lib/db/queries/agent-reads';

// N1c (agent read endpoints). GET /api/agent/projects -- no issue scope, so this authenticates
// directly (authenticateAgentRequest) rather than going through withAgentIssue. Lists the
// calling key's own org's projects (B7: organizationId from AgentAuthContext, never a param).
// sentinel-cli-browser-login: a personal key lists only the projects its owner can reach (getAccessibleProjects).

export const GET: RequestHandler = async ({ request }) => {
	const ctx = await authenticateAgentRequest(request);

	const all = await listAgentProjects(ctx.organizationId);
	const projectsList = ctx.owner ? all.filter((p) => ctx.owner!.projectIds.includes(p.id)) : all;

	return json({ projects: projectsList });
};
