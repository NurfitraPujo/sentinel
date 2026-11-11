import { and, asc, eq, gt, inArray, lt, ne } from 'drizzle-orm';
import { db } from '$lib/server/db';
import {
	oncallSchedules,
	oncallScheduleMembers,
	oncallOverrides,
	organizationMembers,
	users,
} from '$lib/db/schema';
import { ISSUE_WRITE_ROLES } from '$lib/server/issue-access';
import {
	formatHandoffTime,
	whoIsOnCall,
	type OncallMemberSpec,
	type OncallOverrideSpec,
	type OncallScheduleSpec,
	type RotationKind,
} from '$lib/server/oncall';

/**
 * sentinel-user-report-issue §4.4a — the database half of the on-call schedule.
 *
 * The resolution ITSELF is not here: `whoIsOnCall` lives in `$lib/server/oncall.ts` as a pure
 * function with no database and no clock (§11's boundary), and this module's only job is to hand
 * it rows and shape what comes back into §4.4a's `camelCase` response.
 *
 * Two rules are enforced by the database rather than by code here, on purpose:
 *   - **At most one active schedule per organization** — `idx_oncall_schedules_one_active`, the
 *     partial unique index on `(organization_id) WHERE active`. `replaceSchedule` updates the
 *     existing active row in place instead of inserting a second one, and a concurrent insert that
 *     loses the race hits the index, not a handler check.
 *   - **`ends_at > starts_at`** on an override — `oncall_overrides_window_check`. The route still
 *     validates it to return §4.4a's `400` with a message rather than a 500, but the constraint is
 *     what makes the rule true for every writer.
 */

/** §4.4a's member shape, plus the display name the `GET` response carries. */
export interface OncallMemberView {
	userId: string;
	name: string | null;
	position: number;
	/** Live: does this user still hold an `ISSUE_WRITE_ROLES` org role? Never a stored column. */
	eligible: boolean;
}

export interface OncallOverrideView {
	id: string;
	userId: string;
	startsAt: Date;
	endsAt: Date;
}

export interface OncallScheduleView {
	id: string;
	name: string;
	timezone: string;
	kind: RotationKind;
	/** `"HH:MM"` (§3), normalised off the `TIME` column's `"HH:MM:SS"`. */
	handoffTime: string;
	/** ISO 8601, 1 = Monday … 7 = Sunday. Null unless `kind === 'weekly'`. */
	handoffWeekday: number | null;
	/** SR-4's anchor. Not part of §4.4a's response shape; carried for the resolution. */
	rotationAnchorAt: Date;
	members: OncallMemberView[];
	overrides: OncallOverrideView[];
}

export interface OncallResolutionView {
	userId: string;
	name: string | null;
	via: 'rotation' | 'override';
}

export interface ScheduleInput {
	name: string;
	timezone: string;
	kind: RotationKind;
	handoffTime: string;
	handoffWeekday: number | null;
	memberIds: string[];
}

/**
 * The active schedule for `organizationId`, with its ordered members and its overrides — or null
 * when the organization has none (never created one, or `DELETE /oncall` deactivated it).
 */
