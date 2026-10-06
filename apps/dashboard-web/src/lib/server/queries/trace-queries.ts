import { db } from '$lib/server/db';
import { errorOccurrences, issues, projects } from '$lib/db/schema';
import { and, count, countDistinct, eq, inArray, ne, sql, type SQL } from 'drizzle-orm';
import type { SelectedFields } from 'drizzle-orm/pg-core';
import type { SelectResultFields } from 'drizzle-orm/query-builders/select.types';
import { environmentScopeCondition } from '$lib/server/environment-access';

/**
 * sentinel-cross-repo-trace-view D1–D6 — the ONE guarded read of "every occurrence sharing a trace
 * id", composed by three callers: the `/{org}/traces/{traceId}` page, the issue page's
 * "N related errors in other projects" count (D5) and `GET /api/agent/traces/{traceId}` (D6). It is
 * one module so the tenancy and environment guards cannot be spelled three different ways.
 *
 * **The caller supplies the project set and it MUST be one organization's.** Trace ids are
 * client-asserted (a W3C `traceparent` header), so another organization can send the same id; this
 * is why the set comes from `getAccessibleProjects(userId, orgId)` and never from `/search`'s
 * cross-organization union (D2). An empty set DENIES (`sql\`false\``): `inArray([])` throws in
 * Drizzle 0.30, and a dropped predicate would be a leak, so the empty case is spelled out.
 *
 * Tenancy and environment scope read `issues.project_id` / `issues.environment`, not the nullable,
 * un-backfilled `error_occurrences.project_id` — the same column the other read guards use, which is
 * what lets {@link environmentScopeCondition} compose here unchanged.
 */

// D1's validator lives in `$lib/trace-id` (the issue page's template needs it too); re-exported so
// the server callers have one import.
export { parseTraceId } from '$lib/trace-id';

/** D4 — rows per page, and the ceiling at which counts stop and read "1,000+". */
export const TRACE_PAGE_SIZE = 200;
export const TRACE_COUNT_CAP = 1000;

export interface TraceScope {
	/** One organization's reachable projects. Empty denies. */
	projectIds: string[];
	/** `resolveEnvironmentScopes` map; omit for callers that are not environment-scoped (agent keys). */
	environmentScopes?: Map<string, string[]>;
	/** An optional caller-chosen FILTER (D6) — not a guard. */
	environment?: string;
}

function traceConditions(traceId: string, scope: TraceScope): SQL[] {
	const conditions: SQL[] = [
		eq(errorOccurrences.traceId, traceId),
		scope.projectIds.length > 0 ? inArray(issues.projectId, scope.projectIds) : sql`false`,
	];
	const guard = environmentScopeCondition(scope.environmentScopes ?? new Map());
	if (guard) conditions.push(guard);
	if (scope.environment) conditions.push(eq(issues.environment, scope.environment));
	return conditions;
}

// created_at is a nullable timestamptz (the Drizzle mirror declares it `timestamp`; the migrated column
// is what the keyset compares) and received_at is NULL on pre-v2 rows; both are folded to a
// total order so the keyset comparison below never meets a NULL (a NULL makes a row comparison
// UNKNOWN, which silently drops the row from every later page).
const createdKey = sql`coalesce(${errorOccurrences.createdAt}, 'infinity'::timestamptz)`;
const receivedKey = sql`coalesce(extract(epoch from ${errorOccurrences.receivedAt}), 0)::numeric`;

export interface TraceCursor {
	created: string;
	received: string;
	id: string;
}

// The keys travel as the TEXT Postgres rendered, never through a JS Date: a Date holds milliseconds
// and the column holds microseconds, so a round-tripped cursor would re-select the row it follows.
const CREATED_TEXT_RE =
	/^(?:infinity|(\d{4})-(\d{2})-(\d{2}) (\d{2}):(\d{2}):(\d{2})(?:\.\d{1,6})?(?:([+-])(\d{2})(?::(\d{2}))?)?)$/;
