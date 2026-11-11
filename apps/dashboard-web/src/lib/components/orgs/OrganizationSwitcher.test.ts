import { describe, it, expect } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/svelte';
import OrganizationSwitcher from './OrganizationSwitcher.svelte';

const ORG_A = {
	id: 'org-1',
	name: 'Acme Corp',
	slug: 'acme',
	avatarUrl: 'https://example.com/acme.png',
};

const ORG_B = {
	id: 'org-2',
	name: 'Stark Industries',
	slug: 'stark',
	avatarUrl: null,
};

describe('OrganizationSwitcher', () => {
	it('renders active organization name and avatar', () => {
		render(OrganizationSwitcher, {
			props: {
				activeOrg: ORG_A,
				organizations: [ORG_A, ORG_B],
			},
		});

		expect(screen.getByText('Acme Corp')).toBeTruthy();
		const avatar = screen.getByRole('button').querySelector('img');
		expect(avatar).toBeTruthy();
		expect(avatar?.getAttribute('src')).toBe('https://example.com/acme.png');
	});

	it('renders "Select Organization" when activeOrg is null', () => {
		render(OrganizationSwitcher, {
			props: {
				activeOrg: null,
				organizations: [ORG_A, ORG_B],
			},
		});

		expect(screen.getByText('Select Organization')).toBeTruthy();
	});

	it('toggles dropdown menu on click', async () => {
		render(OrganizationSwitcher, {
			props: {
				activeOrg: ORG_A,
				organizations: [ORG_A, ORG_B],
			},
		});

		const trigger = screen.getByRole('button', { name: /switch organization/i });
		expect(screen.queryByRole('menu')).toBeNull();

		await fireEvent.click(trigger);
		expect(screen.getByRole('menu')).toBeTruthy();
		expect(screen.getByText('Organizations')).toBeTruthy();

		// Links to organization projects
		const links = screen.getAllByRole('menuitem');
		expect(links.length).toBe(3); // ORG_A, ORG_B, Create New Organization
		expect(links[0].getAttribute('href')).toBe('/acme/projects');
		expect(links[1].getAttribute('href')).toBe('/stark/projects');
		expect(links[2].getAttribute('href')).toBe('/onboarding');
	});

	it('marks the active organization with checkmark in menu', async () => {
		render(OrganizationSwitcher, {
			props: {
				activeOrg: ORG_A,
				organizations: [ORG_A, ORG_B],
			},
		});

		const trigger = screen.getByRole('button', { name: /switch organization/i });
		await fireEvent.click(trigger);

		const menu = screen.getByRole('menu');
		const items = menu.querySelectorAll('a.menu-item');
		expect(items[0].classList.contains('active')).toBe(true);
		expect(items[0].textContent).toContain('Acme Corp');
		expect(items[0].textContent).toContain('✓');

		expect(items[1].classList.contains('active')).toBe(false);
		expect(items[1].textContent).toContain('Stark Industries');
	});

	it('closes dropdown on Escape key', async () => {
		render(OrganizationSwitcher, {
			props: {
				activeOrg: ORG_A,
				organizations: [ORG_A, ORG_B],
			},
		});

		const trigger = screen.getByRole('button', { name: /switch organization/i });
		await fireEvent.click(trigger);
		expect(screen.getByRole('menu')).toBeTruthy();

		await fireEvent.keyDown(window, { key: 'Escape' });
		expect(screen.queryByRole('menu')).toBeNull();
	});

	it('closes dropdown when clicking outside', async () => {
		render(OrganizationSwitcher, {
			props: {
				activeOrg: ORG_A,
				organizations: [ORG_A, ORG_B],
			},
		});

		const trigger = screen.getByRole('button', { name: /switch organization/i });
		await fireEvent.click(trigger);
		expect(screen.getByRole('menu')).toBeTruthy();

		await fireEvent.click(document.body);
		expect(screen.queryByRole('menu')).toBeNull();
	});
});
