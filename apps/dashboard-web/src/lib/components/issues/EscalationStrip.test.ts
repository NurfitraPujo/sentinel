import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, fireEvent, screen } from '@testing-library/svelte';
import EscalationStrip from './EscalationStrip.svelte';
import {
	escalationAcknowledgedFixture,
	escalationExhaustedFixture,
	escalationHopOneFixture,
	escalationHopTwoFixture,
} from '$lib/fixtures/report-detail';

// sentinel-user-report-issue §4.4/§4.2, fe.md rows S8–S12.
//
// The load-bearing assertion is the Acknowledge gate, and it is asserted as ABSENCE FROM THE DOM,
// not as a disabled attribute: fe.md row S10 says "a non-assignee sees no Acknowledge control at
// all", and a control that is present-but-disabled still tells a reader they might be allowed to
// press it later. `queryByTestId(...)` returning null is that requirement; `toBeDisabled()` would
// pass against the defect.
//
// The second is that `escalation === null` renders NOTHING. §4.2 says null and a value mean
// different things; a strip that says "not escalated" would report a state the feature never
// entered, and would read as an ordinary assignment for a report that simply has no rota.

const ISSUE_ID = 'report-issue-id';
const ASSIGNEE = escalationHopOneFixture.assigneeId;
const SOMEONE_ELSE = 'usr_not_the_assignee';

describe('EscalationStrip — null is not a state to draw', () => {
	it('renders no strip at all when escalation is null', () => {
		render(EscalationStrip, {
			issueId: ISSUE_ID,
			escalation: null,
			currentUserId: ASSIGNEE,
		});

		expect(screen.queryByTestId('escalation-strip')).toBeNull();
		expect(screen.queryByTestId('escalation-acknowledge')).toBeNull();
	});
});

describe('EscalationStrip — the Acknowledge control is the assignee’s alone (S9, S10)', () => {
	beforeEach(() => {
		vi.restoreAllMocks();
	});

	it('renders the control for the current assignee', () => {
		render(EscalationStrip, {
			issueId: ISSUE_ID,
			escalation: escalationHopOneFixture,
			currentUserId: ASSIGNEE,
		});

		expect(screen.getByTestId('escalation-acknowledge')).toBeTruthy();
	});

	it('leaves the control ABSENT FROM THE DOM for another org member on the same report', () => {
		render(EscalationStrip, {
			issueId: ISSUE_ID,
			escalation: escalationHopOneFixture,
			currentUserId: SOMEONE_ELSE,
		});

		// Not "disabled", not "hidden" — absent. The strip itself still renders: the non-assignee is
		// entitled to see who holds it and by when.
		expect(screen.getByTestId('escalation-strip')).toBeTruthy();
		expect(screen.queryByTestId('escalation-acknowledge')).toBeNull();
	});

	it('POSTs to the ack route and drops the control and the countdown once acknowledged', async () => {
		const fetchMock = vi.fn().mockResolvedValue({ ok: true, json: async () => ({ ok: true }) });
		vi.stubGlobal('fetch', fetchMock);

		render(EscalationStrip, {
			issueId: ISSUE_ID,
			escalation: escalationHopOneFixture,
			currentUserId: ASSIGNEE,
		});

		expect(screen.getByTestId('escalation-countdown')).toBeTruthy();
		await fireEvent.click(screen.getByTestId('escalation-acknowledge'));
		await Promise.resolve();

		expect(fetchMock).toHaveBeenCalledWith(
			`/api/issues/${ISSUE_ID}/escalation/ack`,
			expect.objectContaining({ method: 'POST' })
		);
		expect(await screen.findByTestId('escalation-acknowledged')).toBeTruthy();
		expect(screen.queryByTestId('escalation-acknowledge')).toBeNull();
		expect(screen.queryByTestId('escalation-countdown')).toBeNull();
	});

	it('shows no control and no countdown for an already-acknowledged escalation', () => {
		render(EscalationStrip, {
			issueId: ISSUE_ID,
			escalation: escalationAcknowledgedFixture,
			currentUserId: ASSIGNEE,
		});

		expect(screen.getByTestId('escalation-acknowledged')).toBeTruthy();
		expect(screen.queryByTestId('escalation-acknowledge')).toBeNull();
		expect(screen.queryByTestId('escalation-countdown')).toBeNull();
	});
});

