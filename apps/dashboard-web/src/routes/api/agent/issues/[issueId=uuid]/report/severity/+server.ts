import { agentOpRoute } from '$lib/server/agent-ops';

// A09 (N7e, docs/plans/AGENT_AUTOMATION_REMEDIATION_PLAN.md). PATCH .../report/severity
// { severity } -> updateManualIssueReport (reports.ts) with only `severity` set, actorType
// 'agent'. 400 when the issue isn't a `user_report` (agent-ops.ts's `issuesReportSeverity`).
//
// Thin wrapper over the `issues.report.severity` op in agent-ops.ts, the SAME op POST
// /api/agent/batch drives -- same pattern as status/+server.ts.

export const PATCH = agentOpRoute('issues.report.severity');
