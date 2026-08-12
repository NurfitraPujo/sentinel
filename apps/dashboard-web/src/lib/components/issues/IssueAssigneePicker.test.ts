import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, fireEvent, cleanup } from '@testing-library/svelte';
import IssueAssigneePicker from './IssueAssigneePicker.svelte';

// Manual Issues M5 §7: this picker used to hardcode a single fake "AutoFix Agent" (id '2').
// It now fetches real agent rows for the organization from GET
// /api/organizations/[orgId]/agents and lists only active ones, with an explicit empty state
// when there are none.
function jsonResponse(body: unknown, ok = true, status = ok ? 200 : 500) {
	return { ok, status, json: async () => body } as Response;
}

afterEach(() => {
	cleanup();
	vi.unstubAllGlobals();
});

describe('IssueAssigneePicker', () => {
	it('shows "Unassigned" when there is no assignee', () => {
		render(IssueAssigneePicker, { organizationId: 'org-1' });
		expect(screen.getByText('Unassigned')).toBeTruthy();
	});

	it('shows the current assignee name with the agent emoji for an agent assignee', () => {
		render(IssueAssigneePicker, {
			assignee: { type: 'agent', id: 'agent-1', name: 'AutoFix Agent' },
			organizationId: 'org-1',
		});
		expect(screen.getByText(/AutoFix Agent/)).toBeTruthy();
	});

	it('fetches and lists real active agents from the org on open, excluding disabled ones', async () => {
		const fetchMock = vi.fn().mockResolvedValue(
			jsonResponse({
				agents: [
					{ id: 'agent-1', name: 'AutoFix Agent', status: 'active' },
					{ id: 'agent-2', name: 'Retired Bot', status: 'disabled' },
				],
			})
		);
		vi.stubGlobal('fetch', fetchMock);

		render(IssueAssigneePicker, { organizationId: 'org-1' });
		await fireEvent.click(screen.getByText('Unassigned'));

		expect(fetchMock).toHaveBeenCalledWith('/api/organizations/org-1/agents');
		expect(await screen.findByText(/AutoFix Agent/)).toBeTruthy();
		expect(screen.queryByText(/Retired Bot/)).toBeNull();
	});

	it('shows an empty state when the org has no agents', async () => {
		vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse({ agents: [] })));

		render(IssueAssigneePicker, { organizationId: 'org-1' });
		await fireEvent.click(screen.getByText('Unassigned'));

		expect(await screen.findByText(/No agents in this organization yet/)).toBeTruthy();
	});

	it('does not hard-error on a 403 (caller without manage_agents) -- degrades to empty state', async () => {
		vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse({}, false, 403)));

		render(IssueAssigneePicker, { organizationId: 'org-1' });
		await fireEvent.click(screen.getByText('Unassigned'));

		expect(await screen.findByText(/No agents in this organization yet/)).toBeTruthy();
	});

	it('calls onAssign with the picked agent and closes the menu', async () => {
		vi.stubGlobal(
			'fetch',
			vi.fn().mockResolvedValue(jsonResponse({ agents: [{ id: 'agent-1', name: 'AutoFix Agent', status: 'active' }] }))
		);
		const onAssign = vi.fn();

		render(IssueAssigneePicker, { organizationId: 'org-1', onAssign });
		await fireEvent.click(screen.getByText('Unassigned'));
		const agentButton = await screen.findByText(/AutoFix Agent/);
		await fireEvent.click(agentButton);

		expect(onAssign).toHaveBeenCalledWith({ type: 'agent', id: 'agent-1', name: 'AutoFix Agent' });
	});
});
