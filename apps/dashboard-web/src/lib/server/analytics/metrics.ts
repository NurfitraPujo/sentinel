import type { ServiceSli } from '../sli/config';
import { accessPredicate, type AccessFilter } from './access';
import type { Run, Row } from './databend';
import { enumList, enumLiteral, tsLiteral, uuidLiteral } from './literals';

/**
 * sentinel-reporting-and-dashboard D10: the metric definitions. This module is the ONLY place they are computed. The
 * dashboard page and the D20 export (`/api/metrics/insights`) both call these functions, so the two cannot drift.
 *
 * Conventions (all over Databend `sentinel.*`, D2):
 * - "In range" is `[from, to)`. `issue_type = 'system_error'` unless a metric says otherwise.
 * - D3: "current state" queries (open backlog, reports waiting) filter `deleted_at IS NULL`; "what happened in the
 *   period" queries do not, so history survives Sentinel's 30-day retention.
 * - D11: every query carries `accessPredicate(...)` in its WHERE. `projects` is not replicated (D2), so a project is
 *   always filtered by `issues.project_id`, never joined.
 * - Additive metrics return rows grouped by (project_id, environment); the page sums them, the export labels them.
 *   Quantiles are not additive, so those take `grouped`.
 * - Nothing here interpolates a request string: ids, enums and timestamps go through literals.ts.
 */

export type Group = 'api' | 'jobs' | 'nats' | 'other' | 'unattributed';
export const GROUPS: readonly Group[] = ['api', 'jobs', 'nats', 'other', 'unattributed'];

export interface MetricCtx {
	run: Run;
	access: AccessFilter;
	/** The service names of the http SLIs (D19 group 1: request_scoped AND service in this set). */
	httpServices: string[];
}

export interface Window {
	from: Date;
	to: Date;
}

const n = (v: string | null | undefined): number => (v === null || v === undefined || v === '' ? 0 : Number(v));
const nOrNull = (v: string | null | undefined): number | null => (v === null || v === undefined || v === '' ? null : Number(v));

const BASE_ISSUES = `i.issue_type = 'system_error'`;

/**
 * Databend's hash join of `error_occurrences` to `issues` takes 44-89 s at 5.75M x 1.5k rows (measured on prod,
 * 2026-10-09), blowing the 10 s per-query budget. Every occurrence read therefore aggregates `error_occurrences` FIRST
 * (window + occurrence-column filters, GROUP BY issue_id and the columns the outer query needs) and joins `issues` on
 * that ~1.5k-row aggregate, which takes ~1.3 s with identical results. `issues` supplies project_id, the issue type and
 * the access check; counts come back as `cnt` and are re-summed outside.
 *
 * An issue's identity includes its environment (processor `StoreEvent`), so all of an issue's occurrences share one
 * `environment`: grouping by it as well as by issue_id loses nothing.
 */
const OCC_SELECT = `o.issue_id AS issue_id, o.environment AS environment`;
const OCC_ISSUES = (agg: string) => `(${agg}) o JOIN issues i ON i.id = o.issue_id`;

/** D19's five groups, decided in order. Group 1 needs the project's http SLI services (D7). */
export function groupCase(httpServices: readonly string[]): string {
	const api = httpServices.length ? `o.request_scoped AND o.service IN (${enumList(httpServices, httpServices)})` : `o.request_scoped`;
	return `CASE WHEN ${api} THEN 'api' WHEN o.tag_source = 'job' THEN 'jobs' WHEN o.tag_source IN ('worker_handler', 'nats_client') THEN 'nats' WHEN o.service IS NOT NULL THEN 'other' ELSE 'unattributed' END`;
}

const inRange = (col: string, w: Window) => `${col} >= ${tsLiteral(w.from)} AND ${col} < ${tsLiteral(w.to)}`;

export interface CountRow {
	projectId: string;
	environment: string;
	n: number;
}

/** Error events in range by project, environment, D19 group, level and service. */
export interface EventRow extends CountRow {
	group: Group;
	level: string;
	service: string | null;
}

