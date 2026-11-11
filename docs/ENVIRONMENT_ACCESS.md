# Environment access — what the boundary is, and what it deliberately does not do

Sentinel's issues carry an `environment` (`production`, `staging`, `eu-west-1`, …). A person can be
**narrowed** to a subset of a project's environments through
`PUT /api/organizations/{orgId}/projects/{projectId}/environment-grants/{userId}`, and every read
path then filters their issues, searches, reports, relations and notifications to that subset.

**The absence of a grant row means full access to every environment of that project, present and
future.** That is the whole model. No grant row is created by a migration, by traffic, or by
accepting an invitation without a scope — narrowing is always an explicit act performed by a person.

The rules live in one place, `apps/dashboard-web/src/lib/server/environment-access.ts`:

```ts
// null     => unrestricted (no grant row): every environment, present and future.
// string[] => exactly these environment names, in this project.
resolveEnvironmentScope(userId, projectId): Promise<string[] | null>

// The map holds an entry ONLY for a (user, project) pair that has a grant row.
// A project ABSENT from the map is unrestricted — the same "absence means all" rule.
resolveEnvironmentScopes(userId): Promise<Map<projectId, string[]>>
```

Denial on the environment dimension is a **`404`, never a `403`**: an issue in an environment you may
not see must be indistinguishable from an issue that does not exist. A `403` on `/issues/<uuid>`
tells a contractor that a production issue with that id exists, which is the fact the boundary was
drawn to withhold.

---

## Known limits — properties of the shipped system, not gaps

**These are not deferred work and not oversights. They are what this feature deliberately does not
do, and they ship documented.**

**1. The environment boundary is enforced on READ and absent on WRITE.**

> `project_api_keys` has no environment column and `apps/ingestor-go/mapping/mapper.go` copies the
> client-asserted `payload.Environment` through untouched, so **anyone holding a project's ingest key
> can write into the `production` partition that a contractor is walled out of.** The operator has
> accepted this on the stated ground that **ingest keys are infrastructure credentials held by deploy
> pipelines and never by the people the boundary excludes.**

**2. Key-authenticated reads are not environment-scoped either.** `/api/agent/issues` and the rest of
`/api/agent/*` authenticate a `project_api_keys` row with `scope='agent'` to an **organization**
(`agent-auth.ts: AgentAuthContext` has an `organizationId` and no `userId`). There is no person, no
membership row and no grant to resolve, so a registered agent reads every environment of every project
in its organization. §4.6's `environment` parameter is a **filter, not a guard**. Same accepted class
as limit 1, same ground.

**3. An environment's name is not protected; its data is.** The management view
(`GET …/environments?includeHidden=true`, `write` required) returns every environment regardless of
the caller's grant, and a duplicate-name `POST` would leak the same fact through its `409` anyway. A
narrowed member who opens project settings can learn that a `production` environment exists. They
cannot read one row of its data.

**4. Environment is client-asserted.** Every filter, every guard and every scoped alert rests on
services telling the truth about which environment they are. A staging pod misconfigured to report
`production` creates production issues, fires production alerts and is visible to production-scoped
readers only. A misconfiguration is indistinguishable from an assertion.

**5. A user report defaults to `production`, and is therefore guarded like production data.** A
report whose reporting client asserts nothing is written the literal `production`, which is a real,
grantable environment name — not a sentinel value — so a report **is** filtered by the guard exactly
like an error issue. **A member granted only `{staging}` therefore sees no user reports at all**,
neither on `/[orgSlug]/reports` nor in Manual Issues, and neither in the count. The remedy is in the
product: an administrator grants them `production`.

**5b. The unscoped page is a DELIBERATE boundary crossing on the notification path.** The ordinary
rule is that a subscriber outside the issue's environment gets neither a `notifications` row nor an
email — the only guard that stops something *leaving* the product. **That rule is not absolute, and
this is the exception.** When the one candidate an on-call rota resolves does not hold the issue's
environment, one `notifications` row is written **per rota member** — `schedule.members` plus the
override holder where one is in force — and each is emailed, **including members who do not hold that
environment**. Mail leaves the product to people the environment boundary would otherwise exclude.

> **This is the operator's stated over-page choice, made with the trade in view**: *a rota that can
> silently page nobody for a production incident is a worse failure than one that occasionally pages
> too many.* **What limits the exposure:** the email and the notification text carry the word
> `unscoped`, so a recipient is told why they were included; the deep link still `404`s; and the rota
> is an org-level on-call list, not an arbitrary audience. **What is exposed:** the issue's title,
> through the email subject and the notification row.

**6. An org-wide alert config is guarded against a UNION, not an intersection.** The names a caller
may put on an org-wide config are the union over the projects they can see, so a member unrestricted
in project B may scope an org-wide config to `production` even though they are walled out of project
A's `production` — and that config then pages on project A's production errors. This is a limit on
**alert scoping**, not on data visibility: that member still cannot read one row of project A's
production issues.

**7. One related issue's EXISTENCE is visible to a viewer who may not read it.** On the three
issue-detail loaders, a relation whose *other* issue is outside the viewer's environment scope
renders as a **redacted placeholder** — a row saying a related issue exists that they cannot see —
rather than being omitted. The `404` rule makes environment-dimension denial indistinguishable from
non-existence precisely so absence and non-existence cannot be told apart; a placeholder **leaks
existence**, which that rule forbids.

> **Accepted deliberately, and only here.** Every list, search, count and direct fetch keeps the
> `404` shape. The operator chose this over silently omitting the relation because a panel that drops
> relations misrepresents the graph, and a triager can re-link an issue that is already linked with
> no indication why the link will not take. **The existence fact the placeholder leaks is one the
> triager is about to rediscover by trying.** The placeholder carries no id, message, error class,
> status or fingerprint, so what leaks is one bit: *something is there*.

**8. A `viewer` sees no reports today, and still sees none.**
`routes/api/organizations/[orgId]/reports/+server.ts` narrows a `viewer` by
`getAccessibleProjectIds`, which reads `project_members` — a table nothing in this application
writes. So an org `viewer` resolves an empty set and sees **zero** reports. **That branch is left
exactly as it is**: widening it would make viewers *start* seeing their organization's reports, which
is an access decision and not a mechanical one.

---

## Operating notes

- **A grant cannot be created for an environment that does not exist.** Every name in a grant must be
  a row in that project's registry — **every row of it, including `hidden` ones**. Hiding removes a
  name from *selectors*, not from *grants*: a grant is the boundary, not the menu.
- **An organization `owner` cannot be narrowed** (`422`). The person who can undo a narrowing must
  never be the person narrowed out.
- **An empty grant is refused** (`400`). A member who may see nothing is a member who should be
  removed, not stored.
- **Removing an organization member deletes their grants** for that organization's projects, in the
  same transaction as the removal. A grant does not outlive the membership it narrows: otherwise a
  contractor narrowed to `{staging}`, removed, and invited back later as a full engineer would be
  walled out of production with no UI anywhere saying why.
- **A scoped member's bookmark stops working.** A URL that returned `200` yesterday returns `404`
  after somebody narrows their grant. That is the boundary working, and the not-found page says the
  same thing it says for any missing issue — which is the point.
