import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, fireEvent, screen } from '@testing-library/svelte';
import TriagePanel from './TriagePanel.svelte';
import {
	suggestionCandidateFixture,
	triageNotRunFixture,
	triageWithCandidatesFixture,
	triageWithNoMatchesFixture,
	writtenLinkCandidateFixture,
} from '$lib/fixtures/report-detail';

// sentinel-user-report-issue §4.2/§4.3, fe.md rows S2/S3/S4/S6.
//
// Two things are asserted here that a "does it render" test would miss, and both are defects the
// contract names explicitly:
//
//   1. `triage: null` and `triage: {candidates: []}` are DIFFERENT STATES. §4.2: "`null` and
//      `{"candidates": []}` mean different things and a client must render them differently".
//      Asserting only that each renders some text would pass against a shared empty-state component
//      handed a different string, which is exactly what fe.md says is not enough. So the assertions
//      below are that the two states produce DIFFERENT ELEMENTS: the not-run branch renders
//      `triage-not-run` and no `triage-no-matches`, and vice versa.
//   2. Confirming a suggestion goes to the EXISTING relations route. §4.3 writes a link only above
//      the 0.7 threshold and never writes a suggestion; the confirm affordance therefore has to
//      POST /api/issues/{issueId}/relations, whose writer is the session user, so the relation and
//      its activity row are attributed to the human. A route this feature invented would attribute
//      them to `system` and lose the distinction the whole panel exists to draw.

const ISSUE_ID = 'report-issue-id';

describe('TriagePanel — the three states are three renderings (S2, S3, S4)', () => {
	it('renders "not triaged yet" for triage === null, and NOT the no-matches rendering', () => {
		render(TriagePanel, { issueId: ISSUE_ID, triage: triageNotRunFixture });

		expect(screen.getByTestId('triage-not-run')).toBeTruthy();
		expect(screen.queryByTestId('triage-no-matches')).toBeNull();
		expect(screen.queryByTestId('triage-candidates')).toBeNull();
		expect(screen.getByTestId('triage-not-run').textContent).toContain('Not triaged yet');
	});

	it('renders "no matching service errors" for an empty candidate list, and NOT the not-run rendering', () => {
		render(TriagePanel, { issueId: ISSUE_ID, triage: triageWithNoMatchesFixture });

		expect(screen.getByTestId('triage-no-matches')).toBeTruthy();
		expect(screen.queryByTestId('triage-not-run')).toBeNull();
		expect(screen.queryByTestId('triage-candidates')).toBeNull();
		expect(screen.getByTestId('triage-no-matches').textContent).toContain(
			'No matching service errors'
		);
	});

	// The S3-vs-S4 distinction, asserted as a distinction rather than as two strings: the two states
	// do not share a test id, and the run that happened shows its own metadata while the run that
	// did not happen shows none. Collapse the two branches into one and this fails.
	it('the two empty states share no element and only the run that happened reports its strategy', () => {
		const notRun = render(TriagePanel, { issueId: ISSUE_ID, triage: triageNotRunFixture });
		const notRunHtml = notRun.container.innerHTML;
		notRun.unmount();

		const noMatches = render(TriagePanel, { issueId: ISSUE_ID, triage: triageWithNoMatchesFixture });
		const noMatchesHtml = noMatches.container.innerHTML;

		expect(notRunHtml).not.toBe(noMatchesHtml);
		expect(notRunHtml).not.toContain('triage-no-matches');
		expect(noMatchesHtml).not.toContain('triage-not-run');
		// `strategy` is a property of a run. The not-run branch must not claim one.
		expect(noMatchesHtml).toContain(triageWithNoMatchesFixture.strategy);
		expect(notRunHtml).not.toContain(triageWithNoMatchesFixture.strategy);
	});

	it('lists each candidate with its message, error class, signals and score as a rank — never a percentage', () => {
		render(TriagePanel, { issueId: ISSUE_ID, triage: triageWithCandidatesFixture });

		expect(screen.getByTestId('triage-candidates')).toBeTruthy();
		expect(screen.getByText(writtenLinkCandidateFixture.message)).toBeTruthy();
		expect(screen.getByText(writtenLinkCandidateFixture.errorClass)).toBeTruthy();
		for (const signal of writtenLinkCandidateFixture.signals) {
			expect(screen.getAllByText(signal).length).toBeGreaterThan(0);
		}

		const scores = screen.getAllByTestId('candidate-score').map((el) => el.textContent ?? '');
		expect(scores[0]).toContain('0.80');
		expect(scores[0]).toContain('in this report');
		// §4.2: a rank, not a probability. "80%" invites the cross-report comparison the contract
		// says is meaningless, so no rendering of a score may carry a percent sign.
		expect(scores.join(' ')).not.toContain('%');
	});
});

