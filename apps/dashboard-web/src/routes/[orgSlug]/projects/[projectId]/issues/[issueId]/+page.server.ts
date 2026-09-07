import type { PageServerLoad } from './$types';
import { error } from '@sveltejs/kit';
import { requireProjectAccess } from '$lib/server/issue-access';
import {
	resolveEnvironmentScope,
	resolveEnvironmentScopes,
} from '$lib/server/environment-access';
import { resolveEnvironmentDisplayName } from '$lib/db/queries/environments';
import { issueQueries } from '$lib/server/queries/issue-queries';
import { getIssueRelations } from '$lib/db/queries/issues';
import { getOrgRole } from '$lib/server/report-access';

// D03: this route previously had no loader at all, so `data` was undefined and the page fell
// through to hardcoded mock data (currentIssueId = 'ISSUE-123'). Every mutation then carried a
// non-UUID id and failed at the DB layer — mirrors the working legacy loader at
// src/routes/issues/[id]/+page.server.ts, but scoped under the org/project path and re-verifying
// that the issue actually belongs to :projectId (a bare issueId lookup alone would let a caller
// view any issue by guessing/crawling ids as long as they had access to SOME project).
//
// sentinel-project-environments §4.5a: this is the SECOND door to one issue — the one a reviewer
// reading only `routes/issues/[id]/` misses — and it takes BOTH of that loader's layers, decision
// 46's helper swap and decision 20's `404` on the environment dimension.
export const load: PageServerLoad = async ({ params, locals }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}

	const { projectId, issueId } = params;

	const issue = await issueQueries.getIssueById(issueId);

	if (!issue || issue.projectId !== projectId) {
		throw error(404, 'Issue not found');
	}

	// §4.5a layer 1, decisions 35 and 46 — `checkProjectAccess` read `project_members` only, which
	// nothing writes, so this page `403`'d every user in any real deployment. `requireProjectAccess`
	// resolves the widened population and throws instead of returning a boolean; its errors are
	// contract §7's, on the PROJECT dimension, and its `404 Project not found` is not the
	// environment-dimension `404` below.
	await requireProjectAccess(session.user.id, projectId, 'read');

	// §4.5a layer 2 — the ENVIRONMENT dimension, `404` and never `403` (decision 20).
	const scope = await resolveEnvironmentScope(session.user.id, projectId);
	if (scope !== null && !scope.includes(issue.environment)) {
		throw error(404, 'Issue not found');
	}

	const project = await issueQueries.getProjectById(projectId);
	const occurrences = await issueQueries.getOccurrencesByIssueId(issueId);
	// Flow `D2`: a real `COUNT(*)` for the heading, not the capped page size.
	const occurrenceTotal = await issueQueries.countOccurrencesByIssueId(issueId);
	// §4.5a: registry display name, falling back to the raw name — the normal path (decisions 8+9).
	const environmentDisplayName = await resolveEnvironmentDisplayName(projectId, issue.environment);
	// §2.2, decision 37: the relations panel is guarded on the RELATED issue too, per project — an
	// out-of-scope relation comes back as a redacted placeholder rather than being dropped or 404'd.
	const relations = await getIssueRelations(
		issueId,
		await resolveEnvironmentScopes(session.user.id)
	);

	// Manual Issues M3 (design §5): the thread mounts here too -- "discussion on service issues is
	// the deliberate by-product" -- so the loader needs organizationId (for uploads) and the
	// caller's org role (for the thread's owner/admin delete-others rule), same shape as the
	// reports detail loader.
	const organizationId = project?.organizationId ?? null;
	const userRole = organizationId ? await getOrgRole(session.user.id, organizationId) : null;

	return {
		issue,
		project: project ?? null,
		occurrences,
		occurrenceTotal,
		environmentDisplayName,
		relations,
		organizationId,
		userId: session.user.id,
		userRole,
	};
};
