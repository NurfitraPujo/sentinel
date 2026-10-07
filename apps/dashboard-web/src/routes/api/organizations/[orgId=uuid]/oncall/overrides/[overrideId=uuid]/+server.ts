import { error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { requireOncallWriteAccess } from '$lib/server/issue-access';
import { deleteOverride, getActiveSchedule } from '$lib/db/queries/oncall';

/**
 * sentinel-user-report-issue §4.4a — `DELETE /api/organizations/{orgId}/oncall/overrides/{id}`.
 * `204`, same `owner`/`admin` bar as every other on-call write (§2).
 *
 * The delete is scoped to THIS organization's active schedule, and a row that does not belong to it
 * is a `404` rather than a `204`. Returning `204` for a delete that removed nothing would let a
 * caller probe another organization's override ids by timing or by absence of an error — and it
 * would tell an honest caller that a stale id had been cleaned up when it had not.
 */

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

export const DELETE: RequestHandler = async ({ params, locals }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}
	const userId = session.user.id;

	const { orgId, overrideId } = params;
	if (!orgId) {
		throw error(400, 'Missing organizationId');
	}
	if (!overrideId) {
		throw error(400, 'Missing overrideId');
	}
	// `oncall_overrides.id` is a UUID column, so a non-UUID path segment would reach Postgres as
	// `invalid input syntax for type uuid` and surface as a 500. It is a 404 — the id names no
	// override, which is the same answer as an id that simply does not exist.
	if (!UUID.test(overrideId)) {
		throw error(404, 'Override not found');
	}

	await requireOncallWriteAccess(userId, orgId);

	const schedule = await getActiveSchedule(orgId);
	if (!schedule) {
		throw error(404, 'No active on-call schedule for this organization');
	}

	const removed = await deleteOverride(schedule.id, overrideId);
	if (!removed) {
		throw error(404, 'Override not found');
	}

	return new Response(null, { status: 204 });
};
