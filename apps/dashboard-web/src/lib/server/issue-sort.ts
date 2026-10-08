import { AGENT_ISSUE_SOURCES, type AgentIssueSource } from '$lib/db/queries/agent-work';
import { formatTagParam, MAX_TAG_FILTERS, parseTagParam, type TagFilter } from '$lib/tag-filter';

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

/**
 * fix-sentinel-issues-dashboard D2 — the `status` vocabulary, as `issues.check_status` allows it:
 * `unresolved | resolved | ignored`. The page used to link `?status=open`, a value no row has ever
 * carried. `open` is kept as an ALIAS (old bookmarks, the `/issues?status=open` redirect) and
 * coerced to `unresolved`; anything else unknown is `all`. The coerced value is what the queries
 * see and what `filters.status` echoes, like `sort` and `new`.
 */
export const ISSUE_STATUSES = ['all', 'unresolved', 'resolved', 'ignored'] as const;

export type IssueStatus = (typeof ISSUE_STATUSES)[number];

export function coerceIssueStatus(value: string | null | undefined): IssueStatus {
	if (value === 'open') return 'unresolved';
	return (ISSUE_STATUSES as readonly string[]).includes(value ?? '')
		? (value as IssueStatus)
		: 'all';
}

/**
 * D3 — `volume=high` is `issues.count >= HIGH_VOLUME_THRESHOLD`. Coerced like `new`: unknown is
 * `all` (no predicate).
 */
export const ISSUE_VOLUMES = ['all', 'high'] as const;

export type IssueVolume = (typeof ISSUE_VOLUMES)[number];

export const HIGH_VOLUME_THRESHOLD = 100;

export function coerceIssueVolume(value: string | null | undefined): IssueVolume {
	return (ISSUE_VOLUMES as readonly string[]).includes(value ?? '')
		? (value as IssueVolume)
		: 'all';
}

/**
 * D3 — the `seen` window on `last_seen`: `seen=1h|24h|7d|14d|30d` is `last_seen >= now − window`
 * against the loader's ONE instant. Unknown is `all` (no predicate).
 */
export const ISSUE_SEEN_WINDOWS = ['all', '1h', '24h', '7d', '14d', '30d'] as const;

export type IssueSeenWindow = (typeof ISSUE_SEEN_WINDOWS)[number];

export function coerceIssueSeen(value: string | null | undefined): IssueSeenWindow {
	return (ISSUE_SEEN_WINDOWS as readonly string[]).includes(value ?? '')
		? (value as IssueSeenWindow)
		: 'all';
}

const SEEN_WINDOW_MS: Record<Exclude<IssueSeenWindow, 'all'>, number> = {
	'1h': 60 * 60 * 1000,
	'24h': 24 * 60 * 60 * 1000,
	'7d': 7 * 24 * 60 * 60 * 1000,
	'14d': 14 * 24 * 60 * 60 * 1000,
	'30d': 30 * 24 * 60 * 60 * 1000,
};

/** The `last_seen` lower bound a coerced `seen` stands for, against ONE instant; `all` is none. */
export function seenLowerBound(seen: IssueSeenWindow, now: Date): Date | undefined {
	return seen === 'all' ? undefined : new Date(now.getTime() - SEEN_WINDOW_MS[seen]);
}

/** `seen_from` / `seen_to`: an unparsable value is ignored (undefined), never a 500. */
export function parseSeenInstant(value: string | null | undefined): Date | undefined {
	if (!value) return undefined;
	const d = new Date(value);
	return Number.isNaN(d.getTime()) ? undefined : d;
}

/**
 * sentinel-issue-list-filters §5 items 2-5 — `type`, `source`, `regressed`, `waiting`: server
 * filters coerced like `volume` (the loader coerces once, the queries see the coerced value, the
 * echo carries it so `href()` never rides a bad string). Anything unknown is the absence.
 */
export const ISSUE_TYPES = ['system_error', 'user_report'] as const;
export type IssueTypeFilter = 'all' | (typeof ISSUE_TYPES)[number];

export function coerceIssueType(value: string | null | undefined): IssueTypeFilter {
	return (ISSUE_TYPES as readonly string[]).includes(value ?? '') ? (value as IssueTypeFilter) : 'all';
}

export type IssueSourceFilter = 'all' | AgentIssueSource;

export function coerceIssueSource(value: string | null | undefined): IssueSourceFilter {
	return (AGENT_ISSUE_SOURCES as readonly string[]).includes(value ?? '')
		? (value as IssueSourceFilter)
		: 'all';
}

/** `regressed=1` or absent; anything else is absent (`false`). */
export function coerceIssueRegressed(value: string | null | undefined): boolean {
	return value === '1';
}

export const ISSUE_WAITING = ['any', 'reporter', 'team'] as const;
export type IssueWaitingFilter = 'all' | (typeof ISSUE_WAITING)[number];

export function coerceIssueWaiting(value: string | null | undefined): IssueWaitingFilter {
	return (ISSUE_WAITING as readonly string[]).includes(value ?? '') ? (value as IssueWaitingFilter) : 'all';
}

/** `release`: a non-empty version string, or `all`. Project scoping is the loader's. */
export function coerceIssueRelease(value: string | null | undefined): string {
	return value && value !== 'all' ? value : 'all';
}

/**
 * sentinel-reporting-and-dashboard D19: `service=<name>`, issues with at least one occurrence from that service. A
 * non-empty string of at most 64 characters (the stored `service` cap), trimmed, or `all`. It is matched against the
 * `issue_tag_values` rollup as a bound parameter, never interpolated.
 */
export function coerceIssueService(value: string | null | undefined): string {
	const v = (value ?? '').trim();
	return v && v !== 'all' && v.length <= 64 ? v : 'all';
}

/**
 * Repeatable `tag=<key>:<value>`: parsed with `parseTagParam`, de-duplicated, first
 * `MAX_TAG_FILTERS` kept, malformed ignored. `param` is `formatTagParam` — what `href()` re-emits.
 */
export interface IssueTagFilter extends TagFilter {
	param: string;
}

export function coerceIssueTags(params: string[]): IssueTagFilter[] {
	const tags: IssueTagFilter[] = [];
	const seen = new Set<string>();
	for (const raw of params) {
		const parsed = parseTagParam(raw);
		if (!parsed) continue;
		const id = JSON.stringify([parsed.key, parsed.value]);
		if (seen.has(id)) continue;
		seen.add(id);
		if (tags.length >= MAX_TAG_FILTERS) break;
		tags.push({ ...parsed, param: formatTagParam(parsed.key, parsed.value) });
	}
	return tags;
}

/** `page` is an integer >= 1: NaN, zero and negatives are page 1 (D4). */
export function coercePage(value: string | null | undefined): number {
	const n = parseInt(value ?? '1', 10);
	return Number.isFinite(n) && n >= 1 ? n : 1;
}
