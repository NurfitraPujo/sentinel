import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/svelte';

// sentinel-reporting-and-dashboard D12, flows R6 and R7: the header nav's active state comes from the path.
const { pathname } = vi.hoisted(() => ({ pathname: { value: '/' } }));
vi.mock('$app/state', () => ({ page: { get url() { return new URL(`https://sentinel.test${pathname.value}`); } }, updated: { check: async () => false } }));
vi.mock('$app/navigation', () => ({ onNavigate: () => {} }));

import Layout from './+layout.svelte';

const data = { session: null, organizations: [], orgSlug: null } as never;
const at = (p: string) => {
	pathname.value = p;
	render(Layout, { data, children: (() => {}) as never });
	return {
		dashboard: screen.getByRole('link', { name: 'Dashboard' }),
		issues: screen.getByRole('link', { name: 'Issues' }),
	};
};

describe('header nav (R6)', () => {
	it('/ is Dashboard, and only Dashboard', () => {
		const n = at('/');
		expect(n.dashboard.getAttribute('href')).toBe('/');
		expect(n.dashboard.classList.contains('active')).toBe(true);
		expect(n.dashboard.getAttribute('aria-current')).toBe('page');
		expect(n.issues.getAttribute('href')).toBe('/issues');
		expect(n.issues.classList.contains('active')).toBe(false);
	});
	it('/issues is Issues, and only Issues', () => {
		const n = at('/issues');
		expect(n.issues.classList.contains('active')).toBe(true);
		expect(n.issues.getAttribute('aria-current')).toBe('page');
		expect(n.dashboard.classList.contains('active')).toBe(false);
	});
	it('an issue detail under /issues keeps Issues active', () => {
		const n = at('/issues/7d3e0c9a-1111-2222-3333-444444444444');
		expect(n.issues.classList.contains('active')).toBe(true);
		expect(n.dashboard.classList.contains('active')).toBe(false);
	});
	it('another page marks neither', () => {
		const n = at('/search');
		expect(n.issues.classList.contains('active')).toBe(false);
		expect(n.dashboard.classList.contains('active')).toBe(false);
	});
	it('the logo goes to the dashboard', () => {
		at('/issues');
		expect(screen.getByText('SENTINEL').closest('a')!.getAttribute('href')).toBe('/');
	});
});
