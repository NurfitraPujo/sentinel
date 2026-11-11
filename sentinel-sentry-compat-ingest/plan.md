# Sentinel Sentry-compatible ingest: implementation plan

**Feature:** `sentinel-sentry-compat-ingest` · **Repository:** `sentinel` (`NurfitraPujo/sentinel`) · **Base:** `origin/main`, at planning
time `2812b23` (v0.2.39 production, v0.2.40-staging)
**Built from:** `sentinel-sentry-compat-ingest/spec.md` @ `42b1560` (branch `NurfitraPujo/sentinel-sentry-compat-ingest-specifying`,
passed spec review round 2 with 0 majors), with its header re-pinned in this commit, and contract
`prd/sentinel-sentry-compat-ingest/{contract,be,fe}.md` @ `c14e9b9` (D1-D15). If this plan and the spec disagree, **the spec wins**
and the seat reports the conflict upward. There is one exception: where the spec and the **contract** disagree, the contract wins
(the spec's own header says so). §6 lists the places where this plan adds to the spec.
**Operator rule (2026-09-26):** 0 majors passes, and the review's minors and notes become implementation steps. §4 names each open
finding, the step that closes it, and the proof.
**Cards:** side `be` (`apps/ingestor-go`, the chart, tests, docs) gets `TASK-02`, `TASK-03`, `TASK-04`, `TASK-05` and `TASK-07`.
Side `fe` (`apps/dashboard-web`) gets `TASK-06` only. `TASK-01` (the cost check) is done.
**Tier:** standard. The declared risk surfaces are auth (two new key sources), credentials (ingest-only keys), a trust boundary
(a compat-only proxy list, shipped empty) and untrusted compressed input. There is no migration.

"Red before green" follows CLAUDE.md. Every proof is seen to fail first, either because the production code does not exist yet or
because the spec's §11.2 "Mutation" is applied and then reverted. Each commit body records which, and what failed. `ig/` means
`apps/ingestor-go/`, `dw/` means `apps/dashboard-web/`, and `tu/` means `tests/unit/`.

---

## 1. Seats and sides

**One pull request, two sides.** The `be` commits (B1-B9) and the `fe` commit (F1) share exactly one path:
`tests/fixtures/sentry/project-id-vectors.json`. Its bytes are fixed by spec §3.4, so whichever side writes it first writes those
bytes, and the other side's add is byte-identical (git merges an identical add/add without conflict). F1 depends on nothing in B1-B9
(the interface-first FE rule): it builds against the spec's §3.4 vector and §8. It may run in its own `fe` seat in parallel, on a branch
merged into the PR branch before the gate. Either way, the PR's final commit carries both sides.

| | |
|---|---|
| **Implementing branch** | `NurfitraPujo/sentinel-sentry-compat-ingest-implementing`, cut from `origin/main` after `git fetch origin`. The first action is to merge `origin/NurfitraPujo/sentinel-sentry-compat-ingest-specifying` (docs only: `spec.md`, `plan.md`), so the PR carries the spec it implements. |
| **Side `be`** | B1-B9 (§3), in order. B1-B6 build packages without the route, B7 mounts the routes, and B8 adds relay identity on top of B7's handlers. |
| **Side `fe`** | F1 (§3), independent of every `be` commit. |
| **Pull request** | one, against `main`: `feat: Sentry-compatible ingest — envelope/store routes, Sentry key auth, bounded decoder, Sentry→v2 mapper, Dart grouping, relay trust, Sentry project id (sentinel-sentry-compat-ingest)` |
| **Gate** | §5, run on the final pushed commit. Its output goes into the PR body and the report. |

### 1.1 Paths (the whole change's surface; spec §10.4)

```
# side be
apps/ingestor-go/sentrycompat/{routes,chain,decode,envelope,handler,mapper,metrics,relay,shed}.go   (new)
apps/ingestor-go/auth/sentry.go                  (new) SentryKey, ParseSentryAuth, SentryProjectID, ScopesSentryCompat, SentryMiddleware
apps/ingestor-go/auth/apikey.go                  resolveKey factored out (Middleware byte-identical); NewAPIKeyAuthenticatorWithLookup
apps/ingestor-go/middleware/ratelimit.go         limit core, SentryMiddleware, RetryAfterSeconds, SentryRateLimitsValue
apps/ingestor-go/clientip/clientip.go            Resolvers, parseTrustedList, PeerTrusted, Info.PeerTrusted
apps/ingestor-go/service/service.go              ingestAt factored out (IngestAt byte-identical); IngestAtWithin, Shedder, MaxPublishBytes,
                                                 MaxShedRounds, ErrEventTooLarge, the ingest.event_shed log
apps/ingestor-go/validation/bounds.go            MarkServerTruncated, IsMetadataExemptKey (applyBounds unchanged)
apps/ingestor-go/main.go                         Resolvers call, compat Config, NewItemRecorder, two chains, Mount
deploy/helm/sentinel/values.yaml, templates/ingestor.yaml                 sentryCompatTrustedProxyCidrs: [] → SENTRY_COMPAT_TRUSTED_PROXY_CIDRS
deploy/helm/sentinel/values.aws.staging.yml, values.aws.production.yml    explicit sentryCompatTrustedProxyCidrs: [] with its comment
tests/unit/sentry_compat_{auth,envelope,mapper,budget,grouping,relay,fixtures,docs}_test.go, tests/unit/helm_sentry_compat_trusted_proxy_test.go  (new)
tests/e2e/sentry_compat_test.go                  (new) U40 (supplementary)
tests/fixtures/sentry/**                         (new) spec §10.1-§10.2, README.md, the deny-list of identifier hashes
docs/sdk-specification.md, deploy/helm/sentinel/RUNBOOK.md, README.md      spec §9
# side fe
dw/src/lib/sentry-project-id.ts (+ .test.ts)     (new) spec §8.2, P6.1
dw/src/routes/[orgSlug]/projects/[projectId]/settings/+page.svelte (+ page.sentry-project-id.test.ts)   spec §8.1, P6.2
tests/fixtures/sentry/project-id-vectors.json    shared with be, byte-identical to spec §3.4
```

**Paths no commit may write:** `apps/processor-go/**` (spec §6.3, §7), every migration, the `/ingest` handlers in `ig/main.go`
(`handleIngest`, `applyAuthenticatedScope`, `ingestChain`, `batchChain` stay as they are), `packages/shared-go/**`, `packages/proto/**`,
`gen/**`, `dw/src/lib/components/keys/ApiKeyCreateModal.svelte`, the keys pages, the K8s overlays and `values-prod-example.yaml`.
No overlay carries a non-empty `sentryCompatTrustedProxyCidrs` (D15).

Rules that hold in every commit:
- `tu/` is one flat package (B4). Every new helper is prefixed `sc`, and existing helpers (`newTestIngestService`,
  `fakeEventPublisher`, `newMiniRedisClient`, `readHelmValues`, `helmPath`) are reused and never redeclared.
- No proof calls `t.Skip`, reads an opt-in variable, or needs Postgres, NATS, real Redis or the network (spec §11.1). U40 is the
  only exception, and it is supplementary.
- The key, the DSN and the body are never logged, echoed, or put on a span or metric (spec §3.2).
- No `+page.svelte`/`+page.server.ts` gains a non-allowlisted export (B12).

---

## 2. Before the first commit

1. `git fetch origin`, cut the branch (§1), and merge the specifying branch.
2. **Record the baseline** (the merge gate is "no worse than the baseline", never "all green"), on the merged commit before any
   production change:
   - `GOWORK=off go test ./tests/unit/... -count=1`: pass/fail/skip counts. At planning, on `2812b23`: see §5.2.
   - `go test ./apps/ingestor-go/... -count=1`: the package list and result.
   - `cd dw && pnpm build && pnpm check && pnpm test`: the check's errors/warnings and vitest's file/test counts.
   - `helm lint deploy/helm/sentinel`, plus `helm template` over the four overlays the `lint-helm-k8s` job renders.

   Write the numbers into the first commit body. A failure already present here does not belong to this feature, and the seat does
   not fix it.
3. Copy the two source events from `~/.cache/orch/p4-fixtures/` (spec §10.1). If they are missing, B3 cannot build its fixtures;
   report `BLOCKED_EXTERNAL` naming the files rather than fabricating a "real" event.

---

## 3. Ordered steps and commits

Each commit is green on the §5 gate at its own tip (no worse than the baseline), and its proofs were red first. The card in each
heading is the one the commit serves.

### B1 [be] — TASK-02: Sentry key presentation, ingest-only keys, derived project id, Sentry rate-limit headers
- `ig/auth/sentry.go` (new): `SentryKey` (K1 → K2 → K3, the first present source only), `ParseSentryAuth`, `SentryProjectID`,
  `ScopesSentryCompat = {ingest}`, and `SentryMiddleware` (A1-A6). Spec §3.2-§3.4.
- `ig/auth/apikey.go`: `resolveKey` factored out of `Middleware`, which stays byte-identical. Add `NewAPIKeyAuthenticatorWithLookup`
  with the doc comment of spec §12.3.
- `ig/middleware/ratelimit.go`: the `limit(next, onLimited)` core, `SentryMiddleware`, `RetryAfterSeconds`, and
  `SentryRateLimitsValue` (§3.5).
- `tests/fixtures/sentry/project-id-vectors.json`, byte-identical to §3.4 (skip if F1 already landed it).
- Proofs, in `tu/sentry_compat_auth_test.go`: P2.2, P2.3, P2.6-P2.10 and P6.3. P2.4 and P2.5 need `Mount` and land in B7.

### B2 [be] — TASK-02 §3.6 / TASK-07: two resolvers, `PeerTrusted`, the chart value shipped empty (D15)
- `ig/clientip/clientip.go`: `parseTrustedList(envName, spec)`, with `ParseTrusted` delegating to it and its messages unchanged.
  Add `Resolvers(globalSpec, compatSpec)`, `(*Resolver).PeerTrusted`, and `Info.PeerTrusted` filled by `Middleware` (§3.6.1, §3.7.2).
- The chart: `values.yaml` gets `sentryCompatTrustedProxyCidrs: []` with the §3.6.3 comment. `templates/ingestor.yaml` gets the
  always-rendered `SENTRY_COMPAT_TRUSTED_PROXY_CIDRS` entry. Both AWS overlays get an explicit `[]` with its comment.
- Proofs: the resolver-only half of P2.11 (trusted, trusted plus an internal hop, private XFF, untrusted peer, both error prefixes),
  P2.12, P7.8's `PeerTrusted` half, and P2.13 in `tu/helm_sentry_compat_trusted_proxy_test.go`.
  `TestHelmTrustedProxyCidrsEmptyOnK8s` stays unchanged and green.

### B3 [be] — TASK-03: bounded decoder, envelope reader, item metric, fixtures
- `ig/sentrycompat/decode.go` (the limits, `DecodeBody` with the zlib/raw sniff, `capReader`, error classes; §4.1),
  `envelope.go` (`ReadEnvelope`, the two line helpers; §4.3) and `metrics.go` (`MetricItems`, outcomes, `ItemTypes`, `ItemTypeLabel`,
  `NewItemRecorder`; §4.6).
- `tests/fixtures/sentry/`: the two anonymized events (§10.1), the five envelopes built with the non-Go commands of §10.2, `README.md`
  with the regeneration commands, and the committed deny-list of **SHA-256 hashes** of the original identifiers. The originals are
  never written to the repository.
- Proofs: P3.1-P3.4 in `tu/sentry_compat_envelope_test.go`, and `TestSentryFixturesAreAnonymized` in
  `tu/sentry_compat_fixtures_test.go`. For the anonymization check, the mutation is to put one original identifier back, which turns it red.
  P3.5-P3.8 need the handler and land in B7.

### B4 [be] — TASK-04: the Sentry event → `ErrorPayload` mapper, **with NUL-free tag keys (finding `d14-nul-tag-key`)**
- `ig/sentrycompat/mapper.go`: `MapEvent`, `parseSentryTime`, `str`/`num`/`stringify`/`truncRunes`/`truncBytes`/`firstLine`, and
  rows M1-M23, E1-E4, S1-S3, F1-F8 and Q1-Q5 (§5). Frames are reversed crash-first here, and that reversal is TASK-05's grouping input.
- **Named step `d14-nul-tag-key`** (coordinator ruling; §4 row 1): M15 strips U+0000 from every tag **key**, in the object form
  and in the pairs form, **before** `relay_auth` is deleted. If a NUL-bearing key cleans to a key the body also sent, the exact key wins. If
  several NUL-bearing keys clean to the same key, the first in sorted raw-key order wins. This is the rule `boundTags` already
  applies (`ig/validation/bounds.go:365-374`), so the result never depends on map iteration order. In the pairs form the existing "a later
  pair for the same key wins" rule applies to the cleaned key. Because B8's `Apply` then deletes `tenant_id` from an already-clean
  map, the same step also closes the anonymous `tenant_id\u0000` path.
- `tests/fixtures/sentry/flutter-typeerror-unhandled.payload.golden.json` (P4.1).
- Proofs, in `tu/sentry_compat_mapper_test.go`: P4.1, P4.3, P4.4 and P4.5. P4.3 gains two rows for this step:
  `{"relay_auth\u0000":"user","tenant_id\u0000":"victim-tenant","env\u0000":"a","env":"b"}` maps to tags `{tenant_id: victim-tenant,
  env: b}` with no `relay_auth`, and the pairs form of the same input gives the same result. The mapper-half of P7.9 is also here: the
  object and pairs forms of `relay_auth: user`, plus `relay_auth\u0000: user` in both forms, leave no `relay_auth`. Mutation: delete
  `relay_auth` before stripping NUL, which turns the `relay_auth\u0000` rows red. P4.2 and P4.6 need the handler and land in B7.

### B5 [be] — TASK-05: Dart grouping proofs
- No production code; B4's reversal and F1/F7 are the implementation (§6). The processor is not changed (§6.3).
- Proofs: P5.1-P5.4 in `tu/sentry_compat_grouping_test.go`. The mutation is to skip the reversal in `mapper.go`, which turns P5.2 red.
  The commit body records that the obfuscation status was noted on 2026-09-27 (§6.2).

### B6 [be] — TASK-04 (D13): the compat publish budget
- `ig/service/service.go`: move `IngestAt`'s body unchanged into `ingestAt(ctx, p, receivedAt, budget, shed)`. `IngestAt` passes
  `(0, nil)`. Add `IngestAtWithin`, `Shedder`, `MaxPublishBytes = 1_000_000`, `MaxShedRounds = 4`, `ErrEventTooLarge`, the shed loop
  and the `ingest.event_shed` Warn line (§4.7).
- `ig/validation/bounds.go`: add `MarkServerTruncated` and `IsMetadataExemptKey`. `applyBounds` is unchanged.
- `ig/sentrycompat/shed.go`: `Shed` over B1-B5 with the lower-bound estimates, and `ShedSlack = 16 << 10`.
- Proofs, in `tu/sentry_compat_budget_test.go`: P4.10, and P4.9's `MaxShedRounds` case (a stub `Shedder` that returns `true` without
  removing anything ends in `ErrEventTooLarge` after 4 rounds, called directly on `IngestAtWithin`). The existing service and bounds
  tests stay green unchanged. P4.8 and P4.9's handler cases land in B7.

