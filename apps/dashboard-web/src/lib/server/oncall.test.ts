import { describe, it, expect } from 'vitest';
import {
	formatHandoffTime,
	handoffBoundaryCount,
	isIsoWeekday,
	isValidIanaTimeZone,
	parseHandoffTime,
	parseInstant,
	whoIsOnCall,
	type OncallMemberSpec,
	type OncallOverrideSpec,
	type OncallScheduleSpec,
} from './oncall';

/**
 * sentinel-user-report-issue §11's named boundary: "whoIsOnCall(schedule, members, overrides, at)
 * (§4.4) | new — a pure function | Rotation arithmetic, override precedence, ineligible-member
 * skipping and the wall-clock DST boundary are all table-driven and must be testable without a
 * database or a clock."
 *
 * Nothing in this file touches a database, and nothing reads an ambient clock: every `at` is a
 * literal instant. If a test here ever needs `Date.now()`, the function has stopped being the
 * boundary the contract names.
 *
 * The zones are chosen for what they prove:
 *   - `Asia/Jakarta` (UTC+7, NO DST) — the non-UTC zone of §3's own example, where a handoff at
 *     09:00 local is 02:00Z all year and an off-by-one-zone bug is visible immediately.
 *   - `America/New_York` — spring forward 2026-03-08 02:00 → 03:00, fall back 2026-11-01 02:00 →
 *     01:00. The 23-hour and 25-hour days, the non-existent wall-clock time and the doubled one.
 */

const MEMBERS: OncallMemberSpec[] = [
	{ userId: 'u-a', position: 0, eligible: true },
	{ userId: 'u-b', position: 1, eligible: true },
	{ userId: 'u-c', position: 2, eligible: true },
];

function weekly(overrides: Partial<OncallScheduleSpec> = {}): OncallScheduleSpec {
	return {
		timezone: 'Asia/Jakarta',
		kind: 'weekly',
		handoffTime: '09:00',
		handoffWeekday: 1, // ISO Monday
		// 2026-09-02T00:00:00Z is Wednesday 2026-09-02 07:00 in Jakarta.
		rotationAnchorAt: new Date('2026-09-02T00:00:00Z'),
		...overrides,
	};
}

function daily(overrides: Partial<OncallScheduleSpec> = {}): OncallScheduleSpec {
	return {
		timezone: 'America/New_York',
		kind: 'daily',
		handoffTime: '09:00',
		handoffWeekday: null,
		rotationAnchorAt: new Date('2026-03-01T00:00:00Z'),
		...overrides,
	};
}

describe('whoIsOnCall — weekly rotation at a wall-clock time in a non-UTC zone', () => {
	// The anchor is Wed 2026-09-02 07:00 Jakarta. The first Monday 09:00 boundary at or after it is
	// Mon 2026-09-07 09:00 +07 = 2026-09-07T02:00Z. Before that, position 0 is on call (SR-4).
	const schedule = weekly();

	const cases: Array<{ at: string; expected: string; why: string }> = [
		{
			at: '2026-09-02T00:00:00Z',
			expected: 'u-a',
			why: 'exactly at the anchor: no boundary has passed, so position 0',
		},
		{
			at: '2026-09-07T01:59:59Z',
			expected: 'u-a',
			why: 'one second before the first handoff (08:59:59 Jakarta) — still position 0',
		},
		{
			at: '2026-09-07T02:00:00Z',
			expected: 'u-b',
			why: 'exactly at the first boundary: "at or before T" counts it, so index 1',
		},
		{
			at: '2026-09-13T23:59:59Z',
			expected: 'u-b',
			why: 'the whole first week belongs to position 1',
		},
		{ at: '2026-09-14T02:00:00Z', expected: 'u-c', why: 'second Monday handoff → index 2' },
		{ at: '2026-09-21T02:00:00Z', expected: 'u-a', why: 'third handoff wraps the three-member list' },
		{ at: '2026-10-05T02:00:00Z', expected: 'u-c', why: 'five boundaries in, 5 % 3 = 2' },
	];

	for (const { at, expected, why } of cases) {
		it(`${at} → ${expected} (${why})`, () => {
			expect(whoIsOnCall(schedule, MEMBERS, [], new Date(at))).toEqual({
				userId: expected,
				via: 'rotation',
			});
		});
	}

	it('hands over on Monday 09:00 LOCAL, which is 02:00Z — not on Monday 09:00 UTC', () => {
		// 2026-09-07T09:00Z is 16:00 Jakarta, seven hours AFTER the handoff. A zone-blind
		// implementation that handed over at 09:00Z would agree here by accident, so the assertion
		// that matters is the pair: 01:59:59Z is still the old engineer, 02:00:00Z is the new one.
		expect(whoIsOnCall(schedule, MEMBERS, [], new Date('2026-09-07T01:59:59Z'))?.userId).toBe('u-a');
		expect(whoIsOnCall(schedule, MEMBERS, [], new Date('2026-09-07T02:00:00Z'))?.userId).toBe('u-b');
	});

	it('treats handoffWeekday as ISO, so 7 means Sunday and not Saturday', () => {
		// Sunday 2026-09-06 09:00 +07 = 2026-09-06T02:00Z. Saturday would be a day earlier.
		const sunday = weekly({ handoffWeekday: 7 });
		expect(handoffBoundaryCount(sunday, new Date('2026-09-05T23:59:59Z'))).toBe(0);
		expect(handoffBoundaryCount(sunday, new Date('2026-09-06T02:00:00Z'))).toBe(1);
	});
});

