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
