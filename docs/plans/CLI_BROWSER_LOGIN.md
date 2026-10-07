# CLI browser login and personal keys (CBL, sentinel-cli-browser-login/TASK-01)

Status: implemented on branch `NurfitraPujo/cbl-sentinel`. An independent security review follows.

## Goal

Any organization member, an `engineer` for example, can set up the `sentinel` CLI without an admin:

```sh
curl -fsSL https://sentinel.tuju.ai/install.sh | sh
sentinel login --profile production      # opens the browser; the user approves; done
sentinel agent install
```

The CLI receives a **personal key**. That key can never do more than its owner can do in the dashboard
at the moment of the request.

## Data model (migration `1725900000_add_cli_device_login.sql`)

| Object | Change |
|---|---|
| `agents.kind` | CHECK widened from `('ai','bot')` to `('ai','bot','personal')`. This uses a `pg_constraint` guard so a replay is a no-op. |
| `agents.owner_user_id` | New column, `TEXT NULL REFERENCES "user"(id) ON DELETE CASCADE`. A personal agent is bound to its owner. When the user row goes, so does the agent, and its keys go with it through `project_api_keys.agent_id ON DELETE CASCADE`. |
| `agents_personal_owner_check` | `(kind = 'personal') = (owner_user_id IS NOT NULL)`. A personal agent always has an owner, and an org agent never has one. |
| `uq_agents_personal_owner` | `UNIQUE (org_id, owner_user_id) WHERE owner_user_id IS NOT NULL`. There is one personal agent per (user, org), shared by all of that user's devices, so claims and comments read as one person's agent. |
| `project_api_keys` | No new column. A personal key is an ordinary `scope='agent'` key whose `agent_id` names a personal agent. `name` holds the **device name**. "One active personal key per (user, org, device)" holds because minting runs in a transaction that row-locks the personal agent and revokes that agent's active keys of the same name. |
| `cli_device_authorizations` | New table holding one row per `POST /api/cli/device`. It stores `device_code_hash` (sha256 hex, UNIQUE), `user_code` (8 chars, a partial UNIQUE index over `status='pending'`), status, the request metadata (client, version, device name, profile, IP, user agent), `expires_at`, the poll state (`last_polled_at`, `poll_interval_seconds`), the decision (`approved_by_user_id`, `organization_id`, `decided_at`) and delivery (`api_key_id`, `delivered_at`). It never holds a key or a raw `device_code`. |

The retention sweep deletes device rows one day after they expire (`retention.ts`).

## Device authorization flow (RFC 8628 shape)

```
CLI                                   dashboard                                   browser (signed in)
 | POST /api/cli/device ------------->| row: pending, device_code hashed          |
 |<-- device_code, user_code, uri ----|                                           |
 | prints code + uri, opens browser   |                                           |
 |                                    |<---- GET /cli/approve?code=XXXX-XXXX -----|
 |                                    |----- device, IP, time, version, org pick ->|
 |                                    |<---- POST ?/approve (session + Origin) ---| status: approved
 | POST /api/cli/device/token ------->| approved → mint key in the SAME tx that   |
 |<-- key (once), prefix, org, url ---|   marks the row delivered                  |
```

### `POST /api/cli/device` (unauthenticated)

- Body: `{client, version, device_name, profile}`. Each field is length-bounded and stripped of control
  characters. `profile` must match the CLI's profile rule or be empty.
- Rate limits, both in-process like every other limiter in this app (so per replica): 10 starts per IP
  per 10 minutes, and 300 starts per 10 minutes globally.
- "Per IP" means the **client's** IP, which needs adapter-node's `ADDRESS_HEADER` / `XFF_DEPTH` behind a
  proxy. Without them, every caller is the proxy and shares one bucket. Helm sets them through
  `config.dashboard.clientIp`, which is rendered only when set. The AWS values use
  `x-forwarded-for` with depth `2`: the ALB appends the client and Envoy appends the ALB.
  `clientIp()` (`src/lib/server/client-ip.ts`) never throws. A missing or short header falls back to the
  TCP peer, then to `'unknown'`. The sign-in page's audit IP uses it too.
