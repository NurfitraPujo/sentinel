/**
 * sentinel-user-report-issue §4.4 + SR-4 — who is on call at an instant.
 *
 * This module is a **pure function and its helpers**: no database, no ambient clock, no `Date.now()`.
 * §11 names `whoIsOnCall(schedule, members, overrides, at)` as a test boundary for exactly that
 * reason — rotation arithmetic, override precedence, ineligible-member skipping and the wall-clock
 * DST boundary are table-driven, and driving them through HTTP would need a seeded schedule per case.
 *
 * **The rotation is anchored on wall-clock handoff BOUNDARIES, not on elapsed periods** (SR-4).
 * §4.4's original wording ("whole handoff periods between created_at and T") contradicted its own
 * DST sentence — a 23- or 25-hour day is not a whole period — and left `kind='weekly'` undefined,
 * since `handoff_weekday` never entered that formula. The settled rule, implemented here:
 *
 *   - A handoff boundary is the local wall-clock instant matching `handoffTime` on a qualifying
 *     local date: every date for `daily`, the dates whose ISO weekday equals `handoffWeekday`
 *     for `weekly`.
 *   - The index is the COUNT of boundaries at or before `at`, counted from the first boundary at
 *     or after `rotationAnchorAt`, modulo the member count.
 *   - Before the first boundary, position 0 is on call.
 *
 * Three encodings are settled with it and are load-bearing here:
 *   - `handoffWeekday` is **ISO 8601**: 1 = Monday … 7 = Sunday. (Not `Date#getDay()`, which is
 *     0 = Sunday — naming the two side by side is how the migration got this wrong the first time.)
 *   - A `handoffTime` that **does not exist** on a DST day resolves **forward** to the next valid
 *     instant; one that exists **twice** resolves to the **first**.
 *   - The anchor is a column of its own (`oncall_schedules.rotation_anchor_at`), not `created_at`,
 *     because §4.4a's `PUT` is create-or-replace: saving the editor to fix a typo in the schedule's
 *     name must not re-phase every future handoff.
 *
 * Because handoff is wall-clock, a handoff day that crosses a DST boundary is 23 or 25 hours long.
 * That is deliberate: fixing the period at exactly 24 hours walks the handoff an hour off local
 * time twice a year, which is worse for the person carrying the pager.
 */

/** §4.6's `oncall_schedules.kind` CHECK. */
export const ROTATION_KINDS = ['daily', 'weekly'] as const;
export type RotationKind = (typeof ROTATION_KINDS)[number];

/**
 * The schedule facts the resolution needs — deliberately not the database row, so this function
 * can be called with a literal in a test and with a query result in a route.
 */
export interface OncallScheduleSpec {
	/** IANA zone name (`Asia/Jakarta`). A fixed offset is rejected upstream — see `isValidIanaTimeZone`. */
	timezone: string;
	kind: RotationKind;
	/** `"HH:MM"`, 24-hour, wall-clock in `timezone` (§3). */
	handoffTime: string;
	/** ISO 8601 weekday, 1 = Monday … 7 = Sunday. Null unless `kind === 'weekly'`. */
	handoffWeekday: number | null;
	/** SR-4's explicit anchor: the instant the rotation's phase is measured from. */
	rotationAnchorAt: Date;
}

/**
 * One row of the ordered member list. `eligible` is the LIVE answer to "does this user still hold
 * an `ISSUE_WRITE_ROLES` org role", resolved by the caller against `organization_members` — it is
 * not a column, because §4.4 skips such a member rather than removing them.
 */
export interface OncallMemberSpec {
	userId: string;
	position: number;
	eligible: boolean;
}

/** An override interval, in absolute UTC. Half-open: `[startsAt, endsAt)`. */
export interface OncallOverrideSpec {
	id: string;
	userId: string;
	startsAt: Date;
	endsAt: Date;
}

export type OncallVia = 'rotation' | 'override';

export interface OncallResolution {
	userId: string;
	via: OncallVia;
}

const MS_PER_DAY = 86_400_000;

