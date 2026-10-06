import type { RequestHandler } from './$types';
import { cliProfileName, publicOrigin, renderInstallScript } from '$lib/server/cli-dist';

// GET /install.sh — public, unauthenticated (needed before login). Renders its own origin into the
// script so the binary comes from the same server; see publicOrigin() for why that is not Host-trusting.
export const GET: RequestHandler = ({ url }) => {
	const origin = publicOrigin(url.origin);
	if (!origin) {
		return new Response('install.sh: cannot determine a safe public origin for this server (set AUTH_URL)\n', {
			status: 500,
			headers: { 'content-type': 'text/plain; charset=utf-8' },
		});
	}
	return new Response(renderInstallScript(origin, cliProfileName()), {
		headers: {
			'content-type': 'text/x-shellscript; charset=utf-8',
			// The script embeds the origin and must track deploys; never let an intermediary pin it.
			'cache-control': 'no-cache',
			'x-content-type-options': 'nosniff',
		},
	});
};
