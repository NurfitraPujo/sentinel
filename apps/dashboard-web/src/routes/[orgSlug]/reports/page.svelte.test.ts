import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/svelte';
import Page from './+page.svelte';

// N7e (A07, docs/plans/AGENT_AUTOMATION_REMEDIATION_PLAN.md): UI-only "agent working" visibility
// -- no new status. Asserts the badge renders when a row's issue is currently claimed by an
// agent (assigneeType='agent'), and does NOT render for a user-claimed or unclaimed row. Guards
// against deleting `isAgentWorking`'s `{#if}` in +page.svelte: that would make this test fail red
// (badge never renders) while a status-flag test alone would stay green.

function makeData(overrides: { assigneeType: string | null; assignedTo: string | null; claimedAt: Date | null }) {
	return {
		session: null,
		orgId: 'org-1',
		orgSlug: 'acme',
		tab: 'all' as const,
		userId: 'user-1',
		reports: [
			{
				issue: {
					id: 'issue-1',
					message: 'Something broke',
					status: 'unresolved',
					firstSeen: new Date('2026-08-01T00:00:00Z'),
					waitingOn: null,
					assigneeType: overrides.assigneeType,
					assignedTo: overrides.assignedTo,
					claimedAt: overrides.claimedAt,
				},
				report: { severity: 'medium' },
				projectName: 'Widgets',
				projectIsInbox: false,
				reporterName: 'Alice',
				reporterEmail: 'alice@example.com',
			},
		],
	};
}

describe('reports list "agent working" badge (A07)', () => {
	it('renders the badge with a claimedAt tooltip when an agent currently holds the claim', () => {
		const claimedAt = new Date('2026-08-14T10:00:00Z');
		render(Page, { data: makeData({ assigneeType: 'agent', assignedTo: 'agent-1', claimedAt }) });

		const badge = screen.getByTestId('agent-working-badge');
		expect(badge).toBeTruthy();
		expect(badge.getAttribute('title')).toContain('Agent working');
		expect(badge.getAttribute('title')).toContain(claimedAt.toLocaleString());
	});

	it('does not render the badge for a user-claimed issue', () => {
		render(Page, { data: makeData({ assigneeType: 'user', assignedTo: 'user-1', claimedAt: new Date() }) });
		expect(screen.queryByTestId('agent-working-badge')).toBeNull();
	});

	it('does not render the badge for an unclaimed issue', () => {
		render(Page, { data: makeData({ assigneeType: null, assignedTo: null, claimedAt: null }) });
		expect(screen.queryByTestId('agent-working-badge')).toBeNull();
	});
});
