---
name: sentinel-agent
description: >
  Triage Sentinel issues as a registered agent through the `sentinel` CLI (/api/agent/*): find work,
  claim/release, read issue detail (stacktraces or user reports), comment/progress/question, set
  status, link relations, batch mutations. Use whenever the task is to act as an AI agent operating
  ON a Sentinel instance (not on Sentinel's own codebase), from any repository.
---

# Sentinel Agent

Instructions for triaging Sentinel issues with the `sentinel` command-line client. This skill is
installed globally by `sentinel agent install`, so it is available in every repository; it does not
assume you are inside the Sentinel codebase. Do **not** use it for changes to Sentinel's own source.

**Full reference:** run `sentinel guide` — it prints the complete agent guide (endpoints, schemas,
batch semantics, webhooks, curl appendix). Read it before writing non-trivial triage logic. This
file is the condensed version.

## 1. Check setup first

```bash
sentinel whoami      # profile in use, server URL, key prefix, "auth: ok" + agent name / organization
sentinel config      # which profile, and where url and key each come from (flag / env pair / profile)
sentinel profile list
```

**Read the `profile:` and `url:` lines of `sentinel whoami` before acting** — the human may have several
Sentinels (e.g. `production` and `staging`) and mutating the wrong one is the failure to avoid. When the
human names an environment ("triage staging"), pass it explicitly: `sentinel --profile staging issues list`
(global flag, before the subcommand). Do not change the current profile with `sentinel profile use` unless
asked.

**Never `export SENTINEL_URL`** (or `SENTINEL_AGENT_KEY`) and never write them into scripts or shell rc
files: env credentials override the profile file and a stale value silently points the CLI at the wrong
Sentinel. Use `--profile` instead.

If `sentinel` is not found or `whoami` fails (exit 3 = bad/expired key, exit 1 = unreachable, "no server
URL / agent key configured", "unknown profile"), **stop and tell the human** to run:

```bash
sentinel login --url https://<their-sentinel-host> [--profile <name>]   # prints a code; they approve it in the browser
```

Any org member can run it (no admin needed); it gives them a **personal key** that acts as them. Never ask the human
to paste a key into the chat, never print or log the key, and never write it into a repository.

A personal key can do exactly what its owner can in the dashboard, and nothing more. `sentinel whoami` shows
`identity: personal key of ... (<role>, read-write|read-only)`. A **403 "read-only"** on a mutation means the human's
role cannot make changes: report what you found instead of retrying. A **404** on an issue you were given may mean it
is outside their projects. `members` and `repo-credentials` always return 403 for personal keys.

## 2. Commands

```
sentinel issues list [--type T] [--claimed true|false] [--project ID] [--waiting true]
sentinel issues get <issueId>                          # = sentinel issue show <issueId>
sentinel issue activity <issueId> [--all] [--json]     # who claimed it, status changes, comments, progress, regressions, bursts
sentinel issues occurrences <issueId> [--with-spans]   # spansStatus per row; --with-spans = only occurrences with spans
sentinel spans pull <traceId|occurrenceId> [--wait]    # pull a trace's spans now; --wait: exit 0 available, 4 none, 1 timeout
sentinel claim <issueId>            # 409 / exit 5 = owned by someone else, back off
sentinel claim-next [--type T] [--project ID] [--environment E] [--source S]   # pick+claim+get in one call; exit 4 = queue empty
sentinel release <issueId>          # releases only your own claim
sentinel comment <issueId> --body <md>
sentinel progress <issueId> --body <md>       # in-app only, no email
sentinel question <issueId> --body <md> --waiting-on <reporter|team>
sentinel comments <issueId> [--after <ts>]    # poll for replies to a question
sentinel status <issueId> <unresolved|resolved|ignored> [--resolved-in VERSION]
sentinel severity <issueId> <low|medium|high|critical>       # user_report issues only
sentinel link <issueId> <targetIssueId> --type <linked_to|caused_by|duplicate_of>
sentinel events --follow [--type T]           # NDJSON stream
sentinel batch -f ops.json                    # up to 20 mutations, one round trip
```

Exit codes: `0` ok, `1` network/5xx, `2` usage, `3` auth, `4` not found, `5` conflict,
`6` validation. On a non-zero exit the server's message is on stderr.

`sentinel <command> --help` prints that command's usage and sends nothing. Ids are full UUIDs (there are no short
forms); an id that is not one is refused locally with exit `2` — re-read it from `issues list`, don't guess.

Spans: an occurrence's `spansStatus` is `available` | `pending` | `none` (null: no trace id). Few traces are pulled
automatically; `spans pull` fetches one on demand (idempotent; only for organizations allow-listed for span pull —
else 403/exit 3; ~30 accepted pulls per org per hour — shared by people AND agents — then 429/exit 1 with a Retry-After).

## 3. Triage recipe

1. **Discover** — `sentinel claim-next` (atomic pick + claim + detail) or `sentinel issues list
   --claimed false`.
2. **Read** — `sentinel issues get <id>`: `system_error` → `latestOccurrence.stacktrace`;
   `user_report` → `report.bodyMd` / `report.severity`. `sentinel issue activity <id>` shows what has
   already happened to it (earlier claims, comments, regressions, how often it has been bursting) —
   read it before re-doing someone else's work.
3. **Act** — post a triage `comment` with your findings; if blocked on information, `question`
   (sets `waiting_on` and emails; ANY user reply clears it) and `release`; otherwise
   `status resolved` (or `ignored`) and `release`.

`sentinel batch` folds several mutations into one call but has **no cross-op transaction** — partial
completion is normal; check every result, not just the HTTP status.

## 4. Claim etiquette

- Claims are attributed to the engineer's agent (the key you are using). Other agents and humans see
  "claimed by <agent>" — only claim what you will actually work on right now.
- On exit 5 (conflict), skip the issue; never fight over it.
- `release` anything you stop working on, including when you hit an error. Don't leave stale claims.
- Don't change status of issues you have not claimed. Prefer `comment`/`progress` over silent changes
  so the human can follow what you did and why.
- You can only edit or delete your own comments.

## 5. Webhooks

Inbound events carry `X-Sentinel-Signature: t=<unix>,v1=<hex>` = `hex(HMAC-SHA256(secret, "<t>." +
raw_body))`. Worked receivers: `sentinel guide` §10.
