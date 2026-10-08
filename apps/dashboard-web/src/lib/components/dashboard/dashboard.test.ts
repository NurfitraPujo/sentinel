import { describe, it, expect } from 'vitest';
import { render, screen, within } from '@testing-library/svelte';
import HealthRow from './HealthRow.svelte';
import StackedBars from './StackedBars.svelte';
import SliChart from './SliChart.svelte';
import EventsTimeline from './EventsTimeline.svelte';
import ReleaseTable from './ReleaseTable.svelte';
import BacklogSection from './BacklogSection.svelte';
import ResponseSection from './ResponseSection.svelte';
import FilterBar from './FilterBar.svelte';
import FreshnessLine from './FreshnessLine.svelte';
import { delta, duration, listHref, perK, pct, GROUP_LABELS } from '$lib/dashboard-format';

// sentinel-reporting-and-dashboard D17 (fe): the sections, the D19 groups, D23's label, and the accessible table every
// chart carries. The data shapes are the ones lib/server/analytics/service.ts returns.
const groups = (o: Partial<Record<string, number>> = {}) => ({ api: 0, jobs: 0, nats: 0, other: 0, unattributed: 0, ...o });
const sliTile = (o: Record<string, unknown> = {}) => ({
	id: 'core-api', kind: 'http', project: 'core', availability: 0.98, availabilityTarget: 0.995, latency: 0.95, latencyTarget: 0.94,
	budgetRemaining: -1, total: 1000, errors: 20, errorsPer1k: 5, events: 5, ...o,
});
const health = (o: Record<string, unknown> = {}) => ({
	events: { current: groups({ api: 5, jobs: 3, other: 2, unattributed: 1 }), previous: groups({ api: 2, jobs: 3 }) },
	byLevel: {}, newIssues: { current: 4, previous: 2 }, regressions: { current: 1, previous: 1 }, backlog: { open: 12, unassigned: 9 },
	slis: [sliTile(), sliTile({ id: 'core-jobs', kind: 'jobs', availabilityTarget: null, latencyTarget: null, availability: 0.99, latency: null, budgetRemaining: null, total: 100, errors: 3, errorsPer1k: 30, events: 3 }),
		sliTile({ id: 'core-nats', kind: 'nats', availabilityTarget: null, latencyTarget: null, availability: null, latency: null, budgetRemaining: null, total: 0, errors: 0, errorsPer1k: null, events: null })],
	...o,
}) as never;
const FILTERS = { project: 'all', environment: 'all', range: '7d' };

describe('dashboard-format', () => {
	it('names group 4 "Other (outside a request)", never background (D23)', () => {
		expect(GROUP_LABELS.other).toBe('Other (outside a request)');
		expect(Object.values(GROUP_LABELS).join(' ').toLowerCase()).not.toContain('background');
	});
	it('writes numbers one way', () => {
		expect(pct(0.98123)).toBe('98.12%');
		expect(pct(null)).toBe('no data');
		expect(perK(3.456)).toBe('3.5');
		expect(perK(1234.5)).toBe('1,235');
		expect(perK(null)).toBe('no data');
		expect(duration(45)).toBe('45s');
		expect(duration(3 * 3600 + 20 * 60)).toBe('3h 20m');
		expect(duration(2 * 86400 + 4 * 3600)).toBe('2d 4h');
		expect(duration(null)).toBe('no data');
		expect(delta(5, 2).label).toBe('+150%');
		expect(delta(2, 4)).toMatchObject({ label: '−50%', direction: 'down' });
		expect(delta(0, 0).direction).toBe('flat');
		expect(delta(3, 0).direction).toBe('new');
	});
	it('links a count to /issues only where the list has a matching filter (H8)', () => {
		expect(listHref('group:jobs', {})).toBe('/issues?tag=source%3Ajob');
		expect(listHref('group:api', { project: 'p1' }, { apiService: 'api' })).toBe('/issues?project=p1&service=api');
		expect(listHref('group:api', {})).toBeNull(); // no http SLI service configured: no filter means the same
		expect(listHref('group:nats', {})).toBeNull();
		expect(listHref('group:other', {})).toBeNull();
		expect(listHref('regressions', { project: 'p1', environment: 'staging' })).toBe('/issues?project=p1&environment=staging&regressed=1');
		expect(listHref('regressions', { project: 'all', environment: 'staging' })).toBe('/issues?regressed=1'); // environment is per project
		expect(listHref('unassigned', {})).toBe('/issues?status=unresolved&claim=unclaimed');
		expect(listHref('reports', {})).toBe('/issues?status=unresolved&type=user_report');
	});
});