export async function errorEvents(c: MetricCtx, w: Window): Promise<EventRow[]> {
	const rows = await c.run(`
		SELECT i.project_id, o.environment, ${groupCase(c.httpServices)} AS grp, o.level, o.service, sum(o.cnt) AS n
		FROM ${OCC_ISSUES(`
		  SELECT ${OCC_SELECT}, o.level AS level, o.service AS service, o.tag_source AS tag_source, o.request_scoped AS request_scoped, count(*) AS cnt
		  FROM error_occurrences o WHERE ${inRange('o.created_at', w)}
		  GROUP BY o.issue_id, o.environment, o.level, o.service, o.tag_source, o.request_scoped`)}
		WHERE ${BASE_ISSUES} AND ${accessPredicate(c.access, 'i.project_id', 'o.environment')}
		GROUP BY i.project_id, o.environment, grp, o.level, o.service`);
	return rows.map((r) => ({ projectId: r[0]!, environment: r[1]!, group: r[2] as Group, level: r[3] ?? 'error', service: r[4], n: n(r[5]) }));
}

export interface SeriesRow {
	bucket: string;
	group: Group;
	service: string | null;
	n: number;
}

/** Error events over time (D13 section 2): hourly buckets for 24h/7d, daily for 28d/90d. */
export async function errorEventsSeries(c: MetricCtx, w: Window, bucket: 'hour' | 'day'): Promise<SeriesRow[]> {
	const trunc = bucket === 'hour' ? 'to_start_of_hour' : 'to_start_of_day';
	const rows = await c.run(`
		SELECT o.b AS b, ${groupCase(c.httpServices)} AS grp, o.service, sum(o.cnt) AS n
		FROM ${OCC_ISSUES(`
		  SELECT ${OCC_SELECT}, ${trunc}(o.created_at) AS b, o.service AS service, o.tag_source AS tag_source, o.request_scoped AS request_scoped, count(*) AS cnt
		  FROM error_occurrences o WHERE ${inRange('o.created_at', w)}
		  GROUP BY o.issue_id, o.environment, b, o.service, o.tag_source, o.request_scoped`)}
		WHERE ${BASE_ISSUES} AND ${accessPredicate(c.access, 'i.project_id', 'o.environment')}
		GROUP BY b, grp, o.service ORDER BY b`);
	return rows.map((r) => ({ bucket: r[0]!, group: r[1] as Group, service: r[2], n: n(r[3]) }));
}

export async function newIssues(c: MetricCtx, w: Window): Promise<CountRow[]> {
	const rows = await c.run(`
		SELECT i.project_id, i.environment, count(*) AS n FROM issues i
		WHERE ${BASE_ISSUES} AND ${inRange('i.first_seen', w)} AND ${accessPredicate(c.access, 'i.project_id', 'i.environment')}
		GROUP BY i.project_id, i.environment`);
	return rows.map((r) => ({ projectId: r[0]!, environment: r[1]!, n: n(r[2]) }));
}

/** `issue_activity` rows with `event_type = 'regressed'` in range. Written by the processor and the dashboard (D10). */
export async function regressions(c: MetricCtx, w: Window): Promise<CountRow[]> {
	const rows = await c.run(`
		SELECT i.project_id, i.environment, count(*) AS n
		FROM issue_activity a JOIN issues i ON i.id = a.issue_id
		WHERE ${BASE_ISSUES} AND a.event_type = 'regressed' AND ${inRange('a.created_at', w)} AND ${accessPredicate(c.access, 'i.project_id', 'i.environment')}
		GROUP BY i.project_id, i.environment`);
	return rows.map((r) => ({ projectId: r[0]!, environment: r[1]!, n: n(r[2]) }));
}

/**
 * `status_changed` rows whose new status is `resolved`. JSON shape transcribed from the writers:
 * dashboard `updateIssueStatus` writes `new_value = {status, resolvedInVersion, ...}`, `old_value = {status, waitingOn}`.
 */
