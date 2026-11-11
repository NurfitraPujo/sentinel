import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, fireEvent, screen } from '@testing-library/svelte';
import IssueRelations from './IssueRelations.svelte';

// D11: unlinking an INCOMING relation always 404s. The DELETE handler at
// /api/issues/[issueId]/relations treats params.issueId as the relation's SOURCE and the body's
// targetIssueId as its TARGET, matching (source, target, relationType) exactly. For an incoming
// relation the stored row is (source=other, target=current) — calling the endpoint at
// /api/issues/{currentIssueId}/relations, as the component used to unconditionally, put
// currentIssueId in the source slot no matter what targetIssueId was sent, which can never match an
// incoming row.
describe('IssueRelations unlink (D11)', () => {
	const CURRENT = 'current-issue-id';
	const OTHER = 'other-issue-id';

	beforeEach(() => {
		vi.restoreAllMocks();
	});

	it('unlinking an OUTGOING relation DELETEs on the current issue, with the target as targetIssueId', async () => {
		const fetchMock = vi.fn().mockResolvedValue({ ok: true, json: async () => ({ success: true }) });
		vi.stubGlobal('fetch', fetchMock);

		render(IssueRelations, {
			currentIssueId: CURRENT,
			initialRelations: [
				{
					id: 'rel-1',
					sourceIssueId: CURRENT,
					targetIssueId: OTHER,
					relationType: 'linked_to',
					direction: 'outgoing',
					relatedIssue: { id: OTHER, errorClass: 'X', message: 'm', status: 'unresolved', fingerprint: 'fp' },
				},
			],
		});

		const unlinkBtn = screen.getByRole('button', { name: /unlink issue/i });
		await fireEvent.click(unlinkBtn);
		await Promise.resolve();

		expect(fetchMock).toHaveBeenCalledWith(
			`/api/issues/${CURRENT}/relations`,
			expect.objectContaining({
				method: 'DELETE',
				body: JSON.stringify({ targetIssueId: OTHER, relationType: 'linked_to' }),
			})
		);
	});

	it('unlinking an INCOMING relation DELETEs on the relation SOURCE issue, with the current issue as targetIssueId — this is the fix, before it this call went to the wrong endpoint and the DELETE 404d', async () => {
		const fetchMock = vi.fn().mockResolvedValue({ ok: true, json: async () => ({ success: true }) });
		vi.stubGlobal('fetch', fetchMock);

		render(IssueRelations, {
			currentIssueId: CURRENT,
			initialRelations: [
				{
					id: 'rel-2',
					sourceIssueId: OTHER,
					targetIssueId: CURRENT,
					relationType: 'caused_by',
					direction: 'incoming',
					relatedIssue: { id: OTHER, errorClass: 'X', message: 'm', status: 'unresolved', fingerprint: 'fp' },
				},
			],
		});

		const unlinkBtn = screen.getByRole('button', { name: /unlink issue/i });
		await fireEvent.click(unlinkBtn);
		await Promise.resolve();

		// The bug: this used to call `/api/issues/${CURRENT}/relations` with targetIssueId: OTHER,
		// which the endpoint matches against (source=CURRENT, target=OTHER) — a row that does not
		// exist, since the stored row is (source=OTHER, target=CURRENT). That mismatch is exactly why
		// unlinking any incoming relation 404d.
		expect(fetchMock).toHaveBeenCalledWith(
			`/api/issues/${OTHER}/relations`,
			expect.objectContaining({
				method: 'DELETE',
				body: JSON.stringify({ targetIssueId: CURRENT, relationType: 'caused_by' }),
			})
		);
	});

	it('rolls back the optimistic removal and shows an error when the DELETE fails', async () => {
		const fetchMock = vi.fn().mockResolvedValue({ ok: false, status: 404 });
		vi.stubGlobal('fetch', fetchMock);

		render(IssueRelations, {
			currentIssueId: CURRENT,
			initialRelations: [
				{
					id: 'rel-3',
					sourceIssueId: OTHER,
					targetIssueId: CURRENT,
					relationType: 'duplicate_of',
					direction: 'incoming',
					relatedIssue: { id: OTHER, errorClass: 'X', message: 'm', status: 'unresolved', fingerprint: 'fp' },
				},
			],
		});

		const unlinkBtn = screen.getByRole('button', { name: /unlink issue/i });
		await fireEvent.click(unlinkBtn);
		await Promise.resolve();
		await Promise.resolve();

		expect(await screen.findByText(/failed to unlink issue/i)).toBeTruthy();
		// The row comes back after rollback.
		expect(screen.getByRole('button', { name: /unlink issue/i })).toBeTruthy();
	});
});

