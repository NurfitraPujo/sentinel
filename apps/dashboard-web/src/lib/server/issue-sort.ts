/**
 * sentinel-affected-users §4.1, decision 10 — the `sort` parameter's vocabulary, in ONE place.
 *
 * Two consumers read it and they must agree: `issue-queries.ts: listIssues` picks the `ORDER BY`
 * from it, and `routes/+page.server.ts` echoes the COERCED value on `filters.sort`, which
 * `+page.svelte`'s `href()` then rides on every anchor. An unknown value is treated as `last_seen`
 * — today's order — and echoed as `last_seen`, never as the bad string: a typo in a bookmark must
 * not 500, must not narrow anything (a sort has no "match nothing" the way an unknown `status`
 * does), and must not keep riding on links the reader has no control to clear it from.
 *
 * sentinel-issue-trend §4.1, decision 5 — the enum grows by `count` and `first_seen` (the
 * extension `sentinel-affected-users` §8 reserved by name); the name, the coercion, the default
 * and the two existing values do not change. What each value orders by is `issueOrder`'s.
 */
export const ISSUE_SORTS = ['last_seen', 'affected_users', 'count', 'first_seen'] as const;

export type IssueSort = (typeof ISSUE_SORTS)[number];

export const DEFAULT_ISSUE_SORT: IssueSort = 'last_seen';

/** The coercion, shared by the query and the loader's echo: anything not in the enum is the default. */
export function coerceIssueSort(value: string | null | undefined): IssueSort {
	return (ISSUE_SORTS as readonly string[]).includes(value ?? '')
		? (value as IssueSort)
		: DEFAULT_ISSUE_SORT;
}

/**
 * sentinel-issue-trend §4.1, decision 6 — the `new` FILTER's vocabulary, beside `sort` because it
 * follows the same rule for the same reason: the loader coerces once, the queries see the coerced
 * value, and `filters.new` echoes it so `href()` never carries a bad string. An enum of one real
 * value (`24h`) plus the absence (`all`), so a second window (`7d`) is an added member, not a rename.
 *
 * Unlike `sort`, this IS a predicate — `first_seen >= now − 24h` inside `buildIssueConditions`, the
 * builder `listIssues` and `countIssues` share — so `pagination.total` narrows with it. The instant
 * is taken ONCE in the loader (`firstSeenLowerBound`) and passed to both queries, so the list and
 * the count agree on what "now" is.
 */
export const ISSUE_NEW_FILTERS = ['all', '24h'] as const;

export type IssueNewFilter = (typeof ISSUE_NEW_FILTERS)[number];

export const DEFAULT_ISSUE_NEW_FILTER: IssueNewFilter = 'all';

/** `sort`'s coercion, for `new`: anything not in the enum is `all` — no predicate, echoed as `all`. */
export function coerceIssueNewFilter(value: string | null | undefined): IssueNewFilter {
	return (ISSUE_NEW_FILTERS as readonly string[]).includes(value ?? '')
		? (value as IssueNewFilter)
		: DEFAULT_ISSUE_NEW_FILTER;
}

const NEW_WINDOW_MS = 24 * 60 * 60 * 1000;

/**
 * The `first_seen` lower bound a coerced `new` value stands for, against ONE instant: `24h` is
 * `now − 24 hours`; `all` is no bound at all (`undefined`, so the builder adds nothing).
 */
export function firstSeenLowerBound(filter: IssueNewFilter, now: Date): Date | undefined {
	switch (filter) {
		case '24h':
			return new Date(now.getTime() - NEW_WINDOW_MS);
		case 'all':
			return undefined;
	}
}
