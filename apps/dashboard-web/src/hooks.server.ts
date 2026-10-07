import type { Handle } from '@sveltejs/kit';
import { sequence } from '@sveltejs/kit/hooks';
import { handle as authHandle } from '$lib/server/auth-config';
import { db } from '$lib/server/db';
import { organizations, organizationMembers, userSessionPreferences, users } from '$lib/db/schema';
import { eq, and } from 'drizzle-orm';
import { error } from '@sveltejs/kit';
import { log } from '$lib/server/observability/log';
import { HTTP_REQUEST_ID_HEADER } from '$lib/server/observability/constants';
import { runWithTraceContext, traceContextForRequest } from '$lib/server/observability/trace';
import { dev } from '$app/environment';
import { applyFrameGuard, needsFrameGuard } from '$lib/server/cli-approve';
import {
	isCsrfExemptPath,
	isForbiddenCrossSiteFormPost,
	crossSiteForbiddenResponse,
} from '$lib/server/csrf';

// csrfHandle re-applies the cross-site form-post check that `svelte.config.js` turns off with
// `kit.csrf.checkOrigin: false`. See src/lib/server/csrf.ts for WHY it is off: SvelteKit's own
// check runs before `handle`, has no per-route escape hatch, and rejects every `Origin`-less
// multipart POST — which is exactly what sentinel-user-report-issue §4.5's service caller sends.
//
// The behaviour is upstream's, unchanged, for every path except the two `X-API-Key` intake
// endpoints. `dev` is honoured because upstream guards its own check with `if (!DEV)`; matching
// that keeps local development exactly as it was rather than making it stricter as a side effect
// of this change.
//
// Exported so the exemption can be driven directly in a test without standing up the Auth.js
// handle chain, the same reason `orgHandle` below is exported.
export const csrfHandle: Handle = async ({ event, resolve }) => {
	if (
		!dev &&
		!isCsrfExemptPath(event.url.pathname) &&
		isForbiddenCrossSiteFormPost(event.request, event.url.origin)
	) {
		return crossSiteForbiddenResponse(event.request);
	}
	return resolve(event);
};

// requestContextHandle is FIRST in the sequence (see `handle` below), deliberately outside/around
// authHandle and orgHandle: it must wrap the entire request lifecycle so every log line emitted by any
// later handle or route carries this request's trace/span id via AsyncLocalStorage (see
// observability/trace.ts).
//
// Per docs/plans/OBSERVABILITY_PLAN.md D-d/D-e: an inbound W3C `traceparent` is honoured (its trace id
// carries through; this request gets its OWN span id, not the caller's) and the trace id is echoed back
// as X-Request-Id in hex — the same header name and log keys packages/shared-go/obs uses, so a trace id
// grepped from a dashboard log line, an ingestor log line, and a processor log line all refer to the
// same request end-to-end. A missing/malformed traceparent degrades to a fresh root trace; it must never
// fail the request (traceContextForRequest handles that).
const requestContextHandle: Handle = async ({ event, resolve }) => {
	const ctx = traceContextForRequest(event.request.headers.get('traceparent'));

	// event.setHeaders (rather than response.headers.set after resolve()) so the header lands on every
	// response produced by the normal resolve() chain — a rendered page, a redirect, or a route handler
	// that itself returns `json({...}, {status})` on an error (verified: /api/alerts's catch blocks all
	// do this, and DO carry the header). It does NOT cover the case where a handle throws instead of
	// returning — e.g. orgHandle's `error(403, ...)`, or an unhandled exception — because SvelteKit's
	// fatal-error path (handle_fatal_error in @sveltejs/kit) builds a brand-new Response directly and
	// never merges event.setHeaders' collected headers into it. Verified by hand: a thrown 403/500 in
	// this app does NOT carry X-Request-Id, only the trace_id in the paired log line. Reconstructing
	// that response ourselves to force the header on would mean duplicating SvelteKit's own error-page/
	// handleError rendering inside this hook — out of scope here per the "do not disturb the auth flow"
	// constraint, and reported as a known gap rather than silently claimed as covered.
	event.setHeaders({ [HTTP_REQUEST_ID_HEADER]: ctx.traceId });

	return runWithTraceContext(ctx, async () => {
		const start = Date.now();
		try {
			const response = await resolve(event);
			log.info('http.request', {
				method: event.request.method,
				path: event.url.pathname,
				status: response.status,
				duration_ms: Date.now() - start,
			});
			return response;
		} catch (err) {
			log.error('http.request_failed', {
				method: event.request.method,
				path: event.url.pathname,
				duration_ms: Date.now() - start,
				error: err,
			});
			throw err;
		}
	});
};

