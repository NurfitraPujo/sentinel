import type { PageServerLoad } from './$types';
import { redirect } from '@sveltejs/kit';
import { issueQueries } from '$lib/server/queries/issue-queries';
import { getUser } from '$lib/server/auth';
import { getAccessibleProjects } from '$lib/server/issue-access';
import { resolveEnvironmentScopes, scopeForProject } from '$lib/server/environment-access';
import { listProjectEnvironments, toEnvironmentApiShape } from '$lib/db/queries/environments';
import { coerceIssueNewFilter, coerceIssueSort, firstSeenLowerBound } from '$lib/server/issue-sort';
import { getRetentionDays } from '$lib/server/retention-window';
import { listTrendWindow, TREND_HOURLY_SLOTS } from '$lib/server/trend-window';

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

	// A user who has just signed in for the first time has no organization and no project, so this
	// page could only ever render an empty list with no way to act on it. Send them to the one
	// place that can fix that. Only when they have NOTHING — a member of a project with no issues
	// yet still gets the real (empty) dashboard. Under the widened derivation above this fires for
	// strictly fewer users than it did on `main`: organization-role staff now reach a real project
	// set and no longer land on /onboarding.
	if (authorizedProjectIds.length === 0) {
		redirect(303, '/onboarding');
	}

	// §2.2's guard, resolved once for every project the caller can reach. One round trip, and the
	// one derivation of "which environments may this user see" — a project absent from the map is
	// unrestricted, never denied.
	const environmentScopes = await resolveEnvironmentScopes(user.id);

	const statusFilter = url.searchParams.get('status') ?? 'all';
	const projectIdFilter = url.searchParams.get('project') ?? 'all';
	const environmentFilter = url.searchParams.get('environment') ?? 'all';
	const claimFilter = url.searchParams.get('claim') ?? 'all';
	const page = parseInt(url.searchParams.get('page') ?? '1', 10);
	const ALLOWED_LIMITS = [20, 50, 100] as const;
	const parsedLimit = parseInt(url.searchParams.get('limit') ?? '20', 10);
	const limit = ALLOWED_LIMITS.includes(parsedLimit as any) ? parsedLimit : 20;

	// Decision 55 (`OQ-41` = (a)): on `project=all` the loader coerces `environment` to `all` and
	// adds NO environment predicate. An environment name is only meaningful inside a project, so
	// across projects `production` would mean "any project's production", which nothing in the data
	// model backs. The state is reachable by an ordinary route and not only by a hand-typed URL:
	// flow `L7` makes the parameter ride on the filter and paging anchors, so it survives a switch
	// to `project=all` and the reload of a bookmark. Since §4.4 returns `environments: []` there,
	// the control is not rendered — applied, the list would be silently narrowed with no on-screen
	// affordance to clear it, on the page whose empty state is required to name the environment.
	// The GUARD (`environmentScopes`) is unaffected; only the offered FILTER is dropped.
	const effectiveEnvironment = projectIdFilter === 'all' ? 'all' : environmentFilter;

	// sentinel-affected-users §4.1, decision 10 — `sort` follows the same rule as `environment`
	// above: the loader coerces, and the COERCED value is what the queries see and what the echo
	// says. `last_seen` is today's order and the default; `affected_users` is the new one; anything
	// else is `last_seen`, never the bad string — a typo in a bookmark must not 500 and must not
	// keep riding on links (`href()` builds every anchor from `filters.sort`).
	const sort = coerceIssueSort(url.searchParams.get('sort'));

	// sentinel-issue-trend §4.1, decision 6 — `new` is coerced the same way (`banana` → `all`, and
	// `all` is what the echo says), but unlike `sort` it is a FILTER: `24h` becomes a `first_seen`
	// lower bound inside the shared condition builder, so the list and `pagination.total` narrow
	// together. ONE instant for the whole request: the bound and the sparkline window below are
	// both taken from it, so the list, the count and the 24 series agree on what "now" is.
	const now = new Date();
	const newFilter = coerceIssueNewFilter(url.searchParams.get('new'));
	const firstSeenAfter = firstSeenLowerBound(newFilter, now);

	const baseFilters = {
		status: statusFilter,
		projectId: projectIdFilter,
		projectIds: authorizedProjectIds,
		environment: effectiveEnvironment,
		environmentScopes,
		// The SAME `Date` to both queries (below), never recomputed per call.
		firstSeenAfter,
	};

	const filters = {
		...baseFilters,
		claim: claimFilter,
		currentUserId: user.id,
	};

	const orgIds = Array.from(new Set(accessibleProjects.map((p) => p.organizationId).filter(Boolean)));

	const [results, userProjects, total, claimantFacets, orgUsersList, orgAgentsList] = await Promise.all([
		// `sort` goes to the list ONLY. It changes the `ORDER BY` of the guarded query and nothing
		// about its `WHERE` — `buildIssueConditions` never reads it — and `countIssues` takes the
		// filters WITHOUT it: `pagination.total` does not change with the order.
		issueQueries.listIssues({ ...filters, sort }, page, limit),
		issueQueries.listProjects(authorizedProjectIds),
		issueQueries.countIssues(filters),
		issueQueries.getClaimantFacetCounts(baseFilters, user.id),
		issueQueries.listOrgUsers(orgIds),
		issueQueries.listOrgAgents(orgIds),
	]);

	// De-duplicate and map orgAgents
	const agentsById: Record<string, { id: string; name: string }> = {};
	for (const a of orgAgentsList) {
		agentsById[a.id] = a;
	}
	const orgAgents = orgAgentsList.slice().sort((a, b) => a.name.localeCompare(b.name));

	// De-duplicate and sort orgUsers
	const usersById: Record<string, { id: string; name: string | null; email: string }> = {};
	for (const u of orgUsersList) {
		usersById[u.id] = u;
		if (u.email) {
			usersById[u.email] = u;
		}
	}
	// Also ensure currentUser is in usersById
	if (user.id) {
		usersById[user.id] = { id: user.id, name: (user as any).name ?? null, email: user.email };
		if (user.email) {
			usersById[user.email] = { id: user.id, name: (user as any).name ?? null, email: user.email };
		}
	}

	const uniqueOrgUsersMap = new Map<string, { id: string; name: string | null; email: string }>();
	for (const u of orgUsersList) {
		uniqueOrgUsersMap.set(u.id, u);
	}
	if (user.id && !uniqueOrgUsersMap.has(user.id)) {
		uniqueOrgUsersMap.set(user.id, { id: user.id, name: (user as any).name ?? null, email: user.email });
	}
	const orgUsers = Array.from(uniqueOrgUsersMap.values()).sort((a, b) =>
		(a.name || a.email).localeCompare(b.name || b.email)
	);

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

	// §4.1's per-card series, decision 10: ONE query for the whole page, keyed on exactly the ids
	// `listIssues` returned under the guards — it takes ids, never a project or a user, so it cannot
	// widen the set (§2). Skipped outright when the page is empty: there is nothing to attach a
	// series to, and `inArray([])` throws in this Drizzle version (the reader guards it too; the
	// loader simply never asks).
	const { windowStart, windowEnd } = listTrendWindow(now);
	const trends =
		results.length > 0
			? await issueQueries.trendForIssues(
					results.map((issue) => issue.id),
					windowStart,
					windowEnd
				)
			: new Map<string, number[]>();

	const issuesWithProject = results.map((issue) => {
		const project = userProjects.find(p => p.id === issue.projectId);
		return {
			...issue,
			projectName: project?.name ?? 'Unknown Project',
			// Dense, oldest first, `[23]` = the current partial UTC hour; `[0 × 24]` for a quiet
			// issue — never `null`, never shorter (§3). The reader returns an entry for every id it
			// was given; the fallback only says what shape the page data has, not where it came from.
			trend24h: trends.get(issue.id) ?? new Array<number>(TREND_HOURLY_SLOTS).fill(0),
		};
	});

	return {
		issues: issuesWithProject,
		projects: userProjects,
		environments,
		claimantFacets,
		usersById,
		orgUsers,
		agentsById,
		orgAgents,
		currentUser: {
			id: user.id,
			email: user.email,
		},
		filters: {
			status: statusFilter,
			project: projectIdFilter,
			// The coerced value, not the raw parameter: the page builds every anchor from this echo
			// (`href()` in `+page.svelte`), so echoing `production` here would keep the parameter
			// riding on links the reader has no control to clear it from (decision 55, flow `L7`).
			environment: effectiveEnvironment,
			// The coerced value again (§4.1): `banana` echoes `last_seen`.
			sort,
			// sentinel-issue-trend §4.1: the coerced value, for the same reason — `href()` builds
			// every anchor from this echo, so `banana` echoes `all`, never the bad string.
			new: newFilter,
			claim: claimFilter,
		},
		// §4.1, decision 21 — top level, beside `filters`. The window every count label carries
		// ("in the last N days"); a template cannot read `$env/dynamic/private`, and the number has
		// to be the one the retention sweep uses, so it comes from the helper the cron route reads.
		retentionDays: getRetentionDays(),
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