describe('HealthRow (H2, H3, H4)', () => {
	it('shows the five D19 groups apart, each against the previous period', () => {
		render(HealthRow, { data: health(), filters: FILTERS, apiService: 'api' });
		const labels = ['API', 'Jobs', 'NATS', 'Other (outside a request)', 'Unattributed'];
		for (const [i, g] of (['api', 'jobs', 'nats', 'other', 'unattributed'] as const).entries()) {
			const tile = screen.getByTestId(`group-${g}`);
			expect(tile.textContent).toContain(labels[i]);
		}
		expect(screen.getByTestId('group-api').textContent).toContain('5');
		expect(screen.getByTestId('group-api').textContent).toContain('+150% vs previous period');
		expect(screen.getByTestId('group-jobs').textContent).toContain('no change vs previous period');
		expect(screen.getByTestId('group-total').textContent).toContain('11');
		expect(document.body.textContent!.toLowerCase()).not.toContain('background');
	});

	it('shows availability vs target with budget left, and latency vs target, for an SLI with a target', () => {
		render(HealthRow, { data: health(), filters: FILTERS });
		const avail = screen.getByTestId('sli-availability-core-api');
		expect(avail.textContent).toContain('98.00%');
		expect(avail.textContent).toContain('target 99.5%');
		expect(avail.textContent).toContain('budget left -100%');
		expect(avail.className).toContain('state-bad'); // below target
		const lat = screen.getByTestId('sli-latency-core-api');
		expect(lat.textContent).toContain('95.00%');
		expect(lat.className).toContain('state-ok'); // above its 94% target
		// the untargeted rates get no SLI tile
		expect(screen.queryByTestId('sli-tile-core-jobs')).toBeNull();
		expect(screen.getByTestId('sli-note').textContent).toContain('long-lived streams');
	});

	it('shows errors per 1k for API, jobs and NATS, and "no data" for core-nats before its counter ships', () => {
		render(HealthRow, { data: health(), filters: FILTERS });
		expect(screen.getByTestId('per1k-api').textContent).toContain('5.0');
		expect(screen.getByTestId('per1k-jobs').textContent).toContain('30');
		expect(screen.getByTestId('per1k-nats').textContent).toContain('no data');
	});

	it('counts link into /issues where a filter exists and carry the project scope', () => {
		render(HealthRow, { data: health(), filters: { project: 'p1', environment: 'production', range: '24h' }, apiService: 'api' });
		const href = (id: string) => within(screen.getByTestId(id)).getByRole('link').getAttribute('href');
		expect(href('group-jobs')).toBe('/issues?project=p1&environment=production&tag=source%3Ajob');
		expect(href('group-api')).toBe('/issues?project=p1&environment=production&service=api');
		expect(href('tile-regressions')).toBe('/issues?project=p1&environment=production&regressed=1');
		expect(href('tile-backlog')).toBe('/issues?project=p1&environment=production&status=unresolved');
		expect(href('tile-new-issues')).toBe('/issues?project=p1&environment=production&new=24h');
		expect(within(screen.getByTestId('group-nats')).queryByRole('link')).toBeNull();
		expect(within(screen.getByTestId('group-other')).queryByRole('link')).toBeNull();
	});

	it('shows no SLI tiles for a viewer with none visible, and still shows the rest', () => {
		render(HealthRow, { data: health({ slis: [] }), filters: FILTERS });
		expect(screen.queryByTestId('sli-tiles')).toBeNull();
		expect(screen.getByTestId('per1k-api').textContent).toContain('no data');
		expect(screen.getByTestId('tile-backlog').textContent).toContain('12');
	});
});

