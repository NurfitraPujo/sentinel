// sentinel-user-report-issue §4.4a, fe.md rows O3 and O5 — the two bits of formatting the on-call
// editor needs, kept out of the components so they can be read and tested as functions.
//
// **No dependency.** Contract §5 is `none` and fe.md names the date/time picker as the dependency
// you will be tempted by: `Intl.DateTimeFormat` plus a native `<input type="time">` covers what
// §4.4a actually needs, and this module is that `Intl` use.

import type { OncallSchedule, RotationKind } from '$lib/types/oncall';

/** ISO 8601 weekday names, 1 = Monday … 7 = Sunday — SR-4's encoding, NOT `Date#getDay()`. */
export const ISO_WEEKDAY_NAMES: Record<number, string> = {
	1: 'Monday',
	2: 'Tuesday',
	3: 'Wednesday',
	4: 'Thursday',
	5: 'Friday',
	6: 'Saturday',
	7: 'Sunday',
};

/**
 * IANA zone names only (§3), mirroring `$lib/server/oncall.ts`'s `IANA_ZONE_NAME`. The pattern is
 * the gate and `Intl` is not: modern ICU **accepts** `"+07:00"` as a time zone, so a bare try/catch
 * around `Intl.DateTimeFormat` would let through exactly the value the API rejects, and the editor
 * would show a working clock for a zone that 400s on save.
 *
 * It is a copy because `$lib/server/*` cannot be imported into a component — SvelteKit refuses it,
 * and rightly. The API remains the authority; this is the pre-check that stops the UI from
 * promising something the API will refuse.
 */
const IANA_ZONE_NAME = /^[A-Za-z][A-Za-z0-9_+-]*(?:\/[A-Za-z0-9_+.-]+)*$/;

/**
 * O3: the current wall-clock time in an IANA zone, shown beside the picker so a wrong pick is
 * visible before it is saved. Returns `null` for anything that is not an IANA name — every fixed
 * offset included, which is the point.
 */
export function localTimeIn(timezone: string, at: Date = new Date()): string | null {
	if (!IANA_ZONE_NAME.test(timezone) || timezone.length > 64) {
		return null;
	}
	try {
		return new Intl.DateTimeFormat('en-GB', {
			timeZone: timezone,
			hour: '2-digit',
			minute: '2-digit',
			weekday: 'short',
		}).format(at);
	} catch {
		return null;
	}
}

/**
 * O5: `kind` + `handoffTime` + `handoffWeekday` restated as one sentence, because three fields is
 * three fields nobody reads back correctly. A `weekly` schedule with no weekday is a shape the API
 * rejects (§4.4a's `400`), so it is described honestly here rather than guessed at.
 */
export function handoffRestatement(
	schedule: Pick<OncallSchedule, 'kind' | 'handoffTime' | 'handoffWeekday' | 'timezone'>
): string {
	const when = `${schedule.handoffTime} ${schedule.timezone}`;
	if (schedule.kind === 'weekly') {
		const day = schedule.handoffWeekday ? ISO_WEEKDAY_NAMES[schedule.handoffWeekday] : null;
		if (!day) {
			return `Hands over weekly at ${when} — but no handoff day is set, so this cannot be saved.`;
		}
		return `Hands over every ${day} at ${when}.`;
	}
	return `Hands over every day at ${when}.`;
}

/**
 * The zones offered by the picker. `Intl.supportedValuesOf` is the runtime's own list and is the
 * right source: hardcoding one would go stale every time the tz database moves a city. The fallback
 * exists because the method is not universal, and an editor that offers no zone at all is worse
 * than one offering a short list — the field stays free-text-backed either way (a `<datalist>`), so
 * a zone missing from the list is still typeable and still validated by the API.
 */
export function listTimeZones(): string[] {
	const supported = (
		Intl as typeof Intl & { supportedValuesOf?: (key: string) => string[] }
	).supportedValuesOf;
	if (typeof supported === 'function') {
		try {
			return supported.call(Intl, 'timeZone');
		} catch {
			/* fall through */
		}
	}
	return ['UTC', 'Asia/Jakarta', 'Asia/Singapore', 'Europe/London', 'America/New_York'];
}

/** A `Date` as the `value` a native `<input type="datetime-local">` wants, in the browser's zone. */
export function toDatetimeLocalValue(date: Date): string {
	const pad = (n: number) => String(n).padStart(2, '0');
	return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}`;
}

export const ROTATION_KIND_LABELS: Record<RotationKind, string> = {
	daily: 'Every day',
	weekly: 'Every week',
};
