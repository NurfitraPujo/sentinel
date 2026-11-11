import { describe, it, expect } from 'vitest';
import { render, screen, within } from '@testing-library/svelte';
import { tick } from 'svelte';
import Page from './+page.svelte';

// sentinel-reporting-and-dashboard D14/D11, flows H10 and H11: the page's states. Each section is its own promise: one
// unavailable section says so and leaves the rest, the nav link to /issues and the filters intact; a viewer with no
// project sees the empty state and nothing from any project.
const ok = <T,>(data: T) => Promise.resolve({ status: 'ok' as const, data });
const unavailable = () => Promise.resolve({ status: 'unavailable' as const });
const groups = { api: 1, jobs: 2, nats: 0, other: 0, unattributed: 0 };
const base = {
	filters: { project: 'all', environment: 'all', range: '7d' },
	projects: [{ id: 'p1', name: 'core' }],
	environments: [],
	databendConfigured: true,
	empty: false,
	apiServices: ['api'],
};
const health = { events: { current: groups, previous: groups }, byLevel: {}, newIssues: { current: 1, previous: 1 }, regressions: { current: 0, previous: 0 }, backlog: { open: 3, unassigned: 1, olderThan30d: 0 }, slis: [] };
const sections = (over: Record<string, Promise<unknown>> = {}) => ({
	freshness: ok({ analyticsAsOf: '2026-10-07T10:00:00.000Z', lagMinutes: 0, warn: false }),
	health: ok(health),
	timeline: ok({ bucket: 'hour', byGroup: [], byService: false, releaseMarkers: [] }),
	slo: ok({ rows: [] }),
	budget: ok({ charts: [] }),
	releases: ok({ rows: [], needsProject: true, allProjects: false }),
	backlog: ok({ weekly: [], age: { '0-7d': 0, '7-30d': 0, '30-90d': 0, '90d+': 0 }, open: 0, unassigned: 0, top: [] }),
	response: ok({ mtta: null, mttr: null, regressionRate: null, changeFailureRate: null, releasesInRange: 0, reportsWaiting: 0 }),
	...over,
});
const settle = async () => { for (let i = 0; i < 6; i++) await tick(); await new Promise((r) => setTimeout(r, 0)); };

describe('/ the dashboard page', () => {
	it('renders every section, in the contract order, with a link to the issue list', async () => {
		render(Page, { data: { ...base, sections: sections() } as never });
		await settle();
		const headings = screen.getAllByRole('heading', { level: 2 }).map((h) => h.textContent);
		expect(headings).toEqual(['Service health (SLO)', 'Error budget', 'Error tracking', 'Release health', 'Backlog', 'Response']);
		expect(screen.getByTestId('to-issues').getAttribute('href')).toBe('/issues');
		expect(screen.getByTestId('health-row')).toBeTruthy();
		expect(screen.getByTestId('freshness').textContent).toContain('analytics as of');
		expect(screen.getByTestId('dashboard-filters')).toBeTruthy();
	});

	it('Databend down: every section says unavailable while the page, the filters and the issues link keep working (H10)', async () => {
		const down = Object.fromEntries(['freshness', 'health', 'timeline', 'slo', 'budget', 'releases', 'backlog', 'response'].map((k) => [k, unavailable()]));
		render(Page, { data: { ...base, sections: down } as never });
		await settle();
		for (const name of ['health', 'timeline', 'slo', 'budget', 'releases', 'backlog', 'response']) {
			expect(screen.getByTestId(`${name}-unavailable`).textContent).toContain('Unavailable');
		}
		expect(screen.getByTestId('to-issues')).toBeTruthy();
		expect(screen.getByTestId('dashboard-filters')).toBeTruthy();
		expect(screen.queryByTestId('freshness')).toBeNull(); // no claim about freshness when there is nothing to base it on
	});

	it('one unavailable section does not take the others down', async () => {
		render(Page, { data: { ...base, sections: sections({ backlog: unavailable() }) } as never });
		await settle();
		expect(screen.getByTestId('backlog-unavailable')).toBeTruthy();
		expect(screen.getByTestId('health-row')).toBeTruthy();
		expect(screen.getByTestId('response-section')).toBeTruthy();
	});

	it('a section still loading shows its own skeleton while the others render', async () => {
		render(Page, { data: { ...base, sections: sections({ timeline: new Promise(() => {}) }) } as never });
		await settle();
		expect(screen.getByTestId('timeline-loading')).toBeTruthy();
		expect(screen.getByTestId('health-row')).toBeTruthy();
	});

	it('asks for a project in the release section when none is selected, and for none when there are no releases', async () => {
		render(Page, { data: { ...base, sections: sections() } as never });
		await settle();
		expect(screen.getByTestId('releases-empty').textContent).toContain('Pick a project');
	});

	it('shows no service levels message when none are tracked', async () => {
		render(Page, { data: { ...base, sections: sections() } as never });
		await settle();
		expect(screen.getByTestId('slo-empty').textContent).toContain('No service levels');
	});

	it('lists the release table when a project is selected', async () => {
		const row = { project: 'core', version: 'v2', environment: 'production', firstSeen: '2026-10-07T10:00:00.000Z', events: groups, newIssues: 0, regressions: 0, errorsPer1k: null, change: null };
		render(Page, { data: { ...base, filters: { ...base.filters, project: 'p1' }, sections: sections({ releases: ok({ rows: [row], needsProject: false, allProjects: false }) }) } as never });
		await settle();
		expect(within(screen.getByTestId('release-table')).getAllByTestId('release-row')).toHaveLength(1);
	});
});

describe('/ release health on all projects (D15)', () => {
	it('shows the Project column and the hint under the table', async () => {
		const mk = (project: string, version: string) => ({ project, version, environment: 'production', firstSeen: '2026-10-07T10:00:00.000Z', events: groups, newIssues: 0, regressions: 0, errorsPer1k: null, change: null });
		render(Page, { data: { ...base, sections: sections({ releases: ok({ rows: [mk('core', 'v2'), mk('web', 'v9')], needsProject: false, allProjects: true }) }) } as never });
		await settle();
		const table = screen.getByTestId('release-table');
		expect(within(table).getByText('Project')).toBeTruthy();
		expect(screen.getAllByTestId('release-project').map((c) => c.textContent)).toEqual(['core', 'web']);
		expect(screen.getByTestId('releases-hint').textContent).toBe('Pick a project to see all its releases.');
	});
	it('one project: no Project column, no hint', async () => {
		const row = { project: 'core', version: 'v2', environment: 'production', firstSeen: '2026-10-07T10:00:00.000Z', events: groups, newIssues: 0, regressions: 0, errorsPer1k: null, change: null };
		render(Page, { data: { ...base, filters: { ...base.filters, project: 'p1' }, sections: sections({ releases: ok({ rows: [row], needsProject: false, allProjects: false }) }) } as never });
		await settle();
		expect(screen.queryByTestId('release-project')).toBeNull();
		expect(screen.queryByTestId('releases-hint')).toBeNull();
	});
});

describe('/ a viewer with no accessible project (H11, D11)', () => {
	it('shows the empty state and nothing from any project, not even a count', () => {
		render(Page, { data: { ...base, projects: [], empty: true, sections: null } as never });
		expect(screen.getByTestId('dashboard-empty').textContent).toContain('No projects yet');
		expect(screen.queryByTestId('health-row')).toBeNull();
		expect(screen.queryByTestId('dashboard-filters')).toBeNull();
		expect(screen.getByRole('link', { name: 'Set up a project' }).getAttribute('href')).toBe('/onboarding');
	});
});
