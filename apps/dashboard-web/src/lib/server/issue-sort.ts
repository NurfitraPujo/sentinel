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
 * `sentinel-occurrence-histogram` adds values to this enum later (§8); neither card renames it.
 */
export const ISSUE_SORTS = ['last_seen', 'affected_users'] as const;

export type IssueSort = (typeof ISSUE_SORTS)[number];

export const DEFAULT_ISSUE_SORT: IssueSort = 'last_seen';

/** The coercion, shared by the query and the loader's echo: anything not in the enum is the default. */
export function coerceIssueSort(value: string | null | undefined): IssueSort {
	return (ISSUE_SORTS as readonly string[]).includes(value ?? '')
		? (value as IssueSort)
		: DEFAULT_ISSUE_SORT;
}