describe('whoIsOnCall — override precedence', () => {
	const schedule = weekly();
	const override: OncallOverrideSpec = {
		id: 'ovr-1',
		userId: 'u-c',
		startsAt: new Date('2026-09-08T00:00:00Z'),
		endsAt: new Date('2026-09-11T00:00:00Z'),
	};

	it('the rotation holds before the override starts', () => {
		expect(whoIsOnCall(schedule, MEMBERS, [override], new Date('2026-09-07T12:00:00Z'))).toEqual({
			userId: 'u-b',
			via: 'rotation',
		});
	});

	it('the override wins inside its interval, and says so via "override"', () => {
		expect(whoIsOnCall(schedule, MEMBERS, [override], new Date('2026-09-09T12:00:00Z'))).toEqual({
			userId: 'u-c',
			via: 'override',
		});
	});

	it('the rotation RESUMES afterwards at the position it would have reached anyway', () => {
		// The override covered part of u-b's week; when it ends, u-b is back — the override did not
		// advance the rotation, it only masked it.
		expect(whoIsOnCall(schedule, MEMBERS, [override], new Date('2026-09-11T00:00:00Z'))).toEqual({
			userId: 'u-b',
			via: 'rotation',
		});
	});

	it('is half-open: the instant an override ends belongs to the rotation again', () => {
		expect(whoIsOnCall(schedule, MEMBERS, [override], new Date('2026-09-08T00:00:00Z'))?.via).toBe(
			'override'
		);
		expect(whoIsOnCall(schedule, MEMBERS, [override], new Date('2026-09-11T00:00:00Z'))?.via).toBe(
			'rotation'
		);
	});

	it('an override may name someone who is not on the rotation at all', () => {
		const outsider: OncallOverrideSpec = { ...override, id: 'ovr-2', userId: 'u-z' };
		expect(whoIsOnCall(schedule, MEMBERS, [outsider], new Date('2026-09-09T12:00:00Z'))).toEqual({
			userId: 'u-z',
			via: 'override',
		});
	});
});

