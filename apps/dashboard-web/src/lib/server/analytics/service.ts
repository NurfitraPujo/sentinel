import type { ServiceSli } from '../sli/config';
import { accessPredicate, type AccessFilter, type Scope } from './access';
import { DatabendUnavailableError, type Run } from './databend';
import { RANGE_MS, isUuid, type Range } from './literals';
import * as m from './metrics';

/**
 * sentinel-reporting-and-dashboard D13/D14: the dashboard page's data, one function per section, each independent.
 *
 * - D14 isolation: a section that cannot be computed (Databend down, timeout, a bad query) is `{ status: 'unavailable' }`
 *   and nothing else changes; the loader streams each section as its own promise.
 * - D14 caching: results are cached for 60 s per (accessible set, filters, section).
 * - D11: the viewer's `scopes` go into every query's WHERE (metrics.ts); an SLI tile exists only for a viewer who can
 *   reach its project with an unrestricted or `production` scope.
 * - The page claims no consistency across sections (contract §8).
 */

export type Section<T> = { status: 'ok'; data: T } | { status: 'unavailable' };

export interface ProjectInfo {
	id: string;
	name: string;
}

export interface DashboardInput {
	range: Range;
	/** 'all' or one project id the viewer can reach. */
	project: string;
	/** 'all' or one environment name (only honoured with one project). */
	environment: string;
	now: Date;
	projects: ProjectInfo[];
	scopes: Scope[];
	knownEnvironments: string[];
	slis: ServiceSli[];
	/** The environment this deployment's `sli_hourly` rows carry (from SLI_CLUSTER); null disables SLI sections. */
	sliEnvironment: string | null;
	run: Run;
}

export interface GroupCounts {
	api: number;
	jobs: number;
	nats: number;
	other: number;
	unattributed: number;
}
const zero = (): GroupCounts => ({ api: 0, jobs: 0, nats: 0, other: 0, unattributed: 0 });
const total = (g: GroupCounts) => g.api + g.jobs + g.nats + g.other + g.unattributed;

function groupsOf(rows: m.EventRow[]): GroupCounts {
	const g = zero();
	for (const r of rows) g[r.group] += r.n;
	return g;
}

const sumN = (rows: { n: number }[]) => rows.reduce((a, r) => a + r.n, 0);

/** The 60 s cache (D14). Keyed by the viewer's accessible set and the filters, so two viewers never share an entry. */
const cache = new Map<string, { at: number; value: Promise<unknown> }>();
export const CACHE_TTL_MS = 60_000;
export function clearAnalyticsCache() {
	cache.clear();
}

async function cached<T>(key: string, now: Date, compute: () => Promise<T>): Promise<T> {
	const hit = cache.get(key);
	if (hit && now.getTime() - hit.at < CACHE_TTL_MS && now.getTime() >= hit.at) return hit.value as Promise<T>;
	const value = compute();
	cache.set(key, { at: now.getTime(), value });
	try {
		return await value;
	} catch (err) {
		cache.delete(key); // never cache a failure
		throw err;
	}
}

async function section<T>(input: DashboardInput, name: string, compute: (c: m.MetricCtx) => Promise<T>): Promise<Section<T>> {
	const key = JSON.stringify([name, input.range, input.project, input.environment, input.scopes, input.sliEnvironment, Math.floor(input.now.getTime() / CACHE_TTL_MS)]);
	try {
		const data = await cached(key, input.now, () => compute(metricCtx(input)));
		return { status: 'ok', data };
	} catch (err) {
		if (!(err instanceof DatabendUnavailableError)) {
			// Any other failure is still contained to its section; the cause is logged by the caller's wrapper.
			console.error(JSON.stringify({ level: 'error', event: 'dashboard.section_failed', section: name, error: String((err as Error)?.message ?? err).slice(0, 300) }));
		} else {
			console.warn(JSON.stringify({ level: 'warn', event: 'dashboard.section_unavailable', section: name, error: String((err as Error)?.message ?? err).slice(0, 300) }));
		}
		return { status: 'unavailable' };
	}
}

/** The scopes the queries run under: the viewer's, narrowed to the selected project. */
function effectiveScopes(i: DashboardInput): Scope[] {
	return i.project === 'all' ? i.scopes : i.scopes.filter((s) => s.projectId === i.project);
}

