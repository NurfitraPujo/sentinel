// @vitest-environment node
import { describe, it, expect, beforeAll } from 'vitest';
import type { Run } from './databend';
import { resetSchema, seed } from './test-support';
import { computeInsights, renderSamples, slugify, type InsightsDeps, type Sample } from './insights';
import { createInsightsState, refresh, render } from './insights-state';
import type { ServiceSli } from '../sli/config';

// sentinel-reporting-and-dashboard D20/D21: the export's samples and label bounds over a REAL Databend, and the
// last-good serving rules with stub dependencies.
const url = process.env.DATABEND_TEST_URL ?? '';
const required = process.env.DATABEND_INTEGRATION_REQUIRED === '1';
let real: Run | null = null;
if (url) {
	try {
		real = await resetSchema(url, 'sentinel_ins');
	} catch (e) {
		if (required) throw e;
	}
}
if (!real && required) throw new Error('DATABEND_INTEGRATION_REQUIRED=1 but no Databend answered');

const P1 = '11111111-1111-1111-1111-111111111111';
const NOW = new Date('2026-10-07T12:00:00Z');
const H = (h: number) => new Date(NOW.getTime() - h * 3_600_000);
const D = (d: number) => new Date(NOW.getTime() - d * 86_400_000);
const slis: ServiceSli[] = [
	{ id: 'core-api', kind: 'http', project: 'Core API', source: 'api_core_80', availabilityTarget: 0.995, latency: { le: '1000.0', target: 0.94 }, service: 'api', windowDays: 28 },
	{ id: 'core-jobs', kind: 'jobs', project: 'Core API', source: 'j', windowDays: 28 },
];
const find = (s: Sample[], name: string, labels: Record<string, string>) =>
	s.find((x) => x.name === name && Object.entries(labels).every(([k, v]) => x.labels[k] === v));