### B7 [be] — TASK-02/03/04: handlers, routes, chain, `main.go` wiring
- `ig/sentrycompat/handler.go` (`Ingester`, `Config`, `NewEnvelopeHandler`, `NewStoreHandler`, `statusFor`; §4.2, §4.5, including the
  `ErrEventTooLarge`/`nats.ErrMaxPayload` → `413` classification), `routes.go` (the four patterns, `Mount`; §3.1) and `chain.go`
  (`Chain`).
- `ig/main.go`: `clientip.Resolvers(...)` replaces `ParseTrusted` + `NewResolver`, with the same exit on error, the existing
  trusted-ranges log line, and one more for the compat list. It also adds the `compat` `Config`, `NewItemRecorder` (a no-op on error),
  the two chains wrapped in `observeMiddleware` + `otelhttp` with fixed operation names, and `Mount`. `ingestChain` and `batchChain`
  keep `clientResolver`.
- Proofs: P2.1, P2.4, P2.5, the chain half of P2.11, P3.5-P3.8, P4.2, P4.6, and P4.8 plus P4.9's handler cases.
- Before committing, grep the call path from `main()` to `sentrycompat.Mount` (CLAUDE.md B3) and record it in the commit body.

### B8 [be] — TASK-07: relay identity headers (D12, D14)
- `ig/sentrycompat/relay.go`: the header and tag constants, `RelayIdentity`, `RelayIdentityFromRequest` (with the
  `sentrycompat.relay_peer_untrusted` Info line and the `relay_auth_invalid` Warn line), and `Apply` steps 1-4 (§3.7.3-§3.7.4).
  Both handlers resolve the identity once per request and call `Apply` after `MapEvent` and before `Config.Scope`.
