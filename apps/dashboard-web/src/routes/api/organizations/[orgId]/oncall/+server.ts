import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { requireReportAccess } from '$lib/server/report-access';
import { requireOncallWriteAccess } from '$lib/server/issue-access';
import {
	getActiveSchedule,
	getEligibleMemberIds,
	patchSchedule,
	replaceSchedule,
	deactivateSchedule,
	resolveCurrentlyOnCall,
	type OncallScheduleView,
	type ScheduleInput,
	type SchedulePatch,
} from '$lib/db/queries/oncall';
import {
	ROTATION_KINDS,
	isIsoWeekday,
	isRotationKind,
	isValidIanaTimeZone,
	parseHandoffTime,
} from '$lib/server/oncall';

/**
 * sentinel-user-report-issue §4.4a — `GET`, `PUT`, `PATCH` and `DELETE` on an organization's
 * on-call schedule.
 *
 * **The permission split is per method, not per route** (§2). READ is any recognised org member —
 * `requireReportAccess(…, 'read')`, viewers included. WRITE is `owner`/`admin` ONLY, which is
 * NARROWER than `ISSUE_WRITE_ROLES`: an `engineer` who may resolve and assign issues may not edit
 * the rota, because a rota any engineer can rewrite is a rota edited by whoever does not want the
 * pager. `requireOncallWriteAccess` (issue-access.ts) is that bar, deliberately sitting next to
 * `ISSUE_WRITE_ROLES` so the gap is visible.
 *
 * Errors are SvelteKit's `error(status, message)` shape, NOT §7's envelope — §4.4a says so
 * explicitly: these are dashboard routes and they inherit what every other dashboard route does.
 *
 * Validation style is the repo's manual allowlist-and-throw (see `reports/+server.ts`), not a
 * schema library.
 */

const MAX_NAME_LENGTH = 255;
const MAX_MEMBERS = 100;

/** §4.4a's `GET` response shape, `camelCase` (§3). The anchor is NOT part of it: it is an internal
 *  phase reference, and exposing it would invite a client to send it back on a `PUT`. */
function serializeSchedule(schedule: OncallScheduleView) {
	return {
		id: schedule.id,
		name: schedule.name,
		timezone: schedule.timezone,
		kind: schedule.kind,
		handoffTime: schedule.handoffTime,
		handoffWeekday: schedule.handoffWeekday,
		members: schedule.members.map((m) => ({
			userId: m.userId,
			name: m.name,
			position: m.position,
			eligible: m.eligible,
		})),
		overrides: schedule.overrides.map((o) => ({
			id: o.id,
			userId: o.userId,
			startsAt: o.startsAt,
			endsAt: o.endsAt,
		})),
	};
}

async function readJsonBody(request: Request): Promise<Record<string, unknown>> {
	let body: unknown;
	try {
		body = await request.json();
	} catch {
		throw error(400, 'Request body must be valid JSON');
	}
	if (typeof body !== 'object' || body === null || Array.isArray(body)) {
		throw error(400, 'Request body must be a JSON object');
	}
	return body as Record<string, unknown>;
}

function validateName(value: unknown): string {
	if (typeof value !== 'string' || value.trim().length === 0) {
		throw error(400, 'name is required');
	}
	const trimmed = value.trim();
	if (trimmed.length > MAX_NAME_LENGTH) {
		throw error(400, `name must be at most ${MAX_NAME_LENGTH} characters`);
	}
	return trimmed;
}

/**
 * §3: IANA names only. A fixed offset such as `"+07:00"` is a `400` and not a stored value —
 * an offset cannot survive a DST boundary, and a rota that is one hour wrong twice a year wakes
 * the wrong person.
 */
function validateTimezone(value: unknown): string {
	if (!isValidIanaTimeZone(value)) {
		throw error(400, 'timezone must be an IANA zone name (e.g. "Asia/Jakarta"), not a fixed offset');
	}
	return value;
}

