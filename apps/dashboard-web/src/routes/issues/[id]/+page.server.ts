import type { PageServerLoad } from './$types';
import { db } from '$lib/server/db';
import { issues, errorOccurrences, projects } from '$lib/db/schema';
import { eq, desc } from 'drizzle-orm';
import { error } from '@sveltejs/kit';

export const load: PageServerLoad = async ({ params }) => {
	const issueId = params.id;

	const [issue] = await db.select().from(issues).where(eq(issues.id, issueId)).limit(1);

	if (!issue) {
		throw error(404, 'Issue not found');
	}

	const [project] = await db.select().from(projects).where(eq(projects.id, issue.projectId)).limit(1);

	const occurrences = await db.select().from(errorOccurrences)
		.where(eq(errorOccurrences.issueId, issueId))
		.orderBy(desc(errorOccurrences.createdAt))
		.limit(100);

	return {
		issue,
		project: project ?? null,
		occurrences,
	};
};