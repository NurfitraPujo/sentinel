import { env } from '$env/dynamic/private';
import { fail, isRedirect } from '@sveltejs/kit';
import { z } from 'zod';
import { checkRateLimit } from '$lib/rate-limit';
import { logAuditEvent } from '$lib/server/audit';
import { signIn } from '$lib/server/auth-config';
import type { RequestEvent } from '@sveltejs/kit';
import type { PageServerLoad, Actions } from './$types';

const emailSchema = z.object({
	email: z.string().email('Invalid email address'),
});

// `signIn` as exported by SvelteKitAuth is a SvelteKit form ACTION — its one argument is the
// RequestEvent, and the provider plus its options are read out of `await event.request.formData()`
// as `providerId` + the remaining fields. It is NOT the `(provider, options)` programmatic helper;
// that signature belongs to the internal `@auth/sveltekit/actions` export, which is not public and
// additionally wants the resolved config and the event.
//
// Calling it as `signIn('email', { email, callbackUrl })` made `event` the string 'email', so
// `event.request` was undefined and it threw `TypeError: Cannot read properties of undefined
// (reading 'formData')` BEFORE any mail was attempted — the action's catch then reported that as
// "Failed to send magic link". Nothing ever reached SMTP; staging logged the TypeError against
// every magic_link_failed audit event.
//
// So drive it the way it wants to be driven: hand it this same event carrying a form body we
// wrote. The headers are the real request's (Auth.js reads Host/Origin/Cookie off them) with the
// content type forced, because the body below is ours, not the browser's.
function authEventFor(event: RequestEvent, fields: Record<string, string>): RequestEvent {
	const headers = new Headers(event.request.headers);
	headers.set('content-type', 'application/x-www-form-urlencoded');

	const request = new Request(event.request.url, {
		method: 'POST',
		headers,
		body: new URLSearchParams(fields),
	});

	return { ...event, request };
}

export const load: PageServerLoad = async ({ url }) => {
	return {
		emailConfigured: !!env.EMAIL_SERVER,
		githubConfigured: !!(env.GITHUB_CLIENT_ID && env.GITHUB_CLIENT_SECRET),
		googleConfigured: !!(env.GOOGLE_CLIENT_ID && env.GOOGLE_CLIENT_SECRET),
		// Auth.js redirects here after it has accepted the address and dispatched the mail; see
		// `pages.verifyRequest` in auth-config.ts.
		linkSent: url.searchParams.get('sent') === '1',
	};
};

export const actions: Actions = {
	// The page's Google button posts here. Before this action existed the button posted to a
	// SvelteKit action that was never defined, so it answered 404 and Google sign-in was as dead
	// as the magic link.
	google: async (event) => {
		await signIn(authEventFor(event, { providerId: 'google', redirectTo: '/' }));
	},

	github: async (event) => {
		await signIn(authEventFor(event, { providerId: 'github', redirectTo: '/' }));
	},

	magiclink: async (event) => {
		const formData = await event.request.formData();
		const email = formData.get('email')?.toString() ?? '';

		const validation = emailSchema.safeParse({ email });
		if (!validation.success) {
			return fail(400, { error: 'Invalid email address', email });
		}

		const rateLimit = checkRateLimit(email);
		if (!rateLimit.allowed) {
			logAuditEvent({
				type: 'magic_link_requested',
				email,
				ip: event.getClientAddress(),
				error: `Rate limit exceeded - retry after ${rateLimit.retryAfter}s`,
			});
			return fail(429, {
				error: 'Too many requests. Please try again later.',
				retryAfter: rateLimit.retryAfter,
				email,
			});
		}

		logAuditEvent({
			type: 'magic_link_requested',
			email,
			ip: event.getClientAddress(),
		});

		try {
			await signIn(authEventFor(event, { providerId: 'email', email, redirectTo: '/' }));
		} catch (err) {
			// A successful send EXITS THROUGH HERE: `signIn` reports success by throwing SvelteKit's
			// redirect to `pages.verifyRequest`. Swallowing it — which the previous shape did — turns
			// every delivered magic link into a 500 the user reads as "it didn't work". Let it fly.
			if (isRedirect(err)) {
				throw err;
			}
			logAuditEvent({
				type: 'magic_link_failed',
				email,
				ip: event.getClientAddress(),
				error: err instanceof Error ? err.message : 'Unknown error',
			});
			return fail(500, { error: 'Failed to send magic link', email });
		}

		// Unreachable in practice (signIn always redirects), but an action must return something and
		// the page still honours this shape.
		return { success: true, email };
	},
};