describe('whoIsOnCall — an ineligible member is skipped, not removed', () => {
	// u-b's org role dropped below ISSUE_WRITE_ROLES. §4.4: skipped, not removed — removing them
	// would silently change the rota behind the person who set it.
	const withIneligible: OncallMemberSpec[] = [
		{ userId: 'u-a', position: 0, eligible: true },
		{ userId: 'u-b', position: 1, eligible: false },
		{ userId: 'u-c', position: 2, eligible: true },
	];
	const schedule = weekly();

	it("hands u-b's week forward to u-c rather than back to u-a", () => {
		expect(whoIsOnCall(schedule, withIneligible, [], new Date('2026-09-07T02:00:00Z'))).toEqual({
			userId: 'u-c',
			via: 'rotation',
		});
	});

	it('does NOT re-phase the rest of the list — u-c still owns index 2', () => {
		// This is the assertion that distinguishes "skip forward" from "resolve against the eligible
		// sub-list". Under the sub-list reading, index 2 % 2 = 0 would land on u-a here.
		expect(whoIsOnCall(schedule, withIneligible, [], new Date('2026-09-14T02:00:00Z'))).toEqual({
			userId: 'u-c',
			via: 'rotation',
		});
		expect(whoIsOnCall(schedule, withIneligible, [], new Date('2026-09-21T02:00:00Z'))).toEqual({
			userId: 'u-a',
			via: 'rotation',
		});
	});

	it('resolves to null when NOBODY is eligible — §4.4 makes that a no-escalation, not an error', () => {
		const noneEligible = withIneligible.map((m) => ({ ...m, eligible: false }));
		expect(whoIsOnCall(schedule, noneEligible, [], new Date('2026-09-14T02:00:00Z'))).toBeNull();
	});

	it('resolves to null when the member list is empty', () => {
		expect(whoIsOnCall(schedule, [], [], new Date('2026-09-14T02:00:00Z'))).toBeNull();
	});

	it('an override still wins even when every rotation member is ineligible', () => {
		const noneEligible = withIneligible.map((m) => ({ ...m, eligible: false }));
		const override: OncallOverrideSpec = {
			id: 'ovr-1',
			userId: 'u-c',
			startsAt: new Date('2026-09-08T00:00:00Z'),
			endsAt: new Date('2026-09-11T00:00:00Z'),
		};
		expect(whoIsOnCall(schedule, noneEligible, [override], new Date('2026-09-09T00:00:00Z'))).toEqual(
			{ userId: 'u-c', via: 'override' }
		);
	});

	it('reads the list by position, not by array order', () => {
		const shuffled: OncallMemberSpec[] = [
			{ userId: 'u-c', position: 2, eligible: true },
			{ userId: 'u-a', position: 0, eligible: true },
			{ userId: 'u-b', position: 1, eligible: true },
		];
		expect(whoIsOnCall(schedule, shuffled, [], new Date('2026-09-07T02:00:00Z'))?.userId).toBe('u-b');
	});
});

describe('handoffBoundaryCount — the DST handoff day is 23 or 25 hours, not 24', () => {
	// America/New_York: spring forward Sun 2026-03-08 (02:00 -> 03:00), fall back Sun 2026-11-01
	// (02:00 -> 01:00). A daily 09:00 handoff crosses both.
	it('produces exactly one boundary on the 23-hour day, at 09:00 LOCAL', () => {
		const schedule = daily({ rotationAnchorAt: new Date('2026-03-07T13:00:00Z') }); // 08:00 EST
		// Sat 2026-03-07 09:00 EST = 14:00Z (first boundary). Sun 2026-03-08 09:00 EDT = 13:00Z.
		// The gap between them is 23 hours, not 24 — that IS the wall-clock rule.
		const first = new Date('2026-03-07T14:00:00Z');
		const second = new Date('2026-03-08T13:00:00Z');
		expect(handoffBoundaryCount(schedule, first)).toBe(1);
		expect(second.getTime() - first.getTime()).toBe(23 * 3_600_000);
		expect(handoffBoundaryCount(schedule, new Date(second.getTime() - 1))).toBe(1);
		expect(handoffBoundaryCount(schedule, second)).toBe(2);
	});

	it('produces exactly one boundary on the 25-hour day, still at 09:00 LOCAL', () => {
		const schedule = daily({ rotationAnchorAt: new Date('2026-10-31T13:00:00Z') }); // 09:00 EDT
		// Sat 2026-10-31 09:00 EDT = 13:00Z (first boundary, exactly at the anchor).
		// Sun 2026-11-01 09:00 EST = 14:00Z. 25 hours apart.
		const first = new Date('2026-10-31T13:00:00Z');
		const second = new Date('2026-11-01T14:00:00Z');
		expect(handoffBoundaryCount(schedule, first)).toBe(1);
		expect(second.getTime() - first.getTime()).toBe(25 * 3_600_000);
		expect(handoffBoundaryCount(schedule, new Date(second.getTime() - 1))).toBe(1);
		expect(handoffBoundaryCount(schedule, second)).toBe(2);
	});

	it('never shifts the local handoff time across a DST boundary', () => {
		// The visible symptom of the elapsed-period reading SR-4 rejects: a fixed 24-hour period
		// walks the handoff an hour off local time. Across the spring-forward week the handoff stays
		// at 09:00 local every single day.
		const schedule = daily({ rotationAnchorAt: new Date('2026-03-05T14:00:00Z') });
		const clock = new Intl.DateTimeFormat('en-US', {
			timeZone: 'America/New_York',
			hourCycle: 'h23',
			hour: '2-digit',
			minute: '2-digit',
		});
		const localHourOfNthBoundary = (n: number): string => {
			// The count is monotonic in `at`, so the nth boundary is the smallest instant whose count
			// has reached n — a binary search, not a walk. Then read that instant's local wall-clock
			// hour back out of Intl, which is the only oracle available here.
			let low = new Date('2026-03-05T00:00:00Z').getTime();
			let high = new Date('2026-03-15T00:00:00Z').getTime();
			expect(handoffBoundaryCount(schedule, new Date(high))).toBeGreaterThanOrEqual(n);
			while (low < high) {
				const mid = low + Math.floor((high - low) / 2);
				if (handoffBoundaryCount(schedule, new Date(mid)) >= n) {
					high = mid;
				} else {
					low = mid + 1;
				}
			}
			return clock.format(new Date(low));
		};
		for (const n of [1, 2, 3, 4, 5]) {
			expect(localHourOfNthBoundary(n)).toBe('09:00');
		}
	});

	it('a handoffTime that does not exist on a DST day resolves FORWARD', () => {
		// 02:30 does not exist on 2026-03-08 in New York: 02:00 EST jumps to 03:00 EDT. SR-4 says
		// resolve forward to the next valid instant — 03:00 EDT = 07:00Z, NOT 03:30.
		const schedule = daily({
			handoffTime: '02:30',
			rotationAnchorAt: new Date('2026-03-08T00:00:00Z'), // 19:00 EST on 2026-03-07
		});
		expect(handoffBoundaryCount(schedule, new Date('2026-03-08T06:59:59Z'))).toBe(0);
		expect(handoffBoundaryCount(schedule, new Date('2026-03-08T07:00:00Z'))).toBe(1);
		// And it is 07:00Z exactly, not 07:30Z — the gap does not push the handoff past the
		// transition by the length of the gap.
		expect(handoffBoundaryCount(schedule, new Date('2026-03-08T07:29:59Z'))).toBe(1);
	});

	it('a handoffTime that exists TWICE on a DST day resolves to the FIRST', () => {
		// 01:30 happens twice on 2026-11-01 in New York: 01:30 EDT = 05:30Z, then 01:30 EST =
		// 06:30Z. SR-4 takes the first.
		const schedule = daily({
			handoffTime: '01:30',
			rotationAnchorAt: new Date('2026-11-01T00:00:00Z'), // 20:00 EDT on 2026-10-31
		});
		expect(handoffBoundaryCount(schedule, new Date('2026-11-01T05:29:59Z'))).toBe(0);
		expect(handoffBoundaryCount(schedule, new Date('2026-11-01T05:30:00Z'))).toBe(1);
		// The second occurrence must NOT count as a second boundary — one handoff per day.
		expect(handoffBoundaryCount(schedule, new Date('2026-11-01T06:30:00Z'))).toBe(1);
	});
});

