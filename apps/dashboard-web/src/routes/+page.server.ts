import type { PageServerLoad } from './$types';
import { redirect } from '@sveltejs/kit';
import { issueQueries } from '$lib/server/queries/issue-queries';
import { getUser } from '$lib/server/auth';

export const load: PageServerLoad = async ({ url, locals }) => {
	const user = await getUser({ locals } as any);
	
	if (!user) {
		redirect(303, '/auth/signin');
	}
	
	const statusFilter = url.searchParams.get('status') ?? 'all';
	const projectIdFilter = url.searchParams.get('project') ?? 'all';
	const page = parseInt(url.searchParams.get('page') ?? '1', 10);
	const limit = 20;

	const filters = {
		status: statusFilter,
		projectId: projectIdFilter,
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