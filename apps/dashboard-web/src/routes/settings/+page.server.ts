import { redirect } from '@sveltejs/kit';
import type { PageServerLoad } from './$types';

// /settings had a +layout.server.ts but no index page, so the header's Settings link 404'd while
// every child of it (alerts, auth, observability) worked. There is no "settings overview" to show,
// so land on the first section instead of inventing one. The auth check in +layout.server.ts runs
// first either way.
export const load: PageServerLoad = async () => {
	redirect(307, '/settings/alerts');
};
