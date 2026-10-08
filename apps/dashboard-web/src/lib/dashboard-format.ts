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
