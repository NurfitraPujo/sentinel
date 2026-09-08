import type { PageServerLoad } from './$types';
import { redirect } from '@sveltejs/kit';
import { issueQueries } from '$lib/server/queries/issue-queries';
import { getUser } from '$lib/server/auth';
import { getUserProjectAccess } from '$lib/server/projects';

export const load: PageServerLoad = async ({ url, locals }) => {
	const user = await getUser({ locals } as any);
	
	if (!user) {
		redirect(303, '/signin');
	}
	
	const userAccess = await getUserProjectAccess(user.id);
	const authorizedProjectIds = userAccess.map(a => a.projectId);

	// A user who has just signed in for the first time has no organization and no project, so this
	// page could only ever render an empty list with no way to act on it. Send them to the one
	// place that can fix that. Only when they have NOTHING — a member of a project with no issues
	// yet still gets the real (empty) dashboard.
	if (authorizedProjectIds.length === 0) {
		redirect(303, '/onboarding');
	}
	
	const statusFilter = url.searchParams.get('status') ?? 'all';
	const projectIdFilter = url.searchParams.get('project') ?? 'all';
	const page = parseInt(url.searchParams.get('page') ?? '1', 10);
	const limit = 20;

	const filters = {
		status: statusFilter,
		projectId: projectIdFilter,
		projectIds: authorizedProjectIds,
	};

	const [results, userProjects, total] = await Promise.all([
		issueQueries.listIssues(filters, page, limit),
		issueQueries.listProjects(),
		issueQueries.countIssues(filters)
	]);

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
		filters: {
			status: statusFilter,
			project: projectIdFilter,
		},
		pagination: {
			page,
			limit,
			total,
			totalPages: Math.ceil(total / limit),
		},
	};
};