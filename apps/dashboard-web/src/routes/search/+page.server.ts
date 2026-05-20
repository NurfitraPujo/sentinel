import type { PageServerLoad } from './$types';
import { searchErrors } from '$lib/server/search';
import { requireAuth } from '$lib/server/auth';
import { db } from '$lib/server/db';
import { projects, projectMembers } from '$lib/db/schema';
import { eq } from 'drizzle-orm';

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

	return {
		results,
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