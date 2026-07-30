# Spec Kit Memory Workflow

This document defines the authoritative memory-first workflow for this repository. All AI agents MUST adhere to these standards.

## Memory Layers (Source of Truth)

- **Governance Layer (`.specify/memory/`)**:
  Stable operating rules, project constitution, architecture standards, and governance-level decisions.
  *   `.specify/memory/constitution.md` (project-level principles)
  *   `.specify/memory/architecture_constitution.md` (authoritative tech standards)
  *   `.specify/memory/DECISIONS.md` (governance decisions)
  *   `.specify/memory/BUGS.md` (systemic or high-risk patterns)
- **Durable Project Memory (`docs/memory/`)**:
  The historical record of technical choices, architecture boundaries, and recurring bug patterns.
  *   `docs/memory/INDEX.md` (the compact routing map for this layer)
  *   `docs/memory/PROJECT_CONTEXT.md` (system purpose and high-level shape)
  *   `docs/memory/ARCHITECTURE.md` (detailed boundaries and integrations)
  *   `docs/memory/DECISIONS.md` (technical and implementation decisions)
  *   `docs/memory/BUGS.md` (recurring bugs and mitigation patterns)
  *   `docs/memory/WORKLOG.md` (sequential ledger of durable changes)
- **Active Feature Memory (`specs/<feature>/memory.md`)**:
  Feature-local constraints, open questions, and watchpoints for the current feature.
- **Ephemeral Run Context**:
  The current prompt, diff, terminal output, and temporary notes. Do not commit these to durable memory.

## Required Workflow

### Before Starting (Specify / Plan / Tasks)
- **Proactive Context**: You **MUST** proactively execute `/speckit.memory-md.prepare-context` to refresh the local cache and identify relevant historical constraints.
- **Read Synthesis**: Read `docs/memory/INDEX.md` and the synthesized `memory-synthesis.md` before accessing raw durable memory files.
- **Conflict Check**: Do not proceed if there is an unresolved hard conflict with project memory or architecture boundaries.

### During Implementation
- **Watchpoints**: Treat implementation and verification watchpoints in `memory-synthesis.md` as requirements, not suggestions.
- **Isolation**: Ensure new changes do not violate trust boundaries or architectural layers defined in `ARCHITECTURE.md`.

### Definition of Done (non-negotiable)

A feature is **not complete** until a test exercises it through its **production entry point** — an HTTP
route, a message consumer, or `main()` — not merely through its package API.

This is not a style preference. Across four separate features in this repository, passing package tests
coexisted with code that was never reachable at runtime: the alert dispatcher was fully implemented and
never constructed; `/ingest` rejected 100% of well-formed events for a full release; rate limiting was
bypassed for every request while its own tests passed; and the dashboard's sign-in page redirected to
itself forever while every build, typecheck and unit-test gate stayed green. In each case the package tests
were correct and the feature did not work.

Two corollaries, each learned the same way:

- **A cross-service signal needs its effect or latency asserted, not its presence in the code.** API-key
  revocation was "implemented" on both sides and took 60 seconds instead of 100ms, because the deployment
  never connected them and the failure path logged instead of failing. The test that caught it asserted a
  1-second bound.
- **An assertion states the required behaviour, never the observed behaviour.** A test written to document
  a known bug asserts the broken behaviour, so it passes *because* the bug is live and fails when the bug
  is fixed. Name tests after the requirement, not the defect.

`tests/e2e/` exists to satisfy this rule for the 32 use cases in `docs/plans/E2E_RECOVERY_PLAN.md`. Adding a
feature means adding its row.

### After Completion (Implement / Verify)
- **Self-Learning Check**: Evaluate if any systemic architectural lessons, reusable security patterns, or technical decisions were established.
- **Formal Capture**: You **MUST** execute `/speckit.memory-md.capture` to propose durable memory and `INDEX.md` updates.
- **Human-in-the-Loop**: Update durable memory only after explicit user approval.

## Maintenance Rules
- Keep entries concise, durable, and reviewable in Git.
- Refuse routine implementation details or speculative memory updates.
- Keep `memory-synthesis.md` under the configured retrieval word budget.
- Fall back to markdown-first retrieval if the SQLite optimizer fails.
