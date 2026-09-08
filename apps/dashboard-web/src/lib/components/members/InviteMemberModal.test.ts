import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/svelte';
import InviteMemberModal from './InviteMemberModal.svelte';

// sentinel-project-environments task 15, case 7 — and it is the case that matters most on this
// screen. Decision 28 is (c): a new member gets no grant row and is unrestricted unless the inviter
// explicitly chooses otherwise, so the untouched control must send NO `environmentScopes` field at
// all. A control that always sends a value has quietly made this (b) — the option the operator
// rejected — and would narrow people by machine on a screen where nobody meant to.
//
// The positive case is asserted beside it deliberately: a test that only proves the field is absent
// cannot tell "correctly omitted" from "never implemented".

const ORG_ID = 'org-1';

function stubFetch() {
	const fetchMock = vi.fn(async (url: string, init?: RequestInit) => {
		void init;
		if (url === `/api/organizations/${ORG_ID}/projects`) {
			return {
				ok: true,
				status: 200,
				json: async () => ({ projects: [{ id: 'proj-1', name: 'Checkout API' }] }),
			};
		}
		if (url.startsWith(`/api/organizations/${ORG_ID}/projects/proj-1/environments`)) {
			return {
				ok: true,
				status: 200,
				json: async () => ({
					environments: [
						{ id: 'env-1', name: 'staging', displayName: 'Staging', isDefault: false, hidden: false },
						{ id: 'env-2', name: 'production', displayName: 'Production', isDefault: true, hidden: false },
					],
				}),
			};
		}
		return { ok: true, status: 201, json: async () => ({ id: 'inv-1', email: 'x@y.z', role: 'engineer' }) };
	});
	vi.stubGlobal('fetch', fetchMock);
	return fetchMock;
}

function invitationBody(fetchMock: ReturnType<typeof stubFetch>) {
	const call = fetchMock.mock.calls.find(
		([url, init]) => url === `/api/organizations/${ORG_ID}/invitations` && init?.method === 'POST'
	);
	expect(call).toBeTruthy();
	return JSON.parse(call?.[1]?.body as string);
}

beforeEach(() => {
	vi.restoreAllMocks();
});

afterEach(() => {
	vi.unstubAllGlobals();
});

describe('InviteMemberModal environment scope (contract §4.10a, flows I1-I3, I5)', () => {
	it('sends NO environmentScopes field when nobody touches the control — flow I1, the common path', async () => {
		const fetchMock = stubFetch();
		render(InviteMemberModal, { show: true, orgId: ORG_ID });

		await fireEvent.input(screen.getByLabelText(/email address/i), {
			target: { value: 'sam@example.com' },
		});
		await fireEvent.click(screen.getByRole('button', { name: /generate invitation/i }));

		await waitFor(() => expect(fetchMock).toHaveBeenCalled());
		const body = invitationBody(fetchMock);

		// Not `[]`, not `null` — ABSENT. `environment_scopes` stays `NULL` on the invitation row, no
		// grant row is written at acceptance, and the member is unrestricted.
		expect('environmentScopes' in body).toBe(false);
		expect(body).toEqual({ email: 'sam@example.com', role: 'engineer' });
	});

	it('sends exactly the chosen scope, one entry per project, when the inviter picks one — flow I2', async () => {
		const fetchMock = stubFetch();
		render(InviteMemberModal, { show: true, orgId: ORG_ID });

		await fireEvent.input(screen.getByLabelText(/email address/i), {
			target: { value: 'sam@example.com' },
		});

		await waitFor(() => expect(screen.getByText('Checkout API')).toBeTruthy());
		await fireEvent.click(screen.getByText('Checkout API'));
		await waitFor(() => expect(screen.getByLabelText('staging')).toBeTruthy());

		// The full list is offered, `production` included — decision 39 reversed the earlier rule that
		// filtered it to the inviter's own scope, because §4.10's grant PUT is not scope-guarded and
		// would accept exactly that one screen later.
		expect(screen.getByLabelText('production')).toBeTruthy();

		await fireEvent.click(screen.getByLabelText('staging'));
		await fireEvent.click(screen.getByRole('button', { name: /generate invitation/i }));

		await waitFor(() => expect(invitationBody(fetchMock).environmentScopes).toBeTruthy());
		expect(invitationBody(fetchMock).environmentScopes).toEqual([
			{ projectId: 'proj-1', environments: ['staging'] },
		]);
	});

	it('explains an empty registry instead of offering an empty multi-select — flow I3', async () => {
		// Under decision 8 this is every project on day one, so it is the state an inviter meets
		// first. An empty multi-select here reads as broken software.
		const fetchMock = vi.fn(async (url: string, init?: RequestInit) => {
			void init;
			if (url === `/api/organizations/${ORG_ID}/projects`) {
				return { ok: true, status: 200, json: async () => ({ projects: [{ id: 'proj-1', name: 'Checkout API' }] }) };
			}
			return { ok: true, status: 200, json: async () => ({ environments: [] }) };
		});
		vi.stubGlobal('fetch', fetchMock);

		render(InviteMemberModal, { show: true, orgId: ORG_ID });
		await waitFor(() => expect(screen.getByText('Checkout API')).toBeTruthy());
		await fireEvent.click(screen.getByText('Checkout API'));

		await waitFor(() => expect(screen.getByTestId('invite-registry-empty')).toBeTruthy());
		expect(screen.getByTestId('invite-registry-empty').textContent).toContain(
			'No environments have been declared in this project yet'
		);
		expect(document.querySelectorAll('input[type="checkbox"]').length).toBe(0);
	});

	it('renders no control at all for an organization owner, with a one-line reason — flow I5', async () => {
		stubFetch();
		render(InviteMemberModal, { show: true, orgId: ORG_ID });

		await fireEvent.change(screen.getByLabelText(/organization role/i), {
			target: { value: 'owner' },
		});

		await waitFor(() => expect(screen.getByTestId('owner-scope-note')).toBeTruthy());
		expect(screen.getByTestId('owner-scope-note').textContent).toContain(
			'cannot be limited to specific environments'
		);
		expect(screen.queryByText('Checkout API')).toBeNull();
	});
});
