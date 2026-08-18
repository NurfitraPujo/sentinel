// N10 part 1 (docs/plans/AGENT_WORKER_PLAN.md rev 4 SS4.5): the repo-connection provider list.
// B12: this is a constant shared between a server module ($lib/db/queries/agent-settings.ts) and
// a client component (the project settings page's Agent automation section), so it lives here in
// plain $lib -- never exported from a route file (+page.server.ts/+server.ts), which is the thing
// B12 actually forbids, and never in $lib/server, which a client component cannot import.
export const AGENT_REPO_PROVIDERS = ['github', 'bitbucket'] as const;
export type AgentRepoProvider = (typeof AGENT_REPO_PROVIDERS)[number];
