import { describe, it, expect } from 'vitest';
import { load } from './+page.server';

describe('/[orgSlug]/projects/[projectId]/issues/[issueId]', () => {
	// Kept as a redirect, not deleted: notify.ts wrote this path into notification emails that are
	// already delivered, and a deleted route would 404 all of them permanently.
	it('redirects to the canonical /issues/[id]', async () => {
		await expect(
			load({ params: { orgSlug: 'acme', projectId: 'proj-1', issueId: 'issue-2' } } as any)
		).rejects.toMatchObject({ status: 308, location: '/issues/issue-2' });
	});

	// It answers the same way for any id, so it cannot be used to probe which issues exist — the
	// redirect target does the real authorisation.
	//
	// This is also why sentinel-project-environments §4.5a's second-door tests are not repeated
	// here: decision 20's environment-dimension `404` is asserted against the loader that still
	// renders, at routes/issues/[id]/page.server.test.ts:132 ('404s — never 403 — on an issue
	// outside the caller's environment grant') and :141. A loader that redirects unconditionally
	// has no environment to test, and giving it one would break the property asserted below.
	it('does not reveal whether the issue exists', async () => {
		await expect(
			load({ params: { orgSlug: 'acme', projectId: 'proj-1', issueId: 'made-up' } } as any)
		).rejects.toMatchObject({ status: 308, location: '/issues/made-up' });
	});
});