export async function resolutions(c: MetricCtx, w: Window): Promise<CountRow[]> {
	const rows = await c.run(`
		SELECT i.project_id, i.environment, count(*) AS n
		FROM issue_activity a JOIN issues i ON i.id = a.issue_id
		WHERE ${BASE_ISSUES} AND a.event_type = 'status_changed' AND a.new_value['status']::String = 'resolved'
		  AND ${inRange('a.created_at', w)} AND ${accessPredicate(c.access, 'i.project_id', 'i.environment')}
		GROUP BY i.project_id, i.environment`);
	return rows.map((r) => ({ projectId: r[0]!, environment: r[1]!, n: n(r[2]) }));
}

export const AGE_BUCKETS = ['0-7d', '7-30d', '30-90d', '90d+'] as const;
export type AgeBucket = (typeof AGE_BUCKETS)[number];

export interface BacklogRow extends CountRow {
	age: AgeBucket;
	unassigned: number;
}

/** Open backlog, now: unresolved and not deleted, by age of `first_seen`, plus the unassigned share. */
export async function openBacklog(c: MetricCtx, now: Date): Promise<BacklogRow[]> {
	const d = (days: number) => tsLiteral(new Date(now.getTime() - days * 86_400_000));
	const rows = await c.run(`
		SELECT i.project_id, i.environment,
		  CASE WHEN i.first_seen >= ${d(7)} THEN '0-7d' WHEN i.first_seen >= ${d(30)} THEN '7-30d' WHEN i.first_seen >= ${d(90)} THEN '30-90d' ELSE '90d+' END AS age,
		  count(*) AS n, count_if(i.assigned_to IS NULL) AS unassigned
		FROM issues i
		WHERE ${BASE_ISSUES} AND i.status = 'unresolved' AND i.deleted_at IS NULL AND ${accessPredicate(c.access, 'i.project_id', 'i.environment')}
		GROUP BY i.project_id, i.environment, age`);
	return rows.map((r) => ({ projectId: r[0]!, environment: r[1]!, age: r[2] as AgeBucket, n: n(r[3]), unassigned: n(r[4]) }));
}

/** User reports waiting: `issue_type = 'user_report'`, unresolved, not deleted. Per project (D20 has no environment). */
export async function userReportsWaiting(c: MetricCtx): Promise<{ projectId: string; n: number }[]> {
	const rows = await c.run(`
		SELECT i.project_id, count(*) AS n FROM issues i
		WHERE i.issue_type = 'user_report' AND i.status = 'unresolved' AND i.deleted_at IS NULL AND ${accessPredicate(c.access, 'i.project_id', 'i.environment')}
		GROUP BY i.project_id`);
	return rows.map((r) => ({ projectId: r[0]!, n: n(r[1]) }));
}

export interface TopIssue {
	id: string;
	projectId: string;
	environment: string;
	message: string;
	errorClass: string;
	users: number;
	events: number;
}

/** Top issues: the 10 with the most distinct `event_user` ids in range; ties broken by event count. */
export async function topIssues(c: MetricCtx, w: Window): Promise<TopIssue[]> {
	const rows = await c.run(`
		SELECT i.id, i.project_id, i.environment, i.message, i.error_class,
		  sum(o.users) AS users, sum(o.cnt) AS events
		FROM ${OCC_ISSUES(`
		  SELECT ${OCC_SELECT}, count(DISTINCT o.event_user['id']::String) AS users, count(*) AS cnt
		  FROM error_occurrences o WHERE ${inRange('o.created_at', w)}
		  GROUP BY o.issue_id, o.environment`)}
		WHERE ${BASE_ISSUES} AND ${accessPredicate(c.access, 'i.project_id', 'o.environment')}
		GROUP BY i.id, i.project_id, i.environment, i.message, i.error_class
		ORDER BY users DESC, events DESC LIMIT 10`);
	return rows.map((r) => ({ id: r[0]!, projectId: r[1]!, environment: r[2]!, message: r[3] ?? '', errorClass: r[4] ?? '', users: n(r[5]), events: n(r[6]) }));
}

export interface QuantileRow {
	projectId: string | null;
	environment: string | null;
	count: number;
	p50: number | null;
	p90: number | null;
}

const unixSecs = (col: string) => `to_unix_timestamp(${col})`;

