import { error, json } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { parseTraceId } from '$lib/trace-id';
import { getSpansState } from '$lib/server/queries/span-queries';
import { earliestTraceOccurrenceAt } from '$lib/server/queries/trace-queries';
import { resolveViewerSpanScope } from '$lib/server/span-scope';
import { SpanPullError, getSpanPullConfig, isSpanPullEnabled, requestSpanPull } from '$lib/server/span-pull';
import { isUuid } from '$lib/uuid';

// sentinel-occurrence-spans TASK-02. The dashboard's "Pull spans" button and its status poll.
//
//   POST /api/spans/pull   { organizationId, traceId }  -> 200 { status, reason, action, enabled } (asks for the pull)
//   GET  /api/spans/pull?organizationId=&traceId=       -> 200 { status, reason, enabled }          (status only)
//
// Session-authenticated, and CSRF-safe the way every dashboard mutation is: POST is a JSON body, so a cross-site <form>
// cannot send it (the hook's cross-site form check rejects form content types without our Origin), and a cross-origin
// fetch needs a CORS preflight this app never grants. Nothing here reads a cookie-less credential.
//
// `organizationId` is a request value, so it is never trusted: the viewer's scope is built FROM it by the access helpers
// (membership + project access + environment grants). A viewer who cannot see an occurrence of the trace gets 404 — the
// app's convention for things a viewer cannot see, so the endpoint is no oracle for which trace ids exist.

async function resolve(locals: App.Locals, orgRaw: unknown, traceRaw: unknown) {
	const session = await locals.auth();
	if (!session?.user?.id) throw error(401, 'Unauthorized');
	const organizationId = typeof orgRaw === 'string' ? orgRaw.toLowerCase() : '';
	const traceId = typeof traceRaw === 'string' ? parseTraceId(traceRaw) : null;
	if (!isUuid(organizationId) || !traceId) throw error(404, 'Trace not found');
	const scope = await resolveViewerSpanScope(session.user.id, organizationId);
	if (!scope) throw error(404, 'Trace not found');
	return { userId: session.user.id, scope, traceId };
}

export const GET: RequestHandler = async ({ locals, url }) => {
	const { scope, traceId } = await resolve(locals, url.searchParams.get('organizationId'), url.searchParams.get('traceId'));
	const projectIds = scope.projects.map((p) => p.id);
	if (!(await earliestTraceOccurrenceAt(traceId, { projectIds, environmentScopes: scope.environmentScopes }))) {
		throw error(404, 'Trace not found');
	}
	const state = await getSpansState(traceId, scope);
	return json({ ...state, enabled: isSpanPullEnabled(scope.orgId) });
};

export const POST: RequestHandler = async ({ locals, request }) => {
	// Belt and braces behind the hook's cross-site form check: only a content type a cross-site <form> cannot produce.
	if (!(request.headers.get('content-type') ?? '').toLowerCase().startsWith('application/json')) {
		throw error(415, 'Content-Type must be application/json');
	}
	let body: unknown;
	try {
		body = await request.json();
	} catch {
		throw error(400, 'Expected a JSON body');
	}
	const b = (body && typeof body === 'object' ? body : {}) as Record<string, unknown>;
	const { userId, scope, traceId } = await resolve(locals, b.organizationId, b.traceId);
	try {
		const result = await requestSpanPull({ traceId, scope, requester: { kind: 'user', id: userId }, config: getSpanPullConfig() });
		return json({ status: result.status, reason: result.reason, action: result.action, enabled: true });
	} catch (err) {
		if (err instanceof SpanPullError) {
			if (err.code === 'not_found') throw error(404, 'Trace not found');
			if (err.code === 'not_allowed') throw error(403, err.message);
			return json(
				{ message: err.message },
				{ status: 429, headers: { 'Retry-After': String(err.retryAfterSeconds ?? 60) } }
			);
		}
		throw err;
	}
};