- Proofs: P7.1-P7.11 in `tu/sentry_compat_relay_test.go`, with the two `d14-nul-tag-key` extensions (§4 row 1):
  - **P7.2** adds a body tag `tenant_id\u0000: victim-tenant` beside `tenant_id: app-tenant`. On a trusted `anonymous` request the
    published tags have **no** `tenant_id`.
  - **P7.9** adds a body tag `relay_auth\u0000: user`, in the object and the pairs form. From an untrusted peer with no relay headers
    the published tags have **no** `relay_auth`, and from a trusted `anonymous` peer `relay_auth` is `anonymous`.

  Both run through `IngestAt`, so `boundTags` runs. Mutation: move B4's NUL-strip after the delete. P7.2 and P7.9 then go red and
  reproduce the reviewer's `map[relay_auth:user tenant_id:victim-tenant]`.

### B9 [be] — docs, pinned names, U40
- `docs/sdk-specification.md` gets `## 10. Sentry-compatible ingest` (§9). `README.md` gets one bullet.
- `deploy/helm/sentinel/RUNBOOK.md` §4 gets the two entries of §9. The relay entry carries **the sentence the coordinator ruled for
  `d14-off-while-list-empty`** (§4 row 2), in substance: *"Order of the rollout: `core-sentinel-mobile-relay` sets this list
  (step 4) and passes the check (step 5) **before** it enables core's anonymous relay route. While the list is empty every relayed
  request is untrusted, so D14's anonymous stripping does not apply yet."* It names no CIDR.