function quantileRows(rows: Row[], grouped: boolean): QuantileRow[] {
	const o = grouped ? 2 : 0;
	return rows.map((r) => ({
		projectId: grouped ? r[0] : null,
		environment: grouped ? r[1] : null,
		count: n(r[o]),
		p50: nOrNull(r[o + 1]),
		p90: nOrNull(r[o + 2]),
	}));
}

/**
 * MTTA: median and p90 of (first `assigned`/`claimed` activity − the issue's `first_seen`), for issues first seen in
 * range that have one. Seconds.
 */
export async function mtta(c: MetricCtx, w: Window, grouped: boolean): Promise<QuantileRow[]> {
	const cols = grouped ? 'project_id, environment,' : '';
	const group = grouped ? 'GROUP BY project_id, environment' : '';
	const rows = await c.run(`
		SELECT ${cols} count(*) AS n, quantile_cont(0.5)(secs) AS p50, quantile_cont(0.9)(secs) AS p90 FROM (
		  SELECT i.project_id AS project_id, i.environment AS environment, i.id AS id,
		    min(${unixSecs('a.created_at')}) - ${unixSecs('i.first_seen')} AS secs
		  FROM issues i JOIN issue_activity a ON a.issue_id = i.id
		  WHERE ${BASE_ISSUES} AND a.event_type IN ('assigned', 'claimed') AND ${inRange('i.first_seen', w)}
		    AND ${accessPredicate(c.access, 'i.project_id', 'i.environment')}
		  GROUP BY i.project_id, i.environment, i.id, i.first_seen
		) t ${group}`);
	return quantileRows(rows, grouped);
}

/** MTTR: median and p90 of (resolved activity − `first_seen`), for resolutions in range. Seconds. */
export async function mttr(c: MetricCtx, w: Window, grouped: boolean): Promise<QuantileRow[]> {
	const cols = grouped ? 'project_id, environment,' : '';
	const group = grouped ? 'GROUP BY project_id, environment' : '';
	const rows = await c.run(`
		SELECT ${cols} count(*) AS n, quantile_cont(0.5)(secs) AS p50, quantile_cont(0.9)(secs) AS p90 FROM (
		  SELECT i.project_id AS project_id, i.environment AS environment,
		    ${unixSecs('a.created_at')} - ${unixSecs('i.first_seen')} AS secs
		  FROM issues i JOIN issue_activity a ON a.issue_id = i.id
		  WHERE ${BASE_ISSUES} AND a.event_type = 'status_changed' AND a.new_value['status']::String = 'resolved'
		    AND ${inRange('a.created_at', w)} AND ${accessPredicate(c.access, 'i.project_id', 'i.environment')}
		) t ${group}`);
	return quantileRows(rows, grouped);
}

/** Regression rate: regressions in range ÷ resolutions in range. `null` when nothing was resolved. */
export function regressionRate(regressionCount: number, resolutionCount: number): number | null {
	return resolutionCount === 0 ? null : regressionCount / resolutionCount;
}

export interface ChangeFailureRow extends CountRow {
	failed: number;
}

/**
 * Change failure rate: releases whose window starts in range and that introduced (`issues.first_release_version = v`)
 * an issue reaching ≥10 events or ≥3 distinct users within 24 h of the window start, ÷ releases whose window starts in
 * range. `n` is the denominator, `failed` the numerator. The window start is D9's `min(created_at)` per
 * (project, environment, version), over all history.
 */
