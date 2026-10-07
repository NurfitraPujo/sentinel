import { agentOpRoute } from '$lib/server/agent-ops';

// Manual Issues M5 stage 2 (design §7 step 3: "POST …/relations (link to service issues, mark
// duplicate_of)"). Mirrors the session-authenticated relations route's validation exactly
// (self-relation guard, same-org guard, duplicate_of cycle guard, same Postgres error-code
// mapping), scoped by the agent's own org (B7) rather than a session role.
//
// N2 (AI-agent-native plan): thin wrappers over the `issues.relations.add` / `issues.relations.remove`
// ops in agent-ops.ts, which is the SAME batch API also drives via POST /api/agent/batch. Behavior
// unchanged from the R16 `withAgentIssue`-based version.

export const POST = agentOpRoute('issues.relations.add');
export const DELETE = agentOpRoute('issues.relations.remove');
