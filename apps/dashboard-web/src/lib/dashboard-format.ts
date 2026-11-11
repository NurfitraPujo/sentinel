import { formatTagParam } from './tag-filter';

/**
 * sentinel-reporting-and-dashboard D13/D19/D12: how the dashboard writes numbers, names the five D19 groups, and links a
 * count to the issue list. Pure and client-safe; every component goes through here so a component test asserts one literal.
 */

/** D12: the dashboard's ranges, default `7d`. Client-safe here; the server's literals.ts re-exports it. */
export const RANGES = ['24h', '7d', '28d', '90d'] as const;
export type Range = (typeof RANGES)[number];

export type GroupKey = 'api' | 'jobs' | 'nats' | 'other' | 'unattributed';
export const GROUP_KEYS: readonly GroupKey[] = ['api', 'jobs', 'nats', 'other', 'unattributed'];

/** D23: group 4 is "Other (outside a request)" on the page, never "background". The key `other` does not change. */
export const GROUP_LABELS: Record<GroupKey, string> = {
	api: 'API',
	jobs: 'Jobs',
	nats: 'NATS',
	other: 'Other (outside a request)',
	unattributed: 'Unattributed',
};

/** The range as the SLO table's column header names it. */
export const RANGE_LABELS: Record<Range, string> = {
	'24h': 'Last 24 hours',
	'7d': 'Last 7 days',
	'28d': 'Last 28 days',
	'90d': 'Last 90 days',
};

/** D12: how the events chart is split. `group` (default) is the D19 origin; `service` works on every project selection. */
export const SPLITS = ['group', 'service'] as const;
export type Split = (typeof SPLITS)[number];
export const SPLIT_LABELS: Record<Split, string> = { group: 'By origin', service: 'By service' };
export const coerceSplit = (v: string | null | undefined): Split => ((SPLITS as readonly string[]).includes(v ?? '') ? (v as Split) : 'group');

export const count = (n: number): string => Number(n).toLocaleString('en-US');

/** `0.9812` → `98.12%`. null → `no data`. */
export function pct(v: number | null | undefined, digits = 2): string {
	return v === null || v === undefined ? 'no data' : `${(v * 100).toFixed(digits)}%`;
}

/** Errors per 1k: one decimal below 10, whole above. null → `no data` (core-nats before core ships its counter). */
export function perK(v: number | null | undefined): string {
	if (v === null || v === undefined) return 'no data';
	return v < 10 ? v.toFixed(1) : Math.round(v).toLocaleString('en-US');
}

/** Seconds as the two most significant units: `45s`, `12m`, `3h 20m`, `2d 4h`. null → `no data`. */
export function duration(seconds: number | null | undefined): string {
	if (seconds === null || seconds === undefined || !Number.isFinite(seconds)) return 'no data';
	const s = Math.max(0, Math.round(seconds));
	if (s < 60) return `${s}s`;
	const m = Math.floor(s / 60);
	if (m < 60) return `${m}m`;
	const h = Math.floor(m / 60);
	if (h < 24) return `${h}h ${m % 60}m`;
	const d = Math.floor(h / 24);
	return `${d}d ${h % 24}h`;
}

export interface Delta {
	label: string;
	/** 'up' | 'down' | 'flat' | 'new' */
	direction: 'up' | 'down' | 'flat' | 'new';
}

/** The change against the previous period of equal length. */
export function delta(current: number, previous: number): Delta {
	if (current === previous) return { label: 'no change', direction: 'flat' };
	if (previous === 0) return { label: 'new', direction: 'new' };
	const p = ((current - previous) / previous) * 100;
	return { label: `${p > 0 ? '+' : '−'}${Math.abs(p).toFixed(Math.abs(p) < 10 ? 1 : 0)}%`, direction: p > 0 ? 'up' : 'down' };
}

/** A burn rate as `0.2×`; null (low traffic or no target) is an em dash. */
export function burn(v: number | null | undefined): string {
	return v === null || v === undefined ? '—' : `${v < 10 ? v.toFixed(1) : Math.round(v)}×`;
}

