import { error } from '@sveltejs/kit';
import type { LayoutServerLoad } from './$types';

// `settings` is listed in hooks.server.ts's reservedRoutes, so orgHandle never
// runs for anything under this path and the root +layout.server.ts does not
// enforce a session either. Every route nested here MUST inherit an auth
// check from this layout — do not rely on individual +page.server.ts files
// to remember it (see D05 in docs/plans/UI_PARITY_REMEDIATION_PLAN.md).
export const load: LayoutServerLoad = async ({ locals }) => {
	const session = await locals.auth();
	if (!session?.user?.email) {
		throw error(401, 'Unauthorized');
	}

	return {
		session
	};
};
