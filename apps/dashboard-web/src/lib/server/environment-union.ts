import { inArray } from 'drizzle-orm';
import { db } from './db';
import { projectEnvironments } from '$lib/db/schema';
import { getAccessibleProjects } from './issue-access';
import { resolveEnvironmentScopes, scopeForProject } from './environment-access';

/**
 * sentinel-project-environments §4.3a — the cross-project environment union, as ONE function with
 * two callers.
 *
 * `GET /api/environments` returns it; `PUT`/`POST /api/alerts` checks an ORG-WIDE config's supplied
 * `environments` against it (§4.7, decision 26). §4.7 is explicit that the org-wide rule "reuses
 * §4.3a's resolver rather than adding a second scope rule", because a picker that offers a name the
 * save then rejects is a bug report waiting to be filed — so the guard has to be the same call the
 * control is populated from, not a re-derivation that agrees with it today.
 *
 * It lives here rather than in `environment-access.ts` for a mechanical reason: it needs
 * `getAccessibleProjects`, `issue-access.ts` already imports `environment-access.ts`, and putting it
 * there would close an import cycle.
 *
 * The two rules that are easy to invert, both load-bearing and both stated at the route:
 *
 * - `hidden` rows are excluded PER PROJECT, before de-duplication (decision 43) — two projects may
 *   both declare `canary` and hide it in only one.
 * - A project ABSENT from the scopes map is unrestricted, not denied (`scopeForProject`).
 *
 * With no `organizationId` the population spans organizations, deliberately (decision 47).
 */
export async function resolveEnvironmentUnion(
	userId: string,
	organizationId?: string
): Promise<string[]> {
	const accessible = await getAccessibleProjects(userId, organizationId);
	if (accessible.length === 0) {
		return [];
	}

	const [rows, scopes] = await Promise.all([
		db
			.select({
				projectId: projectEnvironments.projectId,
				name: projectEnvironments.name,
				hidden: projectEnvironments.hidden,
			})
			.from(projectEnvironments)
			.where(
				inArray(
					projectEnvironments.projectId,
					accessible.map((p) => p.id)
				)
			),
		resolveEnvironmentScopes(userId),
	]);

	const names = new Set<string>();
	for (const row of rows) {
		if (row.hidden) continue;
		const scope = scopeForProject(scopes, row.projectId);
		if (scope !== null && !scope.includes(row.name)) continue;
		names.add(row.name);
	}

	return [...names].sort();
}