export async function changeFailure(c: MetricCtx, w: Window): Promise<ChangeFailureRow[]> {
	const rows = await c.run(`
		WITH occ_rel AS (
		  SELECT ${OCC_SELECT}, o.release_version AS v, min(o.created_at) AS start_at
		  FROM error_occurrences o WHERE o.release_version IS NOT NULL
		  GROUP BY o.issue_id, o.environment, o.release_version
		), rel AS (
		  SELECT i.project_id AS project_id, o.environment AS environment, o.v AS v, min(o.start_at) AS start_at
		  FROM occ_rel o JOIN issues i ON i.id = o.issue_id
		  WHERE ${BASE_ISSUES} AND ${accessPredicate(c.access, 'i.project_id', 'o.environment')}
		  GROUP BY i.project_id, o.environment, o.v
		  HAVING min(o.start_at) >= ${tsLiteral(w.from)} AND min(o.start_at) < ${tsLiteral(w.to)}
		), cand AS (
		  -- an issue has one first_release_version, so it matches at most one rel row: start_at is per issue
		  SELECT r.project_id AS project_id, r.environment AS environment, r.v AS v, i.id AS issue_id, r.start_at AS start_at
		  FROM rel r
		  JOIN issues i ON i.project_id = r.project_id AND i.environment = r.environment AND i.first_release_version = r.v AND ${BASE_ISSUES}
		), occ24 AS (
		  -- only the candidates' occurrences, inside the span of all their 24 h windows, reach the join below
		  SELECT o.issue_id AS issue_id, o.created_at AS created_at, o.event_user AS event_user
		  FROM error_occurrences o
		  WHERE o.issue_id IN (SELECT issue_id FROM cand)
		    AND o.created_at >= (SELECT min(start_at) FROM cand) AND o.created_at < add_hours((SELECT max(start_at) FROM cand), 24)
		), bad AS (
		  SELECT r.project_id AS project_id, r.environment AS environment, r.v AS v
		  FROM cand r
		  JOIN occ24 o ON o.issue_id = r.issue_id AND o.created_at >= r.start_at AND o.created_at < add_hours(r.start_at, 24)
		  GROUP BY r.project_id, r.environment, r.v, r.issue_id
		  HAVING count(*) >= 10 OR count(DISTINCT o.event_user['id']::String) >= 3
		)
		SELECT r.project_id, r.environment, count(*) AS n, count_if(b.v IS NOT NULL) AS failed
		FROM rel r LEFT JOIN (SELECT DISTINCT project_id, environment, v FROM bad) b
		  ON b.project_id = r.project_id AND b.environment = r.environment AND b.v = r.v
		GROUP BY r.project_id, r.environment`);
	return rows.map((r) => ({ projectId: r[0]!, environment: r[1]!, n: n(r[2]), failed: n(r[3]) }));
}

export interface ReleaseWindow {
	projectId: string;
	environment: string;
	version: string;
	start: Date;
	end: Date;
	events: Record<Group, number>;
	newIssues: number;
	regressions: number;
}

const zeroGroups = (): Record<Group, number> => ({ api: 0, jobs: 0, nats: 0, other: 0, unattributed: 0 });

/**
 * D9: in environment E, release `v` starts at `first_seen(v,E) = min(created_at)` of its occurrences, and ends at the
 * next later first_seen of another version of the same project in E, or now. A release with no events in E has no
 * window and is not listed. Returns the `limit` latest windows per (project, environment), newest first, with the
 * counts the release table (D13 section 4) and D20's `release_*` gauges show.
 */
