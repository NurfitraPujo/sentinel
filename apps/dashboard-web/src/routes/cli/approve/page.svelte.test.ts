import { describe, it, expect } from 'vitest';
import { fireEvent, render, screen } from '@testing-library/svelte';
import Page from './+page.svelte';

// sentinel-cli-browser-login (security review, repair round 1): when approving would REPLACE the user's live key for the
// same device name in the chosen org, the page says so -- and follows the org picker, since keys are per org.
const data = (replaces: Array<{ organizationId: string; createdAt: string | null }>) => ({
	signedIn: true as const,
	userEmail: 'ada@example.test',
	organizations: [
		{ id: 'org-1', name: 'Acme', slug: 'acme', role: 'engineer' },
		{ id: 'org-2', name: 'Beta', slug: 'beta', role: 'viewer' },
	],
	defaultOrganizationId: 'org-1',
	code: 'BCDF-GHJK',
	codeError: null,
	replaces,
	request: {
		state: 'pending',
		client: 'sentinel-cli',
		clientVersion: 'v1',
		deviceName: 'ada-laptop',
		requestedProfile: '',
		requestedIp: '203.0.113.7',
		createdAt: '2026-10-07T00:00:00.000Z',
		expiresAt: '2026-10-07T00:10:00.000Z',
	},
});

describe('/cli/approve page: replacement warning', () => {
	it('names the device and the replaced key, for the selected org only', async () => {
		render(Page, { data: data([{ organizationId: 'org-2', createdAt: '2026-09-01T00:00:00.000Z' }]), form: null } as any);
		expect(screen.queryByTestId('replaces-key')).toBeNull(); // org-1 selected: nothing replaced there

		await fireEvent.change(screen.getByLabelText(/The key will be for organization/), { target: { value: 'org-2' } });
		const warning = await screen.findByTestId('replaces-key');
		expect(warning.textContent).toMatch(/This replaces your CLI key for\s+ada-laptop\s+created/);
	});

	it('says nothing when no live key would be replaced', () => {
		render(Page, { data: data([]), form: null } as any);
		expect(screen.queryByTestId('replaces-key')).toBeNull();
	});
});