- `device_code` is 32 random bytes as base64url (256 bits) and is stored as sha256 hex.
- `user_code` is 8 characters from the 20-letter RFC 8628 alphabet `BCDFGHJKLMNPQRSTVWXZ` (no vowels, no
  digits, no look-alikes; about 34.6 bits), displayed as `XXXX-XXXX`.
- Response: `{device_code, user_code, verification_uri, verification_uri_complete, expires_in: 600,
  interval: 5}`. The URIs are built from `publicOrigin()`, which prefers `AUTH_URL`, never from the
  request's Host header.

### Approval page `/cli/approve` (session-authenticated)

- Signed out: the page offers sign-in and comes back afterwards. `/signin?next=` accepts only
  `/cli/approve` with an optional well-formed `code` and nothing else, so it cannot be used as an open
  redirect.
- The user enters or confirms the code and sees it in large type, with the warning *"Only approve a code
  you started yourself in your own terminal. Approving gives that terminal your access."*
- The page shows the device name, the requesting IP, when the request was made, the CLI version, the
  requested profile, and the org the key will be for. The org picker lists only orgs the user is a
  member of.
- Approve and Deny are SvelteKit form actions. CSRF has three layers: the hook's cross-site form-post
  check, an explicit same-origin `Origin` check inside the action (enforced in dev too), and Auth.js's
  `SameSite=Lax` session cookie.
- Code lookups (every page load carrying a code, and every approve/deny) are rate-limited per user: 20 per 10
  minutes.
- Each code is single use. An expired, used or denied code says which of those it is.

### `POST /api/cli/device/token` (unauthenticated)

`{device_code}` returns HTTP 400 with an `error` field, or 200 with the key:

| Row state | Response |
|---|---|
| unknown `device_code` | 400 `invalid_grant` |
| polled faster than `interval` | 400 `slow_down`, and `interval` grows by 5 s (cap 60) |
| `pending` | 400 `authorization_pending` |
| `denied` | 400 `access_denied` |
| past `expires_at` (pending or approved) | 400 `expired_token` |
| `delivered` (replay) | 400 `invalid_grant` |
| `approved` | 200 `{key, key_prefix, organization_id, org_slug, url, cli_profile, expires_at, owner}` |

The approved branch runs one transaction with these steps:

1. Lock the row `FOR UPDATE` and re-assert `status='approved'`.
2. Re-check that the approver is still a member of the org.
3. Get or create the personal agent (`"<user name> (CLI)"`; the name refreshes on every mint).
4. Lock it.
5. Revoke that agent's active keys with the same device name.
6. Mint the key (`sent_agent_` + 64 hex characters, stored as sha256, with `expires_at = now +
   CLI_PERSONAL_KEY_TTL_DAYS`, default 90).
7. Mark the row `delivered`.
8. Write audit rows.

The raw key exists only in that response. After it, the row can never yield a key again.

## Personal-key authorization rule

`authenticateAgentRequest` resolves a personal key to its owner and **reloads the owner's live state on
every request**:

1. The owner's `organization_members` row in the key's org. If it is missing, or the role is not one
   `rbac.ts` knows, the request gets **401**, exactly like a dead key.
2. Accessible projects: `getAccessibleProjects(owner, org)`, which is the dashboard's own rule
   (`project_members` ∪ every project for an org role in `ISSUE_WRITE_ROLES`).
3. Environment grants: `resolveEnvironmentScopes(owner)`, where absence means everything.
4. `canWrite = hasPermission(role, 'write')` from `rbac.ts`.

The authenticated request then carries `ctx.owner` (`null` for org agents). The rules are:

- **Lists** (`issues`, `events`, `projects`, `claim-next` candidates, trace occurrences, issue
  relations) are filtered to the owner's projects and environment grants.
- **Single objects** (an issue and everything under it, an occurrence, a trace) outside that scope
  return **404**, the same as another org's object.
- **Mutations** need `canWrite` and return **403** otherwise. A batch op is checked per op, exactly like
  its single route.
- **Relation targets in the events feed.** A `linked` event is listed when its SOURCE issue is visible, but
  its `newValue.targetIssueId` may name an issue the owner cannot see. That id is redacted the way the issue
  page redacts the same relation: `targetIssueId: null, targetRedacted: true` (`redactLinkedEventTargets`).

### Role → permission table for personal keys