describe('charts (keyboard and screen-reader parity)', () => {
	const series = [{ key: 'a', label: 'API' }, { key: 'j', label: 'Jobs' }];
	const buckets = [{ start: '2026-10-07 10:00:00', values: { a: 3, j: 1 } }, { start: '2026-10-07 11:00:00', values: { a: 0, j: 2 } }];

	it('StackedBars draws a bar per non-zero value with a <title> tooltip and carries every number in a hidden table', () => {
		render(StackedBars, { heading: 'Error events', series, buckets, testId: 'c' });
		expect(screen.getAllByTestId('c-bar')).toHaveLength(3);
		expect(screen.getAllByTestId('c-bar')[0].querySelector('title')!.textContent).toContain('API: 3');
		const table = screen.getByTestId('c-table');
		expect(within(table).getAllByRole('row')).toHaveLength(3); // header + one per bucket
		expect(table.textContent).toContain('Jobs');
		expect(screen.getByTestId('c').querySelector('svg')!.getAttribute('aria-hidden')).toBe('true');
		expect(screen.getByTestId('c').textContent).toContain('Error events: 6');
	});

	it('StackedBars says so when there is no data, and lists release markers in the table caption', () => {
		const { unmount } = render(StackedBars, { heading: 'E', series, buckets: [], testId: 'e' });
		expect(screen.getByTestId('e-empty')).toBeTruthy();
		unmount();
		render(StackedBars, { heading: 'E', series, buckets, markers: [{ start: buckets[1].start, label: 'v2.0.1' }], testId: 'm' });
		expect(screen.getAllByTestId('m-marker')).toHaveLength(1);
		expect(screen.getByTestId('m-table').querySelector('caption')!.textContent).toContain('first seen: v2.0.1');
	});

	it('grouped mode puts the series side by side instead of stacking', () => {
		render(StackedBars, { heading: 'W', series, buckets: [{ start: '2026-10-05', values: { a: 4, j: 4 } }], mode: 'grouped', testId: 'g' });
		const xs = screen.getAllByTestId('g-bar').map((r) => Number(r.getAttribute('x')));
		expect(new Set(xs).size).toBe(2);
	});

	it('SliChart draws bars, the good-ratio line and the target, with a hidden table of the same numbers', () => {
		render(SliChart, { id: 'core-api', goal: 0.995, testId: 's', points: [{ bucket: '2026-10-07 10:00:00', total: 1000, errors: 5 }, { bucket: '2026-10-07 11:00:00', total: 0, errors: 0 }, { bucket: '2026-10-07 12:00:00', total: 500, errors: 1 }] });
		expect(screen.getByTestId('s-line')).toBeTruthy();
		expect(screen.getByTestId('s-target')).toBeTruthy();
		const rows = within(screen.getByTestId('s-table')).getAllByRole('row');
		expect(rows[1].textContent).toContain('99.50%');
		expect(rows[2].textContent).toContain('no data'); // a quiet hour is a gap in the line, never a 100%
	});

	it('EventsTimeline stacks by D19 group, and by service when one project is selected', () => {
		const byGroup = [{ bucket: '2026-10-07 10:00:00', group: 'api', service: 'api', n: 3 }, { bucket: '2026-10-07 10:00:00', group: 'jobs', service: 'jobs worker', n: 2 }, { bucket: '2026-10-07 10:00:00', group: 'unattributed', service: null, n: 1 }];
		const a = render(EventsTimeline, { data: { bucket: 'hour', byGroup, byService: false, releaseMarkers: [] } as never });
		expect(screen.getByTestId('timeline').textContent).toContain('Error events by group');
		expect(screen.getByTestId('timeline-legend').textContent).toContain('Other (outside a request)');
		a.unmount();
		render(EventsTimeline, { data: { bucket: 'hour', byGroup, byService: true, releaseMarkers: [{ version: 'v2', at: '2026-10-07T10:30:00.000Z' }] } as never });
		expect(screen.getByTestId('timeline').textContent).toContain('Error events by service');
		expect(screen.getByTestId('timeline-legend').textContent).toContain('jobs worker');
		expect(screen.getAllByTestId('timeline-marker')).toHaveLength(1);
	});
});

describe('ReleaseTable (H6)', () => {
	const row = (o = {}) => ({ version: 'v2.0.1', environment: 'production', firstSeen: '2026-10-07T10:00:00.000Z', events: groups({ api: 5, jobs: 1 }), newIssues: 2, regressions: 1, errorsPer1k: 3.2, change: 1.5, ...o });
	it('says "First seen", never "deployed", with the group columns and the change from the previous release', () => {
		render(ReleaseTable, { rows: [row(), row({ version: 'v2.0.0', change: null, errorsPer1k: null })] as never });
		const table = screen.getByTestId('release-table');
		expect(table.textContent).toContain('First seen');
		expect(table.textContent!.toLowerCase()).not.toContain('deployed');
		const rows = screen.getAllByTestId('release-row');
		expect(rows[0].textContent).toContain('v2.0.1');
		expect(screen.getAllByTestId('release-change')[0].textContent).toBe('+1.5 per 1k');
		expect(screen.getAllByTestId('release-change')[1].textContent).toBe('—');
	});
	it('falls back to an events change when there is no rate', () => {
		render(ReleaseTable, { rows: [row({ errorsPer1k: null, change: -4 })] as never });
		expect(screen.getByTestId('release-change').textContent).toBe('−4 events');
	});
});

