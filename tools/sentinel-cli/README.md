# sentinel-cli

A provider-agnostic command-line client for Sentinel's agent API (`/api/agent/*`). It is a
standalone Go module — zero dependencies beyond the standard library, zero imports from any other
module in this repository — built for AI coding agents (or humans scripting around them) to triage
issues: list/inspect, claim/release, resolve, comment, ask blocking questions, link related issues,
tail the org's activity feed, and batch several mutations into one HTTP round trip.

## Install

```bash
go install github.com/NurfitraPujo/sentinel/tools/sentinel-cli@latest
```

Or from a checkout:

```bash
cd tools/sentinel-cli && go build -o sentinel .
```

## Configuration

Resolved in this order, highest priority first:

1. `-url` / `-key` command-line flags
2. `SENTINEL_URL` / `SENTINEL_AGENT_KEY` environment variables
3. `$XDG_CONFIG_HOME/sentinel/config.json` (falls back to `~/.config/sentinel/config.json`):

   ```json
   {
     "url": "https://sentinel.example.com",
     "agent_key": "sk_..."
   }
   ```

The key is never logged or printed. `sentinel whoami` prints only its first 8 characters. The
config file should be `chmod 600` — the CLI warns to stderr (without ever printing the key itself)
if it finds the file group- or world-readable.

## Commands

| Command | HTTP call | Notes |
|---|---|---|
| `sentinel issues list [--type T] [--claimed true\|false] [--project ID] [--waiting true]` | `GET /api/agent/issues` | |
| `sentinel issues get <issueId>` | `GET /api/agent/issues/:id` | full detail: issue, report, latest occurrence, relations |
| `sentinel issues occurrences <issueId> [--limit N] [--before TS]` | `GET /api/agent/issues/:id/occurrences` | newest-first page |
| `sentinel claim <issueId>` | `POST /api/agent/issues/:id/claim` | 409 on conflict |
| `sentinel release <issueId>` | `DELETE /api/agent/issues/:id/claim` | releases only YOUR OWN claim |
| `sentinel status <issueId> <unresolved\|resolved\|ignored> [--resolved-in VERSION]` | `PATCH /api/agent/issues/:id/status` | |
| `sentinel comment <issueId> --body <md> [--parent <id>] [--attachment <id> ...]` | `POST /api/agent/issues/:id/comments` | see mismatch note below re. `--parent` |
| `sentinel comment edit <issueId> <commentId> --body <md>` | `PATCH /api/agent/issues/:id/comments/:commentId` | own comment only; 403 otherwise |
| `sentinel comment delete <issueId> <commentId>` | `DELETE /api/agent/issues/:id/comments/:commentId` | own comment only; 403 otherwise |
| `sentinel comments <issueId> [--after <ts>]` | `GET /api/agent/issues/:id/comments` | poll this for replies to a question |
| `sentinel question <issueId> --body <md> --waiting-on <reporter\|team>` | `POST /api/agent/issues/:id/questions` | blocking; sets `issues.waiting_on` |
| `sentinel progress <issueId> --body <md>` | `POST /api/agent/issues/:id/progress` | in-app only, no email |
| `sentinel severity <issueId> <low\|medium\|high\|critical>` | `PATCH /api/agent/issues/:id/report/severity` | `user_report` issues only; 400 on `system_error` |
| `sentinel link <issueId> <targetIssueId> --type <linked_to\|caused_by\|duplicate_of>` | `POST /api/agent/issues/:id/relations` | |
| `sentinel unlink <issueId> <targetIssueId> --type <...>` | `DELETE /api/agent/issues/:id/relations` | see mismatch note below |
| `sentinel projects` | `GET /api/agent/projects` | |
| `sentinel whoami` | `GET /api/agent/self` | prints agentId/name/organizationId + key id/prefix/expiresAt |
| `sentinel key rotate` | `POST /api/agent/key/rotate` | mints a new secret, prints it to stdout ONCE; old key stays valid for its grace window |
| `sentinel events [--after N] [--limit N] [--type T] [--project ID] [--claimed-me]` | `GET /api/agent/events` | one page |
| `sentinel events --follow [--interval SEC]` | polls `GET /api/agent/events` | NDJSON to stdout, cursor persisted |
| `sentinel batch -f ops.json\|- [--stop-on-error=false]` | `POST /api/agent/batch` | up to 20 ops, one round trip |
| `sentinel upload <file> --issue <id> [--comment <text>]` | `POST /api/agent/uploads` then `POST /api/agent/issues/:id/comments` | one-shot upload + attach-to-comment |
| `sentinel upload <issueId> <file>` | `POST /api/agent/uploads` (multipart) | deprecated two-positional form; see mismatch note below |

