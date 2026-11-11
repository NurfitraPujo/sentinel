# Sentinel

An error-tracking pipeline (SDK → ingestor → NATS → processor → PostgreSQL) with a dashboard, a
machine-facing agent API, and autonomous agents that continuously triage, follow up on, and fix
the issues it tracks.

## Language

### Agent automation

**Agent**:
A registered non-human identity in an organization, with its own API keys, claims, and audit
trail. The word means only this — never the process or model acting under the identity.
_Avoid_: bot, AI, service account

**Agent Worker**:
The long-running harness process that operates under one Agent identity: polls events,
dispatches jobs, and applies decisions.
_Avoid_: worker (bare), agent (for the process), daemon

**Advisor**:
The LLM decision layer inside an Agent Worker. An Advisor holds read-only tools and produces a
structured decision; it can never mutate anything itself. Specialized per job kind: Triage
Advisor, Follow-up Advisor.
_Avoid_: brain, model, AI, decision engine

**Assignment**:
The ownership state on an issue — who (a user or an Agent) currently holds it. The single
underlying concept behind both human assignment and Agent claims.
_Avoid_: using "claim" for human ownership

**Claim**:
An Agent's Assignment, acquired atomically by the Agent itself (take-if-unowned), advisory,
heartbeat-maintained, and subject to staleness reaping. Claims are only ever self-acquired —
nothing assigns an issue *to* an Agent on its behalf.
_Avoid_: assign (as the verb for agents), lock

**Fix Executor**:
The external coding CLI an Agent Worker spawns in a sandboxed workspace to implement one fix
brief. It runs arbitrary code by nature; its output is validated before anything leaves the
workspace.
_Avoid_: fixer, coding agent, fix agent, executed agent, $AGENT_CMD (in prose)

**Disposition**:
A Triage or Follow-up Advisor's structured conclusion about an issue, which the Agent Worker
compiles into actions. Disposition names are assessments (`fixable`, `needs_human`,
`attempt_fix`), never action verbs.
_Avoid_: escalate, escalate_to_fix (renamed), action (for the conclusion)

**Fix Brief**:
The Advisor-authored specification a Fix Executor works from: reproduction, suspected files,
acceptance criteria.
_Avoid_: task, prompt (for this artifact)

**Heartbeat**:
A periodic progress update an Agent Worker posts on a held Claim to keep it from being reaped as
stale. Distinct from process health probes.
_Avoid_: keepalive, ping

**Error Event**:
One ingested error payload from an SDK — the pipeline's unit of intake, identified by
`event_id`, becoming an occurrence of an issue.
_Avoid_: event (bare), occurrence (for the payload itself)

**Issue Activity Event**:
One seq-ordered item on the org's activity feed describing something that happened to an issue
(status change, claim, comment, deletion tombstone, …). The unit agents poll, cursor over, and
dedupe by seq. Named after the `issue_activity` table.
_Avoid_: event (bare), feed event, activity (bare)

**Replay**:
Re-executing a journaled decision exactly as recorded. Advisors are never consulted during
replay.
_Avoid_: retry (for this), re-run

**Resume**:
Continuing an interrupted Fix Executor run from its persisted work state (base commit, diff,
progress). The Executor runs again; the decision that spawned it does not change.
_Avoid_: replay (for Fix Executor runs), restart

**Recovery**:
The startup process after any restart: restore state, scan the journal, then replay or resume
each in-flight job.
_Avoid_: using it for the individual replay/resume mechanisms
