import { redirect } from '@sveltejs/kit';
import type { PageServerLoad } from './$types';

export const load: PageServerLoad = async ({ params }) => {
	const { token } = params;
	throw redirect(307, `/auth/accept-invite?token=${encodeURIComponent(token)}`);
};