- Proofs: `TestSentryCompatDocumented` (as §11.2, plus one assertion that the relay entry contains `anonymous` and `before`, so the
  ruled sentence cannot be deleted silently), and `TestSentryCompatNamesPinned`. Supplementary:
  `tests/e2e/sentry_compat_test.go: TestU40_SentryCompatEnvelopeDedupsAndGroups` (P4.7).

### F1 [fe] — TASK-06: the Sentry project id on project settings
- `dw/src/lib/sentry-project-id.ts` (new): `sentryProjectId` (§8.2).
- `dw/src/routes/[orgSlug]/projects/[projectId]/settings/+page.svelte`: the `<section class="sentry-sdk" id="sentry-sdk">` of §8.1,
  directly before "Agent automation" (`+page.svelte:479`). It adds a **copy button** (contract §4.5; §6 A1) that reuses the keys page's
  `navigator.clipboard.writeText` + toast-on-failure pattern (`settings/keys/+page.svelte:33-41`). The load function is unchanged.
- `tests/fixtures/sentry/project-id-vectors.json`, byte-identical to §3.4 (skip if B1 landed it).
- Proofs (vitest): P6.1 `dw/src/lib/sentry-project-id.test.ts`, and P6.2
  `dw/src/routes/[orgSlug]/projects/[projectId]/settings/page.sentry-project-id.test.ts` (the `page.environments.test.ts` harness).
  P6.2 also asserts the copy button calls `navigator.clipboard.writeText('27690631804612')`, with `navigator.clipboard` stubbed. The Go
  half is P2.6/P6.3 in B1.