| Org role | Sees | Mutations (status, claim/release/claim-next, comment, edit/delete own comment, question, progress, severity, link/unlink, upload) | Span pull | Rotate own key / logout |
|---|---|---|---|---|
| owner | every project (environment grants apply) | yes | yes | yes |
| admin | every project (environment grants apply) | yes | yes | yes |
| engineer | every project (environment grants apply) | yes | yes | yes |
| support | every project (environment grants apply) | **no, read-only** | yes | yes |
| viewer | only projects with a `project_members` row (environment grants apply) | **no, read-only** | yes | yes |

The mutation column follows `rbac.ts`'s `write` permission, so for `support` it is deliberately
**narrower** than the dashboard. The dashboard's `ISSUE_WRITE_ROLES` lets `support` resolve issues, and
§9 lets a `viewer` comment. A personal key never exceeds the dashboard; it can be stricter. Span pull is
a read in the dashboard (any viewer who can see the trace), so it is a read here too.

### Denied to personal keys (403, regardless of role)

| Endpoint | Why |
|---|---|
| `GET/POST /api/agent/members`, `DELETE /api/agent/members/{id}` | Member management is owner/admin work and is done in the dashboard. The list is also denied: the brief's default is deny, and a CLI does not need it. |
| `GET /api/agent/repo-credentials` | It delivers write-capable git tokens. That is reserved for admin-provisioned org agents with `can_access_repo_credentials`, a flag a personal agent can never be given. |
| Any admin key operation on a personal agent | `/api/organizations/{org}/agents/{id}` (status, repo-credential flag, key issuance, webhooks) refuses personal agents. An admin can only **revoke** a personal key. |

`POST /api/agent/key/rotate` on a personal key rotates only that key (`rotatePersonalKey`, one
transaction). The new key keeps the owner and device name. Two rules differ from org agents:

- **No grace period.** The calling key is revoked in the same transaction that mints its successor, so a
  device never holds two live keys and `sentinel logout` after a rotate leaves nothing alive.
  `AGENT_KEY_ROTATION_GRACE_HOURS` applies to org agents only.
- **No renewal.** The new key keeps the old key's exact `expires_at`. Only a fresh browser approval
  (`sentinel login`) restarts the clock, so a leaked key cannot rotate itself into a longer life.

`sentinel key rotate` on a personal profile writes the new key into that profile (mode 0600) instead of
printing it, because the old key is already dead.
`POST /api/agent/key/revoke` revokes the calling key (`sentinel logout`). It is personal-only: an org
agent key gets 403, because org keys are an admin's to retire.

Org agents behave exactly as before. Regression tests cover that.

## UI surfaces

- `/settings/cli`, **CLI sessions**: the user's own personal keys in every org, with device name, org,
  created, expires and status, plus Revoke. There is no "last used" column because keys do not track it.
- `/{org}/settings/agents`: org agents are listed as before, and a new **Personal CLI keys** section
  shows each personal key with its owner and a Revoke button for owners and admins. The "connect the
  CLI" snippet now says any member can run `sentinel login`.
- `/cli/approve`: the approval page.
- Attribution: personal agents are named `"<user name> (CLI)"`, so claims, comments and progress render
  through the existing agent-name lookups with no renderer change.

## Audit (`audit_logs`)

| Action | Actor | Written when |
|---|---|---|
| `cli.device.approved` / `cli.device.denied` | approving user | approval page |
| `cli.key.minted` | approving user | token delivery (`resource_id` = key id; metadata has device, IP, agent, org) |
| `cli.key.replaced` | approving user | an older key for the same device was revoked by a new login |
| `cli.key.revoked` | user / admin / the key itself | settings page, Agents page, `sentinel logout` |
| `agent.key.rotated` | the personal agent | unchanged rotate path |

## Threat model

