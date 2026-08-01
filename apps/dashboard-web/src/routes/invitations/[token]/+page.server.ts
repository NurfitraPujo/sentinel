import { redirect } from '@sveltejs/kit';
import { dev } from '$app/environment';
import type { PageServerLoad } from './$types';
import {
	INVITE_TOKEN_COOKIE,
	INVITE_TOKEN_COOKIE_MAX_AGE_SECONDS,
} from '$lib/server/invite-cookie';

// D06: the emailed invitation link is `/invitations/<token>` -- the raw token lives ONLY in this
// path segment, briefly. It must never be forwarded into a query string (browser history, Referer
// headers on outbound requests, and it would otherwise get re-embedded in the OAuth `redirectTo` on
// the sign-in round trip). Instead it is handed off to /auth/accept-invite via a short-lived
// HttpOnly cookie -- see $lib/server/invite-cookie -- and the redirect target carries no token at
// all. That constant used to be exported from THIS file, which SvelteKit rejects at build time
// (route modules have a fixed export allowlist); `pnpm check` and `pnpm test` both passed anyway.

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