function validateHandoffTime(value: unknown): string {
	const parsed = parseHandoffTime(value);
	if (parsed === null) {
		throw error(400, 'handoffTime must be "HH:MM" in 24-hour form');
	}
	return parsed;
}

/**
 * §4.6's multi-column CHECK, restated at the edge so it is a `400` with a sentence rather than a
 * constraint violation: weekly needs a weekday, daily must not carry one. The encoding is ISO 8601
 * (SR-4), 1 = Monday … 7 = Sunday — NOT `Date#getDay()`.
 */
function validateHandoffWeekday(kind: string, value: unknown): number | null {
	if (kind === 'weekly') {
		if (!isIsoWeekday(value)) {
			throw error(400, 'handoffWeekday must be an ISO weekday 1-7 (1 = Monday) for a weekly rotation');
		}
		return value;
	}
	if (value !== undefined && value !== null) {
		throw error(400, 'handoffWeekday must be absent or null for a daily rotation');
	}
	return null;
}

function validateKind(value: unknown): 'daily' | 'weekly' {
	if (!isRotationKind(value)) {
		throw error(400, `kind must be one of: ${ROTATION_KINDS.join(', ')}`);
	}
	return value;
}

/**
 * `memberIds` is an ORDERED list of user ids, and every entry must be an org member holding an
 * `ISSUE_WRITE_ROLES` role — §4.4a's `400`. Duplicates are refused here rather than at the
 * `(schedule_id, user_id)` primary key, so the caller gets a sentence instead of a 500.
 *
 * Note the asymmetry with `eligible: false` in the `GET` response, which is deliberate and not a
 * contradiction: you may not ADD someone below the bar, but a member who DROPS below it afterwards
 * stays on the list and is skipped at resolve time (§4.4).
 */
async function validateMemberIds(organizationId: string, value: unknown): Promise<string[]> {
	if (!Array.isArray(value)) {
		throw error(400, 'memberIds must be an array of user ids');
	}
	if (value.length > MAX_MEMBERS) {
		throw error(400, `memberIds must contain at most ${MAX_MEMBERS} entries`);
	}
	const ids: string[] = [];
	for (const entry of value) {
		if (typeof entry !== 'string' || entry.length === 0) {
			throw error(400, 'memberIds entries must be user id strings');
		}
		if (ids.includes(entry)) {
			throw error(400, `memberIds contains ${entry} more than once`);
		}
		ids.push(entry);
	}

	const eligible = await getEligibleMemberIds(organizationId, ids);
	for (const id of ids) {
		if (!eligible.has(id)) {
			throw error(
				400,
				`memberIds entry ${id} is not an organization member with an issue-write role`
			);
		}
	}
	return ids;
}

function requireOrgId(orgId: string | undefined): string {
	if (!orgId) {
		throw error(400, 'Missing organizationId');
	}
	return orgId;
}

async function requireSession(locals: App.Locals): Promise<string> {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}
	return session.user.id;
}

export const GET: RequestHandler = async ({ params, locals }) => {
	const userId = await requireSession(locals);
	const orgId = requireOrgId(params.orgId);

	// §2/§4.4a: read is the ordinary org-member bar, viewers included.
	await requireReportAccess(userId, orgId, 'read');

	const schedule = await getActiveSchedule(orgId);
	if (!schedule) {
		// An organization that has never created one, or whose schedule was deactivated. Not a 404:
		// the resource is "this org's on-call arrangement" and the answer is "there is none", which
		// is exactly what the on-call editor needs in order to offer a create.
		return json({ schedule: null, currentlyOnCall: null });
	}

	// Request time is the clock, supplied here — the resolution itself never reads one (§11).
	return json({
		schedule: serializeSchedule(schedule),
		currentlyOnCall: resolveCurrentlyOnCall(schedule, new Date()),
	});
};

