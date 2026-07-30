import { SvelteKitAuth } from '@auth/sveltekit';
import Google from '@auth/core/providers/google';
import Email from '@auth/core/providers/email';
import { env } from '$env/dynamic/private';
import { CQRSAdapter } from './auth-adapter';
import { getUserProjectRoles } from './queries/project-members';

const GOOGLE_WORKSPACE_CLIENT_ID = env.GOOGLE_CLIENT_ID;
const GOOGLE_WORKSPACE_CLIENT_SECRET = env.GOOGLE_CLIENT_SECRET;
// P3-4: env-driven, empty (unset) means allow all — a hardcoded literal made "no restriction"
// inexpressible, permanently rejecting every Google account in any deployment that didn't happen to
// use 'company.com'.
const ALLOWED_EMAIL_DOMAIN = env.ALLOWED_EMAIL_DOMAIN ?? '';

const EMAIL_SERVER = env.EMAIL_SERVER;
const EMAIL_FROM = env.EMAIL_FROM ?? 'noreply@sentinel.local';

// @ts-ignore - Auth.js type mismatch between @auth/core and @auth/sveltekit
const providers: any[] = [];

if (GOOGLE_WORKSPACE_CLIENT_ID && GOOGLE_WORKSPACE_CLIENT_SECRET) {
	providers.push(
		Google({
			clientId: GOOGLE_WORKSPACE_CLIENT_ID,
			clientSecret: GOOGLE_WORKSPACE_CLIENT_SECRET,
		})
	);
}

if (EMAIL_SERVER) {
	const isDebugMode = EMAIL_SERVER.startsWith('smtp://debug');
	
	if (isDebugMode) {
		providers.push(
			Email({
				server: {
					jsonTransport: true,
				},
				from: EMAIL_FROM,
				maxAge: 15 * 60,
			})
		);
	} else {
		providers.push(
			Email({
				server: EMAIL_SERVER,
				from: EMAIL_FROM,
				maxAge: 15 * 60,
			})
		);
	}
}

// @ts-ignore - version mismatch between @auth/core and @auth/sveltekit nested @auth/core
export const { handle, signIn, signOut } = SvelteKitAuth({
	adapter: CQRSAdapter(),
	providers,
	callbacks: {
		async signIn({ user, account }) {
			if (account?.provider === 'google') {
				const email = user?.email;
				if (!email) {
					return false;
				}
				const domain = email.split('@')[1];
				if (ALLOWED_EMAIL_DOMAIN && domain !== ALLOWED_EMAIL_DOMAIN) {
					return false;
				}
			}
			if (account?.provider === 'email') {
				return true;
			}
			return true;
		},
		async jwt({ token, account, user }) {
			if (account?.provider === 'email' && user?.email) {
				const roles = await getUserProjectRoles(user.email);
				token.projectRoles = roles;
			}
			return token;
		},
		// NOTE ON STRATEGY. `adapter: CQRSAdapter()` is configured above and no
		// `session.strategy` override is set, so Auth.js uses the DATABASE strategy. That means
		// this callback is invoked with `{ session, user }` — `token` is undefined, and the `jwt`
		// callback above never runs at all.
		//
		// Reading `token.projectRoles` here therefore threw a TypeError on every request. Auth.js
		// swallows it as SessionTokenError and `locals.auth()` returns null, so `hooks.server.ts`
		// treated even an organization OWNER as unauthenticated and every protected route 401'd.
		//
		// Both shapes are handled so this keeps working if the strategy is ever switched to jwt.
		async session({ session, user, token }) {
			const fromToken = (token as any)?.projectRoles;
			if (fromToken) {
				(session as any).projectRoles = fromToken;
				return session;
			}

			// Database strategy: the roles are not carried on a token, so resolve them here.
			const email = user?.email ?? session.user?.email;
			if (email) {
				(session as any).projectRoles = await getUserProjectRoles(email);
			}
			return session;
		},
	},
	trustHost: true,
	pages: {
		// Auth.js's own `handle` hook reserves /auth/signin for its built-in signin action —
		// pointing a custom page at that same path makes @auth/core's page handler redirect back
		// to pages.signIn unconditionally, forever (every visitor 6-hop loops on
		// GET /auth/signin?callbackUrl=...). The custom page must live at a path Auth.js does not
		// own; /signin is outside its reserved /auth/* namespace. See
		// src/routes/signin/+page.server.ts and +page.svelte for the actual page.
		signIn: '/signin',
	},
});
