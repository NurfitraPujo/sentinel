import { error } from '@sveltejs/kit';
import type { PageServerLoad } from './$types';

// Manual Issues M1 (docs/plans/MANUAL_ISSUES_DESIGN.md §10): manual-issues dashboard. Imitates
// the auth pattern at [orgSlug]/settings/keys/+page.server.ts -- authenticate before any lookup
// that could reveal whether an org exists (D39), then delegate the actual listing to the
// already-auth-gated `/api/organizations/[orgId]/reports` endpoint (report-access.ts's
// `requireReportAccess` runs there too, so this loader is not the only gate, just the page-level
// one consistent with every other org-scoped route).
const VALID_TABS = ['all', 'mine', 'claimed-by-me', 'unclaimed', 'needs-input', 'triage'] as const;
type ReportTab = (typeof VALID_TABS)[number];

function isValidTab(value: string | null): value is ReportTab {
	return value !== null && (VALID_TABS as readonly string[]).includes(value);
}

// sentinel-user-report-issue OQ-8 / fe.md rows L3-L4: the source filter.
//
// **It is orthogonal to `ReportTab`, not a seventh tab.** `tab` narrows by claim state and `source`
// narrows by which door the report came through; the two compose, both are written to the URL, and
// both survive a reload. The allowlist below is deliberately the same shape as `VALID_TABS` above.
//
// **An unknown value falls back, it does not throw.** A hand-edited or stale URL should show the
// unfiltered list, not a 400 page -- same as `tab` has always behaved. The API endpoint validates
// its own `source` parameter strictly (400), which is the right answer for a programmatic caller
// and the wrong one for a link somebody pasted.
//
// The vocabulary here is the UI's, not `issues.source_channel`'s, because the control fe.md
// specifies is *All / External / Org members* and "org members" is not one channel value. The
// mapping is one hop, done below and nowhere else:
//   external    -> `?source=user_report_api`, the one value this feature's door writes (SS4.2)
//   org-members -> every report with a `manual_issue_reports` companion, which is exactly
//                  `reporterKind === 'org_member'`. The list endpoint takes a single channel and
//                  cannot express "not this one", so this half is narrowed here on the row's own
//                  reporter kind rather than by enumerating the three other channel values --
//                  enumerating them would silently drop any channel a later migration adds.
const VALID_SOURCES = ['all', 'external', 'org-members'] as const;
type ReportSourceFilter = (typeof VALID_SOURCES)[number];

function isValidSource(value: string | null): value is ReportSourceFilter {
	return value !== null && (VALID_SOURCES as readonly string[]).includes(value);
}

export const load: PageServerLoad = async ({ params, url, fetch, locals }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}

	const currentOrg = locals.currentOrg;
	if (!currentOrg || currentOrg.slug !== params.orgSlug) {
		throw error(403, 'Forbidden: Unauthorized access to organization');
	}

	const tabParam = url.searchParams.get('tab');
	const tab: ReportTab = isValidTab(tabParam) ? tabParam : 'all';

	const sourceParam = url.searchParams.get('source');
	const source: ReportSourceFilter = isValidSource(sourceParam) ? sourceParam : 'all';

	const query = new URLSearchParams({ tab });
	if (source === 'external') {
		query.set('source', 'user_report_api');
	}

	const res = await fetch(`/api/organizations/${currentOrg.id}/reports?${query}`);
	if (!res.ok) {
		const body = await res.json().catch(() => ({ message: 'Failed to load reports' }));
		throw error(res.status, body.message || 'Failed to load reports');
	}

	const { reports } = await res.json();
	// eslint-disable-next-line @typescript-eslint/no-explicit-any -- the endpoint's JSON, as every
	// other loader in this repo consumes it.
	const rows: any[] = reports ?? [];

	return {
		orgId: currentOrg.id,
		orgSlug: currentOrg.slug,
		tab,
		source,
		reports:
			source === 'org-members' ? rows.filter((row) => row.reporterKind === 'org_member') : rows,
		userId: session.user.id,
	};
};
