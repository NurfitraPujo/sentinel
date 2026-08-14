# Agent Instructions

This repository is built to work with Spec Kit Memory Hub.

### Spec Kit

You MUST follow the memory-first workflow defined in [.specify/memory/workflow.md](file://.specify/memory/workflow.md) and proactively execute `/speckit.memory-md.prepare-context` before planning.

## Memory Source of Truth
- **Governance**: `.specify/memory/` (Constitution, Architecture, Workflow)
- **Durable**: `docs/memory/` and `docs/solutions/` (History, Decisions, Patterns, Solutions)
- **Active**: `specs/<feature>/` (Local context and synthesis)

A task is not fully complete until memory has been reviewed and systemic lessons are captured.

## For AI agents operating ON Sentinel (the product)

Everything above this section is guidance for contributing to **this repository's own source
code**. If you are instead an AI agent (or a script) that authenticates against a running Sentinel
instance's `/api/agent/*` API to triage issues — a different, unrelated concern — start here
instead:

- **[docs/agents/SENTINEL_AGENT_GUIDE.md](docs/agents/SENTINEL_AGENT_GUIDE.md)** — the canonical,
  provider-agnostic guide: auth, work discovery, claim etiquette, the triage recipe, the
  question/waiting_on loop, batch semantics, webhook signature verification, and the `sentinel`
  CLI.
- **[.agents/skills/sentinel-agent/SKILL.md](.agents/skills/sentinel-agent/SKILL.md)** — a
  condensed, provider-neutral skill version of the same guidance.
- **[docs/agents/openapi.agent.yaml](docs/agents/openapi.agent.yaml)** — informative OpenAPI 3.1
  schema for `/api/agent/*` (the guide above is authoritative if they ever disagree).
- **[docs/agents/examples/](docs/agents/examples/)** — a runnable batch request, a CLI-based
  triage loop, and Node/Python webhook receivers.