describe('handoffBoundaryCount — the anchor is the phase, and only the anchor', () => {
	it('position 0 holds from the anchor until the first boundary', () => {
		expect(handoffBoundaryCount(weekly(), new Date('2026-09-02T00:00:00Z'))).toBe(0);
		expect(handoffBoundaryCount(weekly(), new Date('2026-09-07T01:59:59Z'))).toBe(0);
	});

	it('an unchanged anchor gives an unchanged rotation, whatever else the edit changed', () => {
		// SR-4's create-or-replace rule, at the level this function can see it: `PUT` updates the
		// row in place, so `rotationAnchorAt` is the same Date before and after, and everything
		// downstream of it is too. Fixing the NAME is invisible here by construction (the name is
		// not an input); what a typo fix must not do is move the anchor.
		const before = weekly({ name: undefined } as Partial<OncallScheduleSpec>);
		const afterTypoFix = weekly();
		const at = new Date('2026-10-05T02:00:00Z');
		expect(handoffBoundaryCount(afterTypoFix, at)).toBe(handoffBoundaryCount(before, at));
		expect(whoIsOnCall(afterTypoFix, MEMBERS, [], at)).toEqual(whoIsOnCall(before, MEMBERS, [], at));
	});

	it('a MOVED anchor re-phases the rotation — which is what preserving it prevents', () => {
		// The counterfactual that gives the test above its teeth: had `PUT` re-created the row and
		// re-anchored it at edit time, this is the answer the pager would have got instead.
		const rephased = weekly({ rotationAnchorAt: new Date('2026-09-16T00:00:00Z') });
		const at = new Date('2026-10-05T02:00:00Z');
		expect(handoffBoundaryCount(weekly(), at)).toBe(5);
		expect(handoffBoundaryCount(rephased, at)).toBe(3);
		expect(whoIsOnCall(weekly(), MEMBERS, [], at)?.userId).toBe('u-c');
		expect(whoIsOnCall(rephased, MEMBERS, [], at)?.userId).toBe('u-a');
	});

	it("skips a qualifying day whose handoff has ALREADY passed at the anchor", () => {
		// Anchor Mon 2026-09-07 12:00 Jakarta (05:00Z) — three hours after that Monday's 09:00
		// handoff. The first boundary is therefore the FOLLOWING Monday, not the one already gone;
		// counting the elapsed one would put position 1 on call from the moment of creation, which
		// is a handover that never happened.
		const schedule = weekly({ rotationAnchorAt: new Date('2026-09-07T05:00:00Z') });
		expect(handoffBoundaryCount(schedule, new Date('2026-09-07T05:00:00Z'))).toBe(0);
		expect(handoffBoundaryCount(schedule, new Date('2026-09-13T23:59:59Z'))).toBe(0);
		expect(handoffBoundaryCount(schedule, new Date('2026-09-14T02:00:00Z'))).toBe(1);
		expect(whoIsOnCall(schedule, MEMBERS, [], new Date('2026-09-10T00:00:00Z'))?.userId).toBe('u-a');
		expect(whoIsOnCall(schedule, MEMBERS, [], new Date('2026-09-14T02:00:00Z'))?.userId).toBe('u-b');
	});

	it('counts nothing before the anchor', () => {
		expect(handoffBoundaryCount(weekly(), new Date('2026-08-31T02:00:00Z'))).toBe(0);
		expect(whoIsOnCall(weekly(), MEMBERS, [], new Date('2026-08-31T02:00:00Z'))?.userId).toBe('u-a');
	});
});