export async function releaseWindows(c: MetricCtx, now: Date, limitPerProjectEnv: number): Promise<ReleaseWindow[]> {
	if (!Number.isInteger(limitPerProjectEnv) || limitPerProjectEnv < 1 || limitPerProjectEnv > 50) throw new Error('bad release limit');
	const nowLit = tsLiteral(now);
	const rows = await c.run(`
		WITH occ_rel AS (
		  SELECT ${OCC_SELECT}, o.release_version AS v, min(o.created_at) AS start_at
		  FROM error_occurrences o WHERE o.release_version IS NOT NULL
		  GROUP BY o.issue_id, o.environment, o.release_version
		), starts AS (
		  SELECT i.project_id AS project_id, o.environment AS environment, o.v AS v, min(o.start_at) AS start_at
		  FROM occ_rel o JOIN issues i ON i.id = o.issue_id
		  WHERE ${BASE_ISSUES} AND ${accessPredicate(c.access, 'i.project_id', 'o.environment')}
		  GROUP BY i.project_id, o.environment, o.v
		), wins AS (
		  SELECT project_id, environment, v, start_at,
		    coalesce(lead(start_at) OVER (PARTITION BY project_id, environment ORDER BY start_at), ${nowLit}) AS end_at,
		    row_number() OVER (PARTITION BY project_id, environment ORDER BY start_at DESC) AS rk
		  FROM starts
		), top AS (SELECT * FROM wins WHERE rk <= ${limitPerProjectEnv}),
		occ_top AS (
		  -- only the shown releases' occurrences, from the earliest shown window start, reach the join below
		  SELECT o.issue_id AS issue_id, o.environment AS environment, o.release_version AS release_version, o.created_at AS created_at,
		    o.request_scoped AS request_scoped, o.tag_source AS tag_source, o.service AS service
		  FROM error_occurrences o
		  WHERE o.release_version IN (SELECT v FROM top) AND o.created_at >= (SELECT min(start_at) FROM top)
		    AND o.issue_id IN (SELECT i.id FROM issues i WHERE ${BASE_ISSUES} AND i.project_id IN (SELECT project_id FROM top))
		)
		SELECT t.project_id, t.environment, t.v, t.start_at, t.end_at, 'ev' AS kind, ${groupCase(c.httpServices)} AS grp, count(*) AS n
		FROM top t
		JOIN issues i ON i.project_id = t.project_id AND ${BASE_ISSUES}
		JOIN occ_top o ON o.issue_id = i.id AND o.environment = t.environment AND o.release_version = t.v
		  AND o.created_at >= t.start_at AND o.created_at < t.end_at
		GROUP BY t.project_id, t.environment, t.v, t.start_at, t.end_at, grp
		UNION ALL
		SELECT t.project_id, t.environment, t.v, t.start_at, t.end_at, 'new', '', count(*)
		FROM top t JOIN issues i ON i.project_id = t.project_id AND i.environment = t.environment AND i.first_release_version = t.v AND ${BASE_ISSUES}
		  AND i.first_seen >= t.start_at AND i.first_seen < t.end_at
		GROUP BY t.project_id, t.environment, t.v, t.start_at, t.end_at
		UNION ALL
		SELECT t.project_id, t.environment, t.v, t.start_at, t.end_at, 'reg', '', count(*)
		FROM top t JOIN issues i ON i.project_id = t.project_id AND i.environment = t.environment AND ${BASE_ISSUES}
		JOIN issue_activity a ON a.issue_id = i.id AND a.event_type = 'regressed' AND a.new_value['releaseVersion']::String = t.v
		  AND a.created_at >= t.start_at AND a.created_at < t.end_at
		GROUP BY t.project_id, t.environment, t.v, t.start_at, t.end_at
		UNION ALL
		SELECT project_id, environment, v, start_at, end_at, 'win', '', 0 FROM top`);
	const byKey = new Map<string, ReleaseWindow>();
	const parse = (s: string) => new Date(s.replace(' ', 'T') + (s.includes('Z') || /[+-]\d\d:?\d\d$/.test(s) ? '' : 'Z'));
	for (const r of rows) {
		const key = `${r[0]}|${r[1]}|${r[2]}`;
		let w = byKey.get(key);
		if (!w) {
			w = { projectId: r[0]!, environment: r[1]!, version: r[2]!, start: parse(r[3]!), end: parse(r[4]!), events: zeroGroups(), newIssues: 0, regressions: 0 };
			byKey.set(key, w);
		}
		const kind = r[5];
		if (kind === 'ev') w.events[r[6] as Group] += n(r[7]);
		else if (kind === 'new') w.newIssues += n(r[7]);
		else if (kind === 'reg') w.regressions += n(r[7]);
	}
	return [...byKey.values()].sort((a, b) => b.start.getTime() - a.start.getTime());
}

export interface WeekRow {
	week: string;
	opened: number;
	resolved: number;
}

