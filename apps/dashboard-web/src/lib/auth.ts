import { SvelteKitAuth } from '@auth/sveltekit';
import Google from '@auth/core/providers/google';
import type { RequestEvent } from '@sveltejs/kit';

const GOOGLE_WORKSPACE_CLIENT_ID = process.env.GOOGLE_CLIENT_ID;
const GOOGLE_WORKSPACE_CLIENT_SECRET = process.env.GOOGLE_CLIENT_SECRET;
const ALLOWED_EMAIL_DOMAIN = 'company.com';

export const { handle, signIn, signOut } = SvelteKitAuth({
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
	pages: {
		signIn: '/auth/signin',
	},
});
