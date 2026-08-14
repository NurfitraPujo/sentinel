import { json, isHttpError, type RequestHandler } from '@sveltejs/kit';
import { authenticateAgentRequest } from '$lib/server/agent-auth';
import { writeAgentAuditLog } from '$lib/server/agent-audit';
import { runAgentOp, UnknownAgentOpError } from '$lib/server/agent-ops';

/**
 * N2 (AI-agent-native plan): "codemode" server half. POST /api/agent/batch lets an agent submit up
 * to 20 mutation ops in one HTTP round trip instead of one call per issue action -- the same
 * `agent-ops.ts` registry each single `/api/agent/issues/[issueId]/*` route now drives.
 *
 * Contract (documented here because it is easy to get wrong by analogy with a "real" transaction):
 *
 * - One `authenticateAgentRequest` call for the whole batch -- the batch counts as ONE request
 *   against the key's rate limit (`project_api_keys.rate_limit_rpm`). Per-op rate charging is
 *   future work, not this endpoint's job.
 * - Ops run SEQUENTIALLY, each through its own op handler, which itself resolves scope from
 *   `ctx.organizationId` (B7, never from the op's `issueId`/`params`) and runs its own underlying
 *   transaction exactly as calling the single route would. There is NO outer transaction wrapping
 *   the whole batch -- this is deliberately at-least-once / partial-completion, identical to what
 *   N sequential HTTP calls to the single routes would already give an agent. A `stopOnError`
 *   batch that fails on op 3 of 5 leaves ops 1-2 committed.
 * - `stopOnError` (default `true`): stop after the first op that isn't `ok`; every op after that
 *   is reported `{ ok: false, skipped: true }` without being executed. `stopOnError: false` runs
 *   every op regardless of earlier failures.
 * - The batch's own HTTP response is 200 whenever the envelope itself is well-formed and
 *   authentication succeeds -- per-op failure is reported IN the body (`results[i].status`,
 *   `.error`), never as the batch's own status code, so a partial failure can never be mistaken
 *   for "nothing happened" by a caller that only checks the top-level status.
 * - Each successful op writes the SAME audit/activity entries its single route would (the op
 *   handler itself writes `issue_activity`; this route writes the `audit_logs` row exactly as
 *   `agentOpRoute` does for the single routes). No separate batch-envelope audit row -- there is no
 *   existing "batch" resource type to hang it on, and a per-op audit trail already records that the
 *   call was made (see agent-audit.ts's own header comment on why a failed audit write must never
 *   turn a committed mutation into a 500).
 */

const MAX_OPERATIONS = 20;

interface BatchOperation {
	op: string;
	issueId: string;
	params?: unknown;
}

interface BatchResult {
	ok: boolean;
	status: number;
	result?: unknown;
	error?: string;
	skipped?: boolean;
}

function isBatchOperation(value: unknown): value is BatchOperation {
	if (!value || typeof value !== 'object') return false;
	const v = value as Record<string, unknown>;
	return typeof v.op === 'string' && v.op.length > 0 && typeof v.issueId === 'string' && v.issueId.length > 0;
}

export const POST: RequestHandler = async (event) => {
	// One authenticate call for the whole batch (see header comment: rate limiting is charged once).
	const ctx = await authenticateAgentRequest(event.request);

	const body = await event.request.json().catch(() => null);
	if (!body || typeof body !== 'object' || !Array.isArray((body as Record<string, unknown>).operations)) {
		return json({ message: 'operations must be an array' }, { status: 400 });
	}

	const operations = (body as Record<string, unknown>).operations as unknown[];
	const stopOnError = (body as Record<string, unknown>).stopOnError !== false;

	if (operations.length === 0) {
		return json({ message: 'operations must not be empty' }, { status: 400 });
	}
	if (operations.length > MAX_OPERATIONS) {
		return json({ message: `operations must not exceed ${MAX_OPERATIONS}` }, { status: 400 });
	}
	if (!operations.every(isBatchOperation)) {
		return json({ message: 'each operation requires a non-empty op and issueId' }, { status: 400 });
	}

	const results: BatchResult[] = [];
	let halted = false;

	for (const rawOp of operations as BatchOperation[]) {
		if (halted) {
			results.push({ ok: false, status: 0, skipped: true });
			continue;
		}

		try {
			const opResult = await runAgentOp(rawOp.op, ctx, rawOp.issueId, rawOp.params, event.url.origin);

			if (opResult.audit) {
				await writeAgentAuditLog(
					ctx,
					opResult.audit.action,
					opResult.audit.resourceType,
					opResult.audit.resourceId,
					opResult.audit.metadata
				);
			}

			results.push({ ok: true, status: opResult.status, result: opResult.body });
		} catch (err) {
			if (err instanceof UnknownAgentOpError) {
				results.push({ ok: false, status: 400, error: err.message });
			} else if (isHttpError(err)) {
				const message =
					typeof err.body === 'object' && err.body && 'message' in err.body
						? String((err.body as { message: unknown }).message)
						: 'Request failed';
				results.push({ ok: false, status: err.status, error: message });
			} else {
				results.push({ ok: false, status: 500, error: 'Internal error' });
			}

			if (stopOnError) {
				halted = true;
			}
		}
	}

	const completed = results.filter((r) => r.ok).length;
	return json({ results, completed }, { status: 200 });
};
