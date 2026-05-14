import { SvelteKitAuth } from '@auth/sveltekit';
import Google from '@auth/core/providers/google';
import { env } from '$env/dynamic/private';

const GOOGLE_WORKSPACE_CLIENT_ID = env.GOOGLE_CLIENT_ID;
const GOOGLE_WORKSPACE_CLIENT_SECRET = env.GOOGLE_CLIENT_SECRET;
const ALLOWED_EMAIL_DOMAIN = 'company.com';

// @ts-ignore - version mismatch between @auth/core and @auth/sveltekit nested @auth/core
export const { handle } = SvelteKitAuth({
	providers: [
		Google({
			clientId: GOOGLE_WORKSPACE_CLIENT_ID,
			clientSecret: GOOGLE_WORKSPACE_CLIENT_SECRET,
		}),
	],
	callbacks: {
		async signIn({ user }) {
			const email = user?.email;
			if (!email) {
				return false;
			}
			const domain = email.split('@')[1];
			if (domain !== ALLOWED_EMAIL_DOMAIN) {
				return false;
			}
			return true;
		},
	},
	trustHost: true,
	pages: {
		signIn: '/auth/signin',
	},
});