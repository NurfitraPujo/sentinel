import { json, error } from '@sveltejs/kit';
import type { RequestEvent } from '@sveltejs/kit';
import { authenticateAgentRequest } from '$lib/server/agent-auth';
import { getAgentOccurrenceInOrg } from '$lib/db/queries/agent-reads';
import { annotateSpans } from '$lib/server/agent-spans';
import { resolveAgentSpanScope } from '$lib/server/span-scope';
import { agentVisibility } from '$lib/server/agent-access';
import { isUuid } from '$lib/uuid';

// sentinel-occurrence-spans TASK-02. GET /api/agent/occurrences/{occurrenceId} -- one occurrence of the KEY'S organization in
// the trace-occurrence shape (so a client holding an occurrence id can reach its trace id: the CLI's
// `sentinel spans pull <occurrenceId>`), with `spansStatus`/`hasSpans`. A malformed id and another organization's occurrence
// are the same 404. Read-only, so unaudited like the other agent reads. sentinel-cli-browser-login: for a personal key, an
// occurrence outside its owner's projects/environment grants is that same 404.
export const GET = async (event: RequestEvent): Promise<Response> => {
	const ctx = await authenticateAgentRequest(event.request);
	const id = (event.params.occurrenceId ?? '').toLowerCase();
	if (!isUuid(id)) throw error(404, 'Occurrence not found');

	const occurrence = await getAgentOccurrenceInOrg(ctx.organizationId, id, agentVisibility(ctx));
	if (!occurrence) throw error(404, 'Occurrence not found');

	const scope = await resolveAgentSpanScope(ctx);
	const [annotated] = await annotateSpans([occurrence], scope);
	return json({ occurrence: annotated });
};
