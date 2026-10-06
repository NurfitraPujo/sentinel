import { json, error } from '@sveltejs/kit';
import type { RequestEvent } from '@sveltejs/kit';
import { authenticateAgentRequest } from '$lib/server/agent-auth';
import { writeAgentAuditLog } from '$lib/server/agent-audit';
import { parseTraceId } from '$lib/server/queries/trace-queries';
import { resolveAgentSpanScope } from '$lib/server/span-scope';
import { SpanPullError, requestSpanPull } from '$lib/server/span-pull';

// sentinel-occurrence-spans TASK-02. POST /api/agent/traces/{traceId}/pull -- pull the spans of a trace from the trace store
// NOW instead of waiting for the throttled automatic pull. No body. The KEY'S organization is the scope (B7), with the same
// rules as the dashboard button (span-pull.ts): the organization must be allow-listed for span pull (403), the trace must have
// an occurrence in it (404, like a malformed id), at most SPAN_PULL_ON_DEMAND_LIMIT accepted requests per organization per hour
// (429 + Retry-After). Idempotent: while the pull is pending a repeat writes nothing. The response is the trace's spans
// state plus what this call did; poll GET /api/agent/traces/{traceId} (`spans`) for the outcome. An accepted request is
// audited (the ledger row is the audit record: requested_by = the agent id).
export const POST = async (event: RequestEvent): Promise<Response> => {
	const ctx = await authenticateAgentRequest(event.request);

	const traceId = parseTraceId(event.params.traceId);
	if (!traceId) throw error(404, 'Trace not found');

	const scope = await resolveAgentSpanScope(ctx.organizationId);
	try {
		const result = await requestSpanPull({ traceId, scope, requester: { kind: 'agent', id: ctx.agentId } });
		if (result.requestId) {
			await writeAgentAuditLog(ctx, 'span_pull.request', 'span_pull', result.requestId, { traceId, action: result.action });
		}
		return json(result);
	} catch (err) {
		if (err instanceof SpanPullError) {
			if (err.code === 'not_found') throw error(404, 'Trace not found');
			if (err.code === 'not_allowed') throw error(403, err.message);
			return json({ message: err.message }, { status: 429, headers: { 'Retry-After': String(err.retryAfterSeconds ?? 60) } });
		}
		throw err;
	}
};