/** Opened (new + regressed) vs resolved per week, for the last 12 weeks (D13 section 5). Weeks start on Monday. */
export async function weeklyFlow(c: MetricCtx, now: Date): Promise<WeekRow[]> {
	const from = new Date(now.getTime() - 12 * 7 * 86_400_000);
	const w = { from, to: now };
	const acc = accessPredicate(c.access, 'i.project_id', 'i.environment');
	const rows = await c.run(`
		SELECT to_monday(t) AS wk, kind, count(*) AS n FROM (
		  SELECT i.first_seen AS t, 'opened' AS kind FROM issues i WHERE ${BASE_ISSUES} AND ${inRange('i.first_seen', w)} AND ${acc}
		  UNION ALL
		  SELECT a.created_at, 'opened' FROM issue_activity a JOIN issues i ON i.id = a.issue_id
		    WHERE ${BASE_ISSUES} AND a.event_type = 'regressed' AND ${inRange('a.created_at', w)} AND ${acc}
		  UNION ALL
		  SELECT a.created_at, 'resolved' FROM issue_activity a JOIN issues i ON i.id = a.issue_id
		    WHERE ${BASE_ISSUES} AND a.event_type = 'status_changed' AND a.new_value['status']::String = 'resolved' AND ${inRange('a.created_at', w)} AND ${acc}
		) u GROUP BY wk, kind ORDER BY wk`);
	const byWeek = new Map<string, WeekRow>();
	for (const r of rows) {
		const wk = r[0]!;
		const row = byWeek.get(wk) ?? { week: wk, opened: 0, resolved: 0 };
		if (r[1] === 'opened') row.opened += n(r[2]);
		else row.resolved += n(r[2]);
		byWeek.set(wk, row);
	}
	return [...byWeek.values()];
}

export interface SliSums {
	sliId: string;
	environment: string;
	total: number;
	errors: number;
	/** null when no row in the window carried a latency count. */
	fast: number | null;
	/** Σtotal over only the rows that carry `fast` — the latency ratio's denominator (rows with fast NULL are excluded). */
	totalWithFast: number;
}

/** Σ of `sli_hourly` over `[from, to)` for the given SLIs (already filtered to the ones the viewer may see, D11). */
export async function sliSums(c: MetricCtx, sliIds: readonly string[], w: Window, environment?: string): Promise<SliSums[]> {
	if (sliIds.length === 0) return [];
	const envClause = environment ? ` AND environment = ${enumLiteral(environment, ['production', 'staging'])}` : '';
	const rows = await c.run(`
		SELECT sli_id, environment, sum(total) AS total, sum(errors) AS errors, sum(fast) AS fast,
		  sum(CASE WHEN fast IS NULL THEN 0 ELSE total END) AS total_with_fast
		FROM sli_hourly WHERE sli_id IN (${enumList(sliIds, sliIds)}) AND ${inRange('hour_start', w)}${envClause}
		GROUP BY sli_id, environment`);
	return rows.map((r) => ({ sliId: r[0]!, environment: r[1]!, total: n(r[2]), errors: n(r[3]), fast: nOrNull(r[4]), totalWithFast: n(r[5]) }));
}

/** Raw hourly rows for the given SLIs, so callers can sum arbitrary windows (the release table) in one query. */
export interface SliHourRow {
	sliId: string;
	hourStart: Date;
	total: number;
	errors: number;
}

export async function sliHourlyRows(c: MetricCtx, sliIds: readonly string[], w: Window, environment: string): Promise<SliHourRow[]> {
	if (sliIds.length === 0) return [];
	const rows = await c.run(`
		SELECT sli_id, hour_start, total, errors FROM sli_hourly
		WHERE sli_id IN (${enumList(sliIds, sliIds)}) AND environment = ${enumLiteral(environment, ['production', 'staging'])} AND ${inRange('hour_start', w)}`);
	return rows.map((r) => ({ sliId: r[0]!, hourStart: new Date(r[1]!.replace(' ', 'T') + 'Z'), total: n(r[2]), errors: n(r[3]) }));
}

/** Hourly or daily good-ratio series for one SLI (D13 section 3). */
export interface SliPoint {
	bucket: string;
	total: number;
	errors: number;
	fast: number | null;
	totalWithFast: number;
}

