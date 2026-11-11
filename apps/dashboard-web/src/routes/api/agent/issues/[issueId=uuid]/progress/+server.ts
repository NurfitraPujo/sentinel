import { agentOpRoute } from '$lib/server/agent-ops';

// Manual Issues M5 stage 2 (design §7 step 3, Q7). In-app notification only -- no email, per
// notify.ts's EMAILABLE_KINDS (deliberately omits 'progress_update'), so unlike claim/questions
// this route never calls sendIssueNotificationEmails.
//
// N2 (AI-agent-native plan): thin wrapper over the `issues.progress` op in agent-ops.ts, which is
// the SAME batch API also drives via POST /api/agent/batch. Behavior unchanged from the R16
// `withAgentIssue`-based version.

export const POST = agentOpRoute('issues.progress');
