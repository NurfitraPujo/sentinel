import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { requireOncallWriteAccess } from '$lib/server/issue-access';
import {
	createOverride,
	getActiveSchedule,
	getEligibleMemberIds,
	hasOverlappingOverride,
} from '$lib/db/queries/oncall';
import { parseInstant } from '$lib/server/oncall';

/**
 * sentinel-user-report-issue §4.4a — `POST /api/organizations/{orgId}/oncall/overrides`.
 *
 * An override is `(user, startsAt, endsAt)` in absolute UTC and it wins over the rotation for its
 * interval. Without overrides, escalation assigns to whoever the arithmetic says is on duty even
 * when that person is on holiday, and the feature's own premise — "available engineers" — is false
 * on day one. That is why this is a write on the schedule and carries the same `owner`/`admin` bar
 * (§2): changing who is woken up is the same act whether it is done by editing the rota or by
 * covering it.
 *
 * `201` on success; `400` for a window whose `endsAt` is not after its `startsAt`; `409` for a
 * window that overlaps an existing override on the same schedule.
 */

export const POST: RequestHandler = async ({ params, request, locals }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}
	const userId = session.user.id;

	const { orgId } = params;
	if (!orgId) {
		throw error(400, 'Missing organizationId');
	}

	await requireOncallWriteAccess(userId, orgId);

	const schedule = await getActiveSchedule(orgId);
	if (!schedule) {
		throw error(404, 'No active on-call schedule for this organization');
	}

	let body: unknown;
	try {
		body = await request.json();
	} catch {
		throw error(400, 'Request body must be valid JSON');
	}
	if (typeof body !== 'object' || body === null || Array.isArray(body)) {
		throw error(400, 'Request body must be a JSON object');
	}
	const { userId: targetUserId, startsAt: startsAtRaw, endsAt: endsAtRaw } = body as Record<
		string,
		unknown
	>;

	if (typeof targetUserId !== 'string' || targetUserId.length === 0) {
		throw error(400, 'userId is required');
	}

	const startsAt = parseInstant(startsAtRaw);
	if (!startsAt) {
		throw error(400, 'startsAt must be an RFC 3339 timestamp');
	}
	const endsAt = parseInstant(endsAtRaw);
	if (!endsAt) {
		throw error(400, 'endsAt must be an RFC 3339 timestamp');
	}

	// §4.4a's 400. `oncall_overrides_window_check` enforces the same rule in the database; this is
	// here so the caller gets a sentence rather than a constraint violation surfaced as a 500.
	if (endsAt.getTime() <= startsAt.getTime()) {
		throw error(400, 'endsAt must be after startsAt');
	}

	// The covering engineer is held to the same bar as a rotation member: an override may not put
	// the pager on someone who is not in this organization with an issue-write role. Tenancy comes
	// from the path org, never from the body (B7) — `getEligibleMemberIds` is scoped to it.
	const eligible = await getEligibleMemberIds(orgId, [targetUserId]);
	if (!eligible.has(targetUserId)) {
		throw error(400, `userId ${targetUserId} is not an organization member with an issue-write role`);
	}

	// §4.4a's 409. Half-open intervals, so back-to-back cover (one ending exactly where the next
	// begins) is allowed — that is the normal way to hand a holiday over.
	if (await hasOverlappingOverride(schedule.id, startsAt, endsAt)) {
		throw error(409, 'Override overlaps an existing override on this schedule');
	}

	const created = await createOverride(schedule.id, targetUserId, startsAt, endsAt, userId);

	return json(
		{
			override: {
				id: created.id,
				userId: created.userId,
				startsAt: created.startsAt,
				endsAt: created.endsAt,
			},
		},
		{ status: 201 }
	);
};