describe('isValidIanaTimeZone', () => {
	it('accepts IANA names', () => {
		for (const zone of ['UTC', 'Asia/Jakarta', 'America/New_York', 'Etc/GMT+7', 'Australia/Lord_Howe']) {
			expect(isValidIanaTimeZone(zone)).toBe(true);
		}
	});

	it('REJECTS a fixed offset, which modern ICU would otherwise accept', () => {
		// §3: "A fixed offset ("+07:00") is rejected: an offset cannot survive a DST boundary, and a
		// rota that is one hour wrong twice a year wakes the wrong person." A bare try/catch around
		// Intl.DateTimeFormat does NOT reject these — this assertion is the reason the regex exists.
		for (const offset of ['+07:00', '-05:00', '+0700', '07:00']) {
			expect(isValidIanaTimeZone(offset)).toBe(false);
		}
	});

	it('rejects nonsense, non-strings and over-long values', () => {
		expect(isValidIanaTimeZone('Not/A_Zone')).toBe(false);
		expect(isValidIanaTimeZone('')).toBe(false);
		expect(isValidIanaTimeZone(null)).toBe(false);
		expect(isValidIanaTimeZone(7)).toBe(false);
		expect(isValidIanaTimeZone('A'.repeat(65))).toBe(false);
	});
});

describe('parseHandoffTime / formatHandoffTime / isIsoWeekday / parseInstant', () => {
	it('accepts HH:MM in 24-hour form only', () => {
		expect(parseHandoffTime('09:00')).toBe('09:00');
		expect(parseHandoffTime('00:00')).toBe('00:00');
		expect(parseHandoffTime('23:59')).toBe('23:59');
	});

	it('rejects everything that is not HH:MM', () => {
		for (const bad of ['9:00', '24:00', '23:60', '09:00:00', '0900', 'nine', '', null, 900]) {
			expect(parseHandoffTime(bad)).toBeNull();
		}
	});

	it("normalises Postgres's TIME rendering back to the wire format", () => {
		expect(formatHandoffTime('09:00:00')).toBe('09:00');
		expect(formatHandoffTime('09:00')).toBe('09:00');
	});

	it('takes ISO weekdays 1-7 and nothing else', () => {
		for (const day of [1, 2, 3, 4, 5, 6, 7]) {
			expect(isIsoWeekday(day)).toBe(true);
		}
		// 0 is `Date#getDay()`'s Sunday and is NOT an ISO weekday — the encoding confusion SR-4
		// settles, and the one the migration got wrong the first time.
		for (const bad of [0, 8, -1, 1.5, '1', null]) {
			expect(isIsoWeekday(bad)).toBe(false);
		}
	});

	it('parses RFC 3339 instants and refuses non-timestamps', () => {
		expect(parseInstant('2026-09-10T00:00:00Z')?.toISOString()).toBe('2026-09-10T00:00:00.000Z');
		for (const bad of ['', 'not a date', null, 0, {}]) {
			expect(parseInstant(bad)).toBeNull();
		}
	});
});