/**
 * IANA zone names only (§3). The pattern is the gate, not `Intl` — modern ICU **accepts**
 * `"+07:00"` as a time zone, so a bare try/catch around `Intl.DateTimeFormat` would let exactly
 * the value the contract rejects through. A leading `+`/`-` or a `:` can never begin an IANA
 * name, so the regex refuses offsets before `Intl` is consulted at all.
 *
 * The 64-character bound matches `oncall_schedules.timezone VARCHAR(64)`, so an over-long value
 * is a 400 rather than a database error.
 */
const IANA_ZONE_NAME = /^[A-Za-z][A-Za-z0-9_+-]*(?:\/[A-Za-z0-9_+.-]+)*$/;

export function isValidIanaTimeZone(value: unknown): value is string {
	if (typeof value !== 'string' || value.length === 0 || value.length > 64) {
		return false;
	}
	if (!IANA_ZONE_NAME.test(value)) {
		return false;
	}
	try {
		new Intl.DateTimeFormat('en-US', { timeZone: value });
		return true;
	} catch {
		return false;
	}
}

/** §3's `"HH:MM"`, 24-hour. Returns the normalised value, or null if it is not that shape. */
const HANDOFF_TIME = /^([01]\d|2[0-3]):([0-5]\d)$/;

export function parseHandoffTime(value: unknown): string | null {
	if (typeof value !== 'string') {
		return null;
	}
	const match = HANDOFF_TIME.exec(value);
	return match ? `${match[1]}:${match[2]}` : null;
}

/**
 * Postgres returns a `TIME` column as `"HH:MM:SS"`. §3's wire format is `"HH:MM"`, and the two
 * must not both circulate — a client that round-trips a `GET` into a `PUT` would otherwise send a
 * value its own validator rejects.
 */
export function formatHandoffTime(value: string): string {
	const match = /^(\d{2}):(\d{2})/.exec(value);
	return match ? `${match[1]}:${match[2]}` : value;
}

/**
 * An RFC 3339 instant from a request body (§3), or null when the value is not one. `Date`'s
 * constructor accepts a great deal that is not a timestamp (`new Date("2026")`, `new Date(0)`),
 * so the type check and the `NaN` check are both load-bearing.
 */
export function parseInstant(value: unknown): Date | null {
	if (typeof value !== 'string' || value.length === 0) {
		return null;
	}
	const parsed = new Date(value);
	return Number.isNaN(parsed.getTime()) ? null : parsed;
}

export function isRotationKind(value: unknown): value is RotationKind {
	return typeof value === 'string' && (ROTATION_KINDS as readonly string[]).includes(value);
}

/** ISO 8601: 1 = Monday … 7 = Sunday. */
export function isIsoWeekday(value: unknown): value is number {
	return typeof value === 'number' && Number.isInteger(value) && value >= 1 && value <= 7;
}

interface CivilTime {
	/** `Date.UTC(y, m - 1, d)` — the local calendar DATE, carried as a UTC-midnight instant so
	 *  date arithmetic is plain integer arithmetic and never touches a zone again. */
	dateMs: number;
	/** `Date.UTC(y, m - 1, d, H, M, S)` — the whole local wall-clock reading, same trick. */
	civilMs: number;
}

/**
 * The wall-clock reading of `instant` in `timeZone`, encoded as UTC-milliseconds of the same
 * civil fields. `Intl.DateTimeFormat` is the only zone database available here (no date library
 * is a dependency of this app, deliberately), and `formatToParts` with `hourCycle: 'h23'` is the
 * one form of it that gives back the numeric fields rather than a localised string.
 */
function civilTimeIn(instant: number, timeZone: string): CivilTime {
	const parts = new Intl.DateTimeFormat('en-US', {
		timeZone,
		hourCycle: 'h23',
		year: 'numeric',
		month: '2-digit',
		day: '2-digit',
		hour: '2-digit',
		minute: '2-digit',
		second: '2-digit',
	}).formatToParts(new Date(instant));

	const field = (type: string): number => {
		const part = parts.find((p) => p.type === type);
		return part ? Number(part.value) : 0;
	};

	const year = field('year');
	const month = field('month');
	const day = field('day');
	// `hourCycle: 'h23'` still renders midnight as "24" in some ICU versions; normalise it here
	// rather than letting a 24 leak into Date.UTC and silently roll the date forward.
	const hour = field('hour') % 24;

	return {
		dateMs: Date.UTC(year, month - 1, day),
		civilMs: Date.UTC(year, month - 1, day, hour, field('minute'), field('second')),
	};
}