---

## 4. Open review findings → steps and proofs

| # | Finding (sev.) | Ruling | Step | Proof that closes it |
|---|---|---|---|---|
| 1 | `d14-nul-tag-key` (minor) | The mapper strips U+0000 from object-form **and** pair-form tag keys **before** the D14 deletes. | B4 (strip), B8 (the anonymous `tenant_id` delete then sees clean keys) | B4: P4.3's two rows and P7.9's mapper half. B8: P7.2 with `tenant_id\u0000`, P7.9 with `relay_auth\u0000`, both through `IngestAt`/`boundTags`. The mutation (strip after the delete) reproduces the finding's `map[relay_auth:user tenant_id:victim-tenant]`. |
| 2 | `d14-off-while-list-empty` (minor) | Accepted as inherent. The core relay's rollout sets the trusted range before it enables the anonymous route (recorded on `core-sentinel-mobile-relay/TASK-03`). One RUNBOOK sentence, no code change. | B9 | The sentence is in the relay RUNBOOK entry, and `TestSentryCompatDocumented` asserts it is present. |
| 3 | `header-prd806-pending` (note) | Re-pin the spec header basis to prd `c14e9b9` in the same commit as this plan. | **Done in this commit** (`spec.md` header, revision 6) | `git show` of this commit: the header names `c14e9b9` and no longer carries the "to be merged and re-pinned" caveat. |
| 4 | `be-brief-per-environment-drift` (note) | No action here. The contract (§4.7, D15) wins, and the coordinator fixes `be.md`. | — | B2's P2.13 already asserts the list is empty in every overlay, per the contract. |
| 5 | `f5-401-final-vs-relay` (note) | F5 says 401 is retryable for the relay, since compat 401s can be transient. | **Done in this commit** (`spec.md` §13 F5) | `git show` of this commit: F5 lists `401` as retryable (idempotent by `event_id`, T14), and `400`/`403`/`413` as final. |

