import { error } from '@sveltejs/kit';
import type { PageServerLoad } from './$types';
import { listNotifications, getNotificationCount } from '$lib/db/queries/notifications';

// Manual Issues M4 (docs/plans/MANUAL_ISSUES_DESIGN.md §8/§10): the full notification list page.
// Org-scoped in the URL (`/[orgSlug]/notifications`) purely for nav consistency with every other
// page in this app -- a user's notification inbox itself spans every org they belong to (same as
// notifications.ts's read side), so this loader does NOT filter by org, only authenticates and
// confirms `orgSlug` resolves to a real org the user can see (same auth-gate pattern as
// [orgSlug]/reports/+page.server.ts). No `reservedRoutes` entry is needed: this route lives under
// the dynamic `[orgSlug]` segment, exactly like /[orgSlug]/reports.
const PAGE_SIZE = 25;

export const load: PageServerLoad = async ({ params, url, locals }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}

	const currentOrg = locals.currentOrg;
	if (!currentOrg || currentOrg.slug !== params.orgSlug) {
		throw error(403, 'Forbidden: Unauthorized access to organization');
	}

	const pageParam = url.searchParams.get('page');
	let page = 1;
	if (pageParam !== null) {
		const parsed = Number(pageParam);
		if (Number.isInteger(parsed) && parsed >= 1) {
			page = parsed;
		}
	}

	const userId = session.user.id;
	const offset = (page - 1) * PAGE_SIZE;

	const [items, total] = await Promise.all([
		listNotifications({ userId, limit: PAGE_SIZE, offset }),
		getNotificationCount(userId),
	]);

	return {
		orgSlug: currentOrg.slug,
		notifications: items,
		page,
		pageSize: PAGE_SIZE,
		total,
	};
};
