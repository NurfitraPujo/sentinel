import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/svelte';
import Page from './+page.svelte';

// sentinel-project-environments task 15, cases 3-6. Every fixture below is built from contract
// §4.1's `Environment` and §4.10's `{ grants, members }` — the shapes are fully specified there,
// including the empty cases, so none of this needs a running backend.
//
// The four cases are together because they are the same screen, and because three of them are
// ABSENCES — a hidden marker, a control that must not be rendered, a request that must not be sent
// — which is exactly what a manual pass does not notice is missing.

const ORG_ID = 'org-1';
const PROJECT_ID = 'proj-1';
const ENVIRONMENTS_URL = `/api/organizations/${ORG_ID}/projects/${PROJECT_ID}/environments?includeHidden=true`;
const GRANTS_URL = `/api/organizations/${ORG_ID}/projects/${PROJECT_ID}/environment-grants`;

function environment(overrides: Record<string, unknown> = {}) {
	return {
		id: 'env-1',
		name: 'production',
		displayName: 'Production',
		isDefault: false,
		hidden: false,
		firstSeenAt: null,
		lastSeenAt: null,
		createdAt: '2026-09-05T22:04:51.000Z',
		...overrides,
	};
}

function pageData() {
	return {
		orgId: ORG_ID,
		orgSlug: 'acme',
		projectId: PROJECT_ID,
		projectName: 'Checkout API',
		// The agent section is off so these assertions are about the two new sections only.
		canManageAgents: false,
		agentSettings: { fixEnabled: false, maxPrsPerDay: null },
		repoConnection: null,
	};
}

function stubFetch(routes: {
	environments?: unknown[];
	members?: unknown[];
	onCall?: (url: string, init?: RequestInit) => void;
}) {
	const calls: { url: string; init?: RequestInit }[] = [];
	const fetchMock = vi.fn(async (url: string, init?: RequestInit) => {
		calls.push({ url, init });
		routes.onCall?.(url, init);
		if (url.startsWith(`/api/organizations/${ORG_ID}/projects/${PROJECT_ID}/environments`)) {
			return { ok: true, status: 200, json: async () => ({ environments: routes.environments ?? [] }) };
		}
		if (url.startsWith(GRANTS_URL)) {
			return { ok: true, status: 200, json: async () => ({ grants: [], members: routes.members ?? [] }) };
		}
		return { ok: true, status: 200, json: async () => ({}) };
	});
	vi.stubGlobal('fetch', fetchMock);
	return { fetchMock, calls };
}

beforeEach(() => {
	vi.restoreAllMocks();
});

afterEach(() => {
	vi.unstubAllGlobals();
});

describe('project settings — Environments (contract §4.1, flows P1 and P4)', () => {
	it('lists a hidden environment and marks it as hidden — the management view is not filtered', async () => {
		// §4.1's `includeHidden=true` is the management view: it returns EVERY row regardless of the
		// caller's scope, because hiding the rows from the person who administers them makes the
		// registry unmanageable by exactly the people this section is for. A hidden environment that
		// simply vanished from this list would look like it had been deleted, which is a thing this
		// feature has no way to do.
		stubFetch({
			environments: [
				environment({ id: 'env-1', name: 'production', displayName: 'Production', isDefault: true }),
				environment({ id: 'env-2', name: 'canary', displayName: 'Canary', hidden: true }),
			],
		});

		render(Page, { data: pageData() });

		await waitFor(() => expect(screen.getByText('Canary')).toBeTruthy());
		const hiddenRow = screen.getByText('Canary').closest('li');
		expect(hiddenRow).toBeTruthy();
		expect(hiddenRow?.textContent).toContain('hidden');
		// And it is offered an Unhide, not a Hide — the control names the state it moves to.
		expect(hiddenRow?.textContent).toContain('Unhide');

		// A project WITH a default does not carry the standing warning; the next test is its inverse.
		expect(screen.queryByTestId('no-default-set')).toBeNull();
	});

	it('shows the standing "no default set" condition when no environment is the default — criterion 12', async () => {
		// Decision 33 made this line the persistent home of decision 11's warning: "no default is set"
		// is a property of the PROJECT, so it belongs where it stays true, while the alert-settings
		// notice is a one-time creation event that nothing persists.
		stubFetch({ environments: [environment({ isDefault: false })] });

		render(Page, { data: pageData() });

		await waitFor(() => expect(screen.getByTestId('no-default-set')).toBeTruthy());
		expect(screen.getByTestId('no-default-set').textContent).toContain('No default set');
	});
});