describe('TriagePanel — a suggestion is not a link (S6)', () => {
	beforeEach(() => {
		vi.restoreAllMocks();
	});

	it('marks a linked candidate and a suggestion with different elements and different words', () => {
		render(TriagePanel, { issueId: ISSUE_ID, triage: triageWithCandidatesFixture });

		const linked = screen.getByTestId('triage-candidate-linked');
		const suggestion = screen.getByTestId('triage-candidate-suggestion');

		expect(linked.textContent).toContain(writtenLinkCandidateFixture.message);
		expect(suggestion.textContent).toContain(suggestionCandidateFixture.message);
		expect(screen.getByTestId('candidate-tag-linked').textContent?.trim()).toBe('linked');
		expect(screen.getByTestId('candidate-tag-suggestion').textContent).toContain('not linked');
		// The suggestion says out loud that nothing has been written yet.
		expect(suggestion.textContent).toContain('Nothing has been written');
	});

	it('offers the confirm affordance on the suggestion only, and only to a write role', () => {
		const writer = render(TriagePanel, {
			issueId: ISSUE_ID,
			triage: triageWithCandidatesFixture,
			canWrite: true,
		});
		// One confirm control, on the suggestion — never on the candidate that is already linked.
		const confirms = screen.getAllByTestId('candidate-confirm');
		expect(confirms).toHaveLength(1);
		expect(screen.getByTestId('triage-candidate-linked').querySelector('button')).toBeNull();
		writer.unmount();

		render(TriagePanel, {
			issueId: ISSUE_ID,
			triage: triageWithCandidatesFixture,
			canWrite: false,
		});
		expect(screen.queryByTestId('candidate-confirm')).toBeNull();
	});

	// The attribution assertion. The URL and the body are the whole point: this is the route whose
	// writer is the session user, so the `issue_relations` row lands with `created_by_type = 'user'`
	// and the `issue_activity` row names the human. Point this at anything else and the relation is
	// attributed to `system`, which is the state a suggestion exists to avoid.
	it('confirming a suggestion POSTs linked_to to the EXISTING /api/issues/{issueId}/relations route', async () => {
		const fetchMock = vi.fn().mockResolvedValue({ ok: true, json: async () => ({ success: true }) });
		vi.stubGlobal('fetch', fetchMock);

		render(TriagePanel, {
			issueId: ISSUE_ID,
			triage: triageWithCandidatesFixture,
			canWrite: true,
		});

		await fireEvent.click(screen.getByTestId('candidate-confirm'));
		await Promise.resolve();

		expect(fetchMock).toHaveBeenCalledWith(
			`/api/issues/${ISSUE_ID}/relations`,
			expect.objectContaining({
				method: 'POST',
				body: JSON.stringify({
					targetIssueId: suggestionCandidateFixture.issueId,
					relationType: 'linked_to',
				}),
			})
		);
	});

	it('surfaces a failed confirm instead of silently leaving the suggestion unconfirmed', async () => {
		const fetchMock = vi
			.fn()
			.mockResolvedValue({ ok: false, status: 403, json: async () => ({ message: 'Forbidden' }) });
		vi.stubGlobal('fetch', fetchMock);

		render(TriagePanel, {
			issueId: ISSUE_ID,
			triage: triageWithCandidatesFixture,
			canWrite: true,
		});

		await fireEvent.click(screen.getByTestId('candidate-confirm'));
		await Promise.resolve();
		await Promise.resolve();

		expect(await screen.findByRole('alert')).toBeTruthy();
		expect(screen.getByRole('alert').textContent).toContain('Forbidden');
	});
});