/** The zone's UTC offset at `instant`, in milliseconds (positive east of Greenwich). */
function zoneOffsetMs(instant: number, timeZone: string): number {
	return civilTimeIn(instant, timeZone).civilMs - instant;
}

/**
 * The instant at which the local wall-clock reading `civilMs` occurs in `timeZone`.
 *
 * Two DST cases have no single answer and SR-4 settles both:
 *   - **The reading exists twice** (a fall-back hour). Both candidates are valid; the FIRST — the
 *     earlier instant, under the pre-transition offset — wins.
 *   - **The reading does not exist** (a spring-forward gap). No candidate is valid, and the answer
 *     is the next valid instant, i.e. the transition itself: 02:30 on a day whose 02:00 jumps to
 *     03:00 hands over at 03:00 local, not at 03:30.
 *
 * The two candidates come from the offsets a day either side of the target, which straddle any
 * transition; the gap case then binary-searches the first instant whose local reading has reached
 * the target, which is exact for any transition size (not every zone shifts by a whole hour).
 */
function instantForCivil(civilMs: number, timeZone: string): number {
	const offsetBefore = zoneOffsetMs(civilMs - MS_PER_DAY, timeZone);
	const offsetAfter = zoneOffsetMs(civilMs + MS_PER_DAY, timeZone);

	const candidates = [civilMs - offsetBefore, civilMs - offsetAfter].filter(
		(candidate) => civilTimeIn(candidate, timeZone).civilMs === civilMs
	);

	if (candidates.length > 0) {
		return Math.min(...candidates);
	}

	let low = Math.min(civilMs - offsetBefore, civilMs - offsetAfter);
	let high = Math.max(civilMs - offsetBefore, civilMs - offsetAfter);
	while (low < high) {
		const mid = low + Math.floor((high - low) / 2);
		if (civilTimeIn(mid, timeZone).civilMs >= civilMs) {
			high = mid;
		} else {
			low = mid + 1;
		}
	}
	return low;
}

/** ISO 8601 weekday of a UTC-midnight calendar date: 1 = Monday … 7 = Sunday. */
function isoWeekdayOf(dateMs: number): number {
	const jsDay = new Date(dateMs).getUTCDay();
	return jsDay === 0 ? 7 : jsDay;
}

/**
 * The handoff instant on the local calendar date `dateMs`, resolving `handoffTime` in the
 * schedule's zone under the two DST rules above.
 */
function boundaryInstantOn(dateMs: number, schedule: OncallScheduleSpec): number {
	const [hours, minutes] = formatHandoffTime(schedule.handoffTime).split(':').map(Number);
	return instantForCivil(dateMs + hours * 3_600_000 + minutes * 60_000, schedule.timezone);
}

/**
 * The number of handoff boundaries at or before `at`, counted from the first boundary at or after
 * the schedule's anchor. Zero before that first boundary — SR-4's "position 0 is on call" case.
 *
 * Exported for the same reason `whoIsOnCall` is: the count IS the rotation arithmetic, and a
 * DST-day assertion reads far more clearly as "this 25-hour day produced exactly one boundary"
 * than as "and therefore person B rather than person C".
 */