/** D13: the state a change in an error count deserves. An increase ≥ 5 % is bad, a decrease ≥ 5 % is good, anything smaller is neutral. */
export const DELTA_STATE_THRESHOLD = 0.05;
export function deltaState(current: number, previous: number): 'warn' | 'ok' | undefined {
	if (current === previous) return undefined;
	if (previous === 0) return current > 0 ? 'warn' : undefined; // from nothing to something
	const change = (current - previous) / previous;
	if (change >= DELTA_STATE_THRESHOLD) return 'warn';
	if (change <= -DELTA_STATE_THRESHOLD) return 'ok';
	return undefined;
}

/** D14: the backlog tile's sub-line. */
export function backlogSub(b: { open: number; unassigned: number; olderThan30d: number }): string {
	const lead = b.open > 0 && b.unassigned === b.open ? 'none assigned' : `${count(b.unassigned)} unassigned`;
	return `${lead} · ${count(b.olderThan30d)} older than 30 days`;
}

export interface DashboardFilters {
	project: string;
	environment: string;
	range: string;
	split?: Split | string;
}

/** `/` with the dashboard's own filters; defaults (all, all, 7d, group) are left out so the bare URL stays bare. */
export function dashboardHref(filters: DashboardFilters, patch: Partial<DashboardFilters> = {}): string {
	const project = patch.project ?? filters.project;
	// a project change drops the environment (it is per project)
	const environment = patch.project !== undefined && patch.project !== filters.project ? 'all' : (patch.environment ?? filters.environment);
	const range = patch.range ?? filters.range;
	const split = patch.split ?? filters.split ?? 'group';
	const q = new URLSearchParams();
	if (project !== 'all') q.set('project', project);
	if (project !== 'all' && environment !== 'all') q.set('environment', environment);
	if (range !== '7d') q.set('range', range);
	if (split !== 'group') q.set('split', split);
	const out = q.toString();
	return out ? `/?${out}` : '/';
}

/** One series of the events chart. `tone` picks its palette slot so a group keeps its colour whatever else is drawn. */
export interface EventSeries {
	key: string;
	label: string;
	tone: number;
	/** The /issues filter this series maps to: a D19 group (`group:api`, `group:jobs`) or a service name. */
	filter: { kind: 'group'; group: GroupKey } | { kind: 'service'; service: string } | null;
}

export interface EventRowLike {
	bucket: string;
	group: string;
	service: string | null;
	n: number;
}

/** D12: at most this many services get their own series on the service split; the rest are "Other services". */
export const MAX_SERVICE_SERIES = 8;
export const OTHER_SERVICES_KEY = '__other';
export const UNATTRIBUTED_KEY = 'unattributed';
/** The palette slot of the neutral series (unattributed, other services). */
export const NEUTRAL_TONE = 9;
/** The slot of "Other services": the first one past the top-8 slots 0-7, so it never shares a colour with Unattributed. */
export const OTHER_SERVICES_TONE = MAX_SERVICE_SERIES;

/** Series and the key each row lands in. By origin: the five D19 groups. By service: top 8 by events in range, the rest "Other services", no service "Unattributed". */
export function stackEvents(rows: readonly EventRowLike[], split: Split): { series: EventSeries[]; keyOf: (r: EventRowLike) => string } {
	if (split === 'group') {
		const tone: Record<GroupKey, number> = { api: 0, jobs: 1, nats: 2, other: 3, unattributed: NEUTRAL_TONE };
		return {
			series: GROUP_KEYS.map((g) => ({ key: g as string, label: GROUP_LABELS[g], tone: tone[g], filter: g === 'api' || g === 'jobs' ? { kind: 'group' as const, group: g } : null })),
			keyOf: (r) => r.group,
		};
	}
	const totals = new Map<string, number>();
	for (const r of rows) if (r.service) totals.set(r.service, (totals.get(r.service) ?? 0) + r.n);
	const top = [...totals.entries()].sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0])).slice(0, MAX_SERVICE_SERIES).map(([k]) => k);
	const series: EventSeries[] = top.map((k, i) => ({ key: k, label: k, tone: i, filter: { kind: 'service', service: k } }));
	if (totals.size > top.length) series.push({ key: OTHER_SERVICES_KEY, label: 'Other services', tone: OTHER_SERVICES_TONE, filter: null });
	if (rows.some((r) => !r.service && r.n > 0)) series.push({ key: UNATTRIBUTED_KEY, label: 'Unattributed', tone: NEUTRAL_TONE, filter: null });
	return { series, keyOf: (r) => (!r.service ? UNATTRIBUTED_KEY : top.includes(r.service) ? r.service : OTHER_SERVICES_KEY) };
}