---

## 5. Gates

### 5.1 The commands (the seat runs every one on the final pushed commit, in this order, and pastes the output)

```bash
GOWORK=off go build ./... && GOWORK=off go vet ./...                      # root module (CI go-root runs these on PRs too)
GOWORK=off go test ./tests/unit/... -count=1                              # every load-bearing Go proof
GOWORK=off go test ./tests/unit/... -count=1 -run 'SentryCompat|SentryProjectID|ParseSentryAuth|SentryEnvelope|SentryDecode|SentryStore|SentryDart|SentryFixtures|RelayIdentity|RelayAuth|RelayUntrusted|RelayHeaders|IngestAuthUnchanged|IngestRateLimitHasNoSentryHeader|IngestResolverIgnoresCompatList|IngestHasNoPublishBudget|HelmSentryCompat|HelmTrustedProxy' -v   # the named packages' proofs, visibly run, 0 skipped
go test ./apps/ingestor-go/... -count=1                                   # existing package tests (not in CI, T16)
GOWORK=off go test ./tests/unit/... -count=1 -run 'TestHelm' -v           # the chart proofs (YAML parsed in Go)
helm lint deploy/helm/sentinel && for f in values-k8s-production.yml values-k8s-staging.yml values.aws.production.yml values.aws.staging.yml; do helm template sentinel ./deploy/helm/sentinel --values ./deploy/helm/sentinel/$f | grep -A1 SENTRY_COMPAT_TRUSTED_PROXY_CIDRS; done   # renders, value "" everywhere
cd apps/dashboard-web && pnpm vitest run src/lib/sentry-project-id.test.ts 'src/routes/[orgSlug]/projects/[projectId]/settings/page.sentry-project-id.test.ts'   # TASK-06
cd apps/dashboard-web && pnpm build && pnpm check && pnpm test && pnpm test --sequence.shuffle            # all three (B12), shuffled (B13)
task test-unit                                                            # the plane's unit gate: Go and vitest suites above
SENTINEL_E2E=1 go test -tags=e2e ./tests/e2e/ -run 'TestU40' -count=1     # supplementary; needs the compose stack (--build --force-recreate)
```

"Green" means **no worse than the §2 baseline**: every new proof passes with 0 skips, and no test that passed at the baseline
fails. The `-run ... -v` line exists so a mis-named or silently skipped proof is visible, not just a package `ok`.

### 5.2 Baseline at planning (`2812b23`, this seat, 2026-09-29)

- `GOWORK=off go test ./tests/unit/... -count=1 -json`: **1,311 pass, 0 fail, 0 skip** (tests and subtests counted from the `-json`
  stream).
- `go test ./apps/ingestor-go/... -count=1`: `ok` for the root, `auth`, `mapping`, `middleware`, `service` and `validation` packages. `clientip`,
  `enrich`, `geoip` and `geoip/refresh` have no test files.
- The dashboard and helm baselines were not run at planning. The seat records them in §2 step 2.

### 5.3 CI does not prove this feature on the PR