// epoch seconds: at most 11 integer digits (year ~5138) and 6 fractional; anything longer is not a
// value this module minted and would overflow Postgres' numeric cast (22003).
const NUMERIC_TEXT_RE = /^\d{1,11}(\.\d{1,6})?$/;
/** A REAL calendar date-time: a shape check alone passes `2026-13-45` / `25:61:61`, which Postgres rejects (22008). */
function isRealCreatedKey(text: string): boolean {
	const m = CREATED_TEXT_RE.exec(text);
	if (!m) return false;
	if (text === 'infinity') return true;
	const [y, mo, d, h, mi, sec] = [1, 2, 3, 4, 5, 6].map((i) => Number(m[i]));
	const probe = new Date(Date.UTC(y, mo - 1, d, h, mi, sec));
	if (
		probe.getUTCFullYear() !== y ||
		probe.getUTCMonth() !== mo - 1 ||
		probe.getUTCDate() !== d ||
		h > 23 || mi > 59 || sec > 59 || y < 1
	)
		return false;
	if (m[7] && (Number(m[8]) > 15 || Number(m[9] ?? 0) > 59)) return false;
	return true;
}

const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

export function encodeTraceCursor(c: TraceCursor): string {
	return Buffer.from(JSON.stringify(c), 'utf8').toString('base64url');
}

/** Throws `Error('invalid cursor')` for anything this module did not mint. */
export function decodeTraceCursor(raw: string): TraceCursor {
	let parsed: Partial<TraceCursor>;
	try {
		parsed = JSON.parse(Buffer.from(raw, 'base64url').toString('utf8'));
	} catch {
		throw new Error('invalid cursor');
	}
	if (
		typeof parsed !== 'object' ||
		parsed === null ||
		typeof parsed.created !== 'string' ||
		!isRealCreatedKey(parsed.created) ||
		typeof parsed.received !== 'string' ||
		!NUMERIC_TEXT_RE.test(parsed.received) ||
		typeof parsed.id !== 'string' ||
		!UUID_RE.test(parsed.id)
	) {
		throw new Error('invalid cursor');
	}
	return { created: parsed.created, received: parsed.received, id: parsed.id };
}

const keyColumns = {
	createdKeyText: sql<string>`${createdKey}::text`,
	receivedKeyText: sql<string>`${receivedKey}::text`,
};

/**
 * `created_at` as an ISO-8601 UTC string, rendered by Postgres. The Drizzle mirror declares the
 * column `timestamp` (no tz) while the migrated column is `timestamptz`, so reading it through the
 * mirror re-interprets the instant in the NODE process's zone; with a non-UTC session TimeZone a 30 s
 * skew read back as hours. Rendering it here is correct in any session setting, and the mirror stays
 * as other code expects it.
 */
export const createdAtIso = sql<string | null>`to_char(${errorOccurrences.createdAt} at time zone 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"')`;

/** received_at − created_at in seconds, computed over the two timestamptz values (null if either is). */
export const skewSeconds = sql<number | null>`extract(epoch from ${errorOccurrences.receivedAt} - ${errorOccurrences.createdAt})::float8`;

/** The columns of one trace row shared by the page and the agent API. */
export const traceRowColumns = {
	id: errorOccurrences.id,
	issueId: errorOccurrences.issueId,
	projectId: issues.projectId,
	projectName: projects.name,
	environment: issues.environment,
	traceId: errorOccurrences.traceId,
	spanId: errorOccurrences.spanId,
	level: errorOccurrences.level,
	errorClass: errorOccurrences.errorClass,
	message: errorOccurrences.message,
	serverName: errorOccurrences.serverName,
	releaseVersion: errorOccurrences.releaseVersion,
	createdAt: createdAtIso,
	receivedAt: errorOccurrences.receivedAt,
	skewSeconds,
};

/**
 * One page of a trace, oldest first by the client's event time (`created_at`), then the server
 * stamp, then `id` (D3). `columns` lets the agent API select its richer occurrence shape; the cursor
 * keys are always appended.
 *
 * Fetches `limit + 1` to learn whether a next page exists without a count. The cursor carries the
 * received key as well as D4's `(created_at, id)` because D3 orders by it: a cursor that skipped it
 * would page past rows that share a `created_at` but sort before the cursor row by `received_at`.
 */
