import type { ServiceSli } from '../sli/config';
import { type AccessFilter } from './access';
import type { Run } from './databend';
import * as m from './metrics';

/**
 * sentinel-reporting-and-dashboard D20/D21: every D10 metric the page shows, re-exported as Prometheus gauges.
 *
 * This is computed by the SAME functions as the page (metrics.ts), so the two cannot drift. It is cross-project by
 * design (the endpoint is token-gated, D20), so it runs with a scope per project that grants every environment.
 *
 * D21 label bounds: `project` is the slugified Postgres name (projects are not in Databend, D2); `environment` is a
 * declared project_environments name or `other`; `service` is at most 30 distinct per project, the rest `other`
 * (counted); `release` appears only on the two-rank release gauges; never an issue, user or message label.
 */

export const WINDOWS = [
	{ label: '24h', ms: 24 * 3_600_000 },
	{ label: '7d', ms: 7 * 24 * 3_600_000 },
	{ label: '28d', ms: 28 * 24 * 3_600_000 },
] as const;

export const SERVICE_LABEL_LIMIT = 30;

export interface InsightsProject {
	id: string;
	name: string;
	/** project_environments names declared for the project. */
	environments: string[];
}

export interface InsightsDeps {
	run: Run;
	projects: InsightsProject[];
	slis: ServiceSli[];
	/** The environment this deployment's sli_hourly rows carry (from SLI_CLUSTER). null = no SLI gauges. */
	sliEnvironment: string | null;
	now: Date;
}

export interface Sample {
	name: string;
	labels: Record<string, string>;
	value: number;
}

export interface InsightsResult {
	samples: Sample[];
	serviceLabelOverflow: number;
}

export function slugify(name: string): string {
	return name.toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '') || 'unknown';
}

