/**
 * sentinel-reporting-and-dashboard D12: the query parameters only the issue list understands. A request to `/` carrying
 * any of them is an old link to the list and gets a `308` to `/issues` with the same query string.
 *
 * The contract lists `status claim q page limit sort new volume seen seen_from seen_to service`. The issue list gained
 * `release type source regressed waiting tag` after that list was written (sentinel-issue-list-filters, v0.2.64); they are
 * list-only too, and a link carrying one must not silently land on a dashboard that ignores it, so they are included.
 * `project` and `environment` are shared with the dashboard and are deliberately NOT here. `release` as a list filter
 * only ever appears next to `project`; on `/` it is still the list's.
 *
 * Lives under `$lib/server/`, not in the route file: a `+page.server.ts` may export only SvelteKit's allowlisted names (B12).
 */
export const LIST_ONLY_PARAMS: ReadonlySet<string> = new Set([
	'status', 'claim', 'q', 'page', 'limit', 'sort', 'new', 'volume', 'seen', 'seen_from', 'seen_to', 'service',
	'release', 'type', 'source', 'regressed', 'waiting', 'tag',
]);