function metricCtx(i: DashboardInput): m.MetricCtx {
	const access: AccessFilter = {
		scopes: effectiveScopes(i),
		environment: i.project === 'all' ? 'all' : i.environment,
		knownEnvironments: i.knownEnvironments,
	};
	return { run: i.run, access, httpServices: i.slis.filter((s) => s.kind === 'http' && s.service).map((s) => s.service!) };
}

const windows = (i: DashboardInput) => {
	const len = RANGE_MS[i.range];
	const cur: m.Window = { from: new Date(i.now.getTime() - len), to: i.now };
	const prev: m.Window = { from: new Date(cur.from.getTime() - len), to: cur.from };
	return { cur, prev, len };
};

/** D13: hourly buckets for 24 h / 7 d, daily for 28 d / 90 d. */
export const bucketFor = (r: Range): 'hour' | 'day' => (r === '24h' || r === '7d' ? 'hour' : 'day');

/** D11: the SLIs this viewer may see, with the project each belongs to. */
export function visibleSlis(i: Pick<DashboardInput, 'slis' | 'projects' | 'scopes' | 'project' | 'sliEnvironment'>): { sli: ServiceSli; projectId: string }[] {
	if (!i.sliEnvironment) return [];
	const out: { sli: ServiceSli; projectId: string }[] = [];
	for (const sli of i.slis) {
		const p = i.projects.find((x) => x.name === sli.project);
		if (!p || (i.project !== 'all' && i.project !== p.id)) continue;
		const scope = i.scopes.find((s) => s.projectId === p.id);
		if (!scope) continue; // not accessible
		if (scope.environments !== null && !scope.environments.includes('production')) continue; // D11
		out.push({ sli, projectId: p.id });
	}
	return out;
}

export interface SliTile {
	id: string;
	kind: ServiceSli['kind'];
	project: string;
	availability: number | null;
	availabilityTarget: number | null;
	latency: number | null;
	latencyTarget: number | null;
	budgetRemaining: number | null;
	total: number;
	errors: number;
	/** Errors per 1k of this SLI's own denominator; null = no matching-event definition or no traffic ("no data"). */
	errorsPer1k: number | null;
	events: number | null;
}

export interface HealthData {
	events: { current: GroupCounts; previous: GroupCounts };
	byLevel: Record<string, number>;
	newIssues: { current: number; previous: number };
	regressions: { current: number; previous: number };
	backlog: { open: number; unassigned: number };
	slis: SliTile[];
}

export function health(input: DashboardInput): Promise<Section<HealthData>> {
	return section(input, 'health', async (c) => {
		const { cur, prev } = windows(input);
		const [evCur, evPrev, niCur, niPrev, rgCur, rgPrev, backlog] = await Promise.all([
			m.errorEvents(c, cur), m.errorEvents(c, prev), m.newIssues(c, cur), m.newIssues(c, prev),
			m.regressions(c, cur), m.regressions(c, prev), m.openBacklog(c, input.now),
		]);
		const byLevel: Record<string, number> = {};
		for (const r of evCur) byLevel[r.level] = (byLevel[r.level] ?? 0) + r.n;
		const slis = await sliTiles(input, c, cur);
		return {
			events: { current: groupsOf(evCur), previous: groupsOf(evPrev) },
			byLevel,
			newIssues: { current: sumN(niCur), previous: sumN(niPrev) },
			regressions: { current: sumN(rgCur), previous: sumN(rgPrev) },
			backlog: { open: sumN(backlog), unassigned: backlog.reduce((a, r) => a + r.unassigned, 0) },
			slis,
		};
	});
}

