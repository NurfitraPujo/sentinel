import { redirect } from '@sveltejs/kit';
import type { PageServerLoad } from './$types';

// The issue LIST is the dashboard at `/` — routes/+page.server.ts loads it (listIssues scoped by
// getUserProjectAccess, paginated, ?status= and ?project= filters). Only the DETAIL page lives
// under this directory, at /issues/[id].
//
// The header nav has always linked /issues (routes/+layout.svelte), so every signed-in user got a
// 404 on it: there was no index here at all. Rather than stand up a second copy of the list, make
// the URL real and send it where the list actually is. The query string carries over so a link
// like /issues?status=open keeps meaning what it says.
export const load: PageServerLoad = async ({ url }) => {
	redirect(308, `/${url.search}`);
};
