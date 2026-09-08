import { describe, it, expect, vi, beforeEach } from 'vitest';
import { redirect } from '@sveltejs/kit';

vi.mock('$env/dynamic/private', () => ({ env: { EMAIL_SERVER: 'smtp://debug' } }));

const auditMock = vi.fn();
vi.mock('$lib/server/audit', () => ({ logAuditEvent: (...args: any[]) => auditMock(...args) }));

const rateLimitMock = vi.fn().mockReturnValue({ allowed: true });
vi.mock('$lib/rate-limit', () => ({ checkRateLimit: (...args: any[]) => rateLimitMock(...args) }));

const signInMock = vi.fn();
vi.mock('$lib/server/auth-config', () => ({ signIn: (...args: any[]) => signInMock(...args) }));

const { load, actions } = await import('./+page.server');

function makeEvent(body: Record<string, string>) {
	return {
		request: new Request('https://sentinel.test/signin?/magiclink', {
			method: 'POST',
			headers: { 'content-type': 'application/x-www-form-urlencoded', origin: 'https://sentinel.test' },
			body: new URLSearchParams(body),
		}),
		url: new URL('https://sentinel.test/signin'),
		cookies: { set: vi.fn() },
		getClientAddress: () => '10.0.0.1',
	} as any;
}

beforeEach(() => {
	vi.clearAllMocks();
	rateLimitMock.mockReturnValue({ allowed: true });
	signInMock.mockResolvedValue(undefined);
});

describe('/signin actions', () => {
	// The regression this file exists for: `signIn` is a form action taking the RequestEvent, and
	// was being called as `signIn('email', {...})`. That threw a TypeError on `event.request` inside
	// @auth/sveltekit and every request came back "Failed to send magic link" without touching SMTP.
	it('hands signIn a RequestEvent whose form body carries providerId + email', async () => {
		await actions.magiclink(makeEvent({ email: 'engineer@example.com' }));

		expect(signInMock).toHaveBeenCalledTimes(1);
		const [authEvent] = signInMock.mock.calls[0];
		expect(typeof authEvent).toBe('object');
		expect(authEvent.request).toBeInstanceOf(Request);

		const sent = await authEvent.request.formData();
		expect(sent.get('providerId')).toBe('email');
		expect(sent.get('email')).toBe('engineer@example.com');
	});

	it('lets signIn\'s success redirect through instead of reporting a 500', async () => {
		signInMock.mockImplementation(() => {
			redirect(302, '/signin?sent=1');
		});

		await expect(actions.magiclink(makeEvent({ email: 'engineer@example.com' }))).rejects.toMatchObject({
			status: 302,
			location: '/signin?sent=1',
		});
		expect(auditMock).not.toHaveBeenCalledWith(expect.objectContaining({ type: 'magic_link_failed' }));
	});

	it('reports a real send failure as a 500 and audits it', async () => {
		signInMock.mockRejectedValue(new Error('smtp refused'));

		const result: any = await actions.magiclink(makeEvent({ email: 'engineer@example.com' }));

		expect(result.status).toBe(500);
		expect(auditMock).toHaveBeenCalledWith(
			expect.objectContaining({ type: 'magic_link_failed', error: 'smtp refused' })
		);
	});

	it('rejects an invalid address before reaching signIn', async () => {
		const result: any = await actions.magiclink(makeEvent({ email: 'not-an-email' }));

		expect(result.status).toBe(400);
		expect(signInMock).not.toHaveBeenCalled();
	});

	it('rate-limits without calling signIn', async () => {
		rateLimitMock.mockReturnValue({ allowed: false, retryAfter: 42 });

		const result: any = await actions.magiclink(makeEvent({ email: 'engineer@example.com' }));

		expect(result.status).toBe(429);
		expect(signInMock).not.toHaveBeenCalled();
	});

	// The Google button posts to ?/google; the action did not exist, so it 404'd.
	it('exposes a google action that drives signIn with providerId=google', async () => {
		await actions.google(makeEvent({}));

		const [authEvent] = signInMock.mock.calls[0];
		const sent = await authEvent.request.formData();
		expect(sent.get('providerId')).toBe('google');
	});

	it('load reports the sent flag off the query string', async () => {
		const sent: any = await load({ url: new URL('https://sentinel.test/signin?sent=1') } as any);
		const fresh: any = await load({ url: new URL('https://sentinel.test/signin') } as any);
		expect(sent.linkSent).toBe(true);
		expect(fresh.linkSent).toBe(false);
	});
});
