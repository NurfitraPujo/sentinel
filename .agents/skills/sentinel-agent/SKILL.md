---
name: sentinel-agent
description: >
  Triage Sentinel issues as a registered agent via /api/agent/*: discover work from the events
  feed, claim/release, read issue detail (stacktraces or user reports), comment/progress/question,
  set status, link relations, batch mutations, and verify inbound webhooks. Use whenever the task
  is to act as an AI agent operating ON a Sentinel instance (not on Sentinel's own codebase).
---

# Sentinel Agent

Provider-neutral instructions for triaging Sentinel issues through its agent API. This file is the
canonical skill definition; `.claude/skills/sentinel-agent/SKILL.md` is a thin Claude-specific shim
that points back here.

**Full reference:** `docs/agents/SENTINEL_AGENT_GUIDE.md` (this repo) — read it before writing any
non-trivial triage logic. This file is a condensed pointer, not a replacement.

## When to use this skill

- You have (or are being asked to obtain) a Sentinel agent Bearer key (`sent_agent_...`) and need
  to list, inspect, claim, comment on, or resolve issues.
- You are building or debugging a webhook receiver for Sentinel's outbound event push.
- You are writing a triage bot / autonomous loop against `/api/agent/*`.

Do **not** use this skill for changes to Sentinel's own source code — that's the repo's normal
`AGENTS.md` / `CLAUDE.md` guidance, a different concern entirely.

## Environment setup

```bash
export SENTINEL_URL=https://sentinel.example.com
export SENTINEL_AGENT_KEY=sent_agent_...   # never log or print this
```

Every request needs `Authorization: Bearer $SENTINEL_AGENT_KEY`. A 401 covers unknown/revoked/
expired/wrong-scope keys and disabled agents indistinguishably by design — see the guide §2.

## Core CLI commands

Prefer the `sentinel` CLI (`tools/sentinel-cli`) over raw curl when scripting:

```
sentinel issues list [--type T] [--claimed true|false] [--project ID] [--waiting true]
sentinel issues get <issueId>
sentinel claim <issueId>            # 409 = owned by someone else, back off
sentinel release <issueId>          # releases only your own claim
sentinel comment <issueId> --body <md>
sentinel comment edit <issueId> <commentId> --body <md>      # own comment only, 403 otherwise
sentinel comment delete <issueId> <commentId>                # own comment only, 403 otherwise
sentinel progress <issueId> --body <md>       # in-app only, no email
sentinel question <issueId> --body <md> --waiting-on <reporter|team>
sentinel status <issueId> <unresolved|resolved|ignored> [--resolved-in VERSION]
sentinel severity <issueId> <low|medium|high|critical>       # user_report issues only
sentinel link <issueId> <targetIssueId> --type <linked_to|caused_by|duplicate_of>
sentinel events --follow [--type T]           # NDJSON stream, ~2s lag guard
sentinel batch -f ops.json                    # up to 20 mutations, one round trip
```

Exit codes: `0` ok, `2` usage, `3` auth, `4` not found, `5` conflict, `6` validation. Full command
table and config resolution order: guide §11.

## Triage recipe (condensed)

1. **Discover** — poll `sentinel events --follow` (or `issues list --claimed false`).
2. **Claim** — `sentinel claim <id>`; on conflict (exit 5), skip and move on.
3. **Read** — `sentinel issues get <id>`: `system_error` → `latestOccurrence.stacktrace`;
   `user_report` → `report.bodyMd` / `report.severity`.
4. **Act** — post a triage `comment`; if blocked, `question` (sets `waiting_on`, forces an email;
   ANY user reply clears it — poll `comments` or `events --type question_answered` to notice) and
   `release`; otherwise `status resolved` (or `ignored`) then `release`.

Batch (`sentinel batch`) folds several of these into one call but has **no cross-op transaction** —
partial completion is normal; check every result, not just the HTTP status (guide §9).

## Webhooks

Verify `X-Sentinel-Signature: t=<unix>,v1=<hex>` = `hex(HMAC-SHA256(secret, "<t>." + raw_body))`.
Worked example and Node/Python receivers: guide §10, `docs/agents/examples/webhook-receiver.md`.

## More

- Raw curl for every endpoint: guide §14.
- OpenAPI (informative): `docs/agents/openapi.agent.yaml`.
- Runnable examples: `docs/agents/examples/`.