| Threat | Mitigations |
|---|---|
| **User-code phishing.** An attacker starts a flow and gets a victim to approve it, ending up with a key carrying the victim's access. | The approval page shows the device name, the requester's IP, the request time, the CLI version and the target org, and warns in plain words to approve only a code you started yourself. Codes expire after 10 minutes and are single use. Approval is an explicit click; nothing auto-approves from a link. Every approval is audited with the requesting IP, and the resulting key is visible and revocable on `/settings/cli` and the Agents page. Personal keys can never exceed the victim's dashboard rights and never reach member management or repo credentials. When approving would REPLACE a live key of the same device name in the chosen org, the page says so explicitly ("This replaces your CLI key for <device> created <date>"), which exposes a phisher who reuses the victim's hostname. The CLI only opens the verification URI in a browser if it is http(s) on the same scheme and host as the configured server; otherwise it prints it with a warning. Residual risk: a convinced victim can still approve. RFC 8628 §5.4 accepts this; the UI makes it as visible as possible. |
| Brute force of `user_code` | About 34.6 bits, valid for 10 minutes, and only pending codes match. Guessing requires a signed-in session and is limited to 20 code lookups (page loads carrying a code, plus approve/deny submissions) per user per 10 minutes. Guessing someone else's code only lets the attacker approve that device **with the attacker's own access**, which gives nothing away. |
| Brute force of `device_code` | 256 bits. It is looked up by sha256 through a unique index, then compared with `timingSafeEqual`. The token endpoint is rate-limited per IP. |
| CSRF on approve | The hook's cross-site form check, an explicit `Origin` check in the action (tested), the `SameSite=Lax` session cookie, and a SvelteKit action rather than a GET side effect. |
| Clickjacking the Approve button | `/cli/approve` and `/signin` (pages, `__data.json`, actions and error responses) send `Content-Security-Policy: frame-ancestors 'none'` and `X-Frame-Options: DENY` (`frameGuardHandle`, the outermost hook). |
| Malformed input | A non-UUID `organizationId` on approve and a malformed escape in `/signin?next=` are both 400 or ignored before any query, never a 500. |
| Key leakage | The key is returned once, in a `Cache-Control: no-store` response, and never stored in plaintext: only its sha256 is kept, and the device row never holds it. It is never logged; the device routes log neither request bodies nor keys. The CLI writes it with mode 0600 and never prints it (only the prefix). |
| Privilege escalation through a personal key | Live per-request authorization from the owner's membership, projects and environment grants, using the dashboard's own helpers. There is no stored grant to go stale. Personal agents cannot be given the repo-credential flag or extra keys by admins, and members/repo-credentials are denied outright. |
| Stale access after removal from the org or a project, or a role change | Rechecked on every request, so removal from the org gives the next request a 401. Project or grant removal hides those objects on the next request, and a role downgrade removes write on the next request. User deletion cascades to the agent and its keys. |
| Replay of `device_code` after delivery | The status moves to `delivered` in the minting transaction; a second poll gets `invalid_grant` and the row holds no key. |
| Open redirect | `verification_uri` comes from `publicOrigin()` (`AUTH_URL`), not the Host header. The `/signin?next=` allow-list accepts only `/cli/approve[?code=XXXX-XXXX]`. |
| Flooding (device starts, polling) | Per-IP and global start limits, RFC `slow_down` with a growing interval plus a per-IP poll limit, a 10-minute TTL, and retention reaping. |
| Long-lived forgotten keys | 90-day default expiry (`CLI_PERSONAL_KEY_TTL_DAYS`, helm `config.dashboard.cliPersonalKeyTtlDays`), with no infinite personal keys. A re-login on the same device revokes the old key, and the user can see and revoke sessions. |

## Configuration

| Env | Default | Meaning |
|---|---|---|
| `CLI_PERSONAL_KEY_TTL_DAYS` | `90` | Lifetime of a personal key from its browser approval. Rotation keeps the expiry and never renews it. Values outside `1..365` fall back to 90. |
| `ADDRESS_HEADER` / `XFF_DEPTH` | unset | adapter-node's client-IP resolution (helm `config.dashboard.clientIp`). See "Rate limits" above. AWS: `x-forwarded-for` / `2`. |

## End-to-end walkthrough (2026-10-07)

Run against the built dashboard (`pnpm build && node build`, `127.0.0.1:5199`) on a disposable, freshly
migrated Postgres 16, with the real CLI binary (`go build ./tools/sentinel`). **Approval was driven over HTTP
with a dashboard session cookie, not a browser**: a seeded Auth.js database session per user, then the page's own
`__data.json` load and its `?/approve` form action with a same-origin `Origin` header, which is exactly what the
page's form submits.

