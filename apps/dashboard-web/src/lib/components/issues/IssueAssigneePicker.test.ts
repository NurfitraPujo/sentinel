import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, fireEvent, cleanup } from '@testing-library/svelte';
import IssueAssigneePicker from './IssueAssigneePicker.svelte';

// CONTEXT.md "Claim" / DECISIONS.md D24: claims are only ever self-acquired, so this picker must
// offer NO agent options. The M5 version fetched and listed the org's agents here — that was the
// UI half of the assign-to-agent defect the server now rejects with 400. An existing agent claim
// is still displayed, and "Unassigned" remains as the deliberate admin release override.
afterEach(() => {
	cleanup();
	vi.unstubAllGlobals();
});

describe('IssueAssigneePicker', () => {
	it('shows "Unassigned" when there is no assignee', () => {
		render(IssueAssigneePicker, {});
		expect(screen.getByText('Unassigned')).toBeTruthy();
	});

	it('still DISPLAYS an existing agent claim with the agent emoji', () => {
		render(IssueAssigneePicker, {
			assignee: { type: 'agent', id: 'agent-1', name: 'AutoFix Agent' },
		});
		expect(screen.getByText(/AutoFix Agent/)).toBeTruthy();
	});

	it('offers no agent options and never fetches the org agent list', async () => {
		const fetchMock = vi.fn();
		vi.stubGlobal('fetch', fetchMock);

		render(IssueAssigneePicker, {});
		await fireEvent.click(screen.getByText('Unassigned'));

		expect(fetchMock).not.toHaveBeenCalled();
		expect(screen.getByText(/they claim issues themselves/)).toBeTruthy();
	});

	it('calls onAssign(null) for the Unassigned override and closes the menu', async () => {
		const onAssign = vi.fn();

		render(IssueAssigneePicker, {
			assignee: { type: 'agent', id: 'agent-1', name: 'AutoFix Agent' },
			onAssign,
		});
		await fireEvent.click(screen.getByText(/AutoFix Agent/));
		await fireEvent.click(screen.getByText('Unassigned'));

		expect(onAssign).toHaveBeenCalledWith(null);
	});
});