export async function computeInsights(d: InsightsDeps): Promise<InsightsResult> {
	const samples: Sample[] = [];
	const projectById = new Map(d.projects.map((p) => [p.id, p]));
	const label = (projectId: string) => slugify(projectById.get(projectId)?.name ?? 'unknown');
	const envLabel = (projectId: string, env: string) => (projectById.get(projectId)?.environments.includes(env) ? env : 'other');
	const add = (name: string, labels: Record<string, string>, value: number | null | undefined) => {
		if (value === null || value === undefined || !Number.isFinite(value)) return;
		samples.push({ name, labels, value });
	};

	// cross-project scope: every project, every environment. knownEnvironments is every name the rows may carry.
	const knownEnvironments = [...new Set(d.projects.flatMap((p) => p.environments).concat(['production', 'staging']))];
	const access: AccessFilter = { scopes: d.projects.map((p) => ({ projectId: p.id, environments: null })), environment: 'all', knownEnvironments };
	// Environment names in the data are free text, and the allow-list for a literal is `knownEnvironments`. Real rows can
	// carry an undeclared name, so the cross-project query is not filtered by environment name at all (environment 'all').
	const c: m.MetricCtx = { run: d.run, access, httpServices: d.slis.filter((s) => s.kind === 'http' && s.service).map((s) => s.service!) };

	// ---- additive metrics by (project, environment), per window
	// Collapsing environments that map to the same label ("other") sums counts; quantiles take the larger sample.
	type PE = { projectId: string; environment: string };
	const SEP = '\u0001';
	const key = (r: PE) => `${r.projectId}${SEP}${envLabel(r.projectId, r.environment)}`;
	const pairs = new Map<string, { project: string; environment: string }>();
	const touch = (r: PE) => pairs.set(key(r), { project: label(r.projectId), environment: envLabel(r.projectId, r.environment) });

	// service label bounding (D21): order by 28-day volume so the busiest services keep their own label.
	const serviceTotals = new Map<string, Map<string, number>>();
	let overflow = 0;
	const windowEvents = new Map<string, m.EventRow[]>();

	for (const w of WINDOWS) {
		const win: m.Window = { from: new Date(d.now.getTime() - w.ms), to: d.now };
		const [ev, ni, rg, rs, mt, mr, cf] = await Promise.all([
			m.errorEvents(c, win), m.newIssues(c, win), m.regressions(c, win), m.resolutions(c, win),
			m.mtta(c, win, true), m.mttr(c, win, true), m.changeFailure(c, win),
		]);
		windowEvents.set(w.label, ev);
		for (const r of ev) {
			touch(r);
			if (r.service) {
				const t = serviceTotals.get(r.projectId) ?? new Map<string, number>();
				t.set(r.service, (t.get(r.service) ?? 0) + r.n);
				serviceTotals.set(r.projectId, t);
			}
		}
		// groups, summed per label pair; zero-filled so a quiet group is a 0 rather than a missing series
		const groups = new Map<string, Record<m.Group, number>>();
		for (const r of ev) {
			const k = key(r);
			const g = groups.get(k) ?? { api: 0, jobs: 0, nats: 0, other: 0, unattributed: 0 };
			g[r.group] += r.n;
			groups.set(k, g);
		}
		for (const [k, g] of groups) {
			const p = pairs.get(k)!;
			for (const grp of m.GROUPS) add('sentinel_insight_error_events', { project: p.project, environment: p.environment, group: grp, window: w.label }, g[grp]);
		}
		const sumBy = (rows: m.CountRow[]) => {
			const out = new Map<string, number>();
			for (const r of rows) {
				touch(r);
				out.set(key(r), (out.get(key(r)) ?? 0) + r.n);
			}
			return out;
		};
		const emit = (name: string, rows: m.CountRow[]) => {
			for (const [k, v] of sumBy(rows)) {
				const p = pairs.get(k)!;
				add(name, { project: p.project, environment: p.environment, window: w.label }, v);
			}
		};
		emit('sentinel_insight_issues_new', ni);
		emit('sentinel_insight_regressions', rg);
		emit('sentinel_insight_resolutions', rs);
		// quantiles: collapsed environments keep the larger sample
		const emitQ = (name: string, rows: m.QuantileRow[]) => {
			const best = new Map<string, m.QuantileRow>();
			for (const r of rows) {
				const k = key({ projectId: r.projectId!, environment: r.environment! });
				touch({ projectId: r.projectId!, environment: r.environment! });
				const cur = best.get(k);
				if (!cur || r.count > cur.count) best.set(k, r);
			}
			for (const [k, r] of best) {
				const p = pairs.get(k)!;
				add(name, { project: p.project, environment: p.environment, quantile: '0.5', window: w.label }, r.p50);
				add(name, { project: p.project, environment: p.environment, quantile: '0.9', window: w.label }, r.p90);
			}
		};
		emitQ('sentinel_insight_mtta_seconds', mt);
		emitQ('sentinel_insight_mttr_seconds', mr);
		// regression rate = regressions ÷ resolutions; change failure = failed ÷ releases started in the window
		const rgBy = sumBy(rg), rsBy = sumBy(rs);
		for (const [k, v] of rgBy) {
			const rate = m.regressionRate(v, rsBy.get(k) ?? 0);
			const p = pairs.get(k)!;
			add('sentinel_insight_regression_rate', { project: p.project, environment: p.environment, window: w.label }, rate);
		}
		const cfBy = new Map<string, { n: number; failed: number }>();
		for (const r of cf) {
			touch(r);
			const cur = cfBy.get(key(r)) ?? { n: 0, failed: 0 };
			cur.n += r.n;
			cur.failed += r.failed;
			cfBy.set(key(r), cur);
		}
		for (const [k, v] of cfBy) {
			const p = pairs.get(k)!;
			add('sentinel_insight_change_failure_rate', { project: p.project, environment: p.environment, window: w.label }, v.n === 0 ? null : v.failed / v.n);
		}
	}

	// error_events_by_service with the D21 service bound
	const keepService = new Map<string, Set<string>>();
	for (const [pid, totals] of serviceTotals) {
		const ordered = [...totals.entries()].sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]));
		keepService.set(pid, new Set(ordered.slice(0, SERVICE_LABEL_LIMIT).map(([s]) => s)));
		overflow += Math.max(0, ordered.length - SERVICE_LABEL_LIMIT);
	}
	for (const w of WINDOWS) {
		const byService = new Map<string, number>();
		for (const r of windowEvents.get(w.label) ?? []) {
			if (!r.service) continue;
			const svc = keepService.get(r.projectId)?.has(r.service) ? r.service : 'other';
			const k = `${key(r)}${SEP}${svc}`;
			byService.set(k, (byService.get(k) ?? 0) + r.n);
		}
		for (const [k, v] of byService) {
			const [pid, env, svc] = k.split(SEP);
			const p = pairs.get(`${pid}${SEP}${env}`)!;
			add('sentinel_insight_error_events_by_service', { project: p.project, environment: p.environment, service: svc, window: w.label }, v);
		}
	}

	// ---- point-in-time: backlog and reports
	const [backlog, waiting] = await Promise.all([m.openBacklog(c, d.now), m.userReportsWaiting(c)]);
	const openBy = new Map<string, { age: Record<string, number>; unassigned: number }>();
	for (const r of backlog) {
		touch(r);
		const cur = openBy.get(key(r)) ?? { age: Object.fromEntries(m.AGE_BUCKETS.map((a) => [a, 0])), unassigned: 0 };
		cur.age[r.age] += r.n;
		cur.unassigned += r.unassigned;
		openBy.set(key(r), cur);
	}
	for (const [k, v] of openBy) {
		const p = pairs.get(k)!;
		for (const a of m.AGE_BUCKETS) add('sentinel_insight_issues_open', { project: p.project, environment: p.environment, age: a }, v.age[a]);
		add('sentinel_insight_issues_open_unassigned', { project: p.project, environment: p.environment }, v.unassigned);
	}
	const waitBy = new Map<string, number>();
	for (const r of waiting) waitBy.set(label(r.projectId), (waitBy.get(label(r.projectId)) ?? 0) + r.n);
	for (const [project, n] of waitBy) add('sentinel_insight_user_reports_waiting', { project }, n);

	// ---- SLIs
	if (d.sliEnvironment && d.slis.length) {
		const env = d.sliEnvironment;
		const ids = d.slis.map((s) => s.id);
		const budgetWin: m.Window = { from: new Date(d.now.getTime() - 28 * 86_400_000), to: d.now };
		for (const s of d.slis) {
			if (s.availabilityTarget) add('sentinel_insight_sli_target', { sli: s.id, objective: 'availability' }, s.availabilityTarget);
			if (s.latency) add('sentinel_insight_sli_target', { sli: s.id, objective: 'latency' }, s.latency.target);
		}
		for (const w of WINDOWS) {
			const win: m.Window = { from: new Date(d.now.getTime() - w.ms), to: d.now };
			const sums = await m.sliSums(c, ids, win, env);
			for (const s of sums) {
				const sli = d.slis.find((x) => x.id === s.sliId)!;
				add('sentinel_insight_sli_total', { sli: s.sliId, environment: s.environment, window: w.label }, s.total);
				add('sentinel_insight_sli_errors', { sli: s.sliId, environment: s.environment, window: w.label }, s.errors);
				add('sentinel_insight_sli_good_ratio', { sli: s.sliId, environment: s.environment, objective: 'availability', window: w.label }, m.availability(s));
				if (sli.latency) add('sentinel_insight_sli_good_ratio', { sli: s.sliId, environment: s.environment, objective: 'latency', window: w.label }, m.latencyRatio(s));
				const proj = d.projects.find((p) => p.name === sli.project);
				if (proj) {
					const events = await m.sliMatchingEvents(c, sli, proj.id, env, win);
					add('sentinel_insight_errors_per_1k', { sli: s.sliId, environment: s.environment, window: w.label }, m.errorsPer1k(events, s.total));
				}
			}
		}
		for (const s of await m.sliSums(c, d.slis.filter((x) => x.availabilityTarget).map((x) => x.id), budgetWin, env)) {
			const sli = d.slis.find((x) => x.id === s.sliId)!;
			add('sentinel_insight_sli_error_budget_remaining', { sli: s.sliId }, m.errorBudgetRemaining(s, sli.availabilityTarget!));
		}
	}

	// ---- releases: only the two latest windows per project and environment (D9, D20)
	const wins = await m.releaseWindows(c, d.now, 2);
	const byPE = new Map<string, m.ReleaseWindow[]>();
	for (const w of wins) {
		const k = `${w.projectId}|${w.environment}`;
		byPE.set(k, [...(byPE.get(k) ?? []), w]);
	}
	for (const list of byPE.values()) {
		list.sort((a, b) => b.start.getTime() - a.start.getTime());
		const httpSli = d.slis.find((s) => s.kind === 'http' && s.service && d.projects.find((p) => p.name === s.project)?.id === list[0].projectId);
		const hourly = httpSli && d.sliEnvironment
			? await m.sliHourlyRows(c, [httpSli.id], { from: list[list.length - 1].start, to: d.now }, d.sliEnvironment)
			: [];
		list.forEach((w, i) => {
			const rank = i === 0 ? 'current' : 'previous';
			const labels = { project: label(w.projectId), environment: envLabel(w.projectId, w.environment), release: w.version, rank };
			add('sentinel_insight_release_events', labels, w.events.api + w.events.jobs + w.events.nats + w.events.other + w.events.unattributed);
			add('sentinel_insight_release_new_issues', labels, w.newIssues);
			add('sentinel_insight_release_regressions', labels, w.regressions);
			if (httpSli && w.environment === d.sliEnvironment) {
				const denom = hourly.filter((h) => h.hourStart >= w.start && h.hourStart < w.end).reduce((a, h) => a + h.total, 0);
				add('sentinel_insight_release_errors_per_1k', labels, m.errorsPer1k(w.events.api, denom));
			}
		});
	}
	return { samples, serviceLabelOverflow: overflow };
}