The seed is org `acme` with projects `web` and `core` and one issue in `core` plus two in `web`. Its users:

| User | Org role | Project rows | What the dashboard gives them |
|---|---|---|---|
| Olivia | owner | none | everything |
| Evan | engineer | none | every project, read-write |
| Pia | viewer | `project_members` row on `web` (role `developer`) | `web` only, read-only |

The dashboard has no "engineer restricted to one project". An org `engineer` role is an org-wide grant
(`requireProjectAccess`), and the only per-project restriction is an org `viewer` plus a `project_members` row,
which `issue-access.ts` makes read-only ("A project grant conveys read only"). So the project restriction is
demonstrated with Pia, and the engineer's write path, denied endpoints and removal are demonstrated with Evan.
Keys are redacted. The CLI never printed one; it shows only the `sent_agent_` prefix.

```text
$ sentinel login --url http://127.0.0.1:5199 --device-name evan-laptop --no-browser
To log in, open this URL in a browser where you are signed in to Sentinel:
    http://127.0.0.1:5199/cli/approve?code=TBGL-CLWH
and check that it shows this code:
    TBGL-CLWH
Only approve it if you started this login yourself, just now.
Waiting for approval (the code expires in 10 minutes; Ctrl-C to cancel)...

## GET /cli/approve?code=TBGL-CLWH as Evan: the page shows
  organizations: [Acme (engineer)], request: {state: pending, client: sentinel-cli, clientVersion: dev,
  deviceName: evan-laptop, requestedIp: 127.0.0.1, createdAt: 02:40:45Z, expiresAt: 02:50:45Z}
## POST ?/approve with Origin: https://evil.example  ->  "Cross-site POST form submissions are forbidden" (403)
## POST ?/approve same-origin                       ->  {"type":"success","status":200, ... "approved","TBGL-CLWH"}

Logged in to http://127.0.0.1:5199
as: Evan Engineer <evan@acme.test> (engineer, read-write) — a personal key: it can do what you can do in the dashboard, nothing more
organization: acme
profile: e2e (current)                       <- the deployment's SENTINEL_CLI_PROFILE
key: sent_agent_ (expires 2027-01-05)
saved: .../home-eng/.config/sentinel/config.json (mode 0600)

$ sentinel whoami
identity: personal key of Evan Engineer <evan@acme.test> (engineer, read-write)
{"name": "Evan Engineer (CLI)", "kind": "personal", "owner": {"userId": "u-eng", "role": "engineer", "canWrite": true}, ...}

$ sentinel projects            -> core, web
$ sentinel issues list         -> core: pq deadlock detected | web: chunk load failed | web: TypeError on checkout
$ sentinel comment <core issue> -body "looking at this from the CLI"
{"comment": {"authorType": "agent", "authorId": "<Evan Engineer (CLI)>", ...}}     [exit=0]
$ sentinel members list
error: server returned 403: Forbidden: personal CLI keys cannot manage organization members; use the dashboard or an org agent key   [exit=3]
$ sentinel members add mallory@evil.example -role admin
error: server returned 403: Forbidden: personal CLI keys cannot manage organization members; use the dashboard or an org agent key   [exit=3]
$ curl -H "Authorization: Bearer <evan key>" -H "Accept: application/json" /api/agent/repo-credentials
{"message":"Forbidden: personal CLI keys cannot fetch repo credentials; use the dashboard or an org agent key"} [HTTP 403]

# Pia: the same device flow (code CFHX-LBLD), approved with her session
## approving CFHX-LBLD a second time -> 400 "This code was already used or denied. Run `sentinel login` again."
as: Pia Project <pia@acme.test> (viewer, read-only) — a personal key: ...
$ sentinel projects            -> web
$ sentinel issues list         -> web: chunk load failed | web: TypeError on checkout      (no core issue)
$ sentinel issues get <core issue>
error: server returned 404: Issue not found                                               [exit=4]
$ sentinel comment <web issue> -body "can I write?"
error: server returned 403: Forbidden: your organization role is read-only, so your personal CLI key cannot make changes   [exit=3]
$ sentinel events   # Evan: [commented on core issue]     Pia: []   (core is outside her scope)

## Olivia (owner, dashboard session): DELETE /api/organizations/<acme>/members/<Evan's membership>
{"success":true,"memberId":"43c7555c-...","userId":"u-eng"} [HTTP 200]
$ sentinel whoami              # Evan, same key, the very next request
auth: failed (401 Invalid API key)                                                        [exit=3]
$ sentinel issues list
error: server returned 401: Invalid API key                                               [exit=3]

     owner      |   device    | status  | revoked |  expires
 evan@acme.test | evan-laptop | revoked | t       | 2026-10-07
 pia@acme.test  | pia-desktop | active  | f       | 2027-01-05

$ sentinel logout              # Pia
revoked key sent_agent_ on http://127.0.0.1:5199
removed profile e2e

 user_code |  status   | device_name | requested_ip | delivered | hash_len      <- cli_device_authorizations
 TBGLCLWH  | delivered | evan-laptop | 127.0.0.1    | t         |       64      (no key, only sha256 of device_code)
 CFHXLBLD  | delivered | pia-desktop | 127.0.0.1    | t         |       64

        action         |        actor        |      resource_type       |   device             <- audit_logs
 cli.device.approved   | evan@acme.test      | cli_device_authorization | evan-laptop
 cli.key.minted        | evan@acme.test      | api_key                  | evan-laptop
 agent.issue.commented | Evan Engineer (CLI) | issue                    |
 cli.device.approved   | pia@acme.test       | cli_device_authorization | pia-desktop
 cli.key.minted        | pia@acme.test       | api_key                  | pia-desktop
 cli.key.revoked       | olivia@acme.test    | api_key                  |              (member removal)
 cli.key.revoked       | Pia Project (CLI)   | api_key                  |              (sentinel logout)
```

