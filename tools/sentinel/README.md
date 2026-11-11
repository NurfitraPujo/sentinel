# sentinel

A provider-agnostic command-line client for Sentinel's agent API (`/api/agent/*`). It is a
standalone Go module — zero dependencies beyond the standard library, zero imports from any other
module in this repository — built for AI coding agents (or humans scripting around them) to triage
issues: list/inspect, claim/release, resolve, comment, ask blocking questions, link related issues,
tail the org's activity feed, and batch several mutations into one HTTP round trip.

## Install

**Engineers** — one command, from your Sentinel dashboard (no Go toolchain, no repo access, no sudo):

```bash
curl -fsSL https://<your-sentinel-host>/install.sh | sh
sentinel login --url https://<your-sentinel-host>   # prints a code and opens the browser: approve it, done
sentinel agent install                              # give Claude Code / Codex / agy the sentinel-agent skill
```

`install.sh` downloads the binary from the same server, verifies its sha256, and installs it atomically
to `${SENTINEL_INSTALL_DIR:-$HOME/.local/bin}/sentinel` (it warns if that is not on your `PATH`, and warns
about — never removes — an old `sentinel-cli`). `sentinel version` prints the dashboard release tag it was
built with. Re-run the one-liner to upgrade.

**Any org member can log in; no admin is needed.** `sentinel login` runs a device authorization flow (RFC 8628). It
prints a short code and a URL (`<dashboard>/cli/approve?code=…`) and tries to open your browser. You check the code,
pick the organization and click Approve, and the CLI receives a **personal key** and saves it. Over SSH or on a
headless machine, open the URL on any device where you are signed in, or pass `--no-browser` to skip the attempt.

A personal key acts as you and never exceeds your dashboard access, re-checked on every request:

- It sees only your projects and environment grants.
- It is read-only if your role is `support` or `viewer`.
- It can never use `members` or `repo-credentials`.
- It stops working when you leave the organization.
- It expires after 90 days by default.