describe('project settings — Environment access (contract §4.10, flows G1, G3 and G4)', () => {
	const members = [
		{
			userId: 'u-unrestricted',
			displayName: 'Ana Beltre',
			projectRole: null,
			orgRole: 'admin',
			// Decision 31: `null`, not `[]`. `[]` is a state the PUT refuses to store, so using it here
			// would make "sees nothing" and "sees everything" the same value on the wire.
			environments: null,
		},
		{
			userId: 'u-narrowed',
			displayName: 'Sam Okoro',
			projectRole: null,
			orgRole: 'engineer',
			environments: ['staging'],
		},
		{
			userId: 'u-owner',
			displayName: 'Riley Owner',
			projectRole: null,
			orgRole: 'owner',
			environments: null,
		},
	];

	it('reads "All environments" for a member with no grant row, and the list for one with a grant — flow G1', async () => {
		// A blank cell for unrestricted reads as MISSING DATA rather than as a stated fact, and the
		// two rows have to be legible side by side: this screen's whole job is telling them apart.
		stubFetch({ environments: [environment({ name: 'staging', displayName: 'Staging' })], members });

		render(Page, { data: pageData() });

		await waitFor(() => expect(screen.getByText('Ana Beltre')).toBeTruthy());

		const unrestricted = screen.getByText('Ana Beltre').closest('li');
		expect(unrestricted?.querySelector('[data-testid="member-scope"]')?.textContent?.trim()).toBe(
			'All environments'
		);

		const narrowed = screen.getByText('Sam Okoro').closest('li');
		expect(narrowed?.querySelector('[data-testid="member-scope"]')?.textContent?.trim()).toBe(
			'Limited to staging'
		);
	});

	it('offers no limit control at all on an organization owner’s row, with a reason — flow G4', async () => {
		// §4.10 answers this with `422`, and the reason is structural: the person who can undo a
		// narrowing must not be the person narrowed out. A control that renders and then fails is a
		// worse version of the same rule.
		stubFetch({ environments: [environment({ name: 'staging', displayName: 'Staging' })], members });

		render(Page, { data: pageData() });

		await waitFor(() => expect(screen.getByText('Riley Owner')).toBeTruthy());

		const ownerRow = screen.getByText('Riley Owner').closest('li');
		expect(ownerRow?.querySelector('[data-testid="owner-note"]')?.textContent).toContain(
			'cannot be limited to specific environments'
		);
		expect(ownerRow?.textContent).not.toContain('Limit to environments');
		expect(ownerRow?.textContent).not.toContain('Change limit');

		// The non-owner rows still have theirs — the absence is about the role, not about the screen.
		expect(screen.getByText('Ana Beltre').closest('li')?.textContent).toContain(
			'Limit to environments'
		);
	});

	it('refuses a deselect-everything save BEFORE the request goes out — flow G3', async () => {
		// `[]` is a `400` at §4.10, and a member who can see nothing should be removed from the
		// project rather than stored as a grant that hides everything. Catching it here is what stops
		// the reader learning that from a status code.
		const { fetchMock } = stubFetch({
			environments: [environment({ name: 'staging', displayName: 'Staging' })],
			members,
		});

		render(Page, { data: pageData() });
		await waitFor(() => expect(screen.getByText('Sam Okoro')).toBeTruthy());

		const narrowedRow = screen.getByText('Sam Okoro').closest('li') as HTMLElement;
		await fireEvent.click(
			[...narrowedRow.querySelectorAll('button')].find((b) =>
				b.textContent?.includes('Change limit')
			) as HTMLElement
		);

		// The control opens on what the member already holds — `staging` — so unticking it is the
		// deselect-everything the flow describes.
		const checkbox = narrowedRow.querySelector('input[type="checkbox"]') as HTMLInputElement;
		expect(checkbox.checked).toBe(true);
		await fireEvent.click(checkbox);

		const callsBefore = fetchMock.mock.calls.length;
		await fireEvent.click(
			[...narrowedRow.querySelectorAll('button')].find((b) =>
				b.textContent?.includes('Save limit')
			) as HTMLElement
		);

		await waitFor(() =>
			expect(narrowedRow.textContent).toContain('Choose at least one environment')
		);
		// The assertion that matters: nothing was sent. A refusal that still fires the request is a
		// refusal the server made, not this one.
		expect(fetchMock.mock.calls.length).toBe(callsBefore);
	});
});