describe('EscalationStrip — the local acknowledgement belongs to ONE report (BUG-02)', () => {
	beforeEach(() => {
		vi.restoreAllMocks();
	});

	// SvelteKit reuses the `[orgSlug]/reports/[issueId]` page component across a param change, so
	// this component sees new props rather than a fresh instance. A bare `acknowledgedLocally`
	// boolean survives that, and §4.4's `escalated` notification links straight to this page as a
	// plain `<a>`, so the navigation is on the feature's own critical path: report B would render
	// "Acknowledged" — with no control and no countdown — on the strength of an ack for report A.
	//
	// This test is a prop change on a LIVE instance on purpose. Every other case in this file
	// renders fresh, which is exactly the shape that cannot observe the defect.
	it('drops the local ack when the issueId prop moves to another report', async () => {
		const fetchMock = vi.fn().mockResolvedValue({ ok: true, json: async () => ({ ok: true }) });
		vi.stubGlobal('fetch', fetchMock);

		const { rerender } = render(EscalationStrip, {
			issueId: ISSUE_ID,
			escalation: escalationHopOneFixture,
			currentUserId: ASSIGNEE,
		});

		await fireEvent.click(screen.getByTestId('escalation-acknowledge'));
		expect(await screen.findByTestId('escalation-acknowledged')).toBeTruthy();

		// Navigate to a different, un-acknowledged report while the instance is reused.
		await rerender({
			issueId: 'a-different-report',
			escalation: escalationHopOneFixture,
			currentUserId: ASSIGNEE,
		});

		expect(screen.queryByTestId('escalation-acknowledged')).toBeNull();
		expect(screen.getByTestId('escalation-live')).toBeTruthy();
		expect(screen.getByTestId('escalation-acknowledge')).toBeTruthy();
		expect(screen.getByTestId('escalation-countdown')).toBeTruthy();

		// And the ack it then sends names the report on screen, not the one it came from.
		await fireEvent.click(screen.getByTestId('escalation-acknowledge'));
		expect(fetchMock).toHaveBeenLastCalledWith(
			'/api/issues/a-different-report/escalation/ack',
			expect.objectContaining({ method: 'POST' })
		);
	});

	it('keeps the local ack across an unrelated prop change on the SAME report', async () => {
		vi.stubGlobal(
			'fetch',
			vi.fn().mockResolvedValue({ ok: true, json: async () => ({ ok: true }) })
		);

		const { rerender } = render(EscalationStrip, {
			issueId: ISSUE_ID,
			escalation: escalationHopOneFixture,
			currentUserId: ASSIGNEE,
		});

		await fireEvent.click(screen.getByTestId('escalation-acknowledge'));
		expect(await screen.findByTestId('escalation-acknowledged')).toBeTruthy();

		// The page polls at ~10 s (fe.md); a re-render with the same report must not undo the ack.
		await rerender({
			issueId: ISSUE_ID,
			escalation: escalationHopOneFixture,
			currentUserId: ASSIGNEE,
			waitingOn: 'assignee',
		});

		expect(screen.getByTestId('escalation-acknowledged')).toBeTruthy();
		expect(screen.queryByTestId('escalation-acknowledge')).toBeNull();
	});
});

describe('EscalationStrip — the hop chain and its terminal state (S11, S12)', () => {
	it('renders "escalated N of 3" at each hop', () => {
		const first = render(EscalationStrip, {
			issueId: ISSUE_ID,
			escalation: escalationHopOneFixture,
			currentUserId: ASSIGNEE,
		});
		expect(screen.getByTestId('escalation-hop').textContent).toContain('escalated 1 of 3');
		first.unmount();

		render(EscalationStrip, {
			issueId: ISSUE_ID,
			escalation: escalationHopTwoFixture,
			currentUserId: escalationHopTwoFixture.assigneeId,
		});
		expect(screen.getByTestId('escalation-hop').textContent).toContain('escalated 2 of 3');
		// The reassigned hop names the NEW assignee, not the one it moved off.
		expect(screen.getByTestId('escalation-strip').textContent).toContain(
			escalationHopTwoFixture.assigneeId
		);
	});

	it('renders exhaustion as its own state, not as an ordinary assignment', () => {
		render(EscalationStrip, {
			issueId: ISSUE_ID,
			escalation: escalationExhaustedFixture,
			currentUserId: escalationExhaustedFixture.assigneeId,
		});

		expect(screen.getByTestId('escalation-exhausted').textContent).toContain(
			'Nobody acknowledged'
		);
		// Not the live rendering, and no acknowledge control even for the last engineer tried: the
		// chain has stopped, and offering the button would suggest pressing it restarts something.
		expect(screen.queryByTestId('escalation-live')).toBeNull();
		expect(screen.queryByTestId('escalation-acknowledge')).toBeNull();
		expect(screen.queryByTestId('escalation-countdown')).toBeNull();
		expect(screen.getByTestId('escalation-strip').className).toContain('escalation-exhausted');
	});
});

describe('EscalationStrip — the waiting_on trap is surfaced, not fixed', () => {
	it('says the external reporter cannot reply when the report is waiting on them', () => {
		render(EscalationStrip, {
			issueId: ISSUE_ID,
			escalation: null,
			currentUserId: ASSIGNEE,
			waitingOn: 'reporter',
			reporterIsExternal: true,
		});

		const trap = screen.getByTestId('waiting-on-reporter-trap');
		expect(trap.textContent?.replace(/\s+/g, ' ')).toContain('no reply channel');
		expect(trap.textContent).toContain('will not clear on its own');
	});

	it('says nothing for an org-member reporter, who can reply', () => {
		render(EscalationStrip, {
			issueId: ISSUE_ID,
			escalation: null,
			currentUserId: ASSIGNEE,
			waitingOn: 'reporter',
			reporterIsExternal: false,
		});

		expect(screen.queryByTestId('waiting-on-reporter-trap')).toBeNull();
	});
});
