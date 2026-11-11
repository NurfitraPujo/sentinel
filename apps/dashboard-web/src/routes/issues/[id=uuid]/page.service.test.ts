import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/svelte';
import Page from './+page.svelte';

// sentinel-reporting-and-dashboard D19/D12, flows R5 and R7: the issue detail names the service(s) of its occurrences
// (read off the tag-facet block) and "← Back to Issues" goes to /issues, not to the dashboard.
function data(tagFacets: unknown[] = []) {
	return {
		issue: {
			id: 'issue-1', projectId: 'proj-1', environment: 'production', errorClass: 'Error', message: 'boom', status: 'unresolved',
			firstSeen: new Date('2026-09-16T00:00:00Z'), lastSeen: new Date('2026-09-16T00:00:00Z'), count: 1, issueType: 'system_error',
		},
		project: { id: 'proj-1', name: 'Core' },
		occurrences: [], occurrenceTotal: 0, environmentDisplayName: 'Production', relations: [], trend: { hourly: [], daily: [] }, tagFacets,
		orgSlug: 'acme', currentUser: { id: 'u1', email: 'u@x.test' },
	} as never;
}
const block = (key: string, values: string[]) => ({ key, total: 10, values: values.map((value) => ({ value, count: 5, percent: 50 })), other: null });

describe('issue detail service and back link', () => {
	it('names a single service', () => {
		render(Page, { data: data([block('service', ['api'])]) });
		const el = screen.getByTestId('issue-services');
		expect(el.textContent).toContain('Service:');
		expect(el.textContent).toContain('api');
	});

	it('names several services, most frequent first', () => {
		render(Page, { data: data([block('service', ['jobs worker', 'api'])]) });
		const el = screen.getByTestId('issue-services');
		expect(el.textContent).toContain('Services:');
		expect(el.textContent).toContain('jobs worker, api');
	});

	it('shows nothing when no occurrence has a service', () => {
		render(Page, { data: data([block('release', ['1.0'])]) });
		expect(screen.queryByTestId('issue-services')).toBeNull();
	});

	it('Back to Issues goes to /issues', () => {
		render(Page, { data: data() });
		expect(screen.getByText(/Back to Issues/).closest('a')!.getAttribute('href')).toBe('/issues');
	});
});
