import type { PageServerLoad } from './$types';
import { redirect } from '@sveltejs/kit';
import { issueQueries } from '$lib/server/queries/issue-queries';
import { getUser } from '$lib/server/auth';
import { getAccessibleProjects } from '$lib/server/issue-access';
import { resolveEnvironmentScopes, scopeForProject } from '$lib/server/environment-access';
import { listProjectEnvironments, toEnvironmentApiShape } from '$lib/db/queries/environments';

export const load: PageServerLoad = async ({ url, locals }) => {
	const user = await getUser({ locals } as any);

	if (!user) {
		redirect(303, '/signin');
	}

	// sentinel-project-environments §4.4, decision 35 — the project derivation WIDENS, from
	// `getUserProjectAccess` (`project_members`, which nothing in this application writes) to
	// `project_members` ∪ `organization_members` in `ISSUE_WRITE_ROLES`. This is user-visible in
	// both directions: `/` starts returning issues to organization-role staff who saw nothing, and
	// it stops returning every issue in the database to everybody — the empty array used to add no
	// project predicate at all. Decision 47: the set spans organizations, because `/` has no
	// organization in scope and never has.
	const accessibleProjects = await getAccessibleProjects(user.id);
	const authorizedProjectIds = accessibleProjects.map((p) => p.id);

	// §2.2's guard, resolved once for every project the caller can reach. One round trip, and the
	// one derivation of "which environments may this user see" — a project absent from the map is
	// unrestricted, never denied.
	const environmentScopes = await resolveEnvironmentScopes(user.id);

	const statusFilter = url.searchParams.get('status') ?? 'all';
	const projectIdFilter = url.searchParams.get('project') ?? 'all';
	const environmentFilter = url.searchParams.get('environment') ?? 'all';
	const page = parseInt(url.searchParams.get('page') ?? '1', 10);
	const limit = 20;

	const filters = {
		status: statusFilter,
		projectId: projectIdFilter,
		projectIds: authorizedProjectIds,
		environment: environmentFilter,
		environmentScopes,
	};

	const [results, userProjects, total] = await Promise.all([
		issueQueries.listIssues(filters, page, limit),
		issueQueries.listProjects(authorizedProjectIds),
		issueQueries.countIssues(filters)
	]);

	// §4.4: `[]` when `project=all`, and the environment control is not rendered. An environment
	// name is only meaningful inside a project — two projects may both report `production` and they
	// are not the same environment — so a cross-project list here would silently mean "any
	// project's production", which nothing in the data model backs. The GUARD still applies on
	// `project=all`; only the offered FILTER does not.
	const environments =
		projectIdFilter !== 'all' && authorizedProjectIds.includes(projectIdFilter)
			? await listSelectableEnvironments(
					projectIdFilter,
					scopeForProject(environmentScopes, projectIdFilter)
				)
			: [];

	const issuesWithProject = results.map((issue) => {
		const project = userProjects.find(p => p.id === issue.projectId);
		return {
			...issue,
			projectName: project?.name ?? 'Unknown Project',
		};
	});

	return {
		issues: issuesWithProject,
		projects: userProjects,
		environments,
		filters: {
			status: statusFilter,
			project: projectIdFilter,
			environment: environmentFilter,
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
 * §4.1 with `includeHidden=false`, inline — the selector feed, which is the registry minus `hidden`
 * rows minus anything outside the caller's grant. A filter for data you cannot read is a control
 * that does nothing.
 *
 * `[]` is the normal state of every project on the day this ships: under decision 8 no registry row
 * is ever created by traffic, only by §4.3's `POST`.
 */
async function listSelectableEnvironments(projectId: string, scope: string[] | null) {
	const rows = await listProjectEnvironments(projectId);
	return rows
		.filter((row) => !row.hidden && (scope === null || scope.includes(row.name)))
		.map(toEnvironmentApiShape);
}
