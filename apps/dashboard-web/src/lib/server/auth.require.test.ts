import { describe, it, expect, vi } from 'vitest';
import { isHttpError } from '@sveltejs/kit';
import { requireAuth, getUser } from './auth';

function eventWith(session: unknown) {
	return { locals: { auth: vi.fn().mockResolvedValue(session) } } as any;
}

describe('requireAuth', () => {
	// Regression: this threw a bare Error, which SvelteKit treats as an unhandled exception. An
	// unauthenticated GET /search therefore answered 500 on staging while / redirected and
	// /settings/* 401'd.
	it('throws a 401 HttpError, not a bare Error, when there is no session', async () => {
		try {
			await requireAuth(eventWith(null));
			expect.unreachable('requireAuth should have thrown');
		} catch (err) {
			expect(isHttpError(err)).toBe(true);
			expect((err as any).status).toBe(401);
			expect((err as any).body.message).toBe('Authentication required');
		}
	});

	it('returns the user when the session has one', async () => {
		const user = await requireAuth(eventWith({ user: { id: 'u1', email: 'e@x.test' } }));
		expect(user).toMatchObject({ id: 'u1', email: 'e@x.test' });
	});

	it('getUser still answers null rather than throwing', async () => {
		expect(await getUser(eventWith(null))).toBeNull();
	});
});