export async function getActiveSchedule(
	organizationId: string
): Promise<OncallScheduleView | null> {
	const scheduleRows = await db
		.select({
			id: oncallSchedules.id,
			name: oncallSchedules.name,
			timezone: oncallSchedules.timezone,
			kind: oncallSchedules.kind,
			handoffTime: oncallSchedules.handoffTime,
			handoffWeekday: oncallSchedules.handoffWeekday,
			rotationAnchorAt: oncallSchedules.rotationAnchorAt,
		})
		.from(oncallSchedules)
		.where(and(eq(oncallSchedules.organizationId, organizationId), eq(oncallSchedules.active, true)));

	const schedule = scheduleRows[0];
	if (!schedule) {
		return null;
	}

	// Eligibility is a LEFT join on purpose: a member whose organization_members row is gone
	// entirely must still appear in the list (§4.4's "shown, skipped, never silently deleted"),
	// with `eligible: false`. An inner join would delete them from the response instead.
	const memberRows = await db
		.select({
			userId: oncallScheduleMembers.userId,
			position: oncallScheduleMembers.position,
			name: users.name,
			role: organizationMembers.role,
		})
		.from(oncallScheduleMembers)
		.leftJoin(users, eq(users.id, oncallScheduleMembers.userId))
		.leftJoin(
			organizationMembers,
			and(
				eq(organizationMembers.userId, oncallScheduleMembers.userId),
				eq(organizationMembers.organizationId, organizationId)
			)
		)
		.where(eq(oncallScheduleMembers.scheduleId, schedule.id))
		.orderBy(asc(oncallScheduleMembers.position));

	const overrideRows = await db
		.select({
			id: oncallOverrides.id,
			userId: oncallOverrides.userId,
			startsAt: oncallOverrides.startsAt,
			endsAt: oncallOverrides.endsAt,
		})
		.from(oncallOverrides)
		.where(eq(oncallOverrides.scheduleId, schedule.id))
		.orderBy(asc(oncallOverrides.startsAt));

	return {
		id: schedule.id,
		name: schedule.name,
		timezone: schedule.timezone,
		kind: schedule.kind as RotationKind,
		handoffTime: formatHandoffTime(schedule.handoffTime),
		handoffWeekday: schedule.handoffWeekday,
		rotationAnchorAt: schedule.rotationAnchorAt,
		members: memberRows.map((row) => ({
			userId: row.userId,
			name: row.name ?? null,
			position: row.position,
			eligible:
				row.role !== null && (ISSUE_WRITE_ROLES as readonly string[]).includes(row.role),
		})),
		overrides: overrideRows,
	};
}

