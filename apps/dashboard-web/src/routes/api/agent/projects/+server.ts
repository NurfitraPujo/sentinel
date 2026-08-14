import { json } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { authenticateAgentRequest } from '$lib/server/agent-auth';
import { listAgentProjects } from '$lib/db/queries/agent-reads';

// N1c (agent read endpoints). GET /api/agent/projects -- no issue scope, so this authenticates
// directly (authenticateAgentRequest) rather than going through withAgentIssue. Lists the
// calling key's own org's projects (B7: organizationId from AgentAuthContext, never a param).

export const GET: RequestHandler = async ({ request }) => {
	const ctx = await authenticateAgentRequest(request);

	const projectsList = await listAgentProjects(ctx.organizationId);

	return json({ projects: projectsList });
};
