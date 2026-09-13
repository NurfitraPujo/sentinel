import { describe, it, expect } from 'vitest';
import { coerceIssueSort, DEFAULT_ISSUE_SORT, ISSUE_SORTS } from './issue-sort';

// sentinel-affected-users §4.1, decision 10 — the enum and its coercion, shared by the query's
// ORDER BY and the loader's echo. What each consumer does with the result is asserted at its own
// boundary (issue-queries.sort.integration.test.ts, page.server.test.ts).
describe('coerceIssueSort (decision 10)', () => {
	it('names exactly the two values this feature introduces, defaulting to today’s order', () => {
		expect(ISSUE_SORTS).toEqual(['last_seen', 'affected_users']);
		expect(DEFAULT_ISSUE_SORT).toBe('last_seen');
	});

	it('passes both known values through unchanged', () => {
		expect(coerceIssueSort('last_seen')).toBe('last_seen');
		expect(coerceIssueSort('affected_users')).toBe('affected_users');
	});

	it('coerces everything else to last_seen — absent, empty, a typo, a case variant', () => {
		for (const bad of [undefined, null, '', 'banana', 'Affected_Users', 'affected_users ', 'count']) {
			expect(coerceIssueSort(bad)).toBe('last_seen');
		}
	});
});
