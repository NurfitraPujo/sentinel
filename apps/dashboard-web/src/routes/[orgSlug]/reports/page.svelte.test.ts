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
		source: 'all' as const,
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
				severity: 'medium',
				projectName: 'Widgets',
				projectIsInbox: false,
				reporterKind: 'org_member',
				reporterDisplayName: 'Alice',
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

// sentinel-user-report-issue OQ-8 / fe.md rows L1-L4. The badge and the filter, on the page rather
// than on the component in isolation: what these two describe is a RELATIONSHIP between rows and
// between controls, and neither is visible from a single component.

function makeRow(overrides: Record<string, unknown> = {}) {
	return {
		issue: {
			id: 'issue-ext',
			message: 'The save button spins forever',
			status: 'unresolved',
			firstSeen: new Date('2026-09-04T00:00:00Z'),
			waitingOn: null,
			assigneeType: null,
			assignedTo: null,
			claimedAt: null,
			sourceChannel: 'user_report_api',
		},
		report: null,
		severity: 'high',
		projectName: 'Widgets',
		projectIsInbox: false,
		reporterKind: 'external',
		reporterDisplayName: 'Sarah K.',
		reporterName: null,
		reporterEmail: null,
		...overrides,
	};
}

function makeListData(rows: unknown[], tab = 'all', source = 'all') {
	return {
		session: null,
		orgId: 'org-1',
		orgSlug: 'acme',
		tab,
		source,
		userId: 'user-1',
		reports: rows,
	} as any;
}

describe('reports list source badge (L1, L2)', () => {
	it('badges the externally-sourced row and leaves the org-member row unbadged, in the same list', () => {
		const orgMemberRow = makeRow({
			issue: {
				...makeRow().issue,
				id: 'issue-org',
				message: 'Filed by a colleague',
				sourceChannel: 'manual_support',
			},
			reporterKind: 'org_member',
			reporterDisplayName: 'Alice',
			report: { severity: 'low' },
			severity: 'low',
		});
		render(Page, { data: makeListData([makeRow(), orgMemberRow]) });

		// One badge across two rows: marking both would be marking neither.
		expect(screen.getAllByTestId('source-badge')).toHaveLength(1);
		expect(screen.getByText('The save button spins forever')).toBeTruthy();
		expect(screen.getByText('Filed by a colleague')).toBeTruthy();
	});

	it('renders an external row at all — no manual_issue_reports companion, so `report` is null', () => {
		render(Page, { data: makeListData([makeRow()]) });

		// The severity cell reads the unified field, not `row.report.severity`, which is null here.
		expect(screen.getAllByText('high').length).toBeGreaterThan(0);
		expect(screen.getByText('Sarah K.')).toBeTruthy();
	});
});

describe('reports list source filter (L3, L4)', () => {
	it('renders the filter as its own control, not as a seventh tab', () => {
		render(Page, { data: makeListData([makeRow()]) });

		const filter = screen.getByTestId('source-filter');
		expect(filter).toBeTruthy();
		// It is outside the tab strip, and the tab strip still has exactly its six tabs.
		const tabs = screen.getByRole('navigation', { name: 'Report filters' });
		expect(tabs.contains(filter)).toBe(false);
		expect(tabs.querySelectorAll('a')).toHaveLength(6);
	});

	it('writes BOTH tab and source into every link, so switching one never drops the other', () => {
		render(Page, { data: makeListData([makeRow()], 'unclaimed', 'external') });

		// A source option keeps the current tab...
		expect(screen.getByTestId('source-option-org-members').getAttribute('href')).toBe(
			'/acme/reports?tab=unclaimed&source=org-members'
		);
		// ...and a tab keeps the current source. This is L4's composition, from the page's side.
		expect(screen.getByRole('link', { name: 'Triage' }).getAttribute('href')).toBe(
			'/acme/reports?tab=triage&source=external'
		);
	});

	it('marks the selected source, and only that one', () => {
		render(Page, { data: makeListData([makeRow()], 'all', 'external') });

		expect(screen.getByTestId('source-option-external').getAttribute('aria-current')).toBe('true');
		expect(screen.getByTestId('source-option-all').getAttribute('aria-current')).toBeNull();
		expect(screen.getByTestId('source-option-org-members').getAttribute('aria-current')).toBeNull();
	});
});
