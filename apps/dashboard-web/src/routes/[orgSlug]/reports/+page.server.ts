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

	const res = await fetch(`/api/organizations/${currentOrg.id}/reports?tab=${tab}`);
	if (!res.ok) {
		const body = await res.json().catch(() => ({ message: 'Failed to load reports' }));
		throw error(res.status, body.message || 'Failed to load reports');
	}

	const { reports } = await res.json();

	return {
		orgId: currentOrg.id,
		orgSlug: currentOrg.slug,
		tab,
		reports: reports ?? [],
		userId: session.user.id,
	};
};