export async function listTraceOccurrences<C extends SelectedFields>(opts: {
	traceId: string;
	scope: TraceScope;
	limit: number;
	cursor?: TraceCursor;
	columns: C;
}) {
	const conditions = traceConditions(opts.traceId, opts.scope);
	if (opts.cursor) {
		conditions.push(
			sql`(${createdKey}, ${receivedKey}, ${errorOccurrences.id}) > (${opts.cursor.created}::timestamptz, ${opts.cursor.received}::numeric, ${opts.cursor.id}::uuid)`
		);
	}

	// Belt and braces behind `decodeTraceCursor`: if a value ever gets past its shape checks and
	// Postgres still refuses the cast (22007/22008/22009 invalid datetime, 22003 numeric overflow, 22P02),
	// that is a bad cursor, not a server error.
	const rows = (await db
		.select({ ...opts.columns, ...keyColumns })
		.from(errorOccurrences)
		.innerJoin(issues, eq(issues.id, errorOccurrences.issueId))
		.innerJoin(projects, eq(projects.id, issues.projectId))
		.where(and(...conditions))
		.orderBy(createdKey, receivedKey, errorOccurrences.id)
		.limit(opts.limit + 1)
		.catch((err: unknown) => {
			const code = (err as { code?: string } | null)?.code;
			if (opts.cursor && ['22007', '22008', '22009', '22003', '22P02'].includes(code ?? '')) {
				throw new Error('invalid cursor');
			}
			throw err;
		})) as unknown as Array<
		SelectResultFields<C> & { createdKeyText: string; receivedKeyText: string; id: string }
	>;

	const page = rows.slice(0, opts.limit);
	const last = page[page.length - 1];
	const nextCursor =
		rows.length > opts.limit && last
			? encodeTraceCursor({ created: last.createdKeyText, received: last.receivedKeyText, id: last.id })
			: undefined;

	return {
		rows: page.map(({ createdKeyText: _c, receivedKeyText: _r, ...rest }) => rest) as unknown as Array<
			SelectResultFields<C>
		>,
		nextCursor,
	};
}

/**
 * Occurrence count under the same guarded conditions, capped: the subquery stops at
 * `TRACE_COUNT_CAP + 1` rows so a pathological trace id costs a bounded scan, and the page renders
 * anything above the cap as "1,000+" (D4).
 */
export async function countTraceOccurrences(
	traceId: string,
	scope: TraceScope,
	opts: { excludeProjectId?: string } = {}
): Promise<{ total: number; capped: boolean }> {
	const conditions = traceConditions(traceId, scope);
	if (opts.excludeProjectId) conditions.push(ne(issues.projectId, opts.excludeProjectId));

	const capped = db
		.select({ one: sql<number>`1`.as('one') })
		.from(errorOccurrences)
		.innerJoin(issues, eq(issues.id, errorOccurrences.issueId))
		.where(and(...conditions))
		.limit(TRACE_COUNT_CAP + 1)
		.as('capped');
	const [row] = await db.select({ n: count() }).from(capped);

	const n = Number(row?.n ?? 0);
	return { total: Math.min(n, TRACE_COUNT_CAP), capped: n > TRACE_COUNT_CAP };
}

/**
 * sentinel-occurrence-spans TASK-02: when the EARLIEST occurrence of the trace the scope can see was recorded, or
 * `null` when it can see none. One bounded index read through the same guard as the page, so "may this viewer ask
 * for this trace's spans" and "may this viewer see an error of this trace" cannot disagree. The trace store needs
 * a couple of minutes to flush a trace, which is what the pull's first fetch time is derived from.
 */
export async function earliestTraceOccurrenceAt(traceId: string, scope: TraceScope): Promise<Date | null> {
	const [row] = await db
		.select({ epoch: sql<number | null>`extract(epoch from min(${errorOccurrences.createdAt}))::float8` })
		.from(errorOccurrences)
		.innerJoin(issues, eq(issues.id, errorOccurrences.issueId))
		.where(and(...traceConditions(traceId, scope)));
	return row?.epoch == null ? null : new Date(Number(row.epoch) * 1000);
}

/** D3's header: the projects the (guarded) trace spans, by name. */
export async function listTraceProjects(
	traceId: string,
	scope: TraceScope
): Promise<Array<{ id: string; name: string }>> {
	return db
		.selectDistinct({ id: projects.id, name: projects.name })
		.from(errorOccurrences)
		.innerJoin(issues, eq(issues.id, errorOccurrences.issueId))
		.innerJoin(projects, eq(projects.id, issues.projectId))
		.where(and(...traceConditions(traceId, scope)))
		.orderBy(projects.name)
		.limit(50);
}

/**
 * How many distinct projects the guarded trace spans — the header's "across N projects". Separate
 * from {@link listTraceProjects}, whose 50-name cap would otherwise under-report it.
 */
export async function countTraceProjects(traceId: string, scope: TraceScope): Promise<number> {
	const [row] = await db
		.select({ n: countDistinct(issues.projectId) })
		.from(errorOccurrences)
		.innerJoin(issues, eq(issues.id, errorOccurrences.issueId))
		.where(and(...traceConditions(traceId, scope)));
	return Number(row?.n ?? 0);
}
