import type { PageServerLoad } from './$types';
import { searchErrors } from '$lib/server/search';
import { requireAuth } from '$lib/server/auth';
import { db } from '$lib/server/db';
import { projects, projectMembers } from '$lib/db/schema';
import { eq } from 'drizzle-orm';
import { getAccessibleProjects } from '$lib/server/issue-access';
import { resolveEnvironmentScopes, scopeForProject } from '$lib/server/environment-access';
import { resolveEnvironmentUnion } from '$lib/server/environment-union';
import { listProjectEnvironments, toEnvironmentApiShape } from '$lib/db/queries/environments';

export const load: PageServerLoad = async ({ url, locals }) => {
	const user = await requireAuth({ locals } as any);

	const userId = url.searchParams.get('user_id') || undefined;
	const tenantId = url.searchParams.get('tenant_id') || undefined;
	const traceId = url.searchParams.get('trace_id') || undefined;
	const spanId = url.searchParams.get('span_id') || undefined;
	const requestId = url.searchParams.get('request_id') || undefined;
	const projectId = url.searchParams.get('project') || undefined;
	const environment = url.searchParams.get('environment') || undefined;
	const platform = url.searchParams.get('platform') || undefined;
	const search = url.searchParams.get('search') || undefined;

	const page = parseInt(url.searchParams.get('page') ?? '1', 10);
	const limit = 50;
	const offset = (page - 1) * limit;

	const userProjects = await db
		.select({
			id: projects.id,
			name: projects.name,
		})
		.from(projects)
		.innerJoin(projectMembers, eq(projectMembers.projectId, projects.id))
		.where(eq(projectMembers.userId, user.id));

	const filters = {
		userId,
		tenantId,
		traceId,
		spanId,
		requestId,
		projectId,
		environment,
		platform,
		search,
	};

	const { results, total } = await searchErrors(user, filters, { limit, offset });

	// sentinel-project-environments §4.5 change 1 — the selector's feed. `/search`'s three literal
	// `<option>` elements are replaced by §4.1's registry for a selected project and by §4.3a's
	// GUARDED union when none is selected; this is the field that carries them.
	//
	// The union is `resolveEnvironmentUnion`, not a list assembled here: §4.3a is a guard, not a
	// convenience (hidden excluded per project BEFORE de-duplication, decision 43; a project absent
	// from the scopes map unrestricted, never denied), and a second derivation of it here would be a
	// second place for those two rules to be inverted. It carries `name` only, because two projects'
	// `production` are two rows with two labels and publishing one of them for the union would be a
	// lie the page tells for free (§4.3a).
	const environments = await listSelectableEnvironments(user.id, projectId);

	return {
		results,
		environments,
		projects: userProjects,
		filters: {
			userId,
			tenantId,
			traceId,
			spanId,
			requestId,
			projectId,
			environment,
			platform,
			search,
		},
		pagination: {
			page,
			limit,
			total,
			totalPages: Math.ceil(total / limit),
		},
	};
};

/**
 * §4.1 with `includeHidden=false` for a selected project, §4.3a's guarded union otherwise — the
 * same two sources §4.5 change 1 names, and the same `hidden`/scope filter `/`'s loader applies.
 *
 * `[]` is a normal answer, not an error: under decision 8 no registry row is ever created by
 * traffic, so every project starts here.
 */
async function listSelectableEnvironments(userId: string, projectId: string | undefined) {
	if (!projectId) {
		const names = await resolveEnvironmentUnion(userId);
		return names.map((name) => ({ name }));
	}

	// The same population `searchErrors` resolves its results over (§4.5 change 3). A registry is
	// not an issue, but it is still this project's, and answering for a project the caller cannot
	// reach would hand out its environment names on a page that returns none of its rows.
	const accessible = await getAccessibleProjects(userId);
	if (!accessible.some((p) => p.id === projectId)) {
		return [];
	}

	const scopes = await resolveEnvironmentScopes(userId);
	const scope = scopeForProject(scopes, projectId);
	const rows = await listProjectEnvironments(projectId);
	return rows
		.filter((row) => !row.hidden && (scope === null || scope.includes(row.name)))
		.map(toEnvironmentApiShape);
}
