import { describe, it, expect } from 'vitest';
import { load } from './+page.server';

// Regression: /settings had a layout but no index page, so the header's Settings link 404'd.
describe('/settings', () => {
	it('redirects to the first settings section', async () => {
		await expect(load({} as any)).rejects.toMatchObject({
			status: 307,
			location: '/settings/alerts',
		});
	});
});
