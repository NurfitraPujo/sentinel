import { describe, it, expect } from 'vitest';
import {
	coerceIssueNewFilter,
	coerceIssueSort,
	DEFAULT_ISSUE_NEW_FILTER,
	DEFAULT_ISSUE_SORT,
	firstSeenLowerBound,
	ISSUE_NEW_FILTERS,
	ISSUE_SORTS,
} from './issue-sort';

// sentinel-affected-users §4.1, decision 10 — the enum and its coercion, shared by the query's
// ORDER BY and the loader's echo. What each consumer does with the result is asserted at its own
// boundary (issue-queries.sort.integration.test.ts, issue-queries.trend.integration.test.ts,
// page.server.test.ts).
describe('coerceIssueSort (decision 10)', () => {
	// sentinel-issue-trend decision 5: two more members, the default unchanged. Four, exactly — a
	// fifth value is a contract change, not a tidy-up.
	it('names exactly the four values across the two features, defaulting to today’s order', () => {
		expect(ISSUE_SORTS).toEqual(['last_seen', 'affected_users', 'count', 'first_seen']);
		expect(ISSUE_SORTS).toHaveLength(4);
		expect(DEFAULT_ISSUE_SORT).toBe('last_seen');
	});

	it('passes every known value through unchanged', () => {
		expect(coerceIssueSort('last_seen')).toBe('last_seen');
		expect(coerceIssueSort('affected_users')).toBe('affected_users');
		expect(coerceIssueSort('count')).toBe('count');
		expect(coerceIssueSort('first_seen')).toBe('first_seen');
	});

	it('coerces everything else to last_seen — absent, empty, a typo, a case variant', () => {
		for (const bad of [
			undefined,
			null,
			'',
			'banana',
			'Affected_Users',
			'affected_users ',
			'Count',
			'firstSeen',
			'first_seen ',
		]) {
			expect(coerceIssueSort(bad)).toBe('last_seen');
		}
	});
});

// sentinel-issue-trend §4.1, decision 6 — the `new` filter's enum, its coercion and the one instant
// the bound is taken against. Whether the bound reaches BOTH queries is the loader's boundary
// (page.server.test.ts); what the predicate does to the rows is the query's
// (issue-queries.trend.integration.test.ts).
describe('coerceIssueNewFilter (decision 6)', () => {
	it('names `all` and `24h`, defaulting to `all`', () => {
		expect(ISSUE_NEW_FILTERS).toEqual(['all', '24h']);
		expect(DEFAULT_ISSUE_NEW_FILTER).toBe('all');
	});

	it('passes both known values through unchanged', () => {
		expect(coerceIssueNewFilter('all')).toBe('all');
		expect(coerceIssueNewFilter('24h')).toBe('24h');
	});

	it('coerces everything else to all — absent, empty, a typo, a case variant, a window that does not exist yet', () => {
		for (const bad of [undefined, null, '', 'banana', '24H', ' 24h', '7d', 'true']) {
			expect(coerceIssueNewFilter(bad)).toBe('all');
		}
	});

	it('turns 24h into now − 24 hours against the instant it is given, and all into no bound', () => {
		const now = new Date('2026-09-13T14:05:09.123Z');

		expect(firstSeenLowerBound('24h', now)).toEqual(new Date('2026-09-12T14:05:09.123Z'));
		expect(firstSeenLowerBound('all', now)).toBeUndefined();
	});
});

describe('fix-sentinel-issues-dashboard coercions', () => {
	it('status: open is an alias of unresolved; unknown is all', async () => {
		const { coerceIssueStatus } = await import('./issue-sort');
		expect(coerceIssueStatus('open')).toBe('unresolved');
		for (const v of ['all', 'unresolved', 'resolved', 'ignored'] as const) {
			expect(coerceIssueStatus(v)).toBe(v);
		}
		for (const v of ['banana', '', null, undefined, 'OPEN']) expect(coerceIssueStatus(v)).toBe('all');
	});

	it('volume and seen: unknown is all; the seen bound is now minus the window', async () => {
		const { coerceIssueVolume, coerceIssueSeen, seenLowerBound } = await import('./issue-sort');
		expect(coerceIssueVolume('high')).toBe('high');
		expect(coerceIssueVolume('banana')).toBe('all');
		expect(coerceIssueSeen('14d')).toBe('14d');
		expect(coerceIssueSeen('2d')).toBe('all');
		const now = new Date('2026-10-01T00:00:00Z');
		expect(seenLowerBound('1h', now)).toEqual(new Date('2026-09-30T23:00:00Z'));
		expect(seenLowerBound('30d', now)).toEqual(new Date('2026-09-01T00:00:00Z'));
		expect(seenLowerBound('all', now)).toBeUndefined();
	});

	it('parseSeenInstant ignores the unparsable; coercePage floors at 1', async () => {
		const { parseSeenInstant, coercePage } = await import('./issue-sort');
		expect(parseSeenInstant('nonsense')).toBeUndefined();
		expect(parseSeenInstant('')).toBeUndefined();
		expect(parseSeenInstant('2026-09-01T00:00:00Z')).toEqual(new Date('2026-09-01T00:00:00Z'));
		expect([coercePage('0'), coercePage('-2'), coercePage('abc'), coercePage(null), coercePage('7')]).toEqual([1, 1, 1, 1, 7]);
	});
});
