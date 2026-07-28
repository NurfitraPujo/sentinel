# Recurring Bug Patterns (`docs/memory/`)

This file stores durable implementation bug patterns and their mitigations. For systemic, high-risk, or governance-level patterns, see `.specify/memory/BUGS.md`.

### 2024-05-20 - Data Loss on Database Outage
**Status**
Active

**Symptoms**
Events are lost or dropped when the Processor cannot reach the PostgreSQL database.

**Root Cause**
Processor service traditionally assumed the database is always available during event processing.

**Future mistake prevented**
Failing to handle transient database connection failures in processing workers.

**Evidence**
Historical analysis of ingestion gaps during database maintenance windows.

**Prevention / Detection**
Use the `GracefulDegradation` buffer in `apps/processor-go/degradation`. Monitor `WARNING: Database unavailable` logs and buffer size metrics.

**Where to look next**
`apps/processor-go/degradation/buffer.go`

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

### 2026-07-26 - Cross-Boundary Payload Contracts Drift Because No Test Spans the Boundary

**Status**
Active

**Symptoms**
Producer and consumer both work, both are tested, and the integration is 100% broken. Confirmed instances:
(a) `packages/sdk-go` emits `error_message`/`context` and no `platform`, while `ingestor-go` reads
`message`/`metadata` and hard-requires `platform` → every SDK event is rejected 400;
(b) the dashboard publishes `{ keyId }` to `api_key.invalidated` while the ingestor reads `key_hash` → API key
cache invalidation never fires.

**Root Cause**
The contract lives in prose (`docs/sdk-specification.md`) rather than in a shared type, and the two sides are
in different modules/languages, so no compiler and no test can span the seam. `packages/sdk-go` is a separate
Go module with no `replace` directive, which made a root-module contract test impossible to write —
**resolved 2026-07-28 by P0-3**, which committed a `go.work`. The test is now writable; see Prevention.

**Future mistake prevented**
Changing a JSON field name, a NATS message body, or a required field on one side of a service boundary
without a test that decodes the *actual bytes* the other side produces.

**Evidence**
Probe test decoding real `sdk-go` output into `validation.ErrorPayload` → `platform="" message=""`, then
`protovalidate` rejection. See `docs/memory/VERIFIED_STATE.md` S4, S7.

**Prevention / Detection**
The `go.work` is in place (P0-3), so the root module can import `packages/sdk-go`. Keep one golden-payload
test per boundary that round-trips producer output through consumer decoding, using
`json.Decoder.DisallowUnknownFields()` so a rename fails the build instead of silently dropping data. Same
for every NATS subject body. Files under `tests/contract/` **must** carry a `//go:build contract` tag — CI's
`go-root` job runs `GOWORK=off`, where an untagged import of `packages/sdk-go` fails to resolve.

**Where to look next**
`packages/sdk-go/event.go`, `apps/ingestor-go/validation/validator.go`,
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

### 2026-07-26 - Authenticated Identity Computed, Then Discarded

**Status**
Active

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

**Evidence**
`apps/ingestor-go/auth/apikey.go:83` vs `apps/ingestor-go/main.go:146-165`.
See `docs/memory/VERIFIED_STATE.md` S6.

**Prevention / Detection**
Handlers must read tenant scope from the request context and reject or overwrite any body-supplied
`project_key` that disagrees. Add a UNIQUE constraint covering `projects(organization_id, name)`.

**Where to look next**
`apps/ingestor-go/main.go`, `apps/ingestor-go/auth/apikey.go`,
`apps/processor-go/store/store.go` (`GetProjectByKey`).