Server log over the whole run: 0 matches for a full `sent_agent_<hex>` key, 0 for any session token. The token
endpoint answered `authorization_pending` (400) before the approval and 200 once, per login.

The walkthrough found one defect, now fixed. The CLI never sent `Accept: application/json`, so SvelteKit
rendered every thrown `error()` (the personal-key 403s included) as an HTML error page, and the CLI printed the
whole document. `client.go` and `device.go` now send the header. `TestErrors_RequestJSONSoThrownErrorsAreOneLine`
pins this: it fails without the header and passes with it.

## Behaviour changes for existing users (PR description material)

These change what an existing caller can do. They are intended, and listed so that the PR states them:

1. **Rotating an ORG agent's key in the dashboard now needs owner or admin.** That route is
   `POST /api/organizations/{org}/keys/{keyId}/rotate`. It used to need `manage_keys`, so an
   `engineer` could rotate an org agent's key and receive the new secret. Rotation hands over a working
   credential, so for an agent key it now needs `manage_agents` (owner/admin), the same as issuing one.
   Project and org API keys (non-agent) are unchanged. Nobody can rotate a personal key there; it can
   only be revoked.
2. **`POST /api/agent/key/revoke` returns 403 for an org agent key.** It is the endpoint behind
   `sentinel logout` and revokes only personal keys. An org agent's keys are retired by an owner or
   admin in the dashboard. The CLI's `logout` then removes the local profile and says the key was
   *not* revoked.
3. **Personal keys get 403 on member management** (`GET`/`POST /api/agent/members`,
   `DELETE /api/agent/members/{id}`) and on `repo-credentials`. Org agent keys are unchanged.
4. **Personal keys get no rotation grace and no renewal** (see above). Org agent rotation keeps
   `AGENT_KEY_ROTATION_GRACE_HOURS` and its inherited lifetime.
5. **The CLI sends `Accept: application/json` on every request**, so a thrown error is a one-line
   `{message}` instead of an HTML page. Scripts that parsed the HTML would see JSON now.
6. **Deployments behind a proxy must set `config.dashboard.clientIp`** (done for AWS). Otherwise the
   per-IP device-flow limits are shared by everyone behind the proxy.

## Security review, repair round 1 (2026-10-07)

The independent review of `ca74876` passed with 0 major findings. Round 1 fixed four minor findings
(client IP behind the proxy, personal-key rotation, malformed-input 500s, relation targets in the events
feed) and four notes (frame-ancestors, the replacement warning, same-origin browser opening, the PR notes
above). Evidence is in the commit message of the repair commit.
