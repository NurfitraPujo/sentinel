/**
 * sentinel-issue-activity TASK-01 — the ONE spelling of "is this a UUID" for the dashboard.
 *
 * Every `uuid` column read with a value Postgres cannot parse answers `22P02 invalid input syntax for type
 * uuid`, which a route surfaces as a 500 (production logged it for `GET /api/agent/issues/--help` and for a
 * truncated `42a26f64-`). Path parameters are kept away from Postgres by the `uuid` param matcher
 * (`src/params/uuid.ts`, applied as `[issueId=uuid]`), which uses this; ids that arrive in a query string or a
 * body are tested with it before they reach a query.
 *
 * Canonical hyphenated form only, any case (Postgres compares uuids case-insensitively). Not under
 * `$lib/server/` because the param matcher also runs in the browser's router.
 */
export const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

export function isUuid(value: unknown): value is string {
	return typeof value === 'string' && UUID_RE.test(value);
}

/** The lower-case form only: for callers that compare against a value they have already lower-cased. */
export function isLowerUuid(value: unknown): value is string {
	return isUuid(value) && value === value.toLowerCase();
}
