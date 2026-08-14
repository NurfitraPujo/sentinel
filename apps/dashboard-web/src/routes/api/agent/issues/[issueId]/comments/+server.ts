import { json, error } from '@sveltejs/kit';
import { withAgentIssue } from '$lib/server/agent-route';
import { listComments } from '$lib/db/queries/comments';
import { agentOpRoute } from '$lib/server/agent-ops';

// Manual Issues M5 stage 2 (design §7 step 3/5). Non-blocking agent comments (reuses M3's
// createComment with authorType 'agent'), and GET .../comments?after= -- the SAME polling
// query M3's session route uses (design §7 step 5: "agents poll GET .../comments?after= to read
// answers -- pull model, Q6").
//
// R16 (docs/plans/PR13_REVIEW_REMEDIATION_PLAN.md): migrated onto `withAgentIssue`. GET has
// nothing to audit (read-only), so its handler omits `audit` entirely.
//
// N2 (AI-agent-native plan): POST is now a thin wrapper over the `issues.comment` op in
// agent-ops.ts, which is the SAME batch API also drives via POST /api/agent/batch. GET stays on
// `withAgentIssue` -- batch is mutations only, so there is no `issues.comment.list` op to share.

export const GET = withAgentIssue(async (_ctx, issue, event) => {
	const afterParam = event.url.searchParams.get('after');
	let after: Date | undefined;
	if (afterParam !== null) {
		const parsed = new Date(afterParam);
		if (Number.isNaN(parsed.getTime())) {
			throw error(400, 'after must be a valid ISO 8601 timestamp');
		}
		after = parsed;
	}

	const comments = await listComments(issue.issueId, { after });
	return { response: json({ comments }) };
});

export const POST = agentOpRoute('issues.comment');