/** The `whoIsOnCall` inputs, extracted from a loaded schedule. */
export function toResolutionInputs(schedule: OncallScheduleView): {
	spec: OncallScheduleSpec;
	members: OncallMemberSpec[];
	overrides: OncallOverrideSpec[];
} {
	return {
		spec: {
			timezone: schedule.timezone,
			kind: schedule.kind,
			handoffTime: schedule.handoffTime,
			handoffWeekday: schedule.handoffWeekday,
			rotationAnchorAt: schedule.rotationAnchorAt,
		},
		members: schedule.members.map((m) => ({
			userId: m.userId,
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

/**
 * §4.4a's `currentlyOnCall`: the resolved user at `at`, or null when nobody resolves — no active
 * schedule, no members, or none of them eligible. Never an error (§4.4).
 *
 * `at` is a parameter and not `new Date()` because the resolution boundary must stay clock-free;
 * the route supplies request time.
 */
export function resolveCurrentlyOnCall(
	schedule: OncallScheduleView,
	at: Date
): OncallResolutionView | null {
	const { spec, members, overrides } = toResolutionInputs(schedule);
	const resolved = whoIsOnCall(spec, members, overrides, at);
	if (!resolved) {
		return null;
	}
	// A member's display name is already loaded; an override may name someone who is not on the
	// rotation, in which case the name is not and the response carries null rather than a lie.
	const named = schedule.members.find((m) => m.userId === resolved.userId);
	return { userId: resolved.userId, name: named?.name ?? null, via: resolved.via };
}

/**
 * The subset of `userIds` that are org members of `organizationId` holding an `ISSUE_WRITE_ROLES`
 * role — §4.4a's `400` gate for `memberIds` and for an override's `userId`.
 *
 * Returns a Set so a caller can report WHICH id failed rather than just that one did.
 */
export async function getEligibleMemberIds(
	organizationId: string,
	userIds: readonly string[]
): Promise<Set<string>> {
	if (userIds.length === 0) {
		return new Set();
	}
	const rows = await db
		.select({ userId: organizationMembers.userId })
		.from(organizationMembers)
		.where(
			and(
				eq(organizationMembers.organizationId, organizationId),
				inArray(organizationMembers.userId, [...userIds]),
				inArray(organizationMembers.role, [...ISSUE_WRITE_ROLES])
			)
		);
	return new Set(rows.map((r) => r.userId));
}

/**
 * §4.4a's `PUT` — create or replace.
 *
 * **The anchor is preserved on a replace** (SR-4): an existing active schedule is UPDATEd in place,
 * so `rotation_anchor_at` — and with it the phase of every future handoff — survives an edit that
 * only fixes a typo in the name. Creating a fresh row instead would re-phase the rotation on every
 * save, which is the failure SR-4's anchor column exists to prevent.
 *
 * Members are replaced wholesale rather than diffed: `memberIds` is an ORDERED list and position is
 * uniquely indexed, so a diff would have to work out an update order that never transiently
 * collides. Delete-then-insert inside one transaction has no such ordering problem.
 */
export async function replaceSchedule(
	organizationId: string,
	input: ScheduleInput,
	createdBy: string
): Promise<string> {
	return await db.transaction(async (tx) => {
		const existing = await tx
			.select({ id: oncallSchedules.id })
			.from(oncallSchedules)
			.where(
				and(eq(oncallSchedules.organizationId, organizationId), eq(oncallSchedules.active, true))
			)
			.for('update');

		let scheduleId: string;

		if (existing.length > 0) {
			scheduleId = existing[0].id;
			await tx
				.update(oncallSchedules)
				.set({
					name: input.name,
					timezone: input.timezone,
					kind: input.kind,
					handoffTime: input.handoffTime,
					handoffWeekday: input.handoffWeekday,
					updatedAt: new Date(),
				})
				.where(eq(oncallSchedules.id, scheduleId));
			await tx
				.delete(oncallScheduleMembers)
				.where(eq(oncallScheduleMembers.scheduleId, scheduleId));
		} else {
			// rotation_anchor_at is left to its DEFAULT NOW(): a brand-new rotation is anchored at the
			// moment it was created, and that is the only instant it can be anchored at.
			const inserted = await tx
				.insert(oncallSchedules)
				.values({
					organizationId,
					name: input.name,
					timezone: input.timezone,
					kind: input.kind,
					handoffTime: input.handoffTime,
					handoffWeekday: input.handoffWeekday,
					createdBy,
				})
				.returning({ id: oncallSchedules.id });
			scheduleId = inserted[0].id;
		}

		if (input.memberIds.length > 0) {
			await tx.insert(oncallScheduleMembers).values(
				input.memberIds.map((userId, index) => ({
					scheduleId,
					userId,
					position: index,
				}))
			);
		}

		return scheduleId;
	});
}

/** The fields §4.4a's `PATCH` may change. Absent = leave alone (§3), so every key is optional. */
export interface SchedulePatch {
	name?: string;
	timezone?: string;
	kind?: RotationKind;
	handoffTime?: string;
	handoffWeekday?: number | null;
	memberIds?: string[];
}

/**
 * §4.4a's `PATCH`. Absent means leave alone and `null` means clear (§3) — which only
 * `handoffWeekday` can be, since every other column is NOT NULL; the route rejects a null on those
 * with a `400` before reaching here.
 *
 * `memberIds` is replace-whole-list, never a merge (§4.4a), for the same position-collision reason
 * `replaceSchedule` gives. The anchor is untouched here too: a `PATCH` is by definition an edit.
 */
export async function patchSchedule(
	scheduleId: string,
	patch: SchedulePatch
): Promise<void> {
	await db.transaction(async (tx) => {
		const changes: Record<string, unknown> = {};
		if (patch.name !== undefined) changes.name = patch.name;
		if (patch.timezone !== undefined) changes.timezone = patch.timezone;
		if (patch.kind !== undefined) changes.kind = patch.kind;
		if (patch.handoffTime !== undefined) changes.handoffTime = patch.handoffTime;
		if (patch.handoffWeekday !== undefined) changes.handoffWeekday = patch.handoffWeekday;

		// updatedAt moves for any field change, and also for a members-only change: the schedule the
		// organization is running did change, and a mtime that lies about that is worse than none.
		changes.updatedAt = new Date();
		await tx.update(oncallSchedules).set(changes).where(eq(oncallSchedules.id, scheduleId));

		if (patch.memberIds !== undefined) {
			await tx
				.delete(oncallScheduleMembers)
				.where(eq(oncallScheduleMembers.scheduleId, scheduleId));
			if (patch.memberIds.length > 0) {
				await tx.insert(oncallScheduleMembers).values(
					patch.memberIds.map((userId, index) => ({ scheduleId, userId, position: index }))
				);
			}
		}
	});
}

/**
 * §4.4a's `DELETE /oncall` — deactivate, never delete. The row and its history stay; the partial
 * unique index only counts `active` rows, so the organization can create a new schedule afterwards.
 * Escalation then stops running for this org and says so (§4.4).
 */
export async function deactivateSchedule(scheduleId: string): Promise<void> {
	await db
		.update(oncallSchedules)
		.set({ active: false, updatedAt: new Date() })
		.where(eq(oncallSchedules.id, scheduleId));
}

/**
 * True when `[startsAt, endsAt)` intersects an existing override on the same schedule — §4.4a's
 * `409`. Half-open on both sides, so an override that ends exactly where another begins is not an
 * overlap; back-to-back cover is the normal way to hand a holiday over.
 *
 * `excludeId` is unused today (no override update endpoint exists) but is the parameter that keeps
 * this callable from one if §4.4a ever grows a `PATCH`; it is threaded rather than added later
 * because forgetting it is how an update starts conflicting with itself.
 */
export async function hasOverlappingOverride(
	scheduleId: string,
	startsAt: Date,
	endsAt: Date,
	excludeId?: string
): Promise<boolean> {
	const conditions = [
		eq(oncallOverrides.scheduleId, scheduleId),
		// Typed operators, not a raw `sql` template: the template would hand postgres.js a bare
		// JS Date it does not know how to serialise for a TIMESTAMPTZ, which fails at runtime while
		// typechecking cleanly. `lt`/`gt` carry the column's type with them.
		lt(oncallOverrides.startsAt, endsAt),
		gt(oncallOverrides.endsAt, startsAt),
	];
	if (excludeId) {
		conditions.push(ne(oncallOverrides.id, excludeId));
	}
	const rows = await db
		.select({ id: oncallOverrides.id })
		.from(oncallOverrides)
		.where(and(...conditions))
		.limit(1);
	return rows.length > 0;
}

export async function createOverride(
	scheduleId: string,
	userId: string,
	startsAt: Date,
	endsAt: Date,
	createdBy: string
): Promise<OncallOverrideView> {
	const inserted = await db
		.insert(oncallOverrides)
		.values({ scheduleId, userId, startsAt, endsAt, createdBy })
		.returning({
			id: oncallOverrides.id,
			userId: oncallOverrides.userId,
			startsAt: oncallOverrides.startsAt,
			endsAt: oncallOverrides.endsAt,
		});
	return inserted[0];
}

/**
 * Deletes an override, but only if it belongs to `scheduleId` — the tenancy check. Returns whether
 * a row was removed, so the route can 404 an id from another organization's schedule rather than
 * silently returning `204` for a delete that did nothing.
 */
export async function deleteOverride(scheduleId: string, overrideId: string): Promise<boolean> {
	const deleted = await db
		.delete(oncallOverrides)
		.where(and(eq(oncallOverrides.id, overrideId), eq(oncallOverrides.scheduleId, scheduleId)))
		.returning({ id: oncallOverrides.id });
	return deleted.length > 0;
}
