---
name: "sentinel-agent"
description: "Triage Sentinel issues as a registered agent via /api/agent/*: discover work from the events feed, claim/release, read issue detail, comment/progress/question, set status, link relations, batch mutations, and verify inbound webhooks."
user-invocable: true
disable-model-invocation: false
---

This is a thin shim. The canonical, provider-neutral skill definition lives at
`.agents/skills/sentinel-agent/SKILL.md` — **follow that file**, not this one, for all
instructions, commands, and the triage recipe.

The full reference guide is `docs/agents/SENTINEL_AGENT_GUIDE.md`.
