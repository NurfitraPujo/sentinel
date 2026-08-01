import { redirect } from '@sveltejs/kit';
import { dev } from '$app/environment';
import type { PageServerLoad } from './$types';

// D06: the emailed invitation link is `/invitations/<token>` -- the raw token lives ONLY in this
// path segment, briefly. It must never be forwarded into a query string (browser history, Referer
// headers on outbound requests, and it would otherwise get re-embedded in the OAuth `redirectTo` on
// the sign-in round trip). Instead it is handed off to /auth/accept-invite via a short-lived
// HttpOnly cookie -- INVITE_TOKEN_COOKIE below, kept in sync with the same constant in
// routes/auth/accept-invite/+page.server.ts -- and the redirect target carries no token at all.
export const INVITE_TOKEN_COOKIE = 'sentinel_invite_token';
const INVITE_TOKEN_COOKIE_MAX_AGE_SECONDS = 10 * 60; // long enough for an OAuth/magic-link round trip

export const load: PageServerLoad = async ({ params, cookies }) => {
	const { token } = params;

	cookies.set(INVITE_TOKEN_COOKIE, token, {
		path: '/',
		httpOnly: true,
		secure: !dev,
		sameSite: 'lax',
		maxAge: INVITE_TOKEN_COOKIE_MAX_AGE_SECONDS,
	});

	throw redirect(307, '/auth/accept-invite');
};
