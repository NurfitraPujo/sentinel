import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/svelte';
import Page from './+page.svelte';
import type { PageData } from './$types';

// sentinel-project-environments task 15, cases 1 and 2 — the two states of `/`'s environment
// control that no manual pass remembers to try, and that a reviewer cannot check from the diff.
//
// Both are contract §4.4 claims about what the page does with `data.environments`, so they are
// asserted here against a fixture rather than through the loader: the loader is BE's under §6a, and
// what is being tested is the rendering decision, not the query that feeds it.

// `session` and `orgSlug` come from the ROOT LAYOUT, not from this page's loader, and nothing under
// test reads `session` — so the fixture carries the §4.4 `data` fields it is about and is cast once
// on the way in, rather than padded with a fake Auth.js session that would only obscure it.
function pageData(overrides: Record<string, unknown> = {}) {
	return {
		orgSlug: 'acme',
		issues: [],
		projects: [{ id: 'proj-1', name: 'Checkout API' }],
		environments: [],
		filters: { status: 'all', project: 'proj-1', environment: 'all' },
		pagination: { page: 1, limit: 20, total: 0, totalPages: 0 },
		...overrides,
	};
}

describe('/ environment filter (contract §4.4, flows L3 and L6)', () => {
	it('renders a designed explanation, not an empty select, when the project has no declared environments — flow L6', () => {
		// Under decision 8 this is the state of EVERY project on the day this ships: no registry row
		// is ever created by traffic, only by somebody declaring one. An empty select here reads as
		// broken software on a screen where nothing is wrong.
		render(Page, { data: pageData({ environments: [] }) as unknown as PageData });

		const empty = screen.getByTestId('environment-registry-empty');
		expect(empty).toBeTruthy();
		expect(empty.textContent).toContain('No environments have been set up for this project yet');
		// It says the thing a reader would otherwise assume is untrue.
		expect(empty.textContent).toContain('still arriving');
		// A link to where the state is fixed, built from the org slug the root layout supplies.
		expect(empty.querySelector('a')?.getAttribute('href')).toBe(
			'/acme/projects/proj-1/settings'
		);
		// And it is an explanation instead of the control, never beside it.
		expect(screen.queryByTestId('environment-filter')).toBeNull();
	});

	it('renders the control when the project has environments, and NOT AT ALL when project=all — flow L3', () => {
		const environments = [
			{ id: 'env-1', name: 'production', displayName: 'Production' },
			{ id: 'env-2', name: 'eu-west-1', displayName: 'EU West' },
		];

		const withProject = render(Page, { data: pageData({ environments }) as unknown as PageData });
		expect(screen.getByTestId('environment-filter')).toBeTruthy();
		expect(screen.getByRole('link', { name: 'EU West' })).toBeTruthy();
		withProject.unmount();

		// `project=all` returns `environments: []` from the loader and the control is absent — not
		// disabled and not empty. An environment name is only meaningful inside a project: two
		// projects may both report `production` and they are not the same environment, so a
		// cross-project filter here would silently mean "any project's production".
		render(Page, {
			data: pageData({
				environments: [],
				filters: { status: 'all', project: 'all', environment: 'all' },
			}) as unknown as PageData,
		});
		expect(screen.queryByTestId('environment-filter')).toBeNull();
		// And the empty-registry explanation is a per-project state, so it is absent here too —
		// otherwise `project=all` would claim a project has no environments.
		expect(screen.queryByTestId('environment-registry-empty')).toBeNull();
	});
});