You see and revoke your keys under *Settings → CLI sessions* (one per device per organization; a re-login from the
same device replaces that device's key), and `sentinel logout` revokes the current one. Org agent keys for CI and
automations are still minted by an org owner/admin on *Settings → Agents*. Use those with
`sentinel login --key-stdin`.

**Developers** — build from source:

```bash
# the repo is private: tell Go not to use the public proxy, and fetch over SSH
go env -w GOPRIVATE=github.com/example-org
git config --global url."git@github.com:".insteadOf "https://github.com/"
go install github.com/NurfitraPujo/sentinel/tools/sentinel@latest   # installs a binary named `sentinel`
```

or from a checkout: `cd tools/sentinel && GOWORK=off go build -o sentinel .` (add
`-ldflags "-X main.version=v1.2.3"` to stamp a version; otherwise it reports the module version / VCS
revision, or `dev`).

### Local setup commands

| Command | What it does |
|---|---|
| `sentinel login` | browser login (above). It prompts for the URL on a terminal if none is known, verifies the key with `GET /api/agent/self` **before** saving, and prints who you are logged in as, the org and the key prefix. It never prints the key. `--device-name N` names this machine on the approval page and in *CLI sessions* (default: hostname). `--no-browser` only prints the URL. |
| `sentinel login --key-stdin` | paste an existing agent key instead (CI, org agents): one line from stdin, or a hidden prompt on a terminal |
| `sentinel logout [--profile P]` | revokes the profile's personal key on the server (`POST /api/agent/key/revoke`) and removes the profile. An org agent key is refused by the server and stays valid, and the CLI says so. On a network error nothing is removed. |
| `sentinel config` | the profile in use + where the URL (value) and key (source only) come from |
| `sentinel agent install [--claude] [--codex] [--agy] [--dry-run] [--force] [--uninstall]` | writes the embedded `sentinel-agent` skill to each tool's **global** skill dir (no flag = every tool whose home dir exists) |
| `sentinel guide` | prints the full agent guide (embedded copy of `docs/agents/SENTINEL_AGENT_GUIDE.md`) |
| `sentinel version` / `--version` | prints the version |

Skill locations: Claude Code `~/.claude/skills/sentinel-agent/SKILL.md`; agy
`~/.gemini/config/skills/sentinel-agent/SKILL.md`; Codex `~/.agents/skills/sentinel-agent/SKILL.md`.
Each file carries an `<!-- installed by sentinel <version>; do not edit -->` marker: re-running updates it in
place, a file *without* the marker is never overwritten unless `--force`, and `--uninstall` removes only
marked files.

### Embedded files (maintainers)

`go:embed` cannot reach outside this directory, so `assets/GUIDE.md` and `.agents/skills/sentinel-agent/SKILL.md`
are generated copies — canonical sources are `docs/agents/SENTINEL_AGENT_GUIDE.md` and `assets/SKILL.md`. After
editing either, run `scripts/sync-cli-embeds.sh`; CI (`sentinel-cli` job) runs it with `--check` and fails on drift.

## Configuration: profiles

`$XDG_CONFIG_HOME/sentinel/config.json` (falls back to `~/.config/sentinel/config.json`), written `0600` by
`sentinel login`, holds **named profiles**, like the AWS CLI:

```json
{
  "current_profile": "production",
  "profiles": {
    "production": { "url": "https://sentinel.example.com",         "agent_key": "sent_agent_..." },
    "staging":    { "url": "https://sentinel.staging.example.com", "agent_key": "sent_agent_..." }
  }
}
```

The older flat `{"url", "agent_key"}` file still loads, as a profile named `default`; the first write migrates
it. Profile names match `[a-z0-9][a-z0-9_-]{0,31}`.

**Which profile:** `--profile P` (global flag, before the subcommand) > `SENTINEL_PROFILE` > `current_profile`
> `default`. An unknown profile exits 2 and lists the ones that exist.

**Where url and key come from** — never from two different sources (a stale `SENTINEL_URL` in a shell used to
be silently paired with the file's key for a *different* Sentinel):

| Situation | URL / key used |
|---|---|
| `-url` / `-key` flags | the flag, per field (flags always win) |
| `--profile` or `SENTINEL_PROFILE` given | that profile — an explicit profile beats env credentials |
| `SENTINEL_AGENT_KEY` set (no explicit profile) | the pair `SENTINEL_URL` + `SENTINEL_AGENT_KEY`; `SENTINEL_AGENT_KEY` without `SENTINEL_URL` is an error (exit 2) |
| `SENTINEL_URL` set **without** `SENTINEL_AGENT_KEY` | **ignored**, with a warning naming the profile actually used |
| none of the above | `current_profile`, else `default` |

`sentinel whoami` and `sentinel config` print the profile in use and where url and key each came from (never the
key), and whose key it is: `identity: personal key of <you> (<role>, read-write|read-only)` or
`identity: org agent "<name>"`. `sentinel login` warns about env variables that would shadow what it saves.

| Command | |
|---|---|
| `sentinel login [--profile P] [--url U] [--device-name N] [--no-browser] [--key-stdin] [--use] [--yes]` | log in (browser, or `--key-stdin`), verify, then write/update profile P (default: the selected profile; on a fresh machine the server's suggested name, else `default`). The first profile becomes current; `--use` forces it. Replacing an existing profile's URL asks first, before any key is minted (`--yes` skips the question and is required without a TTY). |
| `sentinel profile list` | name, URL, `*` marks current, key prefix only |
| `sentinel profile use <name>` / `remove <name>` | switch current / delete a profile |

A deployment can suggest a profile name (helm `config.dashboard.cliProfile` → `SENTINEL_CLI_PROFILE`; set to
`production` / `staging` in the AWS values): `install.sh` and the Agents page then print
`sentinel login --url <origin> --profile <name>`, so installing from both leaves you with two named profiles —
switch with `sentinel --profile staging …` or `SENTINEL_PROFILE=staging`.

The key is never logged or printed. The CLI warns to stderr (without printing the key) if the config file is
group- or world-readable.

## Commands

| Command | HTTP call | Notes |
|---|---|---|
| `sentinel issues list [--type T] [--claimed true\|false] [--project ID] [--waiting true]` | `GET /api/agent/issues` | |
| `sentinel issues get <issueId>` | `GET /api/agent/issues/:id` | full detail: issue, report, latest occurrence, relations; `sentinel issue show <issueId>` is the same |
| `sentinel issue activity <issueId> [--limit N] [--before CURSOR] [--all] [--json]` | `GET /api/agent/issues/:id/activity` | the issue's history, newest first, as a table (WHEN (UTC), WHO, WHAT, DETAIL); a run of consecutive occurrence bursts is one line; prints the `--before` cursor for the next page on stderr; `--all` follows every page; `--json` prints the response (merged with `--all`) |
| `sentinel issues occurrences <issueId> [--limit N] [--before TS] [--with-spans]` | `GET /api/agent/issues/:id/occurrences` | newest-first page; rows carry `spansStatus`; `--with-spans` sends `hasSpans=true` |
| `sentinel spans pull <traceId\|occurrenceId> [--wait] [--timeout 3m]` | `POST /api/agent/traces/:id/pull` | pull a trace's spans now; `--wait` polls to available (exit 0) / finished with none (exit 4) / timeout (exit 1) |
| `sentinel claim <issueId>` | `POST /api/agent/issues/:id/claim` | 409 on conflict |
| `sentinel claim-next [--type T] [--project ID] [--environment E] [--source S] [--sort firstSeen\|lastSeen]` | `POST /api/agent/issues/claim-next` | atomically picks + claims the next unclaimed unresolved issue and prints its full detail (same shape as `issues get`); exit 4 when the queue is empty, never 409 |
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
| `sentinel members list [--role <role>]` | `GET /api/agent/members` | list org members, optional role filter |
| `sentinel members add <email> [--role <role>]` | `POST /api/agent/members` | invites a user to the organization (default role engineer) |
| `sentinel members remove <memberId>` | `DELETE /api/agent/members/:id` | revokes an organization member's access |
| `sentinel whoami` | `GET /api/agent/self` | prints agentId/name/organizationId + key id/prefix/expiresAt |
| `sentinel key rotate` | `POST /api/agent/key/rotate` | org agent key: mints a new secret, prints it to stdout ONCE; old key stays valid for its grace window. Personal key (`sentinel login`): the old key dies at once, the new one keeps its expiry and is saved into the profile, never printed |
| `sentinel events [--after N] [--limit N] [--type T] [--project ID] [--claimed-me]` | `GET /api/agent/events` | one page |
| `sentinel events --follow [--interval SEC]` | polls `GET /api/agent/events` | NDJSON to stdout, cursor persisted |
| `sentinel batch -f ops.json\|- [--stop-on-error=false]` | `POST /api/agent/batch` | up to 20 ops, one round trip |
| `sentinel upload <file> --issue <id> [--comment <text>]` | `POST /api/agent/uploads` then `POST /api/agent/issues/:id/comments` | one-shot upload + attach-to-comment |
| `sentinel upload <issueId> <file>` | `POST /api/agent/uploads` (multipart) | deprecated two-positional form; see mismatch note below |

Global flags (`-url`, `-key`, `-format json|table`) go **before** the subcommand name; every
per-command flag is accepted either before or after its positional arguments. `issue` is an alias of `issues`, and
`show` of `get`.

**Help.** `sentinel <command> [sub] --help` (or `-h`, or `sentinel help <command>`) prints that command's usage to
stdout and exits 0 — it is decided before any command runs (`help.go`), so `--help` can never be sent as an id (it
once was: `sentinel issues get --help` requested `/api/agent/issues/--help`).

**Ids.** Every issue / comment / attachment / target-issue id argument must be a UUID (either case). Anything else —
a flag where the id goes, a truncated id — is refused before any request with exit 2 and a message naming the
argument (`ids.go`). The API has no short-id or prefix forms, so nothing else is accepted. `members remove` also takes
a user id, so it only refuses a flag-looking value.

`-format table` renders list-shaped responses (`issues list`, `issues occurrences`, `comments`,
`projects`, `members list`, `events`, `batch`) as a column table instead of pretty JSON.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | ok |
| 1 | network failure, or an unmapped/5xx server error |
| 2 | usage error (bad flags/arguments, an id that is not a UUID — no request was made) |
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

Or drain the current backlog without listing it first — `claim-next` folds the
"list → claim → get" head of that loop into one request and cannot lose a race to another agent
(the server hands each caller a different issue); it exits `4` once nothing is left:

```bash
while next=$(sentinel claim-next --type system_error); do
  issue_id=$(echo "$next" | jq -r '.issue.id')
  sentinel progress "$issue_id" --body "Investigating — picked up by triage-bot."
done
```

For higher-throughput triage, prefer `sentinel batch` to fold a claim + progress note (or several
issues' worth of status updates) into one HTTP round trip instead of one call per action.
