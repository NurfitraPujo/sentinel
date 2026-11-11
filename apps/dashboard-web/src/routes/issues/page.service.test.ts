import { describe, it, expect } from 'vitest';
import { screen, within } from '@testing-library/svelte';
import { render, EMPTY_FACETS } from './page-fixtures';
import Page from './+page.svelte';
import type { PageData } from './$types';

// sentinel-reporting-and-dashboard D19, flows R4 and R5: the Service facet in the sidebar and the service chips on each
// row. The loader's coercion is page.server.test.ts's; the SQL is issue-queries.service.integration.test.ts's.
function pageData(filters: Record<string, unknown> = {}, over: Record<string, unknown> = {}) {
	return {
		orgSlug: 'acme',
		issues: [],
		projects: [{ id: 'proj-1', name: 'Core' }],
		environments: [],
		releases: [],
		sources: [],
		filters: { status: 'all', project: 'all', environment: 'all', ...filters },
		pagination: { page: 3, limit: 20, total: 0, totalPages: 0 },
		retentionDays: 30,
		...over,
	} as unknown as PageData;
}
const facets = (byService: Record<string, number>) => ({ ...EMPTY_FACETS, service: { byService } });
const hrefOf = (el: HTMLElement) => (el as HTMLAnchorElement).getAttribute('href')!;

describe('/issues service facet (R4)', () => {
	it('lists every service with its count, and All Services, as anchors that set service and reset the page', () => {
		render(Page, { data: pageData({}, { facets: facets({ api: 7, 'jobs worker': 2 }) }) });
		const group = screen.getByTestId('service-filter');
		const api = within(group).getByRole('link', { name: 'api' });
		expect(api.textContent).toContain('7');
		const q = new URLSearchParams(hrefOf(api));
		expect(q.get('service')).toBe('api');
		expect(q.get('page')).toBeNull();
		expect(new URLSearchParams(hrefOf(within(group).getByRole('link', { name: 'jobs worker' }))).get('service')).toBe('jobs worker');
		expect(new URLSearchParams(hrefOf(within(group).getByRole('link', { name: 'All Services' }))).get('service')).toBeNull();
	});

	it('marks the selected service active and carries it on other anchors', () => {
		render(Page, { data: pageData({ service: 'api' }, { facets: facets({ api: 7, 'jobs worker': 2 }) }) });
		const group = screen.getByTestId('service-filter');
		expect(within(group).getByRole('link', { name: 'api' }).classList.contains('active')).toBe(true);
		expect(within(group).getByRole('link', { name: 'All Services' }).classList.contains('active')).toBe(false);
		const status = screen.getAllByRole('link').find((a) => /status=unresolved/.test(a.getAttribute('href') ?? ''))!;
		expect(new URLSearchParams(hrefOf(status)).get('service')).toBe('api');
	});

	it('still shows a URL service that no listed issue carries, as selected', () => {
		render(Page, { data: pageData({ service: 'retired-svc' }, { facets: facets({}) }) });
		const group = screen.getByTestId('service-filter');
		expect(within(group).getByRole('link', { name: 'retired-svc' }).classList.contains('active')).toBe(true);
	});

	it('is absent when no issue has a service and none is selected', () => {
		render(Page, { data: pageData({}, { facets: facets({}) }) });
		expect(screen.queryByTestId('service-filter')).toBeNull();
	});

	it('Reset all filters clears service', () => {
		render(Page, { data: pageData({ service: 'api' }, { facets: facets({ api: 1 }) }) });
		const reset = screen.getAllByRole('link').find((a) => /reset/i.test(a.textContent ?? ''));
		expect(reset).toBeTruthy();
		expect(new URLSearchParams(hrefOf(reset!)).get('service')).toBeNull();
	});
});

describe('/issues service on each row (R5)', () => {
	const issue = (services: string[]) => ({
		id: 'dddddddd-0000-0000-0000-000000000001', projectId: 'proj-1', projectName: 'Core', environment: 'production',
		status: 'unresolved', message: 'boom', errorClass: 'Err', count: 3, firstSeen: new Date(), lastSeen: new Date(),
		affectedUserCount: 0, affectedTenantCount: 0, regressionStatus: 'none', trend24h: new Array(24).fill(0), services,
	});

	it('shows the service name(s) beside the project and environment', () => {
		render(Page, { data: pageData({}, { issues: [issue(['api', 'jobs worker'])], pagination: { page: 1, limit: 20, total: 1, totalPages: 1 } }) });
		expect(screen.getAllByTestId('issue-service').map((e) => e.textContent?.trim())).toEqual(['api', 'jobs worker']);
	});

	it('shows none for an issue with no service', () => {
		render(Page, { data: pageData({}, { issues: [issue([])], pagination: { page: 1, limit: 20, total: 1, totalPages: 1 } }) });
		expect(screen.queryAllByTestId('issue-service')).toHaveLength(0);
	});
});
