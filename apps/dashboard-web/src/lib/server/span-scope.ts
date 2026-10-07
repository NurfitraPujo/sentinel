import { eq } from 'drizzle-orm';
import { db } from '$lib/server/db';
import { projects } from '$lib/db/schema';
import { getAccessibleProjects } from '$lib/server/issue-access';
import { resolveEnvironmentScopes } from '$lib/server/environment-access';
import type { SpanScope } from '$lib/server/queries/span-queries';
import type { AgentAuthContext } from '$lib/server/agent-auth';

/**
 * sentinel-occurrence-spans — the `SpanScope` the guarded span reads take, built the one way each kind of caller gets it.
 * The organization id is always the caller's own (a session's membership, an agent key's organization), never a request
 * value that was not checked against one.
 */

/** A signed-in viewer's scope in one organization. `null` when they can reach none of its projects (or are not a member). */
export async function resolveViewerSpanScope(userId: string, organizationId: string): Promise<SpanScope | null> {
	const accessible = await getAccessibleProjects(userId, organizationId);
	if (accessible.length === 0) return null;
	return { orgId: organizationId, projects: accessible, environmentScopes: await resolveEnvironmentScopes(userId) };
}

/**
 * An agent key's scope. An ORG agent: the whole organization, no environment grants — org-agent reads are
 * organization-scoped by design (docs/ENVIRONMENT_ACCESS.md), exactly as `listAgentTraceOccurrences` is. A PERSONAL key
 * (sentinel-cli-browser-login): its owner's live projects and grants, which `authenticateAgentRequest` already resolved
 * through the same helpers `resolveViewerSpanScope` uses — so the CLI sees exactly the spans the dashboard shows that
 * person (an owner with no project gets an empty set, which the span guards treat as `false`).
 */
export async function resolveAgentSpanScope(ctx: AgentAuthContext): Promise<SpanScope> {
	if (ctx.owner) {
		return {
			orgId: ctx.organizationId,
			projects: ctx.owner.projectIds.map((id) => ({ id })),
			environmentScopes: ctx.owner.environmentScopes,
		};
	}
	const rows = await db.select({ id: projects.id }).from(projects).where(eq(projects.organizationId, ctx.organizationId));
	return { orgId: ctx.organizationId, projects: rows, environmentScopes: new Map() };
}
