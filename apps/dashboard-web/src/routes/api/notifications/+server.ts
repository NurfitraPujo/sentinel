import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import {
	listNotifications,
	getUnreadNotificationCount,
	markNotificationRead,
	markAllNotificationsRead,
} from '$lib/db/queries/notifications';

// Manual Issues M4 (docs/plans/MANUAL_ISSUES_DESIGN.md §8, §10). Manual-validation style
// (allowlists + throw error(status)), matching the invitations/reports/comments endpoints -- no
// schema library. No org-scoping needed here beyond the session -- notifications.user_id IS the
// tenant boundary (a user's own inbox spans every org they belong to, same as their session).

const MAX_LIMIT = 100;

export const GET: RequestHandler = async ({ url, locals }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}
	const userId = session.user.id;

	// §8/§10 unread-count folded into GET, per the M4 task's "unread count endpoint or fold into
	// GET (?count=unread)" -- picking the fold-in, so the bell badge and the list share one route.
	if (url.searchParams.get('count') === 'unread') {
		const count = await getUnreadNotificationCount(userId);
		return json({ count });
	}

	const limitParam = url.searchParams.get('limit');
	let limit: number | undefined;
	if (limitParam !== null) {
		const parsed = Number(limitParam);
		if (!Number.isInteger(parsed) || parsed < 1 || parsed > MAX_LIMIT) {
			throw error(400, `limit must be an integer between 1 and ${MAX_LIMIT}`);
		}
		limit = parsed;
	}

	const offsetParam = url.searchParams.get('offset');
	let offset: number | undefined;
	if (offsetParam !== null) {
		const parsed = Number(offsetParam);
		if (!Number.isInteger(parsed) || parsed < 0) {
			throw error(400, 'offset must be a non-negative integer');
		}
		offset = parsed;
	}

	const items = await listNotifications({ userId, limit, offset });
	return json({ notifications: items });
};

export const PATCH: RequestHandler = async ({ request, locals }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}
	const userId = session.user.id;

	const body = await request.json().catch(() => ({}));
	const { id, all } = body;

	if (all === true) {
		const count = await markAllNotificationsRead(userId);
		return json({ success: true, count });
	}

	if (typeof id !== 'string' || id.length === 0) {
		throw error(400, 'id (string) or all:true is required');
	}

	const updated = await markNotificationRead(id, userId);
	if (!updated) {
		throw error(404, 'Notification not found');
	}

	return json({ success: true });
};
