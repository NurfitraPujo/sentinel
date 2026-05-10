import type { PageServerLoad, Actions } from './$types';
import { db } from '$lib/server/db';
import { issues, projects } from '$lib/db/schema';
import { eq, desc, sql } from 'drizzle-orm';
import { checkProjectAccess } from '$lib/server/projects';
import { requireAuth } from '$lib/server/auth';
import { redirect } from '@sveltejs/kit';

export const load: PageServerLoad = async ({ url, locals }) => {
	const user = await requireAuth({ locals } as any);
	
	const statusFilter = url.searchParams.get('status') ?? 'all';
	const projectIdFilter = url.searchParams.get('project') ?? 'all';
	const page = parseInt(url.searchParams.get('page') ?? '1', 10);
	const limit = 20;
	const offset = (page - 1) * limit;

	const userProjects = await db.select({
		id: projects.id,
		name: projects.name,
	}).from(projects).limit(100);

	let query = db.select({
		id: issues.id,
		projectId: issues.projectId,
		fingerprint: issues.fingerprint,
		message: issues.message,
		errorClass: issues.errorClass,
		status: issues.status,
		firstSeen: issues.firstSeen,
		lastSeen: issues.lastSeen,
		count: issues.count,
	}).from(issues);

	const results = await query.orderBy(desc(issues.lastSeen)).limit(limit).offset(offset);

	const issuesWithProject = await Promise.all(
		results.map(async (issue) => {
			const project = userProjects.find(p => p.id === issue.projectId);
			return {
				...issue,
				projectName: project?.name ?? 'Unknown Project',
			};
		})
	);

	const totalResult = await db.select({ count: sql<number>`count(*)` }).from(issues);
	const total = Number(totalResult[0]?.count ?? 0);

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