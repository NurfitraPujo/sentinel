import type { PageServerLoad } from './$types';
import { redirect } from '@sveltejs/kit';
import { getUser } from '$lib/server/auth';
import { loadShell } from '$lib/server/analytics/shell';
import * as svc from '$lib/server/analytics/service';
import { LIST_ONLY_PARAMS } from '$lib/server/dashboard-routing';

/**
 * sentinel-reporting-and-dashboard D12/D13: `/` is the dashboard. The issue list moved to `/issues`.
 *
 * Old links keep working: a request carrying ANY list-only parameter is `308`ed to `/issues` with the query string
 * unchanged (contract §6), before any session or database work. `project`, `environment` and `range` are the
 * dashboard's own filters and do not trigger it.
 *
 * Each section is its own streamed promise (D14): a slow or failing section never blocks the others, and a Databend
 * outage renders "unavailable" per section while the page, the nav and `/issues` keep working. A user with no
 * accessible project gets the empty state and no query at all (D11).
 */
export const load: PageServerLoad = async ({ url, locals }) => {
	for (const key of url.searchParams.keys()) {
		if (LIST_ONLY_PARAMS.has(key)) redirect(308, `/issues${url.search}`);
	}

	const user = await getUser({ locals } as never);
	if (!user) redirect(303, '/signin');

	const shell = await loadShell(user.id, {
		project: url.searchParams.get('project'),
		environment: url.searchParams.get('environment'),
		range: url.searchParams.get('range'),
		split: url.searchParams.get('split'),
	});
	const { input } = shell;

	const filters = { project: input.project, environment: input.environment, range: input.range, split: input.split ?? 'group' };
	const base = { filters, projects: shell.projects, environments: shell.environments, databendConfigured: shell.databendConfigured };

	if (shell.projects.length === 0) return { ...base, empty: true as const, sections: null };

	return {
		...base,
		empty: false as const,
		// the http SLI's service names, so a count can link to the list's `service` filter (D12)
		apiServices: input.slis.filter((s) => s.kind === 'http' && s.service).map((s) => s.service as string),
		sections: {
			freshness: svc.freshness(input, shell.newestPostgresOccurrence),
			health: svc.health(input),
			timeline: svc.timeline(input),
			slo: svc.sloStatus(input),
			natsBreakdown: svc.natsBreakdown(input),
			budget: svc.budgetBurndown(input),
			releases: svc.releases(input),
			backlog: svc.backlog(input),
			response: svc.response(input),
		},
	};
};