/** A Databend bucket start (`2026-10-20 10:00:00`, UTC) as an ISO instant. */
export const bucketIso = (start: string): string => new Date(/Z|[+-]\d\d:?\d\d$/.test(start) ? start : start.replace(' ', 'T') + 'Z').toISOString();

/**
 * D12: a bar opens the issues seen in its bucket: `seen=all&seen_from&seen_to` plus the current project/environment and the
 * series' own filter where /issues has one (`group:api`, `group:jobs` through listHref; a real service name as `service`).
 */
export function eventsBarHref(opts: { scope: Scope; series: EventSeries; bucketStart: string; bucketMs: number; apiService?: string }): string {
	const { scope, series, bucketStart, bucketMs, apiService } = opts;
	const from = bucketIso(bucketStart);
	const to = new Date(new Date(from).getTime() + bucketMs).toISOString();
	let base: string | null = null;
	if (series.filter?.kind === 'group') base = listHref(`group:${series.filter.group}`, scope, { apiService });
	let path = base ?? `/issues${scopeQuery(scope)}`;
	if (series.filter?.kind === 'service') path = `${path}${path.includes('?') ? '&' : '?'}service=${encodeURIComponent(series.filter.service)}`;
	return `${path}${path.includes('?') ? '&' : '?'}seen=all&seen_from=${encodeURIComponent(from)}&seen_to=${encodeURIComponent(to)}`;
}

function scopeQuery(scope: Scope): string {
	const q: string[] = [];
	if (scope.project && scope.project !== 'all') {
		q.push(`project=${encodeURIComponent(scope.project)}`);
		if (scope.environment && scope.environment !== 'all') q.push(`environment=${encodeURIComponent(scope.environment)}`);
	}
	return q.length ? `?${q.join('&')}` : '';
}

export interface Scope {
	project?: string;
	environment?: string;
}

/**
 * D12: "every count links to /issues with the matching filter, wherever the list supports one". Returns null where the
 * list has no filter that means the same thing (NATS is two tag sources; Other and Unattributed are defined by absence).
 * `apiService` is the http SLI's service name; without one there is no filter for the API group.
 */
export function listHref(kind: string, scope: Scope, extra: { apiService?: string } = {}): string | null {
	const q: string[] = [];
	const add = (k: string, v: string) => q.push(`${k}=${encodeURIComponent(v)}`);
	if (scope.project && scope.project !== 'all') {
		add('project', scope.project);
		if (scope.environment && scope.environment !== 'all') add('environment', scope.environment);
	}
	switch (kind) {
		case 'group:api':
			if (!extra.apiService) return null;
			add('service', extra.apiService);
			break;
		case 'group:jobs':
			q.push(`tag=${encodeURIComponent(formatTagParam('source', 'job'))}`);
			break;
		case 'regressions':
			q.push('regressed=1');
			break;
		case 'open':
			add('status', 'unresolved');
			break;
		case 'unassigned':
			add('status', 'unresolved');
			add('claim', 'unclaimed');
			break;
		case 'reports':
			add('status', 'unresolved');
			add('type', 'user_report');
			break;
		case 'new24h':
			add('new', '24h');
			break;
		default:
			return null;
	}
	return `/issues?${q.join('&')}`;
}
