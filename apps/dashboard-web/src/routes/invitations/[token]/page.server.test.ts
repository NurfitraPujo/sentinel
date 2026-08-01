import { describe, it, expect, vi } from 'vitest';

const { load } = await import('./+page.server');

function makeCookies() {
	const store = new Map<string, { value: string; opts: any }>();
	return {
		set: vi.fn((name: string, value: string, opts: any) => store.set(name, { value, opts })),
		get: (name: string) => store.get(name)?.value,
		delete: vi.fn(),
		_store: store,
	};
}

describe('/invitations/[token] load (D06)', () => {
	it('redirects to /auth/accept-invite with NO query string', async () => {
		const cookies = makeCookies();
		let caught: any;
		try {
			await load({ params: { token: 'abc123raw' }, cookies } as any);
		} catch (e) {
			caught = e;
		}

		expect(caught).toBeDefined();
		expect(caught.status).toBe(307);
		expect(caught.location).toBe('/auth/accept-invite');
		expect(caught.location).not.toContain('?');
		expect(caught.location).not.toContain('token=');
	});

	it('hands the raw token off via a short-lived HttpOnly cookie, not the URL', async () => {
		const cookies = makeCookies();
		try {
			await load({ params: { token: 'abc123raw' }, cookies } as any);
		} catch {
			// redirect throw, expected
		}

		expect(cookies.set).toHaveBeenCalledWith(
			'sentinel_invite_token',
			'abc123raw',
			expect.objectContaining({ httpOnly: true, path: '/' })
		);
	});
});