describe.skipIf(!real)('computeInsights over Databend', () => {
	let samples: Sample[];
	let overflow = 0;
	beforeAll(async () => {
		await seed(real!, {
			issues: [
				{ id: 'i-api', projectId: P1, environment: 'production', firstSeen: D(2), firstRelease: 'v2', assignedTo: 'u1' },
				{ id: 'i-dev', projectId: P1, environment: 'scratch-env', firstSeen: D(1) },
			],
			occs: [
				...[0, 1, 2, 3].map((k) => ({ id: `a${k}`, issueId: 'i-api', environment: 'production', createdAt: H(10 + k), release: k < 2 ? 'v2' : 'v1', service: 'api', tagSource: 'http_handler', requestScoped: true, userId: `u${k}` })),
				{ id: 'd0', issueId: 'i-dev', environment: 'scratch-env', createdAt: H(3), service: 'worker', tagSource: 'worker_handler' },
				// 34 more services on the project so the D21 bound has something to bound (36 distinct in total with api + worker)
				...Array.from({ length: 34 }, (_, k) => ({ id: `s${k}`, issueId: 'i-api', environment: 'production', createdAt: H(2), service: `svc-${String(k).padStart(2, '0')}` })),
			],
			activity: [{ issueId: 'i-api', eventType: 'status_changed', createdAt: H(1), newValue: { status: 'resolved' } }],
			sli: [['core-api', 'production', H(3), 1000, 10, 950]],
		});
		const r = await computeInsights({
			run: real!, now: NOW, slis, sliEnvironment: 'production',
			projects: [{ id: P1, name: 'Core API', environments: ['production'] }],
		});
		samples = r.samples;
		overflow = r.serviceLabelOverflow;
	});

	it('project is the slugified name, never the id', () => {
		expect(slugify('Core API')).toBe('core-api');
		expect(samples.every((s) => !('project_id' in s.labels) && s.labels.project !== P1)).toBe(true);
		expect(samples.filter((s) => s.labels.project).every((s) => s.labels.project === 'core-api')).toBe(true);
	});

	it('an undeclared environment is labelled other', () => {
		const envs = new Set(samples.map((s) => s.labels.environment).filter(Boolean));
		expect([...envs].sort()).toEqual(['other', 'production']);
		expect(envs.has('scratch-env')).toBe(false);
	});

	it('error events carry all five D19 groups, zero-filled, per window', () => {
		for (const w of ['24h', '7d', '28d']) {
			const groups = samples.filter((s) => s.name === 'sentinel_insight_error_events' && s.labels.environment === 'production' && s.labels.window === w).map((s) => s.labels.group).sort();
			expect(groups).toEqual(['api', 'jobs', 'nats', 'other', 'unattributed']);
		}
		expect(find(samples, 'sentinel_insight_error_events', { environment: 'production', group: 'api', window: '24h' })!.value).toBe(4);
		expect(find(samples, 'sentinel_insight_error_events', { environment: 'other', group: 'nats', window: '24h' })!.value).toBe(1);
	});

	it('service is bounded to 30 distinct per project; the rest are other and counted', () => {
		const services = new Set(samples.filter((s) => s.name === 'sentinel_insight_error_events_by_service').map((s) => s.labels.service));
		expect(services.size).toBeLessThanOrEqual(31); // 30 + "other"
		expect(services.has('other')).toBe(true);
		expect(services.has('api')).toBe(true); // the busiest keep their own label
		expect(overflow).toBe(36 - 30);
		const total = samples.filter((s) => s.name === 'sentinel_insight_error_events_by_service' && s.labels.window === '28d').reduce((a, s) => a + s.value, 0);
		expect(total).toBe(4 + 1 + 34); // bounding relabels, it never drops events
	});

	it('SLI gauges: totals, good ratios by objective, targets, budget, errors per 1k', () => {
		expect(find(samples, 'sentinel_insight_sli_total', { sli: 'core-api', window: '24h' })!.value).toBe(1000);
		expect(find(samples, 'sentinel_insight_sli_good_ratio', { sli: 'core-api', objective: 'availability', window: '24h' })!.value).toBeCloseTo(0.99);
		expect(find(samples, 'sentinel_insight_sli_good_ratio', { sli: 'core-api', objective: 'latency', window: '24h' })!.value).toBeCloseTo(0.95);
		expect(find(samples, 'sentinel_insight_sli_target', { sli: 'core-api', objective: 'availability' })!.value).toBe(0.995);
		expect(find(samples, 'sentinel_insight_sli_error_budget_remaining', { sli: 'core-api' })!.value).toBeCloseTo(1 - 0.01 / 0.005);
		expect(find(samples, 'sentinel_insight_errors_per_1k', { sli: 'core-api', window: '24h' })!.value).toBe(4);
		// core-jobs has no sli_hourly rows: no sample, never a fabricated zero
		expect(find(samples, 'sentinel_insight_sli_total', { sli: 'core-jobs' })).toBeUndefined();
	});

	it('release gauges exist for only the two latest windows, labelled by rank', () => {
		const rel = samples.filter((s) => s.name === 'sentinel_insight_release_events');
		expect(rel.map((s) => s.labels.rank).sort()).toEqual(['current', 'previous']);
		expect(rel.every((s) => ['v1', 'v2'].includes(s.labels.release))).toBe(true);
		expect(samples.filter((s) => s.labels.release).every((s) => s.name.startsWith('sentinel_insight_release_'))).toBe(true);
	});

	it('exposition is valid Prometheus text with escaped labels', () => {
		const text = renderSamples([{ name: 'sentinel_insight_x', labels: { a: 'q"uo\\te\nnl' }, value: 1 }]);
		expect(text).toBe('# HELP sentinel_insight_x sentinel_insight_x\n# TYPE sentinel_insight_x gauge\nsentinel_insight_x{a="q\\"uo\\\\te\\nnl"} 1\n');
		expect(renderSamples(samples)).toMatch(/^# HELP sentinel_insight_error_events /);
	});
});

describe('refresh keeps the last good values (D20)', () => {
	const okDeps = (value: string): (() => Promise<InsightsDeps>) => async () => ({
		run: async () => [], projects: [], slis: [], sliEnvironment: null, now: NOW, ...({ value } as object),
	});

	it('a failed refresh serves the previous values and counts the error; a good one stamps the success time', async () => {
		const state = createInsightsState();
		await refresh(state, okDeps('a'));
		expect(state.lastSuccess).not.toBeNull();
		state.body = 'sentinel_insight_marker 1\n'; // what the last good refresh produced
		const before = state.lastSuccess;
		await refresh(state, async () => { throw new Error('databend down'); });
		expect(state.errors).toBe(1);
		expect(state.body).toBe('sentinel_insight_marker 1\n'); // unchanged
		expect(state.lastSuccess).toBe(before);
		const text = render(state);
		expect(text).toContain('sentinel_insight_marker 1');
		expect(text).toContain('sentinel_insights_refresh_errors_total 1');
		expect(text).toContain('sentinel_insights_last_success_timestamp_seconds');
	});

	it('with no successful refresh yet there is no last-success series, only the error counter', async () => {
		const state = createInsightsState();
		await refresh(state, async () => { throw new Error('x'); });
		const text = render(state);
		expect(text).toContain('sentinel_insights_refresh_errors_total 1');
		expect(text).not.toContain('last_success_timestamp');
	});

	it('concurrent refreshes share one run', async () => {
		const state = createInsightsState();
		let runs = 0;
		const deps = async () => { runs++; return { run: async () => [], projects: [], slis: [], sliEnvironment: null, now: NOW } as InsightsDeps; };
		await Promise.all([refresh(state, deps), refresh(state, deps)]);
		expect(runs).toBe(1);
	});
});