CI's `go-root` job runs `go build` and `go vet` on every event, but runs `go test ./tests/unit/...` **only when the event is not
`pull_request`** (`.github/workflows/ci.yml:103-105`). The `dashboard` job's `pnpm check`/`pnpm test` are likewise off on PRs, and
`integration`/`contract`/`e2e` are dispatch-only (`ci.yml:655-658`). `lint-helm-k8s` does run on PRs. A green PR check list is
therefore evidence of a build, not of the feature, and **the plane's gates (§5.1) are the proof**. The seat also dispatches
`gh workflow run ci.yml --ref <branch>` once and records the run URL (spec §11.1), which runs the unit step, `pnpm test`, and U40.

### 5.4 Criterion → commit

| Card | Criterion (contract §5, as amended by D13-D15) | Proofs | Commit |
|---|---|---|---|
| TASK-02 | DSN-built `ingest` key authenticates on both routes, header and query | P2.1-P2.4 | B1, B7 |
| TASK-02 | mismatched path project → 403; org/admin/agent/report keys → 403 | P2.5, P2.6 | B1, B7 |
| TASK-02 | limited request carries `Retry-After` and `X-Sentry-Rate-Limits` | P2.7-P2.9 | B1 |
| TASK-03 | Flutter 9.6 / Next.js 10 fixtures parse, gzip and deflate included; unknown items skipped; bomb and caps → 413 | P3.1-P3.8 | B3, B7 |
| TASK-04 | real Flutter event round-trips (golden); duplicate delivery is one occurrence | P4.1-P4.7 | B4, B7, B9 (U40) |
| TASK-04 (D13) | over-budget event shed in order, else 413, never 500; `/ingest` unchanged | P4.8-P4.10 | B6, B7 |
| TASK-05 | one issue across two devices, two issues for two errors | P5.1-P5.4 | B5 |
| TASK-06 | settings shows the derived id (with copy); TS and Go agree on the vector | P6.1-P6.3, P2.6 | F1, B1 |
| TASK-07 | trusted XFF believed, untrusted not; relay headers from trusted peers only; body cannot claim relay identity (D14, incl. NUL keys) | P2.11, P2.12, P7.1-P7.11 | B2, B7, B8 |
| TASK-07 (D15) | chart value empty everywhere; RUNBOOK says how to derive it, and in what order | P2.13, `TestSentryCompatDocumented` | B2, B9 |

---

## 6. Where this plan adds to the spec (reported upward)

- **A1: a copy button on the Sentry project id (F1).** Contract §4.5 and `fe.md` say "with a copy button", and spec §8.1 omits it.
  The contract wins on this point, and nothing in the spec forbids it. P6.2 gains one assertion.
- **A2: tag-key NUL stripping before the D14 deletes (B4, B8)**, the coordinator's ruling on `d14-nul-tag-key`. The spec's D-c names
  `str`/`stringify`/free-form keys, and M15's object keys are none of these. The tie rule for colliding cleaned keys is the one
  `boundTags` already uses.
- **A3: one RUNBOOK sentence on rollout order (B9)**, the coordinator's ruling on `d14-off-while-list-empty`.
- **A4: two spec edits in this commit**: the header re-pin (finding 3) and §13 F5 (finding 5). Nothing else in the spec changed.
  §3.3's "retrying cannot fix either" still describes a *misconfigured* caller's steady state. The F5 edit is about a transient
  401 (the key cache or a rotation), which a relay may retry safely by T14. The two are compatible, and no code depends on either.

## 7. After the merge (not build items)

- **Staging (§10.3)** is the orchestrator's release step, run from inside the VPN after the release carrying this change is on AWS
  staging. No build commit waits on it.
- **Per-environment `sentryCompatTrustedProxyCidrs`** belongs to `core-sentinel-mobile-relay`'s rollout (D15), done by the RUNBOOK
  entry, in the order B9's sentence states.
- `mobile-sentinel-dual-write` depends on the relay as well as on this run (spec §12.3).

## 8. Reporting

The seat reports COMPLETED with the PR URL, the final commit, the §5.1 output (including the `-run ... -v` line's pass/skip counts
against the §5.2 baseline) and the `ci.yml` dispatch URL. Each commit body names its card(s), the proofs it adds, and the mutation it
ran red. `docs/memory/VERIFIED_STATE.md` gains one entry after the gate, naming what runs and the command that proved it
(CLAUDE.md).
