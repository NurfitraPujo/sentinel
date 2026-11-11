import { json, error } from '@sveltejs/kit';
import type { RequestEvent } from '@sveltejs/kit';
import { authenticateAgentRequest } from '$lib/server/agent-auth';
import { listAgentTraceOccurrences } from '$lib/db/queries/agent-reads';
import { parseTraceId } from '$lib/server/queries/trace-queries';
import { getSpansState } from '$lib/server/queries/span-queries';
import { annotateSpans } from '$lib/server/agent-spans';
import { resolveAgentSpanScope } from '$lib/server/span-scope';
import { agentVisibility } from '$lib/server/agent-access';

// sentinel-cross-repo-trace-view TASK-03 (contract D6). GET /api/agent/traces/{traceId}
// ?limit=&cursor=&environment= -- oldest-first page of every occurrence sharing the trace id across
// the KEY'S ORGANIZATION (agent reads are org-scoped by design, not environment-scoped). `limit`
// is [1, 200] (default 50); `cursor` is the opaque `nextCursor` of a previous page; `environment`
// is a filter. sentinel-occurrence-spans: `spans` is the trace's spans state ({status, reason}) and every occurrence carries
// `spansStatus`/`hasSpans`; this is also the status poll of POST /api/agent/traces/{traceId}/pull. A malformed trace id is a 404 without a query (D1); an unseen, foreign or hidden one is a
// 200 with an empty list, so the three are indistinguishable. Read-only, so unaudited like the other agent reads.
// sentinel-cli-browser-login: a personal key sees only its owner's projects/environment grants (an occurrence outside them
// is simply absent, like a foreign one), and its spans state is computed under that same scope.
export const GET = async (event: RequestEvent): Promise<Response> => {
	const ctx = await authenticateAgentRequest(event.request);

	const traceId = parseTraceId(event.params.traceId);
	if (!traceId) {
		throw error(404, 'Trace not found');
	}

	const limitParam = event.url.searchParams.get('limit');
	let limit: number | undefined;
	if (limitParam !== null) {
		const parsed = Number(limitParam);
		if (!Number.isInteger(parsed) || parsed < 1 || parsed > 200) {
			throw error(400, 'limit must be an integer between 1 and 200');
		}
		limit = parsed;
	}

	const environment = event.url.searchParams.get('environment') || undefined;
	const cursor = event.url.searchParams.get('cursor') || undefined;

	try {
		const { occurrences, nextCursor } = await listAgentTraceOccurrences({
			organizationId: ctx.organizationId,
			traceId,
			environment,
			limit,
			cursor,
			visibility: agentVisibility(ctx),
		});
		const scope = await resolveAgentSpanScope(ctx);
		const spans = await getSpansState(traceId, scope);
		return json({ occurrences: await annotateSpans(occurrences, scope), ...(nextCursor ? { nextCursor } : {}), spans });
	} catch (err) {
		if (err instanceof Error && err.message === 'invalid cursor') {
			throw error(400, 'cursor is invalid');
		}
		throw err;
	}
};
