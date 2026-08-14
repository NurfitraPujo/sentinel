/**
 * A05-comment/A05-progress (docs/plans/AGENT_AUTOMATION_REMEDIATION_PLAN.md N7d): shared dedupe
 * window for natural-key retry detection on `createComment` (queries/comments.ts) and
 * `recordAgentProgress` (queries/agent-work.ts). 2 minutes (B12: named const, not a magic number)
 * -- long enough to absorb the retry-after-a-dropped-response window a network blip realistically
 * produces, short enough that a legitimate rapid identical re-post (rare, but a real human/agent
 * could do it deliberately) is not silently swallowed for long. Documented risk, not a defect:
 * see the plan's "Risks" note for N7d.
 */
export const AGENT_DEDUPE_WINDOW_MS = 120_000;