// Exported (not just used internally) so route-level tests can drive it directly against a fake
// `event`/`resolve` pair without needing to mock the whole Auth.js handle chain — see
// hooks.server.test.ts (D01/P2-1).
export const orgHandle = async ({ event, resolve }: any) => {
	const session = await event.locals.auth();
	if (!session?.user?.email) {
		return resolve(event);
	}

	const userResult = await db.select({ id: users.id }).from(users).where(eq(users.email, session.user.email)).limit(1);
	if (userResult.length === 0) {
		return resolve(event);
	}
	const userId = userResult[0].id;

	const pathParts = event.url.pathname.split('/').filter(Boolean);
	// 'signin' added alongside the /auth/signin -> /signin move (see auth-config.ts) so the
	// top-level custom sign-in page is still treated as reserved, not mistaken for an org slug.
	// 'invitations' added per D01/P2-1: /invitations/<token> is a top-level, non-org route (see
	// routes/invitations/[token]/+page.server.ts) — without it, an AUTHENTICATED user hitting an
	// emailed invitation link had it parsed as an org slug here, found no matching org, and got a 403
	// before the redirect in that route ever ran. Anonymous users were unaffected only because they hit
	// the early return above.
	//
	// This list is hand-maintained and every entry here is a DELIBERATE decision that the corresponding
	// top-level directory under src/routes/ is NOT an org slug. tests/route-manifest-drift.test.ts
	// enumerates src/routes/ and fails if a new top-level route shows up that isn't in this list and
	// isn't `[orgSlug]` — so the next one can't silently repeat this bug.
	const reservedRoutes = [
		'api',
		'auth',
		'issues',
		'search',
		'settings',
		'admin',
		'docs',
		'billing',
		'support',
		'signin',
		'invitations',
		// Without this, orgHandle reads 'onboarding' as an organization slug, finds no membership
		// and 403s — locking out exactly the users the onboarding page exists to serve.
		'onboarding',
		// Unauthenticated CLI distribution (install.sh, /cli/download/<os>/<arch>, /cli/checksums.txt). A signed-in
		// engineer fetching /cli/... would otherwise have `cli` parsed as an org slug and get a 403.
		'cli',
		'install.sh',
	];
	let orgSlugFromUrl = null;

	// If the path looks like a static asset file (e.g. /favicon.png, /robots.txt),
	// do NOT treat it as an organization slug. Treating missing or requested static
	// files as org slugs causes orgHandle to throw a 403 instead of letting SvelteKit
	// or the static file server handle it (returning 404 cleanly).
	const firstSegment = pathParts.length > 0 ? pathParts[0].toLowerCase() : '';
	const isStaticFile = firstSegment.includes('.');

	if (pathParts.length > 0 && !isStaticFile && !reservedRoutes.includes(firstSegment)) {
		orgSlugFromUrl = pathParts[0];
	}

	let activeOrg = null;
	let memberRole = null;

	if (orgSlugFromUrl) {
		const result = await db.select({
			org: organizations,
			member: organizationMembers
		}).from(organizations)
		  .leftJoin(organizationMembers, eq(organizationMembers.organizationId, organizations.id))
		  .where(and(eq(organizations.slug, orgSlugFromUrl), eq(organizationMembers.userId, userId)))
		  .limit(1);

		if (result.length > 0 && result[0].member) {
			activeOrg = result[0].org;
			memberRole = result[0].member.role;
		} else {
			error(403, 'Unauthorized access to organization');
		}
	} else {
		const prefsResult = await db.select({
			orgId: userSessionPreferences.lastActiveOrganizationId
		}).from(userSessionPreferences)
		  .where(eq(userSessionPreferences.userId, userId))
		  .limit(1);

		let fallbackOrgId = prefsResult.length > 0 ? prefsResult[0].orgId : null;

		if (fallbackOrgId) {
			const orgResult = await db.select({
				org: organizations,
				member: organizationMembers
			}).from(organizations)
			  .leftJoin(organizationMembers, eq(organizationMembers.organizationId, organizations.id))
			  .where(and(eq(organizations.id, fallbackOrgId), eq(organizationMembers.userId, userId)))
			  .limit(1);

			if (orgResult.length > 0 && orgResult[0].member) {
				activeOrg = orgResult[0].org;
				memberRole = orgResult[0].member.role;
			}
		}

		if (!activeOrg) {
			const firstOrgResult = await db.select({
				org: organizations,
				member: organizationMembers
			}).from(organizationMembers)
			  .innerJoin(organizations, eq(organizations.id, organizationMembers.organizationId))
			  .where(eq(organizationMembers.userId, userId))
			  .limit(1);

			if (firstOrgResult.length > 0) {
				activeOrg = firstOrgResult[0].org;
				memberRole = firstOrgResult[0].member.role;
			}
		}
	}

	if (activeOrg) {
		event.locals.currentOrg = activeOrg;
		event.locals.orgRole = memberRole;
	}

	return resolve(event);
};

// sentinel-cli-browser-login: `/cli/approve` (and its `/signin` hop) may not be framed -- clickjacking an Approve click
// would hand an attacker's terminal the victim's access. Outermost, so it also covers the CSRF 403 and error pages.
export const frameGuardHandle: Handle = async ({ event, resolve }) => {
	const response = await resolve(event);
	if (needsFrameGuard(event.url.pathname)) applyFrameGuard(response.headers);
	return response;
};

export const handle = sequence(frameGuardHandle, requestContextHandle, csrfHandle, authHandle, orgHandle);