export function handoffBoundaryCount(schedule: OncallScheduleSpec, at: Date): number {
	const stepDays = schedule.kind === 'weekly' ? 7 : 1;
	const anchorMs = schedule.rotationAnchorAt.getTime();
	const atMs = at.getTime();

	if (!Number.isFinite(anchorMs) || !Number.isFinite(atMs)) {
		return 0;
	}

	// The first qualifying local date on or after the anchor's local date, then one step forward if
	// that date's boundary has already passed at the anchor. A step back can never be right: the
	// previous qualifying date's boundary is a whole period earlier and the anchor is on or after
	// this date, so it is always before the anchor.
	let firstDate = civilTimeIn(anchorMs, schedule.timezone).dateMs;
	if (schedule.kind === 'weekly' && schedule.handoffWeekday !== null) {
		const forward = (schedule.handoffWeekday - isoWeekdayOf(firstDate) + 7) % 7;
		firstDate += forward * MS_PER_DAY;
	}
	if (boundaryInstantOn(firstDate, schedule) < anchorMs) {
		firstDate += stepDays * MS_PER_DAY;
	}

	// The last qualifying local date on or before `at`'s local date, stepped back if that date's
	// boundary has not been reached yet. `at` exactly ON a boundary counts ("at or before T").
	let lastDate = civilTimeIn(atMs, schedule.timezone).dateMs;
	if (schedule.kind === 'weekly' && schedule.handoffWeekday !== null) {
		const backward = (isoWeekdayOf(lastDate) - schedule.handoffWeekday + 7) % 7;
		lastDate -= backward * MS_PER_DAY;
	}
	if (boundaryInstantOn(lastDate, schedule) > atMs) {
		lastDate -= stepDays * MS_PER_DAY;
	}

	if (lastDate < firstDate) {
		return 0;
	}
	return Math.round((lastDate - firstDate) / (stepDays * MS_PER_DAY)) + 1;
}

/**
 * The override in force at `at`, or null. Intervals are half-open — an override ending at 09:00
 * and one starting at 09:00 do not both cover 09:00, which is also why `POST /oncall/overrides`
 * can call two such windows non-overlapping and return `201` rather than `409`.
 *
 * Overlaps are refused at write time, so at most one should ever match; the earliest `startsAt`
 * (then the lowest id) is picked anyway so a row that predates that guard resolves deterministically
 * rather than by whatever order the query returned.
 */
function overrideAt(overrides: readonly OncallOverrideSpec[], at: Date): OncallOverrideSpec | null {
	const atMs = at.getTime();
	const covering = overrides.filter(
		(o) => o.startsAt.getTime() <= atMs && atMs < o.endsAt.getTime()
	);
	if (covering.length === 0) {
		return null;
	}
	covering.sort(
		(a, b) => a.startsAt.getTime() - b.startsAt.getTime() || (a.id < b.id ? -1 : a.id > b.id ? 1 : 0)
	);
	return covering[0];
}

/**
 * §4.4's "who is on call at instant `T`", as the pure function §11 names.
 *
 * Order of resolution:
 *   1. An override covering `at` wins outright (`via: 'override'`). Without overrides the feature's
 *      own premise — "available engineers" — is false on day one, so this is not a special case
 *      bolted on top of the rotation but the first question asked.
 *   2. Otherwise the rotation index (see `handoffBoundaryCount`) selects a position in the ORDERED
 *      member list, and an ineligible member there is **skipped forward** to the next eligible one.
 *      Skipping forward rather than resolving the index against the eligible sub-list is the point
 *      of §4.4's "skipped, not removed": removing them from the list would re-phase everybody after
 *      them, which is precisely the silent change to the rota that sentence forbids.
 *   3. `null` when there are no members, or when none of them is eligible. §4.4 makes that a
 *      no-escalation outcome, never an error.
 */
export function whoIsOnCall(
	schedule: OncallScheduleSpec,
	members: readonly OncallMemberSpec[],
	overrides: readonly OncallOverrideSpec[],
	at: Date
): OncallResolution | null {
	const active = overrideAt(overrides, at);
	if (active) {
		return { userId: active.userId, via: 'override' };
	}

	const ordered = [...members].sort((a, b) => a.position - b.position);
	if (ordered.length === 0) {
		return null;
	}

	const start = handoffBoundaryCount(schedule, at) % ordered.length;
	for (let step = 0; step < ordered.length; step++) {
		const candidate = ordered[(start + step) % ordered.length];
		if (candidate.eligible) {
			return { userId: candidate.userId, via: 'rotation' };
		}
	}
	return null;
}
