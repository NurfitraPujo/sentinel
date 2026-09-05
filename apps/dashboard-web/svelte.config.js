import adapter from '@sveltejs/adapter-node';
import { vitePreprocess } from '@sveltejs/vite-plugin-svelte';

/** @type {import('@sveltejs/kit').Config} */
const config = {
	preprocess: vitePreprocess(),
	kit: {
		adapter: adapter(),
		// sentinel-user-report-issue §4.1/§4.5: the intake endpoints are called by a SERVICE
		// holding an X-API-Key, and a service HTTP client sends no `Origin` header. SvelteKit's
		// built-in cross-site form-post check runs before `handle` and before the route, has no
		// per-route escape hatch, and answers any multipart POST with no Origin
		// `403 "Cross-site POST form submissions are forbidden"` — so `POST /api/report/attachments`
		// was unreachable for its only intended caller.
		//
		// Turning it off here does NOT drop the protection: it is re-implemented verbatim in
		// `src/lib/server/csrf.ts` and applied in `src/hooks.server.ts`, exempting exactly the two
		// intake paths, which authenticate on a header credential no browser attaches on its own.
		// Every other route keeps the identical check. `checkOrigin` is deprecated upstream in
		// favour of `csrf.trustedOrigins`, which does not help here — an ABSENT origin is rejected
		// before any trusted list is consulted.
		csrf: { checkOrigin: false },
		alias: {
			'$lib': './src/lib',
			'$lib/*': './src/lib/*'
		}
	}
};

export default config;