// ---------------------------------------------------------------------------------------------------------------
// Exposition

const HELP: Record<string, string> = {
	sentinel_insight_error_events: 'Error events in the window by D19 group (api, jobs, nats, other = outside a request, unattributed)',
	sentinel_insight_error_events_by_service: 'Error events in the window by service (at most 30 per project, the rest other)',
	sentinel_insight_issues_new: 'Issues first seen in the window',
	sentinel_insight_regressions: 'Regressions in the window',
	sentinel_insight_resolutions: 'Issues resolved in the window',
	sentinel_insight_issues_open: 'Open backlog by age of first_seen',
	sentinel_insight_issues_open_unassigned: 'Open backlog with no assignee',
	sentinel_insight_user_reports_waiting: 'User reports waiting (unresolved)',
	sentinel_insight_mtta_seconds: 'Time from first seen to first assigned or claimed, seconds',
	sentinel_insight_mttr_seconds: 'Time from first seen to resolved, seconds',
	sentinel_insight_regression_rate: 'Regressions divided by resolutions in the window',
	sentinel_insight_change_failure_rate: 'Share of releases that introduced an issue reaching 10 events or 3 users within 24 h',
	sentinel_insight_sli_total: 'SLI denominator (requests, job runs or NATS messages) in the window',
	sentinel_insight_sli_errors: 'SLI errors in the window',
	sentinel_insight_sli_good_ratio: 'SLI good ratio by objective (availability, latency)',
	sentinel_insight_sli_target: 'SLI target by objective',
	sentinel_insight_sli_error_budget_remaining: 'Error budget left over 28 days, floored at -1',
	sentinel_insight_errors_per_1k: 'The SLI matching events per 1000 of its denominator',
	sentinel_insight_release_events: 'Error events in the release window (first seen, never deployed)',
	sentinel_insight_release_new_issues: 'New issues introduced in the release window',
	sentinel_insight_release_regressions: 'Regressions in the release window',
	sentinel_insight_release_errors_per_1k: 'API errors per 1000 requests in the release window',
};

const esc = (v: string) => v.replace(/\\/g, '\\\\').replace(/\n/g, '\\n').replace(/"/g, '\\"');

export function renderSamples(samples: Sample[]): string {
	const byName = new Map<string, Sample[]>();
	for (const s of samples) byName.set(s.name, [...(byName.get(s.name) ?? []), s]);
	const out: string[] = [];
	for (const [name, list] of byName) {
		out.push(`# HELP ${name} ${HELP[name] ?? name}`, `# TYPE ${name} gauge`);
		for (const s of list) {
			const labels = Object.entries(s.labels).map(([k, v]) => `${k}="${esc(v)}"`).join(',');
			out.push(`${name}${labels ? `{${labels}}` : ''} ${s.value}`);
		}
	}
	return out.join('\n') + (out.length ? '\n' : '');
}