export async function sliSeries(c: MetricCtx, sliId: string, w: Window, bucket: 'hour' | 'day', environment: string): Promise<SliPoint[]> {
	const trunc = bucket === 'hour' ? 'to_start_of_hour' : 'to_start_of_day';
	const rows = await c.run(`
		SELECT ${trunc}(hour_start) AS b, sum(total), sum(errors), sum(fast), sum(CASE WHEN fast IS NULL THEN 0 ELSE total END)
		FROM sli_hourly WHERE sli_id = ${enumLiteral(sliId, [sliId])} AND environment = ${enumLiteral(environment, ['production', 'staging'])} AND ${inRange('hour_start', w)}
		GROUP BY b ORDER BY b`);
	return rows.map((r) => ({ bucket: r[0]!, total: n(r[1]), errors: n(r[2]), fast: nOrNull(r[3]), totalWithFast: n(r[4]) }));
}

/** Availability: `1 − Σerrors/Σtotal`. null with no traffic. */
export function availability(s: Pick<SliSums, 'total' | 'errors'>): number | null {
	return s.total === 0 ? null : 1 - s.errors / s.total;
}

/** Latency: `Σfast/Σtotal`, over the rows that carry a latency count. null with none. */
export function latencyRatio(s: Pick<SliSums, 'fast' | 'totalWithFast'>): number | null {
	return s.fast === null || s.totalWithFast === 0 ? null : s.fast / s.totalWithFast;
}

/** Error budget left: `1 − (Σerrors/Σtotal)/(1 − target)` over the trailing 28 days, floored at −100% (−1). */
export function errorBudgetRemaining(s: Pick<SliSums, 'total' | 'errors'>, target: number): number | null {
	if (s.total === 0 || !(target > 0 && target < 1)) return null;
	return Math.max(-1, 1 - s.errors / s.total / (1 - target));
}

/** The stored-event match for an SLI's errors-per-1k numerator (D7 "events counted against it", D19). null = none. */
export function sliEventMatch(sli: ServiceSli): string | null {
	switch (sli.kind) {
		case 'http':
			return sli.service ? `o.request_scoped AND o.service = ${enumLiteral(sli.service, [sli.service])}` : null;
		case 'jobs':
			return `o.tag_source = 'job'`;
		case 'nats':
			return `o.tag_source IN ('worker_handler', 'nats_client')`;
	}
}

/**
 * Errors per 1k: the SLI's matching events (D7, D19) ÷ Σtotal in the same hours × 1000. `events` counts occurrences of
 * the SLI's project in the SLI's environment over `[from, to)`; the caller divides by `sliSums(...).total`.
 * `null` when the SLI has no matching events definition (web, whose events carry no `service`).
 */
export async function sliMatchingEvents(c: MetricCtx, sli: ServiceSli, projectId: string, environment: string, w: Window): Promise<number | null> {
	const match = sliEventMatch(sli);
	if (!match) return null;
	const rows = await c.run(`
		SELECT coalesce(sum(o.cnt), 0) FROM ${OCC_ISSUES(`
		  SELECT ${OCC_SELECT}, count(*) AS cnt
		  FROM error_occurrences o
		  WHERE o.environment = ${enumLiteral(environment, [environment])} AND ${inRange('o.created_at', w)} AND (${match})
		  GROUP BY o.issue_id, o.environment`)}
		WHERE ${BASE_ISSUES} AND i.project_id = ${uuidLiteral(projectId)} AND ${accessPredicate(c.access, 'i.project_id', 'o.environment')}`);
	return n(rows[0]?.[0]);
}

export function errorsPer1k(events: number | null, total: number): number | null {
	return events === null || total === 0 ? null : (events / total) * 1000;
}

/** The newest `received_at` Databend holds for the viewer's accessible data: the freshness line (D13). */
export async function latestReceivedAt(c: MetricCtx): Promise<Date | null> {
	const rows = await c.run(`
		SELECT max(o.received_at) FROM ${OCC_ISSUES(`
		  SELECT ${OCC_SELECT}, max(o.received_at) AS received_at FROM error_occurrences o GROUP BY o.issue_id, o.environment`)}
		WHERE ${accessPredicate(c.access, 'i.project_id', 'o.environment')}`);
	const v = rows[0]?.[0];
	if (!v) return null;
	const d = new Date(v.replace(' ', 'T') + (/Z|[+-]\d\d:?\d\d$/.test(v) ? '' : 'Z'));
	return Number.isNaN(d.getTime()) ? null : d;
}
