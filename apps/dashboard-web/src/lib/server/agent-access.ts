import { error } from '@sveltejs/kit';
import { and, inArray, sql, type AnyColumn, type SQL } from 'drizzle-orm';
import { issues } from '$lib/db/schema';
import type { AgentAuthContext } from '$lib/server/agent-auth';
import { environmentScopeCondition, scopeForProject } from '$lib/server/environment-access';

/**
 * sentinel-cli-browser-login (docs/plans/CLI_BROWSER_LOGIN.md, "Personal-key authorization rule"): the ONE place an
 * `/api/agent/*` route asks "what may this key see / do". An org agent (`ctx.owner === null`) keeps today's behaviour
 * exactly -- the whole organization, read and write. A personal key is narrowed to its owner's live access, which
 * `authenticateAgentRequest` resolved for THIS request from the dashboard's own helpers (`getAccessibleProjects`,
 * `resolveEnvironmentScopes`, `rbac.ts`).
 *
 * The conventions, matching the rest of the agent API:
 *   - a list is FILTERED to what the owner may see (`agentVisibility` + `agentVisibilityCondition`);
 *   - a single object outside it is a 404, indistinguishable from another organization's (`isVisibleToAgent`);
 *   - a mutation by a read-only owner is a 403 (`requireAgentWrite`);
 *   - an endpoint no personal key may use is a 403 (`denyPersonalKey`).
 */

/** What a personal key may see; `null` means "the whole organization" (an org agent). */
export interface AgentVisibility {
	projectIds: string[];
	environmentScopes: Map<string, string[]>;
}

export function agentVisibility(ctx: AgentAuthContext): AgentVisibility | null {
	if (!ctx.owner) return null;
	return { projectIds: ctx.owner.projectIds, environmentScopes: ctx.owner.environmentScopes };
}

/**
 * The visibility rule as SQL over a (project, environment) column pair -- `issues` by default. `undefined` for an org
 * agent (no extra predicate). An owner who can reach no project gets `false`, never an empty `IN ()` (drizzle 0.30's
 * `inArray` throws on an empty list, and "no predicate" would be the opposite of what is meant).
 */
export function agentVisibilityCondition(
	visibility: AgentVisibility | null | undefined,
	columns: { projectId: AnyColumn; environment: AnyColumn } = issues
): SQL | undefined {
	if (!visibility) return undefined;
	if (visibility.projectIds.length === 0) return sql`false`;
	return and(inArray(columns.projectId, visibility.projectIds), environmentScopeCondition(visibility.environmentScopes, columns));
}

/** Whether a (project, environment) pair is inside the key's visibility. Always true for an org agent. */
export function isVisibleToAgent(ctx: AgentAuthContext, projectId: string, environment: string): boolean {
	if (!ctx.owner) return true;
	if (!ctx.owner.projectIds.includes(projectId)) return false;
	const scope = scopeForProject(ctx.owner.environmentScopes, projectId);
	return scope === null || scope.includes(environment);
}

export const PERSONAL_KEY_READ_ONLY_MESSAGE =
	'Forbidden: your organization role is read-only, so your personal CLI key cannot make changes';

/** 403 unless the key may mutate: always for an org agent; for a personal key, iff the owner's role has rbac `write`. */
export function requireAgentWrite(ctx: AgentAuthContext): void {
	if (ctx.owner && !ctx.owner.canWrite) {
		throw error(403, PERSONAL_KEY_READ_ONLY_MESSAGE);
	}
}

/** 403 for any personal key on an endpoint reserved to admin-provisioned org agents. */
export function denyPersonalKey(ctx: AgentAuthContext, what: string): void {
	if (ctx.owner) {
		throw error(403, `Forbidden: personal CLI keys cannot ${what}; use the dashboard or an org agent key`);
	}
}

/** True for a personal (`sentinel login`) key. */
export function isPersonalKey(ctx: AgentAuthContext): boolean {
	return ctx.owner !== null;
}