export const PUT: RequestHandler = async ({ params, request, locals }) => {
	const userId = await requireSession(locals);
	const orgId = requireOrgId(params.orgId);
	await requireOncallWriteAccess(userId, orgId);

	const body = await readJsonBody(request);
	const kind = validateKind(body.kind);
	const input: ScheduleInput = {
		name: validateName(body.name),
		timezone: validateTimezone(body.timezone),
		kind,
		handoffTime: validateHandoffTime(body.handoffTime),
		handoffWeekday: validateHandoffWeekday(kind, body.handoffWeekday),
		memberIds: await validateMemberIds(orgId, body.memberIds),
	};

	// Create-or-replace: an existing active schedule is updated in place, which is what preserves
	// SR-4's anchor across the edit. `replaceSchedule` never inserts a second active row, so the
	// partial unique index is the backstop for a concurrent writer rather than this handler's job.
	await replaceSchedule(orgId, input, userId);

	const schedule = await getActiveSchedule(orgId);
	if (!schedule) {
		throw error(500, 'Schedule was not persisted');
	}
	return json({
		schedule: serializeSchedule(schedule),
		currentlyOnCall: resolveCurrentlyOnCall(schedule, new Date()),
	});
};

export const PATCH: RequestHandler = async ({ params, request, locals }) => {
	const userId = await requireSession(locals);
	const orgId = requireOrgId(params.orgId);
	await requireOncallWriteAccess(userId, orgId);

	const existing = await getActiveSchedule(orgId);
	if (!existing) {
		throw error(404, 'No active on-call schedule for this organization');
	}

	const body = await readJsonBody(request);
	const patch: SchedulePatch = {};

	// §3: absent = leave alone, null = clear. Only `handoffWeekday` is clearable, because every
	// other column is NOT NULL — a null on those is a 400 rather than a silent no-op, so a client
	// that means "clear the name" is told it cannot instead of believing it did.
	for (const field of ['name', 'timezone', 'kind', 'handoffTime'] as const) {
		if (body[field] === null) {
			throw error(400, `${field} cannot be null`);
		}
	}

	if (body.name !== undefined) patch.name = validateName(body.name);
	if (body.timezone !== undefined) patch.timezone = validateTimezone(body.timezone);
	if (body.handoffTime !== undefined) patch.handoffTime = validateHandoffTime(body.handoffTime);
	if (body.kind !== undefined) patch.kind = validateKind(body.kind);

	// The weekday rule is a rule about the PAIR, so it is checked against the kind this patch
	// LEAVES BEHIND, not against the kind it happens to carry: switching weekly -> daily without
	// clearing the weekday, or daily -> weekly without supplying one, both violate §4.6's CHECK.
	const effectiveKind = patch.kind ?? existing.kind;
	if (body.handoffWeekday !== undefined || patch.kind !== undefined) {
		const supplied = body.handoffWeekday !== undefined ? body.handoffWeekday : existing.handoffWeekday;
		patch.handoffWeekday = validateHandoffWeekday(effectiveKind, supplied);
	}

	if (body.memberIds !== undefined) {
		if (body.memberIds === null) {
			throw error(400, 'memberIds cannot be null');
		}
		// §4.4a: replace-whole-list, never a merge.
		patch.memberIds = await validateMemberIds(orgId, body.memberIds);
	}

	await patchSchedule(existing.id, patch);

	const updated = await getActiveSchedule(orgId);
	if (!updated) {
		throw error(500, 'Schedule was not persisted');
	}
	return json({
		schedule: serializeSchedule(updated),
		currentlyOnCall: resolveCurrentlyOnCall(updated, new Date()),
	});
};

export const DELETE: RequestHandler = async ({ params, locals }) => {
	const userId = await requireSession(locals);
	const orgId = requireOrgId(params.orgId);
	await requireOncallWriteAccess(userId, orgId);

	const existing = await getActiveSchedule(orgId);
	if (!existing) {
		throw error(404, 'No active on-call schedule for this organization');
	}

	// Deactivate, never delete (§4.4a). Escalation then stops running for this org and says so.
	await deactivateSchedule(existing.id);
	return new Response(null, { status: 204 });
};