async function sliTiles(input: DashboardInput, c: m.MetricCtx, cur: m.Window): Promise<SliTile[]> {
	const vis = visibleSlis(input);
	if (vis.length === 0 || !input.sliEnvironment) return [];
	const env = input.sliEnvironment;
	const budgetWindow: m.Window = { from: new Date(input.now.getTime() - 28 * 86_400_000), to: input.now };
	const ids = vis.map((v) => v.sli.id);
	const [rangeSums, budgetSums] = await Promise.all([m.sliSums(c, ids, cur, env), m.sliSums(c, ids, budgetWindow, env)]);
	return Promise.all(
		vis.map(async ({ sli, projectId }) => {
			const s = rangeSums.find((x) => x.sliId === sli.id) ?? { sliId: sli.id, environment: env, total: 0, errors: 0, fast: null, totalWithFast: 0 };
			const b = budgetSums.find((x) => x.sliId === sli.id);
			const events = await m.sliMatchingEvents(c, sli, projectId, env, cur);
			return {
				id: sli.id, kind: sli.kind, project: sli.project,
				availability: m.availability(s), availabilityTarget: sli.availabilityTarget ?? null,
				latency: m.latencyRatio(s), latencyTarget: sli.latency?.target ?? null,
				budgetRemaining: b && sli.availabilityTarget ? m.errorBudgetRemaining(b, sli.availabilityTarget) : null,
				total: s.total, errors: s.errors,
				errorsPer1k: m.errorsPer1k(events, s.total), events,
			};
		})
	);
}

export interface TimelineData {
	bucket: 'hour' | 'day';
	/** `group` series always; `service` series when one project is selected (D13). */
	byGroup: m.SeriesRow[];
	byService: boolean;
	releaseMarkers: { version: string; at: string }[];
}

export function timeline(input: DashboardInput): Promise<Section<TimelineData>> {
	return section(input, 'timeline', async (c) => {
		const { cur } = windows(input);
		const bucket = bucketFor(input.range);
		const byGroup = await m.errorEventsSeries(c, cur, bucket);
		let releaseMarkers: TimelineData['releaseMarkers'] = [];
		if (input.project !== 'all') {
			const wins = await m.releaseWindows(c, input.now, 10);
			releaseMarkers = wins.filter((w) => w.start >= cur.from).map((w) => ({ version: w.version, at: w.start.toISOString() }));
		}
		return { bucket, byGroup, byService: input.project !== 'all', releaseMarkers };
	});
}

export interface SliSeriesData {
	id: string;
	bucket: 'hour' | 'day';
	points: m.SliPoint[];
	total: number;
	errors: number;
	availabilityTarget: number | null;
}

export function sliSection(input: DashboardInput): Promise<Section<SliSeriesData[]>> {
	return section(input, 'slis', async (c) => {
		const vis = visibleSlis(input);
		if (vis.length === 0 || !input.sliEnvironment) return [];
		const { cur } = windows(input);
		const bucket = bucketFor(input.range);
		return Promise.all(
			vis.map(async ({ sli }) => {
				const points = await m.sliSeries(c, sli.id, cur, bucket, input.sliEnvironment!);
				return { id: sli.id, bucket, points, total: points.reduce((a, p) => a + p.total, 0), errors: points.reduce((a, p) => a + p.errors, 0), availabilityTarget: sli.availabilityTarget ?? null };
			})
		);
	});
}

export interface ReleaseRow {
	version: string;
	environment: string;
	firstSeen: string;
	events: GroupCounts;
	newIssues: number;
	regressions: number;
	errorsPer1k: number | null;
	/** Change in errors per 1k (or in events when no rate exists) against the release before it; null for the oldest. */
	change: number | null;
}

export function releases(input: DashboardInput): Promise<Section<{ rows: ReleaseRow[]; needsProject: boolean }>> {
	return section(input, 'releases', async (c) => {
		if (input.project === 'all') return { rows: [], needsProject: true };
		// one extra window per (project, environment) so the oldest listed release still has a "previous" to compare with
		const wins = (await m.releaseWindows(c, input.now, 11)).filter((w) => w.projectId === input.project);
		const project = input.projects.find((p) => p.id === input.project);
		const vis = visibleSlis(input).filter((v) => v.projectId === input.project && v.sli.kind === 'http' && v.sli.service);
		const hourly = vis.length && input.sliEnvironment && wins.length
			? await m.sliHourlyRows(c, vis.map((v) => v.sli.id), { from: new Date(Math.min(...wins.map((w) => w.start.getTime()))), to: input.now }, input.sliEnvironment)
			: [];
		const rate = (w: m.ReleaseWindow): number | null => {
			if (!vis.length || w.environment !== input.sliEnvironment) return null;
			const denom = hourly.filter((h) => h.hourStart >= w.start && h.hourStart < w.end).reduce((a, h) => a + h.total, 0);
			return m.errorsPer1k(w.events.api, denom);
		};
		void project;
		const sorted = wins.slice().sort((a, b) => b.start.getTime() - a.start.getTime());
		const rows: ReleaseRow[] = sorted.map((w, idx) => {
			const prev = sorted.slice(idx + 1).find((p) => p.environment === w.environment);
			const r = rate(w);
			const pr = prev ? rate(prev) : null;
			const change = prev ? (r !== null && pr !== null ? r - pr : total(w.events) - total(prev.events)) : null;
			return { version: w.version, environment: w.environment, firstSeen: w.start.toISOString(), events: w.events, newIssues: w.newIssues, regressions: w.regressions, errorsPer1k: r, change };
		});
		return { rows: rows.slice(0, 10), needsProject: false };
	});
}

