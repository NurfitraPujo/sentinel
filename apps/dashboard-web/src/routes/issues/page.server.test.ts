import { describe, it, expect } from 'vitest';
import { load } from './+page.server';

// Regression: this index route did not exist, so the header's /issues link 404'd for every user.
describe('/issues', () => {
	it('redirects to the dashboard, which is the issue list', async () => {
		await expect(load({ url: new URL('https://sentinel.test/issues') } as any)).rejects.toMatchObject({
			status: 308,
			location: '/',
		});
	});

	it('carries the query string over so filters survive the hop', async () => {
		await expect(
			load({ url: new URL('https://sentinel.test/issues?status=open&page=2') } as any)
		).rejects.toMatchObject({ location: '/?status=open&page=2' });
	});
});
