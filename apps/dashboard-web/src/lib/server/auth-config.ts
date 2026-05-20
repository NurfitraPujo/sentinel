import { SvelteKitAuth } from '@auth/sveltekit';
import Google from '@auth/core/providers/google';
import Email from '@auth/core/providers/email';
import { env } from '$env/dynamic/private';
import { CQRSAdapter } from './auth-adapter';
import { getUserProjectRoles } from './queries/project-members';

const GOOGLE_WORKSPACE_CLIENT_ID = env.GOOGLE_CLIENT_ID;
const GOOGLE_WORKSPACE_CLIENT_SECRET = env.GOOGLE_CLIENT_SECRET;
const ALLOWED_EMAIL_DOMAIN = 'company.com';

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
				if (domain !== ALLOWED_EMAIL_DOMAIN) {
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
		async session({ session, token }) {
			if (token.projectRoles) {
				(session as any).projectRoles = token.projectRoles;
			}
			return session;
		},
	},
	trustHost: true,
	pages: {
		signIn: '/auth/signin',
	},
});
