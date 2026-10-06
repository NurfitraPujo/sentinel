import { eq } from 'drizzle-orm';
import { db } from '$lib/server/db';
import { projects } from '$lib/db/schema';
import { getAccessibleProjects } from '$lib/server/issue-access';
import { resolveEnvironmentScopes } from '$lib/server/environment-access';
import type { SpanScope } from '$lib/server/queries/span-queries';

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
 * An agent key's scope: the whole organization, no environment grants — agent reads are organization-scoped by design
 * (docs/ENVIRONMENT_ACCESS.md), exactly as `listAgentTraceOccurrences` is.
 */
export async function resolveAgentSpanScope(organizationId: string): Promise<SpanScope> {
	const rows = await db.select({ id: projects.id }).from(projects).where(eq(projects.organizationId, organizationId));
	return { orgId: organizationId, projects: rows, environmentScopes: new Map() };
}
