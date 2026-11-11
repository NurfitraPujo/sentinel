import { describe, it, expect } from 'vitest';
import { render, screen, within } from '@testing-library/svelte';
import HealthRow from './HealthRow.svelte';
import StackedBars from './StackedBars.svelte';
import EventsTimeline from './EventsTimeline.svelte';
import ReleaseTable from './ReleaseTable.svelte';
import BacklogSection from './BacklogSection.svelte';
import ResponseSection from './ResponseSection.svelte';
import FilterBar from './FilterBar.svelte';
import FreshnessLine from './FreshnessLine.svelte';
import { delta, duration, listHref, perK, pct, GROUP_LABELS, coerceSplit, deltaState, backlogSub, stackEvents, eventsBarHref, dashboardHref, MAX_SERVICE_SERIES } from '$lib/dashboard-format';

// sentinel-reporting-and-dashboard D17 (fe): the sections, the D19 groups, D23's label, and the accessible table every
// chart carries. The data shapes are the ones lib/server/analytics/service.ts returns.
const groups = (o: Partial<Record<string, number>> = {}) => ({ api: 0, jobs: 0, nats: 0, other: 0, unattributed: 0, ...o });
const sliTile = (o: Record<string, unknown> = {}) => ({ id: 'core-api', kind: 'http', project: 'core', total: 1000, errorsPer1k: 5, events: 5, ...o });
const health = (o: Record<string, unknown> = {}) => ({
	events: { current: groups({ api: 5, jobs: 3, other: 2, unattributed: 1 }), previous: groups({ api: 2, jobs: 3 }) },
	byLevel: {}, newIssues: { current: 4, previous: 2 }, regressions: { current: 1, previous: 1 }, backlog: { open: 12, unassigned: 9, olderThan30d: 2 },
	slis: [sliTile(), sliTile({ id: 'core-jobs', kind: 'jobs', total: 100, errorsPer1k: 30, events: 3 }),
		sliTile({ id: 'core-nats', kind: 'nats', total: 0, errorsPer1k: null, events: null })],
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

describe('HealthRow: the Error tracking tiles (H2, H3, H4)', () => {
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

	it('no longer carries SLO tiles: the SLO table owns targets, budgets and the latency caveat (D2, D10)', () => {
		render(HealthRow, { data: health(), filters: FILTERS });
		expect(screen.queryByTestId('sli-tiles')).toBeNull();
		expect(screen.queryByTestId('sli-note')).toBeNull();
		expect(document.body.textContent).not.toContain('long-lived streams');
	});

	it('says "captured exceptions" with the denominator, never an unqualified "errors" (D9)', () => {
		render(HealthRow, { data: health(), filters: FILTERS });
		const api = screen.getByTestId('per1k-api');
		expect(api.textContent).toContain('Captured API exceptions per 1k requests');
		expect(api.textContent).toContain('5.0');
		expect(api.textContent).toContain('5 exceptions / 1,000 requests');
		expect(screen.getByTestId('per1k-jobs').textContent).toContain('Captured job exceptions per 1k runs');
		expect(screen.getByTestId('per1k-jobs').textContent).toContain('3 exceptions / 100 runs');
		expect(screen.getByTestId('per1k-nats').textContent).toContain('Captured NATS exceptions per 1k messages');
		expect(screen.getByTestId('per1k-nats').textContent).toContain('no data');
		expect(screen.getByTestId('health-row').textContent).toContain('Captured exceptions by origin');
		expect(screen.getByTestId('group-total').textContent).toContain('All captured exceptions');
		expect(screen.getByTestId('health-row').textContent).not.toMatch(/\berrors\b/i);
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

	it('shows no per-SLI rates for a viewer with none visible, and still shows the rest', () => {
		render(HealthRow, { data: health({ slis: [] }), filters: FILTERS });
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

	it('EventsTimeline stacks by D19 origin, or by service on any project selection', () => {
		const byGroup = [{ bucket: '2026-10-07 10:00:00', group: 'api', service: 'api', n: 3 }, { bucket: '2026-10-07 10:00:00', group: 'jobs', service: 'jobs worker', n: 2 }, { bucket: '2026-10-07 10:00:00', group: 'unattributed', service: null, n: 1 }];
		const a = render(EventsTimeline, { data: { bucket: 'hour', byGroup, split: 'group', releaseMarkers: [] } as never });
		expect(screen.getByTestId('timeline').textContent).toContain('Captured exceptions by origin');
		expect(screen.getByTestId('timeline-legend').textContent).toContain('Other (outside a request)');
		a.unmount();
		// project=all: the service split works there too, and release markers still appear only when the data carries them
		render(EventsTimeline, { data: { bucket: 'hour', byGroup, split: 'service', releaseMarkers: [{ version: 'v2', at: '2026-10-07T10:30:00.000Z' }] } as never, filters: { project: 'all', environment: 'all', range: '7d', split: 'service' } });
		expect(screen.getByTestId('timeline').textContent).toContain('Captured exceptions by service');
		expect(screen.getByTestId('timeline-legend').textContent).toContain('jobs worker');
		expect(screen.getByTestId('timeline-legend').textContent).toContain('Unattributed');
		expect(screen.getAllByTestId('timeline-marker')).toHaveLength(1);
	});
});

describe('events chart: split, palette, click-through (D12)', () => {
	const ev = (service: string | null, n: number, group = 'other', bucket = '2026-10-07 10:00:00') => ({ bucket, group, service, n });

	it('coerces split like range: anything unknown is by origin', () => {
		expect(coerceSplit('service')).toBe('service');
		expect(coerceSplit('group')).toBe('group');
		expect(coerceSplit('rubbish')).toBe('group');
		expect(coerceSplit(null)).toBe('group');
	});

	it('the toggle keeps the other filters, and the default split stays out of the URL', () => {
		expect(dashboardHref({ project: 'p1', environment: 'staging', range: '28d', split: 'group' }, { split: 'service' })).toBe('/?project=p1&environment=staging&range=28d&split=service');
		expect(dashboardHref({ project: 'all', environment: 'all', range: '7d', split: 'service' }, { split: 'group' })).toBe('/');
		expect(dashboardHref({ project: 'all', environment: 'all', range: '7d', split: 'service' }, { range: '24h' })).toBe('/?range=24h&split=service');
		render(EventsTimeline, { data: { bucket: 'hour', byGroup: [ev('api', 1)], split: 'group', releaseMarkers: [] } as never, filters: { project: 'p1', environment: 'all', range: '7d', split: 'group' } });
		const links = within(screen.getByTestId('split-toggle')).getAllByRole('link');
		expect(links.map((l) => l.textContent)).toEqual(['By origin', 'By service']);
		expect(links.map((l) => l.getAttribute('href'))).toEqual(['/?project=p1', '/?project=p1&split=service']);
		expect(links[0].getAttribute('aria-current')).toBe('true');
	});

	it('service split keeps the top 8 by events in range, folds the rest into Other services, and names events with no service', () => {
		const rows = [...Array.from({ length: 11 }, (_, i) => ev(`svc${i}`, 100 - i)), ev(null, 5)];
		const { series, keyOf } = stackEvents(rows, 'service');
		expect(series.map((s) => s.label)).toEqual([...Array.from({ length: MAX_SERVICE_SERIES }, (_, i) => `svc${i}`), 'Other services', 'Unattributed']);
		expect(keyOf(ev('svc7', 1))).toBe('svc7');
		expect(keyOf(ev('svc8', 1))).toBe('__other');
		expect(keyOf(ev('svc10', 1))).toBe('__other');
		expect(keyOf(ev(null, 1))).toBe('unattributed');
		expect(new Set(series.map((s) => s.tone)).size).toBe(series.length); // every series of the split has its own tone
		expect(series.find((s) => s.key === '__other')!.tone).not.toBe(series.find((s) => s.key === 'unattributed')!.tone);
	});

	it('service split omits series that do not exist', () => {
		expect(stackEvents([ev('a', 1)], 'service').series.map((s) => s.label)).toEqual(['a']);
	});

	it('the 326k "Other (outside a request)" no longer drowns the rest: services are the split on project=all', () => {
		const a = render(EventsTimeline, { data: { bucket: 'hour', byGroup: [ev('api', 5, 'api'), ev('worker', 9, 'other'), ev(null, 2, 'unattributed')], split: 'service', releaseMarkers: [] } as never });
		expect(screen.getAllByTestId('timeline-bar')).toHaveLength(3);
		a.unmount();
	});

	it('a bar links to /issues with seen=all, the bucket as seen_from/seen_to, and the project/environment scope', () => {
		render(EventsTimeline, {
			data: { bucket: 'hour', byGroup: [ev('api', 3, 'api'), ev('jobs worker', 2, 'jobs'), ev('x', 4, 'other')], split: 'group', releaseMarkers: [] } as never,
			filters: { project: 'p1', environment: 'production', range: '7d', split: 'group' }, apiService: 'api',
		});
		const hrefs = screen.getAllByTestId('timeline-link').map((a) => new URL(a.getAttribute('href')!, 'https://x.test'));
		expect(hrefs).toHaveLength(3);
		for (const u of hrefs) {
			expect(u.pathname).toBe('/issues');
			expect(u.searchParams.get('seen')).toBe('all');
			expect(u.searchParams.get('seen_from')).toBe('2026-10-07T10:00:00.000Z');
			expect(u.searchParams.get('seen_to')).toBe('2026-10-07T11:00:00.000Z');
			expect(u.searchParams.get('project')).toBe('p1');
			expect(u.searchParams.get('environment')).toBe('production');
		}
		expect(hrefs[0].searchParams.get('service')).toBe('api'); // group:api through listHref
		expect(hrefs[1].searchParams.get('tag')).toBe('source:job'); // group:jobs
		expect(hrefs[2].searchParams.get('service')).toBeNull(); // "other" has no list filter
	});

	it('daily buckets span a day; a service-split bar carries the service as the list filter', () => {
		const href = eventsBarHref({ scope: { project: 'all' }, series: { key: 'worker', label: 'worker', tone: 0, filter: { kind: 'service', service: 'worker' } }, bucketStart: '2026-10-07 00:00:00', bucketMs: 86_400_000 });
		const u = new URL(href, 'https://x.test');
		expect(u.searchParams.get('service')).toBe('worker');
		expect(u.searchParams.get('seen_from')).toBe('2026-10-07T00:00:00.000Z');
		expect(u.searchParams.get('seen_to')).toBe('2026-10-08T00:00:00.000Z');
		expect(u.searchParams.get('project')).toBeNull();
	});

	it('every bar is keyboard reachable and says its bucket and count; the drawing is exposed because it is interactive', () => {
		render(EventsTimeline, { data: { bucket: 'hour', byGroup: [ev('api', 3, 'api')], split: 'group', releaseMarkers: [] } as never, apiService: 'api' });
		const link = screen.getByTestId('timeline-link');
		expect(link.tagName.toLowerCase()).toBe('a');
		expect(link.getAttribute('tabindex')).not.toBe('-1');
		expect(link.getAttribute('aria-label')).toBe('10-07 10:00 · API: 3');
		expect(screen.getByTestId('timeline').querySelector('svg')!.getAttribute('aria-hidden')).toBeNull();
	});

	it('series use the palette tokens, one slot each, not shades of one hue', () => {
		render(EventsTimeline, { data: { bucket: 'hour', byGroup: [ev('api', 3, 'api'), ev('w', 2, 'jobs')], split: 'group', releaseMarkers: [] } as never });
		const fills = screen.getAllByTestId('timeline-bar').map((r) => r.getAttribute('fill'));
		expect(fills).toEqual(['var(--chart-1)', 'var(--chart-2)']);
		expect(screen.getByTestId('timeline-legend').innerHTML).toContain('var(--chart-4)'); // other keeps its own slot
		expect(screen.getByTestId('timeline-legend').innerHTML).not.toContain('oklch(');
	});
});

describe('deltas say good or bad (D13) and the backlog tile (D14)', () => {
	it('an increase of 5% or more is warn, a decrease of 5% or more is ok, smaller is neutral', () => {
		expect(deltaState(105, 100)).toBe('warn');
		expect(deltaState(104, 100)).toBeUndefined();
		expect(deltaState(96, 100)).toBeUndefined();
		expect(deltaState(95, 100)).toBe('ok');
		expect(deltaState(1, 100)).toBe('ok'); // −99% is not the same as +99%
		expect(deltaState(199, 100)).toBe('warn');
		expect(deltaState(5, 0)).toBe('warn');
		expect(deltaState(0, 0)).toBeUndefined();
		expect(deltaState(0, 7)).toBe('ok');
		expect(deltaState(100, 100)).toBeUndefined();
	});

	it('colours the tiles that carry a trend, and keeps the sign in the text', () => {
		render(HealthRow, { data: health({ events: { current: groups({ api: 1, jobs: 6 }), previous: groups({ api: 100, jobs: 3 }) }, newIssues: { current: 4, previous: 4 }, regressions: { current: 2, previous: 1 } }), filters: FILTERS });
		expect(screen.getByTestId('group-api').className).toContain('state-ok');
		expect(screen.getByTestId('group-api').textContent).toContain('−99% vs previous period');
		expect(screen.getByTestId('group-jobs').className).toContain('state-warn');
		expect(screen.getByTestId('group-jobs').textContent).toContain('+100% vs previous period');
		expect(screen.getByTestId('tile-new-issues').className).not.toMatch(/state-/);
		expect(screen.getByTestId('tile-regressions').className).toContain('state-warn');
	});

	it('the backlog sub-line says "none assigned" only when every open issue is unassigned', () => {
		expect(backlogSub({ open: 1087, unassigned: 1087, olderThan30d: 300 })).toBe('none assigned · 300 older than 30 days');
		expect(backlogSub({ open: 12, unassigned: 9, olderThan30d: 2 })).toBe('9 unassigned · 2 older than 30 days');
		expect(backlogSub({ open: 0, unassigned: 0, olderThan30d: 0 })).toBe('0 unassigned · 0 older than 30 days');
		render(HealthRow, { data: health({ backlog: { open: 6, unassigned: 6, olderThan30d: 1 } }), filters: FILTERS });
		expect(screen.getByTestId('tile-backlog').textContent).toContain('none assigned · 1 older than 30 days');
		expect(screen.getByTestId('tile-backlog').textContent).not.toContain('6 / 6');
	});
});

describe('ReleaseTable (H6)', () => {
	const row = (o = {}) => ({ project: 'core', version: 'v2.0.1', environment: 'production', firstSeen: '2026-10-07T10:00:00.000Z', events: groups({ api: 5, jobs: 1 }), newIssues: 2, regressions: 1, errorsPer1k: 3.2, change: 1.5, ...o });
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