export interface BacklogData {
	weekly: m.WeekRow[];
	age: Record<m.AgeBucket, number>;
	open: number;
	unassigned: number;
	top: m.TopIssue[];
}

export function backlog(input: DashboardInput): Promise<Section<BacklogData>> {
	return section(input, 'backlog', async (c) => {
		const { cur } = windows(input);
		const [weekly, open, top] = await Promise.all([m.weeklyFlow(c, input.now), m.openBacklog(c, input.now), m.topIssues(c, cur)]);
		const age = { '0-7d': 0, '7-30d': 0, '30-90d': 0, '90d+': 0 } as Record<m.AgeBucket, number>;
		for (const r of open) age[r.age] += r.n;
		return { weekly, age, open: sumN(open), unassigned: open.reduce((a, r) => a + r.unassigned, 0), top };
	});
}

export interface ResponseData {
	mtta: m.QuantileRow | null;
	mttr: m.QuantileRow | null;
	regressionRate: number | null;
	changeFailureRate: number | null;
	releasesInRange: number;
	reportsWaiting: number;
}

export function response(input: DashboardInput): Promise<Section<ResponseData>> {
	return section(input, 'response', async (c) => {
		const { cur } = windows(input);
		const [a, r, rg, rs, cf, waiting] = await Promise.all([
			m.mtta(c, cur, false), m.mttr(c, cur, false), m.regressions(c, cur), m.resolutions(c, cur), m.changeFailure(c, cur), m.userReportsWaiting(c),
		]);
		const n = cf.reduce((x, r2) => x + r2.n, 0);
		const failed = cf.reduce((x, r2) => x + r2.failed, 0);
		return {
			mtta: a[0] && a[0].count > 0 ? a[0] : null,
			mttr: r[0] && r[0].count > 0 ? r[0] : null,
			regressionRate: m.regressionRate(sumN(rg), sumN(rs)),
			changeFailureRate: n === 0 ? null : failed / n,
			releasesInRange: n,
			reportsWaiting: sumN(waiting),
		};
	});
}

export interface FreshnessData {
	analyticsAsOf: string | null;
	/** Whole minutes Databend is behind the newest Postgres occurrence; null when unknown. */
	lagMinutes: number | null;
	warn: boolean;
}

/** D13: "analytics as of <time>", with a warning when Databend is more than 10 minutes behind Postgres. */
export async function freshness(input: DashboardInput, newestPostgresOccurrence: () => Promise<Date | null>): Promise<Section<FreshnessData>> {
	// Only the Databend read is cached; the Postgres side is read fresh so the lag is never a minute stale.
	const db = await section(input, 'freshness', (c) => m.latestReceivedAt(c));
	if (db.status !== 'ok') return db;
	const pgAt = await newestPostgresOccurrence().catch(() => null);
	const dbAt = db.data;
	const lag = dbAt && pgAt ? Math.max(0, Math.round((pgAt.getTime() - dbAt.getTime()) / 60_000)) : null;
	return { status: 'ok', data: { analyticsAsOf: dbAt?.toISOString() ?? null, lagMinutes: lag, warn: lag !== null && lag > 10 } };
}

/** Reads a project id off a request param, or 'all'. */
export function coerceProject(v: string | null, accessible: ProjectInfo[]): string {
	return v && isUuid(v) && accessible.some((p) => p.id === v) ? v : 'all';
}

export { accessPredicate };
