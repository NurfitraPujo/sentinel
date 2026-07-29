# Recurring Bug Patterns (`docs/memory/`)

This file stores durable implementation bug patterns and their mitigations. For systemic, high-risk, or governance-level patterns, see `.specify/memory/BUGS.md`.

### 2024-05-20 - Data Loss on Database Outage (mitigation found broken 2026-07-29, still Active)
**Status**
Active. **The mitigation this entry originally pointed to does not do what it says — verified 2026-07-29,
unchanged by the P0/P1/P2/P2b work. Do not treat `GracefulDegradation` as a working safety net without
first reading the Root Cause update below.**

**Symptoms**
Events are lost or dropped when the Processor cannot reach the PostgreSQL database.

**Root Cause**
Processor service traditionally assumed the database is always available during event processing.

**Root Cause — update 2026-07-29**
The `GracefulDegradation` buffer this entry recommends as Prevention (`apps/processor-go/degradation/buffer.go`)
has its own defect, unrelated to and unfixed by any of the P0–P2b work: `CheckAndBuffer` returns `true` in
two *different* situations — DB healthy, **and** DB down but the event was successfully pushed into the
buffer (`Push`'s return value is passed straight through). The caller,
`ProcessorService.ProcessEvent` (`processor_service.go:36-41`), only special-cases `false`:

```go
if !s.degradation.CheckAndBuffer(ctx, data) {
    log.Printf("Event buffered due to database unavailability")
    return nil          // ACKs the NATS message
}
return s.processEventInternal(ctx, data)
```

Net effect, verified by reading the code (not yet by a live outage drill):
- DB down, buffer has room → `CheckAndBuffer` returns `true` → **`processEventInternal` is called anyway**,
  against a database that is down. It fails, and the resulting error is what actually gets retried — via
  D10's NATS redelivery (added in this same round of work), not via the buffer. The in-memory buffer entry
  is essentially inert: it is only ever drained by `Flush`, which is called at the end of a *successful*
  `processEventInternal` run, so nothing drains it until some other event succeeds — and since the buffer is
  in-memory and unpersisted, a process restart during the outage loses it anyway, silently.
- DB down, buffer full → `Push` returns `false` → `CheckAndBuffer` returns `false` → the log line
  `"Event buffered due to database unavailability"` fires and `ProcessEvent` returns `nil`, **ACKing the
  message with no record of the event anywhere**. The log line fires exactly when the event was *not*
  buffered — it describes a drop as a save.
This means D10 (bounded-retry NATS delivery, `DECISIONS.md`), not this buffer, is what currently keeps an
event alive across a *transient* outage — and this buffer's only observable effect is the buffer-full case,
which is a silent, mislabeled data loss path strictly worse than doing nothing.

**Future mistake prevented**
Failing to handle transient database connection failures in processing workers. Also, newly: a boolean
return value that is `true` for two semantically different reasons ("all is well" vs "the fallback path
absorbed it") will eventually be read as "all is well" by a caller that only checks for `false`.

**Evidence**
Historical analysis of ingestion gaps during database maintenance windows (original). Code reading of
`apps/processor-go/degradation/buffer.go` (`CheckAndBuffer`, `Push`) and
`apps/processor-go/service/processor_service.go:36-41`, 2026-07-29 — no live-outage test exists for this
path; see `docs/memory/VERIFIED_STATE.md` S9 for the same finding with a table of the three cases.

**Prevention / Detection**
Do not use `GracefulDegradation`/`CheckAndBuffer` as evidence that an outage is handled until its return
value is split into three states (healthy / buffered / dropped) and `ProcessEvent` branches on all three —
in particular, `processEventInternal` must never run while `CheckAndBuffer` reports the DB down. Until then,
treat D10's bounded NATS retry as the actual (partial) mitigation, and the buffer-full case as an open,
silent data-loss bug. Monitor `WARNING: Database unavailable` logs, but read the very next log line —
`"Event buffered"` there currently means the opposite of what it says whenever it follows a full buffer.

**Where to look next**
`apps/processor-go/degradation/buffer.go` (`CheckAndBuffer`, `Push`), `apps/processor-go/service/processor_service.go`
(`ProcessEvent`), `docs/memory/DECISIONS.md` D10 (the mitigation that is actually load-bearing today).

---

### 2026-07-24 - Reserved Path Collision Guard for Dynamic Slug Routing

**Status**
Active

**Symptoms**
Navigating to global static pages like `/settings` or `/admin` triggers a 403 Forbidden error because server middleware attempts to look up an organization named `settings`.

**Root Cause**
Catch-all dynamic top-level parameters (`/[orgSlug]`) in SvelteKit intersect with global static routes unless explicit reservation checks are implemented.

**Future mistake prevented**
Top-level dynamic routing breaking system endpoints or trapping users in unauthorized org lookup errors.

**Evidence**
Ripple scan finding `R-002` in `specs/005-organization-layer/ripple-report.md`.

**Prevention / Detection**
Maintain an explicit `RESERVED_SLUGS` list (`['admin', 'settings', 'api', 'auth', 'docs', 'billing', 'support']`) enforced both at creation validation (`createOrganization`) and in server middleware (`hooks.server.ts`).

**Where to look next**
`apps/dashboard-web/src/lib/db/queries/organizations.ts` (`RESERVED_SLUGS`) and `apps/dashboard-web/src/hooks.server.ts`.

---

### 2026-07-26 - "Shipped" Features That Are Never Invoked From `main()`

**Status**
Active

**Symptoms**
A package is fully implemented, has hundreds of lines of passing unit/integration tests, is marked Completed
in `specs/` and celebrated in `WORKLOG.md` — and does nothing in production because no call site reaches it.
Confirmed instances: `apps/processor-go/alerts` (Dispatcher never constructed by `NewProcessorService`),
`apps/processor-go/notifiers` (zero non-test references), `apps/ingestor-go/validation.ValidatePayload`
(superseded by protovalidate), and the entire authenticated project context in
`apps/ingestor-go/auth/apikey.go` (written to request context, never read by the handlers).

**Root Cause**
Spec-driven tasks are scoped per-package and their acceptance tests are written against the package's public
API in isolation. No task in any `tasks.md` asserts "the running binary reaches this code", and no test
exercises a composed service graph.

**Future mistake prevented**
Declaring a feature complete on the strength of green package tests, and recording that claim in durable
memory where later sessions trust it.

**Evidence**
`grep -rn "alerts\.\|Dispatch(" --include='*.go' apps/ packages/ | grep -v _test.go` returns only the
declaration. See `docs/memory/VERIFIED_STATE.md` S6, S8.

**Prevention / Detection**
Before marking any feature complete, run a reachability grep from the feature package back to `main()` or an
HTTP route, and add at least one test that constructs the real service graph. Treat an unreferenced exported
symbol outside `packages/sdk-go` as a release blocker.

**Where to look next**
`apps/processor-go/service/processor_service.go` (`NewProcessorService`), `apps/ingestor-go/main.go`.

---

### 2026-07-26 - One Broken Test File Silently Disables an Entire Go Test Package

**Status**
Active

**Symptoms**
`task test-unit` fails with what looks like a single compile error in one file. In fact **all 11 files** in
`tests/unit/` — ~2,800 lines covering fingerprinting, masking, normalization, validation, degradation and
indexing — have not executed since the break. Regressions land unnoticed because "the failing test" reads as
a known, isolated annoyance.

**Root Cause**
Go compiles per-package, not per-file. `middleware.NewRateLimiter` was resignatured for feature 008 and
`ratelimiter.Allow()` deleted, but `tests/unit/ingestor_middleware_test.go` was not updated. With all unit
tests in one flat `tests/unit` package, any single stale file zeroes out coverage for the whole suite.

**Future mistake prevented**
Reading a build failure as "one bad file" rather than "the suite is off", and changing shared API signatures
without grepping the flat test package.

**Evidence**
`go vet ./...` / `go test ./tests/unit/...` → `tests/unit/ingestor_middleware_test.go:17:48: too many
arguments in call to middleware.NewRateLimiter`. See `docs/memory/VERIFIED_STATE.md` S1.

**Prevention / Detection**
Always run `go build ./... && go vet ./...` before and after touching any exported signature under
`apps/` or `packages/shared-go/`. Distinguish "0 tests ran" from "tests passed" when reading output.

**Where to look next**
`tests/unit/ingestor_middleware_test.go`, `apps/ingestor-go/middleware/ratelimit.go`.

---

### 2026-07-26 - Cross-Boundary Payload Contracts Drift Because No Test Spans the Boundary (test landed 2026-07-29)

**Status**
Resolved for the SDK↔ingestor seam — a contract test now exists and runs in CI. The dashboard↔ingestor
NATS seam (symptom (b) below) is untouched by this work and remains open; do not mark it resolved.

**Symptoms**
Producer and consumer both work, both are tested, and the integration is 100% broken. Confirmed instances:
(a) `packages/sdk-go` emits `error_message`/`context` and no `platform`, while `ingestor-go` reads
`message`/`metadata` and hard-requires `platform` → every SDK event is rejected 400;
(b) the dashboard publishes `{ keyId }` to `api_key.invalidated` while the ingestor reads `key_hash` → API key
cache invalidation never fires. **(b) is still an open bug — see VERIFIED_STATE.md S7. Nothing in this round
of work touched it.**

**Root Cause**
The contract lives in prose (`docs/sdk-specification.md`) rather than in a shared type, and the two sides are
in different modules/languages, so no compiler and no test can span the seam. `packages/sdk-go` is a separate
Go module with no `replace` directive, which made a root-module contract test impossible to write —
resolved 2026-07-28 by P0-3, which committed a `go.work`.

**Future mistake prevented**
Changing a JSON field name, a NATS message body, or a required field on one side of a service boundary
without a test that decodes the *actual bytes* the other side produces.

**Evidence**
Probe test decoding real `sdk-go` output into `validation.ErrorPayload` → `platform="" message=""`, then
`protovalidate` rejection. See `docs/memory/VERIFIED_STATE.md` S4, S7.

The test envisioned in the previous version of this entry now exists: `tests/contract/sdk_ingestor_test.go`
(471 lines, added 2026-07-29 / P2-4). It builds an event with the real `packages/sdk-go` API, marshals it
exactly as the SDK's transport does, decodes it with the real `apps/ingestor-go/validation.ErrorPayload`
(`json.Decoder.DisallowUnknownFields()`), maps it with the real `mapping.MapPayloadToEvent`, and validates
the result with the real `protovalidate` validator built from the real proto — no field list or shape is
reimplemented. Verified running green: `go test -tags=contract ./tests/contract/...` → 4 passed. Per the
S16 fix in this same round (the SDK's single `ProjectKey` field used to carry both the secret and the body
project name — see the API-key-management entry below), this is also the test that would have caught S16 had
it existed sooner: it is the first place the *real* `sentinel.Config`/`sentinel.NewEvent` output is decoded
by the *real* ingestor code, rather than by a hand-written fixture standing in for one side.

**Prevention / Detection**
The `go.work` is in place (P0-3), so the root module can import `packages/sdk-go`. Files under
`tests/contract/` **must** carry a `//go:build contract` build tag: CI's `go-root` job pins `GOWORK=off` on
purpose (to keep exercising the root module exactly as a real downstream `go get` would see it — see
ARCHITECTURE.md's three-module-layout entry), and under `GOWORK=off` an untagged import of `packages/sdk-go`
does not resolve at all — it would turn `go-root` red with a confusing "no required module provides package"
error rather than a clear test failure. The `contract` CI job runs the tagged directory explicitly in
workspace mode. Keep one golden-payload test per boundary that round-trips producer output through consumer
decoding this way. The dashboard↔ingestor NATS seam ((b) above) has no equivalent test yet and is the next
candidate for this same treatment.

**Where to look next**
`tests/contract/sdk_ingestor_test.go`, `packages/sdk-go/event.go`, `apps/ingestor-go/validation/validator.go`,
`apps/dashboard-web/src/lib/db/queries/apikeys.ts`, `apps/ingestor-go/auth/apikey.go`.

---

### 2026-07-26 - Normalization Destroys the Fields Read After It

**Status**
Active

**Symptoms**
`release_version` is always the literal string `<VERSION>` on every processed event, so
`isRegressionVersion` compares `0` to `0` and the whole regression-tracking feature (006) is inert.

**Root Cause**
`ErrorEvent.Normalize` rewrites the metadata map through `NormalizeString` — whose `versionRegex` replaces
any semver with `<VERSION>` — and only *afterwards* reads `metadata["release_version"]` out of the rewritten
map. Normalization is designed to destroy high-cardinality values; anything extracted from metadata for
semantic use must be read before it runs.

**Future mistake prevented**
Adding a new metadata-derived field (release, build id, tenant, commit sha) below the `Normalize` call and
silently getting a placeholder token.

**Evidence**
Probe: input `{"release_version":"1.4.2"}` → `release_version="<VERSION>"`.
`apps/processor-go/event/event.go:41-49`. See `docs/memory/VERIFIED_STATE.md` S5.

**Prevention / Detection**
Extract all semantically-meaningful metadata fields *before* `Normalize`, or maintain an explicit
passthrough key set in `normalizer`. Assert on a real semver in any test touching this path.

**Where to look next**
`apps/processor-go/event/event.go`, `apps/processor-go/normalizer/normalizer.go`,
`apps/processor-go/store/store.go` (`isRegressionVersion`).

---

### 2026-07-26 - Authenticated Identity Computed, Then Discarded (resolved 2026-07-29)

**Status**
Resolved. Kept here — do not delete — because the original defect and the near-miss on its first fix
attempt are both worth knowing about; see "Fix, and how the fix could have failed" below.

**Symptoms**
Any holder of an active `ingest`-scope API key can write error events into any other tenant's project by
naming that project in the JSON request body.

**Root Cause**
`APIKeyAuthenticator.Middleware` resolves the key's project and stores it at `ctx["project_key"]`, but
`handleIngest`/`handleBatchIngest` read `payload.ProjectKey` from the untrusted body instead. `processor-go`
then resolves that body value against `projects.name` — a column with no UNIQUE constraint. The
authenticated identity is used only for rate-limiting and log lines.

**Future mistake prevented**
Treating "the request was authenticated" as equivalent to "the request is authorized for the resource it
names". Every tenant-scoped write must derive its scope from the credential, never from the payload.

**Fix, and how the fix could have failed**
`apps/ingestor-go/main.go`'s new `applyAuthenticatedScope` now forces tenancy onto the identity resolved
from the API key, in both `handleIngest` and `handleBatchIngest`, before `svc.Ingest` runs. Two credential
shapes:
- **Project-scoped key**: the project is fixed by the credential; a body naming a *different* project is
  rejected 403 rather than silently overwritten.
- **Organization-wide key**: the caller selects the target project by name, either via the `X-Project-Key`
  header or the body's `project_key`. **Both are resolved by `auth.ResolveProjectInOrg`, scoped to the
  authenticated organization** — a name outside that org is 403, and the error is deliberately
  indistinguishable between "no such project" and "belongs to another org" so it can't be used to enumerate
  tenants.
- The header takes precedence when both are present; the body is then not consulted at all for that field.
This last point is the trap this class of fix walks into: moving the trusted value from the request *body*
into a request *header* looks like progress but is not, by itself, a fix — a header is exactly as
attacker-controlled as a body field unless the new location is *also* validated against the credential's
own scope. A naive version of this fix (take `X-Project-Key` and resolve it against `projects.name`
globally, the way `GetProjectByKey` used to) would have reintroduced S6 verbatim under a new spelling. What
actually closes the hole is `ResolveProjectInOrg`'s `WHERE name = $1 AND organization_id = $2` — the
organization id itself comes only from the authenticated context (`auth.OrganizationIDFromContext`), never
from anything client-supplied. **Verified**: project-scoped key + body naming another project → 403; org-wide
key naming a project in another org → 403, zero rows written. Also added: `CREATE UNIQUE INDEX
idx_projects_org_name ON projects (organization_id, name)` (`1721800000_add_organization_layer.sql`), closing
the "two orgs, same project name, arbitrary resolution" hole that let this bug bite even a careful caller.

**Evidence**
`apps/ingestor-go/auth/apikey.go` (`ResolveProjectInOrg`), `apps/ingestor-go/main.go`
(`applyAuthenticatedScope`), `apps/ingestor-go/auth/context.go` (`WithIdentity`, `IsOrgWideKey`).
See `docs/memory/VERIFIED_STATE.md` S6.

**Prevention / Detection**
When a security fix relocates a value from one attacker-reachable channel to another (body → header, query
param → header, etc.), that is not itself the fix. Ask "what stops the new channel from naming an
out-of-scope resource" before calling it closed — the answer must be a lookup scoped by something read from
the *authenticated credential*, never by another piece of client input, however differently spelled.

**Where to look next**
`apps/ingestor-go/main.go` (`applyAuthenticatedScope`), `apps/ingestor-go/auth/apikey.go`
(`ResolveProjectInOrg`), `apps/ingestor-go/auth/context.go`, `apps/processor-go/store/store.go`
(`ResolveProjectID`, `GetProjectByKey`).

---

### 2026-07-29 - A Typed Context Key That No Longer Matches a Bare String Key Fails Silently, Not Loudly

**Status**
Resolved — self-inflicted during this round of work and caught before landing on `main`. Recorded because
the failure mode is generalizable and gave zero errors anywhere.

**Symptoms**
Rate limiting passed 100% of requests regardless of how many were sent — no error, no log line, no failing
test. Twelve rapid requests against a configured limit of 5 all returned `202`.

**Root Cause**
`apps/ingestor-go/auth/context.go` was introduced to replace bare `string` context keys
(`context.WithValue(ctx, "api_key_hash", ...)`) with a private `ctxKey int` type — the standard Go fix for
cross-package key collisions, and also part of the fix for the "Authenticated Identity Computed, Then
Discarded" entry above (B7/S6). But `apps/ingestor-go/middleware/ratelimit.go`
was not updated in the same change: it still called `r.Context().Value("api_key_hash").(string)`, a bare
string key. `context.Value` compares the **dynamic type** of the key, not just its printed form — a
`ctxKey(3)` and the string `"api_key_hash"` are never equal, even if `ctxKey` is backed by an int and would
`fmt.Sprintf` to something readable. The lookup did not panic or error; the two-result type assertion just
returned `("", false)`, which `ratelimit.go`'s existing `if !ok { next.ServeHTTP(w, r); return }` treated as
"this request has no rate-limit identity, let it through" — its designed behavior for genuinely
unauthenticated traffic, now silently firing for every authenticated request too.

**Why nothing caught it**
Three test files hand-built `context.WithValue(ctx, "api_key_hash", ...)` with the same bare string literal
the middleware was reading — so the tests and the (broken) production code agreed with each other and both
disagreed with the actual context-construction path (`auth.WithIdentity`) that real requests go through.
A test that constructs its input the way a unit test author finds convenient, rather than the way the real
call site constructs it, cannot detect the two having drifted apart.

**Future mistake prevented**
Changing a context key's type without grepping every `ctx.Value(...)` call site for that key across package
boundaries — `go vet` does not catch this; a raw `interface{}` key comparison is not a compile-time property.
And: hand-constructing a `context.Context` in a test using a *literal* key instead of the package's own
constructor function hides exactly this class of drift, because the test's fake input and the code's real
input can silently diverge without either side failing.

**Evidence**
`apps/ingestor-go/middleware/ratelimit.go` (before fix): `apiKeyHash, ok := r.Context().Value("api_key_hash").(string)`.
`apps/ingestor-go/auth/context.go`: `type ctxKey int`. Proven via a 12-request sweep against a limit of 5:
`202 202 202 202 202 429 429 429 429 429 429 429` only after the fix (`Retry-After: 60`,
`X-RateLimit-Limit: 5`, `X-RateLimit-Remaining: 0`). See `docs/memory/VERIFIED_STATE.md` R1.

**Prevention / Detection**
When introducing a typed context key to replace a bare string one, grep every `.Value(` call for the old
string literal across the *whole* module, not just the package the key type lives in, and change them all in
the same commit. Export accessor functions (`auth.APIKeyHashFromContext`, `auth.WithIdentity`) from the
package that owns the key type so no other package can construct or read the value any other way — tests
included. A context lookup that "fails" must be distinguishable in a test from a context lookup that
"correctly found nothing"; here they produced the identical `("", false)`.

**Where to look next**
`apps/ingestor-go/auth/context.go` (`WithIdentity` and the typed accessors),
`apps/ingestor-go/middleware/ratelimit.go`, `apps/ingestor-go/middleware/ratelimit_test.go`.

---

### 2026-07-29 - A Component With Zero Throughput Proves Nothing About What's Behind It

**Status**
Resolved (both halves) — recorded for the general lesson, which will recur.

**Symptoms**
The ingest pipeline's front door (`/ingest`, S3) rejected 100% of well-formed events for the life of the
project. Once that was fixed, the database turned out to be uninsertable for an entirely unrelated reason
(S12: two contradictory `CHECK` constraints on `issues.status` whose intersection excluded the only value
the application ever writes) — a defect that had been sitting there just as long, completely undetected,
because nothing had ever gotten far enough to trip it.

**Root Cause**
S3 and S12 are causally unrelated — one is a protovalidate field-rule bug, the other is a migration that
never dropped a superseded constraint — but S3 sat in front of S12 on every possible code path, so S12 could
not produce a symptom. `issues` had **zero rows, ever**, for the life of the project, and that fact alone
was never noticed because nothing was tracking "does this table have any rows" as a health signal — every
existing check (unit tests, package-level integration tests) exercised each component in isolation with
mocked or bypassed neighbors, so a real end-to-end write path never ran until this recovery work built one.

**Future mistake prevented**
Reading "the front-door bug is fixed" as "the pipeline works," and treating a component that has never
received real traffic as validated because its own tests pass. Zero throughput through component A is not
evidence that component B (downstream of A) is fine — it is the *absence* of evidence, and the two are easy
to conflate because both look like "no errors reported."

**Evidence**
`docs/memory/VERIFIED_STATE.md` S3, S12. Before this round: 0 rows in `issues`/`error_occurrences`, ever.
After fixing S3 alone (without S12), inserts began failing with SQLSTATE 23514 — a *new*, previously
invisible failure. After S12: a POSTed event now produces both an `issues` row and an `error_occurrences`
row (verified live against `sentinel-postgres`: `select count(*) from issues` → non-zero, first time ever).

**Prevention / Detection**
When fixing a defect that sits at the front of a pipeline, do not stop at "the front door now accepts
input" — trace at least one request all the way to its storage side effect and check that the effect
actually happened (a row exists, not just "no error was returned"). Treat "this table has zero rows" as
itself a finding worth investigating, independent of whether any test is red. A green test suite for a
downstream component says only that the component behaves correctly *for the inputs the suite constructs*,
not that any real input has ever reached it.

**Where to look next**
`docs/memory/VERIFIED_STATE.md` S3, S12; `packages/db-migrations/migrations/1716508800_init.sql:22` and
`1721900000_add_issue_lifecycle_and_relations.sql` (the two constraints); `apps/processor-go/store/store.go`.

---

### 2026-07-29 - Multiple Independent Migration Ledgers Pointed at One Physical Database

**Status**
Active — a hazard discovered, not yet structurally closed.

**Symptoms**
Running the integration suite corrupts the shared local development database: a test's migration `down`
dropped `project_api_keys` while the main application's own `schema_migrations` ledger still recorded
version `1722000000` (which creates that table) as applied. The dev database and the "current schema
version" it believes it's at can disagree after nothing more than running tests.

**Root Cause**
`packages/db-migrations` migrations are re-run under several different goose `TableName` values —
`schema_migrations` (the app's own), plus `seq_migrations`, `processor_migrations`, and
`baseline_test_migrations` used by different integration test setups (`tests/integration/setup_test.go` and
neighbors) — **all against the same physical Postgres instance** in local/dev use. Each ledger independently
tracks "have I applied version X", but the actual tables and constraints in that one database are shared and
mutated by whichever ledger's `up`/`down` ran most recently. A `down` issued under one ledger's belief that
it owns the schema will drop objects that another ledger's bookkeeping still says exist.

**Future mistake prevented**
Assuming a local Postgres instance is safe to run the integration suite against just because "it's just
dev data" — the risk here is not data loss in rows, it's the *schema itself* going out of sync with what
every ledger believes, which then produces confusing, seemingly-random failures in unrelated code that
depend on a column or constraint one ledger's `down` just removed.

**Evidence**
Observed during the P2/P2b recovery work: `seq_migrations`, `processor_migrations`, `baseline_test_migrations`,
and `dashboard_migrations` (four names) coexist in the codebase's test setup, all against one physical
database in the default local/dev configuration.

**Prevention / Detection**
Point integration-test migration runs at a dedicated, disposable database (a fresh testcontainer or a
throwaway schema/database name), never at the developer's shared dev database, regardless of what
`TableName` the test uses for its own ledger — a distinct ledger name does not imply a distinct or safe
target database. Until that separation exists, treat "I just ran the integration suite" as a reason to
suspect your local dev database's schema state, and re-provision it (`task infra-up` from a clean volume)
before trusting it for manual verification. See also `docs/plans/E2E_RECOVERY_PLAN.md` and the "multiple
hand-maintained schema copies" note under ARCHITECTURE.md A1.

**Where to look next**
`tests/integration/setup_test.go` and neighboring `*_test.go` files (each `MigrationOptions.TableName`),
`packages/db-migrations/cmd/migrate/`, `scripts/db/init.sql` (a third, independently hand-maintained schema
copy — see ARCHITECTURE.md A1).