Global flags (`-url`, `-key`, `-format json|table`) go **before** the subcommand name; every
per-command flag is accepted either before or after its positional arguments.

`-format table` renders list-shaped responses (`issues list`, `issues occurrences`, `comments`,
`projects`, `events`, `batch`) as a column table instead of pretty JSON.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | ok |
| 1 | network failure, or an unmapped/5xx server error |
| 2 | usage error (bad flags/arguments — no request was made) |
| 3 | auth failure (401/403) |
| 4 | not found (404) |
| 5 | conflict (409) — e.g. claiming an already-claimed issue |
| 6 | validation error (400/422) |

On any non-zero exit that reached the server, the server's error message is printed to stderr.

## Documented API-contract mismatches

The CLI shape below is intentionally slightly narrower than a naive reading of the feature request,
because the actual route handlers (source of truth: `apps/dashboard-web/src/routes/api/agent/**`
and `apps/dashboard-web/src/lib/server/agent-ops.ts`) do less than their names might suggest. Where
the CLI and server disagreed, the server won:

- **`comment --parent <commentId>`**: the underlying `createComment()` query supports threading via
  `parentId`, but the agent comment op (`agent-ops.ts`'s `issuesComment`, which backs both the
  single route and `batch`) only reads `body_md` and `attachment_ids` from the request body — it
  does not read a parent id at all. The flag is still accepted and sent as `parent_id` for
  forward-compatibility, but the server silently ignores it today.
- **`unlink <issueId> <targetIssueId> --type <...>`**: there is no relation-id-based delete
  endpoint. `DELETE /api/agent/issues/:id/relations` (`issuesRelationsRemove`) identifies the
  relation to remove the same way `link` identifies one to create — `{target_issue_id,
  relation_type}` — not by the relation row's own id.
- **`whoami`** (resolved in N7f, R1): `GET /api/agent/self` now exists and returns
  `{agentId, name, organizationId, key: {id, prefix, expiresAt, lastUsedAt}}` — `whoami` calls it
  directly and prints the real identity. `lastUsedAt` is always `null`: `project_api_keys` tracks
  no such column; this is documented in the response shape, not a bug.
- **`upload <issueId> <file>`** (resolved in N7f, A15, for the common case): `POST
  /api/agent/uploads` still takes no `issueId` itself — the uploaded attachment row is still
  inserted with `issue_id = NULL`. But `sentinel upload <file> --issue <id> [--comment <text>]`
  now chains the follow-up `POST /api/agent/issues/:id/comments` call with
  `attachment_ids: [<uploaded id>]` for you, in one command. The OLD two-positional form
  (`sentinel upload <issueId> <file>`, no flags) is still accepted for backward compatibility — it
  performs a plain upload with no follow-up comment, and the `issueId` is still silently ignored,
  exactly as before — but now prints a deprecation warning pointing at the flag form.

## Example: a triage loop

Poll the org's activity feed for newly created issues, claim the first one seen, and post a
progress note — a minimal building block for an autonomous triage agent:

```bash
export SENTINEL_URL=https://sentinel.example.com
export SENTINEL_AGENT_KEY=sk_...

sentinel events --follow --type issue.created | while read -r line; do
  issue_id=$(echo "$line" | jq -r '.issue.id')
  echo "saw new issue $issue_id"

  if sentinel claim "$issue_id"; then
    sentinel progress "$issue_id" --body "Investigating — picked up by triage-bot."
  else
    echo "could not claim $issue_id (already claimed?)"
  fi
done
```

For higher-throughput triage, prefer `sentinel batch` to fold a claim + progress note (or several
issues' worth of status updates) into one HTTP round trip instead of one call per action.
