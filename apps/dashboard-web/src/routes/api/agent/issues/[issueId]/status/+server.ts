import { agentOpRoute } from '$lib/server/agent-ops';

// Manual Issues M5 stage 2 (design §7 step 6). PATCH .../status { status, resolved_in_version? }
// -> updateIssueStatus with actorType 'agent' (resolved_by_type='agent' when resolving).
//
// N2 (AI-agent-native plan): thin wrapper over the `issues.status` op in agent-ops.ts, which is
// the SAME batch API also drives via POST /api/agent/batch. Behavior unchanged from the R16
// `withAgentIssue`-based version.

export const PATCH = agentOpRoute('issues.status');
