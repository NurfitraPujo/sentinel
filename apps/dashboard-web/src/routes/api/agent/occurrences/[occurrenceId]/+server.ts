import { json, error } from '@sveltejs/kit';
import type { RequestEvent } from '@sveltejs/kit';
import { authenticateAgentRequest } from '$lib/server/agent-auth';
import { getAgentOccurrenceInOrg } from '$lib/db/queries/agent-reads';
import { annotateSpans } from '$lib/server/agent-spans';
import { resolveAgentSpanScope } from '$lib/server/span-scope';

// sentinel-occurrence-spans TASK-02. GET /api/agent/occurrences/{occurrenceId} -- one occurrence of the KEY'S organization in
// the trace-occurrence shape (so a client holding an occurrence id can reach its trace id: the CLI's
// `sentinel spans pull <occurrenceId>`), with `spansStatus`/`hasSpans`. A malformed id and another organization's occurrence
// are the same 404. Read-only, so unaudited like the other agent reads.
const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

export const GET = async (event: RequestEvent): Promise<Response> => {
	const ctx = await authenticateAgentRequest(event.request);
	const id = (event.params.occurrenceId ?? '').toLowerCase();
	if (!UUID_RE.test(id)) throw error(404, 'Occurrence not found');

	const occurrence = await getAgentOccurrenceInOrg(ctx.organizationId, id);
	if (!occurrence) throw error(404, 'Occurrence not found');

	const scope = await resolveAgentSpanScope(ctx.organizationId);
	const [annotated] = await annotateSpans([occurrence], scope);
	return json({ occurrence: annotated });
};