// sentinel-project-environments task 15, case 8 — decision 37's RENDERING half. Contract §11 makes
// this file the boundary for it: "a redacted placeholder is a row, not a gap; it carries no
// identifier; it is not a link; and an in-scope relation beside it is unchanged."
//
// This is the ONE place in the feature where the product says "something exists here that you
// cannot open"; everywhere else denial is a `404` indistinguishable from non-existence (decision
// 20). It is a deliberate, recorded exception (§12 *Known limits* 7), chosen against the review
// panel's recommendation, and an implementer who "fixes" it into an omission or a not-found has
// removed a decision — which is why it is asserted rather than left to a screenshot.
describe('IssueRelations redacted placeholder (decision 37, flows D7-D9)', () => {
	const CURRENT = 'current-issue-id';
	const VISIBLE = 'visible-issue-id';

	beforeEach(() => {
		vi.restoreAllMocks();
	});

	// The shape is BE's, taken from the committed `RedactedRelatedIssue` in
	// `lib/db/queries/issues.ts`: the `redacted` discriminant is the whole object, and the far end of
	// `sourceIssueId` / `targetIssueId` is nulled too — the related issue's id is also one of those
	// two, so redacting `relatedIssue.id` alone would hand the same value back under another key.
	const REDACTED = {
		id: 'rel-redacted',
		sourceIssueId: CURRENT,
		targetIssueId: null,
		relationType: 'linked_to' as const,
		direction: 'outgoing' as const,
		relatedIssue: { redacted: true as const },
	};

	const WHOLE = {
		id: 'rel-visible',
		sourceIssueId: CURRENT,
		targetIssueId: VISIBLE,
		relationType: 'linked_to' as const,
		direction: 'outgoing' as const,
		relatedIssue: {
			redacted: false as const,
			id: VISIBLE,
			errorClass: 'TimeoutError',
			message: 'upstream timed out',
			status: 'unresolved',
			fingerprint: 'fp-visible',
		},
	};

	it('renders the out-of-scope relation as a row that carries no identifier and is not a link, beside a whole one', () => {
		render(IssueRelations, { currentIssueId: CURRENT, initialRelations: [WHOLE, REDACTED] });

		// A ROW, not a gap. The panel says two relations exist, because two do.
		const placeholder = screen.getByTestId('redacted-relation');
		expect(placeholder.tagName).toBe('LI');
		expect(placeholder.textContent).toContain('A related issue exists that you do not have access to');

		// Carries nothing the grant was drawn to withhold: no id, no message, no error class, no
		// status, no fingerprint — and not even a partial identifier.
		const placeholderText = placeholder.textContent ?? '';
		for (const withheld of ['rel-redacted', 'unresolved', 'TimeoutError', 'fp-']) {
			expect(placeholderText).not.toContain(withheld);
		}
		// Not a link, and no unlink target either — there is no pair of ids to address the row with.
		expect(placeholder.querySelector('a')).toBeNull();
		expect(placeholder.querySelector('button')).toBeNull();

		// The regression that matters most: the in-scope relation beside it is exactly as it was.
		// This is every viewer on the day this ships.
		const visibleLink = screen.getByRole('link', { name: /upstream timed out/i });
		expect(visibleLink.getAttribute('href')).toBe(`/issues/${VISIBLE}`);
		expect(screen.getByText('TimeoutError')).toBeTruthy();
		expect(screen.getByText(VISIBLE)).toBeTruthy();
	});

	it('leaves an unrestricted viewer’s panel untouched — every relation whole, no placeholder — flow D9', () => {
		// A user with no grant row resolves to `null`, no predicate is applied, and nothing is
		// redacted. If a placeholder can appear here, the branch is keyed on the wrong thing.
		render(IssueRelations, { currentIssueId: CURRENT, initialRelations: [WHOLE] });

		expect(screen.queryByTestId('redacted-relation')).toBeNull();
		expect(screen.getByRole('link', { name: /upstream timed out/i })).toBeTruthy();
		expect(screen.getByRole('button', { name: /unlink issue/i })).toBeTruthy();
	});

	it('redacts an INCOMING relation the same way it redacts an outgoing one', () => {
		// `getIssueRelations` joins on a different column in each branch, and covering only one is the
		// natural half-fix — invisible from the page unless the relation happens to run the other way.
		render(IssueRelations, {
			currentIssueId: CURRENT,
			initialRelations: [
				{ ...REDACTED, id: 'rel-incoming', direction: 'incoming', sourceIssueId: null, targetIssueId: CURRENT },
			],
		});

		const placeholder = screen.getByTestId('redacted-relation');
		expect(placeholder.textContent).toContain('A related issue exists that you do not have access to');
		expect(placeholder.querySelector('a')).toBeNull();
	});
});
