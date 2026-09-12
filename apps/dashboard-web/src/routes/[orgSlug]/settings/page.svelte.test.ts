import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/svelte';
import Page from './+page.svelte';

// Pins the hub's tile list. Instance-wide settings (alerts, auth, observability) are not this
// organization's and must not be presented under it, and the org-scoped observability page is not
// actually org-scoped (its loader ignores orgSlug). The routes themselves still answer by URL —
// only the tiles are gone — so this test is what turns red if either creeps back onto the hub.

const data = {
	session: null,
	orgSlug: 'acme',
	org: { name: 'Acme', slug: 'acme' },
	projects: [{ id: 'proj-1', name: 'Web app' }],
};

function cardHrefs() {
	return Array.from(document.querySelectorAll('a.card')).map((a) => a.getAttribute('href'));
}

describe('/[orgSlug]/settings hub tiles', () => {
	it('lists exactly API keys, Members, Agents, On-call and the org projects', () => {
		render(Page, { data });

		expect(cardHrefs()).toEqual([
			'/acme/settings/keys',
			'/acme/settings/members',
			'/acme/settings/agents',
			'/acme/settings/on-call',
			'/acme/projects/proj-1/settings/keys',
		]);
		expect(screen.getByText('Projects')).toBeTruthy();
	});

	it('does not link to the org-scoped observability page', () => {
		render(Page, { data });
		expect(cardHrefs()).not.toContain('/acme/settings/observability');
		expect(screen.queryByText('Observability')).toBeNull();
	});

	it('does not render the instance-wide "Account & instance" section', () => {
		render(Page, { data });
		expect(screen.queryByText('Account & instance')).toBeNull();
		for (const href of ['/settings/alerts', '/settings/auth', '/settings/observability']) {
			expect(cardHrefs()).not.toContain(href);
		}
		expect(screen.queryByText('Alerts')).toBeNull();
		expect(screen.queryByText('Auth')).toBeNull();
	});
});
