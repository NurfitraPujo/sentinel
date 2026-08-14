import { agentOpRoute } from '$lib/server/agent-ops';

// Manual Issues M5 stage 2 (design §7 step 2, §6). POST claims (atomic conditional UPDATE, 409 on
// conflict); DELETE releases the agent's OWN claim -- an agent can never force-release (that stays
// owner/admin-only through the session-authenticated route per §9).
//
// N2 (AI-agent-native plan): thin wrappers over the `issues.claim` / `issues.claim.release` ops in
// agent-ops.ts, which is the SAME batch API also drives via POST /api/agent/batch. Behavior
// unchanged from the R16 `withAgentIssue`-based version.

export const POST = agentOpRoute('issues.claim');
export const DELETE = agentOpRoute('issues.claim.release');
