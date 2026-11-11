import { describe, it, expect } from 'vitest';
import {
	nextUntriedMember,
	resolveAckMinutes,
	resolveMaxHops,
	unscopedRecipients,
	DEFAULT_ESCALATION_ACK_MINUTES,
	DEFAULT_ESCALATION_MAX_HOPS,
} from './escalation';

/**
 * sentinel-user-report-issue §4.4 — the parts of escalation that are pure.
 *
 * The chain's transitions are an INTEGRATION boundary (§11: "transitions across rows, not a pure
 * function") and live in `escalation.sweep.integration.test.ts`, driven through
 * `POST /api/cron/oncall-escalation` against a real Postgres. What is left here is the successor
 * rule and the two config knobs — both genuinely functions of their arguments, and both worth
 * asserting directly rather than through three seeded database hops.
 */

function member(userId: string, position: number, eligible = true) {
	return { userId, position, eligible };
}

describe('§4.4 nextUntriedMember — "the next member of the rotation who has not already been tried"', () => {
	const rota = [member('a', 0), member('b', 1), member('c', 2)];

	it('walks FORWARD from the current assignee, not from the top of the list', () => {
		// From b the successor is c, never a: the chain follows the rota's order, which is the
		// order the person who set it up chose.
		expect(nextUntriedMember(rota, 'b', new Set(['b']))).toBe('c');
	});

	it('wraps past the end of the list', () => {
		expect(nextUntriedMember(rota, 'c', new Set(['c']))).toBe('a');
	});

	it('skips a member who has already carried this report', () => {
		// §4.4: "the next member … who has not already been tried on this report". Without the
		// tried-set a three-member rota would hand hop 3 back to whoever held hop 1.
		expect(nextUntriedMember(rota, 'c', new Set(['c', 'a']))).toBe('b');
	});

	it('skips an ineligible member rather than removing them from the rota', () => {
		// §4.4's "skipped, not removed": b's org role dropped below ISSUE_WRITE_ROLES, so the
		// successor of a is c — and b's POSITION is untouched, which is what stops the rest of the
		// rota re-phasing behind them.
		const withIneligible = [member('a', 0), member('b', 1, false), member('c', 2)];
		expect(nextUntriedMember(withIneligible, 'a', new Set(['a']))).toBe('c');
	});

	it('returns null when every eligible member has been tried', () => {
		// The chain cannot move. The sweep turns this into `exhausted: true` — the same terminal
		// state max_hops produces, and the same alert notices it.
		expect(nextUntriedMember(rota, 'c', new Set(['a', 'b', 'c']))).toBeNull();
	});

	it('returns null for an empty rota', () => {
		expect(nextUntriedMember([], 'a', new Set())).toBeNull();
	});

	it('starts from the top when the current assignee is not on the rota at all', () => {
		// An override can put somebody on call who is not a rotation member, and a member can be
		// removed after they were escalated to. Either way the successor must still be defined.
		expect(nextUntriedMember(rota, 'zz', new Set(['zz']))).toBe('a');
	});

	it('reads position, not array order', () => {
		const shuffled = [member('c', 2), member('a', 0), member('b', 1)];
		expect(nextUntriedMember(shuffled, 'a', new Set(['a']))).toBe('b');
	});
});

describe('§8 config knobs', () => {
	it('defaults to the contract values', () => {
		expect(resolveAckMinutes('')).toBe(DEFAULT_ESCALATION_ACK_MINUTES);
		expect(resolveMaxHops('')).toBe(DEFAULT_ESCALATION_MAX_HOPS);
		expect(DEFAULT_ESCALATION_ACK_MINUTES).toBe(15);
		expect(DEFAULT_ESCALATION_MAX_HOPS).toBe(3);
	});

	it('reads a valid override', () => {
		expect(resolveAckMinutes('5')).toBe(5);
		expect(resolveMaxHops('7')).toBe(7);
	});

	it('degrades a malformed knob to the default rather than to NaN', () => {
		// A NaN ack window would be written into `ack_deadline_at`, which is NOT NULL timestamptz --
		// the whole intake would fail on a typo in an environment variable, which is exactly the
		// outcome §4.4's "escalation never fails a report" forbids.
		for (const bad of ['nonsense', '0', '-5', '   ']) {
			expect(resolveAckMinutes(bad)).toBe(DEFAULT_ESCALATION_ACK_MINUTES);
			expect(resolveMaxHops(bad)).toBe(DEFAULT_ESCALATION_MAX_HOPS);
		}
	});
});

/**
 * sentinel-project-environments §2.2, decisions 38 and 41 — the fan-out rule, table-driven and with
 * no database and no clock, exactly as contract §11 asks for the resolution half.
 *
 * The rest of the fallback is rows: one `notifications` row per recipient, no `issue_escalations`
 * row, no `assigned_to`, and — in the sweep only — a terminated row and its own counter. Those live
 * in `escalation.sweep.integration.test.ts`, because three of them are ABSENCES and an absence is
 * exactly what a mock that returns itself for every call cannot distinguish from "did nothing".
 */
describe('§2.2 unscopedRecipients — "schedule.members PLUS the override holder"', () => {
	const rota = [member('a', 0), member('b', 1), member('c', 2)];

	it('is the whole rota when the candidate came from the rotation', () => {
		expect(unscopedRecipients(rota, { userId: 'b', via: 'rotation' }).sort()).toEqual([
			'a',
			'b',
			'c',
		]);
	});

	it('ADDS an override holder who is not on the rota at all', () => {
		// The case the union exists for: `whoIsOnCall` returns an override holder outright, and such
		// a holder need not appear in `schedule.members`. Fanning out over `members` alone silently
		// drops the one person actually resolved.
		expect(unscopedRecipients(rota, { userId: 'z', via: 'override' }).sort()).toEqual([
			'a',
			'b',
			'c',
			'z',
		]);
	});

	it('does not duplicate an override holder who IS on the rota', () => {
		// "Exactly one `notifications` row per rota member" — two rows for one person is a second
		// page, not a thorough one.
		expect(unscopedRecipients(rota, { userId: 'a', via: 'override' }).sort()).toEqual([
			'a',
			'b',
			'c',
		]);
	});

	it('is the members alone when there is no resolution to union in — the sweep’s call', () => {
		// `sweepOne` resolves its candidate through `nextUntriedMember`, which only ever returns a
		// member of the rota, so there is no override holder to add there.
		expect(unscopedRecipients(rota, null).sort()).toEqual(['a', 'b', 'c']);
	});

	it('is empty for an empty rota rather than throwing', () => {
		expect(unscopedRecipients([], null)).toEqual([]);
	});
});
