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
import { getRetentionDays } from '$lib/server/retention-window';
import { detailTrendWindow } from '$lib/server/trend-window';

export const load: PageServerLoad = async ({ params, locals }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}

	const issueId = params.id;
	const issue = await issueQueries.getIssueById(issueId);

	if (!issue) {
		throw error(404, 'Issue not found');
	}

	// sentinel-project-environments §4.5a layer 1, decisions 35 and 46 — the PROJECT dimension, and
	// it changes here by SWAPPING THE HELPER, not by editing the old one.
	//
	// This was `checkProjectAccess(userId, projectId, 'viewer')`, which resolves access through
	// `project_members` only — a table nothing in this application writes — so it threw `403` for
	// every user in any real deployment, the opposite failure from `/`'s open read on the same
	// cause. `requireProjectAccess` already resolves the widened population (`isIssueWriteRole` on
	// the org role, with a `project_members` row as the read-only alternative path) and computes no
	// `roleHierarchy` at all, which is why widening the old helper's comparison was refused: org
	// roles are not project roles, `roleHierarchy['engineer']` is `undefined`, and the fix would
	// have been a silent no-op.
	//
	// It THROWS rather than returning a boolean, so the error strings on this page become the
	// helper's own (contract §7). Four are reachable at `'read'` and all four are on the PROJECT
	// dimension — including its `404 Project not found`, which is NOT the environment-dimension
	// `404` below.
	await requireProjectAccess(session.user.id, issue.projectId, 'read');

	// §4.5a layer 2 — the ENVIRONMENT dimension. `404`, never `403` (decision 20): a `403` here
	// tells the caller that a production issue with this id exists, which is the fact the boundary
	// was drawn to withhold.
	const scope = await resolveEnvironmentScope(session.user.id, issue.projectId);
	if (scope !== null && !scope.includes(issue.environment)) {
		throw error(404, 'Issue not found');
	}

	const project = await issueQueries.getProjectById(issue.projectId);
	// sentinel-issue-trend §4.2, decision 11 — AFTER both access checks above, keyed on the same
	// `issueId` the loader has just been allowed to read (§2). One PK range query over the 30 UTC
	// calendar days ending today, bounded at both ends, densified by the reader into `hourly` (24)
	// and `daily` (30). The window is taken from one instant so the two headings agree on "now".
	const { windowStart, windowEnd } = detailTrendWindow(new Date());
	const trend = await issueQueries.trendForIssue(issueId, windowStart, windowEnd);
	const occurrences = await issueQueries.getOccurrencesByIssueId(issueId);
	// Flow `D2`: `Occurrence History ({occurrences.length})` presented a page size as a total —
	// `getOccurrencesByIssueId` caps at 100. The heading takes this real `COUNT(*)`; the list below
	// says it is showing the most recent n.
	const occurrenceTotal = await issueQueries.countOccurrencesByIssueId(issueId);
	// §4.5a: the registry's display name, falling back to the raw name. Decisions 8+9 make the
	// fallback the normal path for a new environment, not an error case.
	const environmentDisplayName = await resolveEnvironmentDisplayName(
		issue.projectId,
		issue.environment
	);
	// §2.2, decision 37: the relations panel is guarded on the RELATED issue too, per project — an
	// out-of-scope relation comes back as a redacted placeholder rather than being dropped or 404'd.
	const relations = await getIssueRelations(
		issueId,
		await resolveEnvironmentScopes(session.user.id)
	);

	return {
		issue,
		project: project ?? null,
		occurrences,
		occurrenceTotal,
		environmentDisplayName,
		relations,
		// sentinel-issue-trend §4.2 — top level beside `issue`: `{ hourly: TrendPoint[24], daily:
		// TrendPoint[30] }`, dense, oldest first, the last entry of each the current partial slot.
		trend,
		// sentinel-affected-users §4.2, decision 21 — top level beside `issue`, the same number `/`
		// returns and the retention sweep uses: `Affected users (last {retentionDays} days)`. The two
		// counts themselves need no loader change; `getIssueById` selects the whole row.
		retentionDays: getRetentionDays(),
	};
};
