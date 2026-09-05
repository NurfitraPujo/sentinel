import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, fireEvent, screen } from '@testing-library/svelte';
import Page from './+page.svelte';
import { escalationHopOneFixture } from '$lib/fixtures/report-detail';

// sentinel-user-report-issue/BUG-07 — the page half of the state-leak finding first fixed as
// BUG-02.
//
// SvelteKit reuses this page component across an `[issueId]` param change, so a client-side
// navigation from one report to another delivers new props to a LIVE instance rather than a fresh
// one. `{#key data.detail.issue.id}` re-creates the markup and every child (that is what resets
// `EscalationStrip`'s local acknowledgement, covered in EscalationStrip.test.ts) but it does NOT
// touch state declared in the page's own `<script>`: `editing`, `editTitle`, `editBodyMd`,
// `editSeverity` and the error banners are reset by the `$effect` keyed on the same id.
//
// Nothing covered that effect. §4.4's `escalated` notification links straight here from
// `NotificationBell.svelte` as a plain `<a>`, so this navigation is on the feature's critical path,
// and an open edit of report A surviving it means Save PATCHes report B with A's draft.
//
// These cases are prop changes on a live instance on purpose — a fresh render cannot observe the
// defect. Delete the `$effect` from `+page.svelte` and both go red.

const REPORT_A = 'issue-report-a';
const REPORT_B = 'issue-report-b';

function makeData(issueId: string, message: string, bodyMd: string) {
	return {
		orgId: 'org-1',
		orgSlug: 'acme',
		userId: 'user-1',
		userRole: 'admin',
		canWrite: true,
		canEditOwnReport: true,
		canDeleteReport: false,
		detail: {
			issue: {
				id: issueId,
				message,
				status: 'unresolved',
				firstSeen: new Date('2026-09-01T00:00:00Z'),
				waitingOn: null,
				assignedTo: null,
				assigneeType: null,
				sourceChannel: 'web',
			},
			bodyMd,
			severity: 'low',
			projectId: 'proj-1',
			projectName: 'Widgets',
			projectIsInbox: false,
			reporterKind: 'org_member',
			reporterDisplayName: 'Alice',
			reporterEmailAddress: 'alice@example.com',
			reporterExternalId: null,
			reporterFirstSeenAt: null,
		},
		relations: [],
		activity: [],
		attachments: [],
		projects: [{ id: 'proj-1', name: 'Widgets', isInbox: false }],
		triage: null,
		escalation: escalationHopOneFixture,
		// eslint-disable-next-line @typescript-eslint/no-explicit-any
	} as any;
}

describe('report detail page — an edit draft belongs to ONE report (BUG-07)', () => {
	beforeEach(() => {
		vi.restoreAllMocks();
		// The page's children (CommentThread, SubscriptionToggle) fetch on mount; nothing here
		// asserts on those calls, they just must not reject.
		vi.stubGlobal(
			'fetch',
			vi.fn().mockResolvedValue({ ok: true, status: 200, json: async () => ({}) })
		);
	});

	it('closes an open edit form when the issue id moves to another report', async () => {
		const { rerender } = render(Page, { data: makeData(REPORT_A, 'Report A title', 'Body A') });

		await fireEvent.click(screen.getByRole('button', { name: 'Edit' }));
		const title = screen.getByLabelText('Title') as HTMLInputElement;
		expect(title.value).toBe('Report A title');
		await fireEvent.input(title, { target: { value: 'a draft nobody saved' } });

		await rerender({ data: makeData(REPORT_B, 'Report B title', 'Body B') });

		// The form is gone, so there is no Save button left to PATCH report B with A's draft.
		expect(screen.queryByLabelText('Title')).toBeNull();
		expect(screen.queryByRole('button', { name: 'Save' })).toBeNull();
		expect(screen.getByRole('button', { name: 'Edit' })).toBeTruthy();
	});

	it('re-seeds the draft from the report on screen, not the one navigated away from', async () => {
		const { rerender } = render(Page, { data: makeData(REPORT_A, 'Report A title', 'Body A') });

		await fireEvent.click(screen.getByRole('button', { name: 'Edit' }));
		await fireEvent.input(screen.getByLabelText('Title'), {
			target: { value: 'a draft nobody saved' },
		});

		await rerender({ data: makeData(REPORT_B, 'Report B title', 'Body B') });
		await fireEvent.click(screen.getByRole('button', { name: 'Edit' }));

		expect((screen.getByLabelText('Title') as HTMLInputElement).value).toBe('Report B title');
		expect((screen.getByLabelText('Description') as HTMLTextAreaElement).value).toBe('Body B');
	});
});