describe('BacklogSection and ResponseSection (H7)', () => {
	const backlog = { weekly: [{ week: '2026-09-28', opened: 4, resolved: 2 }], age: { '0-7d': 3, '7-30d': 2, '30-90d': 0, '90d+': 1 }, open: 6, unassigned: 4,
		top: [{ id: 'iss-1', projectId: 'p', environment: 'production', message: 'boom', errorClass: 'TypeError', users: 7, events: 20 }] };
	it('shows age buckets, the unassigned share with a link, and top issues linking to their detail', () => {
		render(BacklogSection, { data: backlog as never, filters: { project: 'all', environment: 'all' } });
		expect(screen.getByTestId('age-0-7d').textContent).toContain('3');
		expect(screen.getByTestId('age-30-90d').textContent).toContain('0');
		expect(screen.getByTestId('unassigned-share').textContent).toContain('4 unassigned of 6 open (67%)');
		expect(within(screen.getByTestId('unassigned-share')).getByRole('link').getAttribute('href')).toBe('/issues?status=unresolved&claim=unclaimed');
		const top = within(screen.getByTestId('top-issues')).getByRole('link');
		expect(top.getAttribute('href')).toBe('/issues/iss-1');
		expect(screen.getByTestId('top-issues').textContent).toContain('7 users · 20 events');
		expect(screen.getByTestId('weekly')).toBeTruthy();
	});
	it('handles an empty backlog and no top issues', () => {
		render(BacklogSection, { data: { weekly: [], age: { '0-7d': 0, '7-30d': 0, '30-90d': 0, '90d+': 0 }, open: 0, unassigned: 0, top: [] } as never, filters: { project: 'all', environment: 'all' } });
		expect(screen.getByTestId('unassigned-share').textContent).toContain('Nothing open');
		expect(screen.getByTestId('top-empty')).toBeTruthy();
		expect(screen.getByTestId('weekly-empty')).toBeTruthy();
	});
	it('response tiles: MTTA, MTTR, regression rate, change failure rate, reports waiting', () => {
		render(ResponseSection, { data: { mtta: { count: 3, p50: 600, p90: 3600, projectId: null, environment: null }, mttr: null, regressionRate: 0.25, changeFailureRate: null, releasesInRange: 0, reportsWaiting: 2 } as never, filters: { project: 'all', environment: 'all' } });
		expect(screen.getByTestId('mtta').textContent).toContain('10m');
		expect(screen.getByTestId('mtta').textContent).toContain('p90 1h 0m');
		expect(screen.getByTestId('mttr').textContent).toContain('no data');
		expect(screen.getByTestId('regression-rate').textContent).toContain('25%');
		expect(screen.getByTestId('change-failure-rate').textContent).toContain('no data');
		expect(within(screen.getByTestId('reports-waiting')).getByRole('link').getAttribute('href')).toBe('/issues?status=unresolved&type=user_report');
	});
});

describe('FilterBar and FreshnessLine (H1, H9)', () => {
	it('offers the four ranges, defaults to 7d (absent from the URL), and links every selection', () => {
		render(FilterBar, { filters: { project: 'all', environment: 'all', range: '7d' }, projects: [{ id: 'p1', name: 'core' }], environments: [] });
		const ranges = within(screen.getByTestId('range-filter')).getAllByRole('link');
		expect(ranges.map((a) => a.textContent)).toEqual(['24h', '7d', '28d', '90d']);
		expect(ranges.map((a) => a.getAttribute('href'))).toEqual(['/?range=24h', '/', '/?range=28d', '/?range=90d']);
		expect(within(screen.getByTestId('project-filter')).getByRole('link', { name: 'core' }).getAttribute('href')).toBe('/?project=p1');
		expect(screen.queryByTestId('environment-filter')).toBeNull(); // only inside one project
	});
	it('a project change drops the environment; the range survives', () => {
		render(FilterBar, { filters: { project: 'p1', environment: 'staging', range: '28d' }, projects: [{ id: 'p1', name: 'core' }, { id: 'p2', name: 'web' }], environments: [{ name: 'staging', displayName: 'Staging' }, { name: 'production', displayName: 'Production' }] });
		expect(within(screen.getByTestId('project-filter')).getByRole('link', { name: 'web' }).getAttribute('href')).toBe('/?project=p2&range=28d');
		expect(within(screen.getByTestId('environment-filter')).getByRole('link', { name: 'Production' }).getAttribute('href')).toBe('/?project=p1&environment=production&range=28d');
		expect(within(screen.getByTestId('range-filter')).getByRole('link', { name: '28d' }).getAttribute('aria-current')).toBe('true');
	});
	it('the freshness line warns past 10 minutes behind, and says so when there is no data', () => {
		const a = render(FreshnessLine, { data: { analyticsAsOf: '2026-10-07T10:00:00.000Z', lagMinutes: 15, warn: true } });
		expect(screen.getByTestId('freshness').textContent).toContain('analytics as of');
		expect(screen.getByTestId('freshness-warning').textContent).toContain('15 minutes behind');
		a.unmount();
		render(FreshnessLine, { data: { analyticsAsOf: null, lagMinutes: null, warn: false } });
		expect(screen.getByTestId('freshness').textContent).toContain('no analytics data yet');
		expect(screen.queryByTestId('freshness-warning')).toBeNull();
	});
});
