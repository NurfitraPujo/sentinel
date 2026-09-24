# Sentinel event enrichment: implementation plan

**Feature:** `sentinel-event-enrichment` · **Repository:** `sentinel` · **Base:** `main` @ `1e5c6e2`
**Built from:** `sentinel-event-enrichment/spec.md` **revision 5** (`3fe6dc1` on `NurfitraPujo/sentinel-event-enrichment-planning`),
which repairs the fourth spec review (`enr-rev4-*`, §13.0). Contract `prd/sentinel-event-enrichment/contract.md` @ `774039e`
(decision 11 binding), `be.md` @ `e07441f`. If this plan and the spec disagree, **the spec wins**, and the seat reports the conflict.
It does not resolve it silently.
**Cards:** `sentinel-event-enrichment/TASK-01`, `TASK-03`, `TASK-04`, `TASK-05`, `TASK-06`: **one seat, one pull request**.
`TASK-07` (the MaxMind account and sealing the key) belongs to the operator. Nothing below depends on it to build or test: the
refresh job's tests use a counting fake MaxMind, and no key is read at build or test time (spec §8.2, contract decision 6).

---

## 1. Seats

There is **one** implementing seat. The work cannot be split without two seats sharing paths:
- `apps/ingestor-go/main.go`, `service/service.go`, `go.mod` / `go.sum` and the `enrich` package are written by TASK-01, 03, 04 and 05
  alike (spec §9).
- `tests/integration/enrichment_test.go` and `tests/integration/testcontainers/ingestor.go` serve TASK-01, 03, 04 and 06.
- The Go replay of the tunnel fixtures (`tests/unit/sdk_js_tunnel_fixture_test.go`) needs TASK-01's resolver, TASK-03's geo and
  TASK-04's UA parser, **and** TASK-06's committed fixtures.

So the cards run as a **sequence of commits inside one seat** (§3).

| | |
|---|---|
| **Seat** | `sentinel-event-enrichment-implementing` |
| **Repository** | `sentinel` (`tuju-ai/sentinel`) |
| **Branch** | `NurfitraPujo/sentinel-event-enrichment-implementing`, cut from `main` @ `1e5c6e2`. It also merges or cherry-picks `sentinel-event-enrichment/spec.md` and this `plan.md`, so the PR carries the spec it implements. |
| **Cards** | TASK-01, TASK-03, TASK-04, TASK-05, TASK-06 |
| **Depends on** | nothing inside this run. Spec revision 5 must pass its review (plane gate) before the seat starts. TASK-07 is **not** a dependency. |
| **Pull request** | one, against `main`, titled `feat: server-side event enrichment — client IP, geo, User-Agent (sentinel-event-enrichment)` |
| **Gate** | §4, run in that order, with the results cited in the report |

### 1.1 Paths the seat may write (the whole change's surface)

```
apps/ingestor-go/clientip/**                         (new)   TASK-01
apps/ingestor-go/enrich/**                           (new)   TASK-01, 03, 04
apps/ingestor-go/geoip/**                            (new)   TASK-03, 05   (includes geoip/refresh/**)
apps/ingestor-go/service/service.go                          TASK-01 (Option / WithEnricher / enrich.Apply call)
apps/ingestor-go/validation/bounds.go                        TASK-04 (exported ContextFits only)
apps/ingestor-go/main.go                                     TASK-01, 03, 04, 05
go.mod, go.sum, go.work.sum                                  TASK-03, 04, 05
packages/sdk-js/src/nextjs/route.ts                          TASK-06
packages/sdk-js/README.md, packages/sdk-js/CHANGELOG.md      TASK-06
packages/sdk-js/tests/route/route.test.ts                    TASK-06
packages/sdk-js/tests/route/bucket.test.ts                   TASK-06
packages/sdk-js/tests/tunnel-fixtures.test.ts        (new)   TASK-06
packages/sdk-js/tests/tunnel-fixtures/**             (new)   TASK-06
deploy/helm/sentinel/templates/geoip-refresh-cronjob.yaml (new)  TASK-05
deploy/helm/sentinel/templates/ingestor.yaml                 TASK-01, 05 (ingestor CONTAINER env only)
deploy/helm/sentinel/values.yaml                             TASK-01, 05
deploy/helm/sentinel/values-huawei-staging.yml               TASK-01, 05
deploy/helm/sentinel/values-huawei-production.yml            TASK-01, 05
deploy/helm/sentinel/RUNBOOK.md                              TASK-05
tests/fixtures/geoip/**                              (new)   TASK-03
Taskfile.yml                                                 TASK-06 gate (test-unit only)
.github/workflows/ci.yml                                     TASK-06 gate (sdk-js-node job only)
tests/unit/pnpm_pin_test.go                          (new)   TASK-06 gate
tests/unit/ingestor_clientip_test.go                 (new)   TASK-01
tests/unit/ingestor_enrich_ip_test.go                (new)   TASK-01
tests/unit/ingestor_geoip_test.go                    (new)   TASK-03
tests/unit/ingestor_geoip_s3_test.go                 (new)   TASK-03 / TASK-05 criterion 3
tests/unit/ingestor_useragent_test.go                (new)   TASK-04
tests/unit/ingestor_enrich_order_test.go             (new)   TASK-04 (compatibility rows)
tests/unit/fingerprint_golden_test.go                        TASK-04 (one added case; existing cases untouched)
tests/unit/ingestor_geoip_refresh_test.go            (new)   TASK-05
tests/unit/ingestor_geoip_imports_test.go            (new)   TASK-05
tests/unit/helm_geoip_test.go                        (new)   TASK-01, 05
tests/unit/sdk_js_tunnel_fixture_test.go             (new)   TASK-06
tests/integration/enrichment_test.go                 (new)   TASK-01, 03, 04, 06
tests/integration/testcontainers/ingestor.go                 TASK-01 (new StartIngestorWithEnv; StartIngestor unchanged)
docs/sdk-specification.md                                    TASK-06 commit (subsection for all cards, spec §9)
sentinel-event-enrichment/{spec.md,plan.md}                  carried from the planning branch, NOT edited
```

Package-internal `_test.go` files are allowed **only inside the three new packages** (`clientip`, `enrich`, `geoip`). Every proof the
spec names lives in `tests/unit` or `tests/integration`, because that is what reviewers read (spec §10).

### 1.2 Paths the seat must not write

```
apps/processor-go/**           (the parity test IMPORTS symbolicate.UsePathStyle; it never edits it)
apps/dashboard-web/**
packages/proto/**, gen/**, packages/db-migrations/**, packages/sdk-go/**
packages/shared-go/**
deploy/helm/sentinel/templates/ingestor-ingress.yaml, templates/ingress.yaml, templates/secret.yaml
the Service document in templates/ingestor.yaml, and any other Service / Ingress / Endpoints template   (G8 enforces this)
deploy/helm/sentinel/.kube-linter.yaml                         (no waiver, spec §8.4)
deploy/helm/sentinel/argocd-app/**
.github/workflows/* other than ci.yml; in ci.yml, every job except sdk-js-node
Taskfile.yml tasks other than test-unit, and test-unit's two dashboard lines
sentinel-event-enrichment/spec.md                              (a spec defect is reported, not patched)
```

**Declared risk surface.** This change touches a trust boundary (the caller-IP rule), CI, a Helm chart that carries sealed-secret
wiring, and a published package's behaviour (sdk-js tunnel). That is the **full tier**, matching contract decision 7 (`credentials`).

---

## 2. Machine and environment rules (binding on the seat)

1. **Private Go cache.** A cleanup timer deletes the shared cache. Before any Go command:
   `export GOCACHE=/home/fitrapujo/.cache/go-build-sentinel-event-enrichment` (fish: `set -gx GOCACHE …`).
2. **Containers: once, alone, at the end.** This laptop has kernel-panicked several times while podman starts many containers.
   `task test-integration` runs **exactly once**, after `task build` and `task test-unit` pass, and nothing else that starts containers
   runs at the same time: no compose stack, no e2e, no second terminal's suite. No per-commit integration run, and no integration
   mutation run (§3.7 explains how the G3 rows' mutations are covered).
3. `docker` here is podman's CLI shim. Use the `task test-integration` invocation this machine already uses. Do not change
   `TESTCONTAINERS_PROVIDER` or the Taskfile to suit it.
4. **Prefix commands with `rtk`** (CLAUDE.md), including inside `&&` chains.
5. `go.work` puts three modules in workspace mode. CI's `go-root` runs with **`GOWORK=off`**, so each Go check below also runs once
   with `GOWORK=off`, because the new dependencies must resolve from the root `go.mod` alone.
6. The ingestor image builds with `GOPROXY=https://goproxy.cn,direct` (`apps/ingestor-go/Dockerfile:10`). The three new modules must
   be fetchable through it, and `task test-integration` finds out at its `docker build` step. If they are not, report
   `BLOCKED_EXTERNAL`. Do not vendor and do not change the Dockerfile.
7. Network is needed once, at implementation time, to fetch the two MaxMind test databases from `github.com/maxmind/MaxMind-DB` at
   commit `0eef25a46e20f4e96d27b951d0228efabe21323f` (spec §5.6). Verify each file's `DatabaseType`, `BuildEpoch` and size against the
   spec's table before committing it. If they differ, stop and report `NEEDS_DECISION`: every lookup and staleness assertion is built on
   those values. Nothing is downloaded at test time.

---

## 3. Ordered steps and commit boundaries

Six commits, one per card, except TASK-06, which has two: its **gate** commit goes first, so every later commit runs under the
widened `test-unit` (contract decision 9). Every commit follows the repository's must-fail rule (CLAUDE.md working conventions):

- **Red first.** Write the step's tests, then run them and see them fail **on behaviour**, not only on a missing symbol. For a new
  package, add the exported stubs (signatures only, zero-value returns) first, so the tests compile and fail on their assertions.
- **Green.** Write the production code until the tests pass.
- **Mutation.** Apply each listed mutation (each comes from spec §10.2's "Mutation" column), watch the named test go red, then revert.
  Record `mutation → test → red` in the commit body. A test that stays green under its mutation is decoration: fix the test before
  committing.
- **Per-commit check** (no containers):

  ```bash
  export GOCACHE=/home/fitrapujo/.cache/go-build-sentinel-event-enrichment
  rtk go build ./... && rtk go vet ./...                      # vets tests/unit AND compiles tests/integration (untagged)
  GOWORK=off rtk go build ./... && GOWORK=off rtk go vet ./...
  rtk go test ./tests/unit/... -count=1 && rtk go test ./apps/ingestor-go/... -count=1
  ```

  `tests/unit` is one flat package, so one broken file silences all of it (B4). `go vet ./...` must be clean before a green run is
  trusted.
- Commit messages name the card: `feat(ingestor): … (sentinel-event-enrichment/TASK-01)`.

### 3.1 C1: the widened unit gate (TASK-06, gate part; contract decision 9)

| | |
|---|---|
| **Files** | `Taskfile.yml` (`test-unit` only), `.github/workflows/ci.yml` (`sdk-js-node` only), `tests/unit/pnpm_pin_test.go` (new) |
| **Red** | `pnpm_pin_test.go` (spec §10.1). It parses `Taskfile.yml` and `ci.yml` as YAML, with no exec. It asserts that `test-unit` has **at least one** `pnpm@` and that every one names an exact `MAJOR.MINOR.PATCH`, that `sdk-js-node`'s `pnpm/action-setup` `version` equals that string, and that neither side is a bare major or a range. Against `main` it is red: `test-unit` has no `pnpm@`, and CI's is `version: 10`. |
| **Green** | Replace `test-unit` with spec §10.1's block, keeping the comment convention and adding a comment that names contract decision 9. In `ci.yml`'s `sdk-js-node` job: `version: 10.34.5`, the "bump both together" comment, and after `pnpm test`, under the same push-only `if:`, the two drift checks with pathspec **`tests/tunnel-fixtures`**, plus the comment on why that pathspec (spec §4.5). |
| **Mutations** | Set the Taskfile's pin back to `pnpm@10`: red. Bump `ci.yml` alone to `10.34.6`: red. |
| **Criteria** | None directly. This makes G2 the gate for the ingestor packages and the sdk-js suite. |
| **Extra check** | `npx -y pnpm@10.34.5 --dir packages/sdk-js install --frozen-lockfile && npx -y pnpm@10.34.5 --dir packages/sdk-js test`: 26 files / 459 tests, lockfile untouched (`git status --porcelain` empty). This is the measured baseline in spec §10.1. |

### 3.2 C2: client IP and the trusted-proxy rule (TASK-01)

| | |
|---|---|
| **Files** | `apps/ingestor-go/clientip/` (new): `ParseTrusted` (IPv4-mapped entries are errors, C12/C16/C17 messages exactly as §3.3 states), `IsPublic` (§3.5: `IsGlobalUnicast() && !IsPrivate()` minus the N6-N11 prefix list), `Resolver.Resolve` (the marker check first, §3.8; the walk, §3.4; then `IsPublic`), `Info`, and `WithRequestInfo` / `RequestInfoFromContext` on a **private key type**, plus `Middleware`. `apps/ingestor-go/enrich/` (new): the client-platform predicate (§3.1), the `{{auto}}` rule **I1-I6 plus I3t** (§3.2, §3.9), and the `Apply` skeleton with the geo and UA hooks nil. `service/service.go`: `Option`, `WithEnricher`, `NewIngestService(publisher, opts ...Option)`, and `enrich.Apply` between the `ReceivedAt` stamp and `ValidatePayload` (§7.1). `main.go`: parse `TRUSTED_PROXY_CIDRS` before listening (a bad entry logs the §3.3 text and exits 1), the Info line `trusted proxy ranges`, and `clientip.Middleware(resolver)` around both ingest chains, **inside** `observeMiddleware` (§7.2). Chart: `values.yaml` `config.ingestor.trustedProxyCidrs: []`; `templates/ingestor.yaml` gets `TRUSTED_PROXY_CIDRS` (`join ","`) on the ingestor **container**; both overlays get `trustedProxyCidrs: []` with §8.4's comment. `tests/integration/testcontainers/ingestor.go`: `StartIngestorWithEnv(…, extraEnv map[string]string)`, with `StartIngestor` unchanged and delegating. |
| **Red** | `tests/unit/ingestor_clientip_test.go`: C1-C19, M1-M5, H1-H3 (the Huawei list **read from `values-huawei-staging.yml`** with the T26 parser, and asserted empty and equal to production's), and the full `IsPublic` table of §3.7. `tests/unit/ingestor_enrich_ip_test.go`: I1-I6 and I3t × {`javascript`+`browser`, `javascript` with no runtime, `javascript`+`node`, `javascript`+`edge`, `go`, `cocoa`, `dart`}. `tests/unit/helm_geoip_test.go` (first part): both overlays **declare** `config.ingestor.trustedProxyCidrs`, and it is empty. `tests/integration/enrichment_test.go` (first part, compiled but not run): (i) unset list, `{{auto}}` plus `X-Forwarded-For: 6.6.6.6` → stored empty; (ii) `TRUSTED_PROXY_CIDRS=127.0.0.0/8,::1/128`, `X-Forwarded-For: 6.6.6.6, 198.51.100.23` → `198.51.100.23`. |
| **Mutations** (spec §10.2) | Believe XFF always: C1, C2, H1 red. Walk left-to-right: C4 red. Drop `IsPublic` from `Resolve`: C13-C15 and H1-H3 red. Drop N7 while narrowing N6: the `100.125.64.17` case red. Drop the N8-N11 prefixes: their predicate cases red. Accept an IPv4-mapped entry: C16/C17 red. Ignore the marker: M2-M4 red. Drop the runtime split: the `javascript`+`node` row red. Store `{{auto}}` through: I2 red. Apply `IsPublic` to body values: the `10.1.2.3` case red. **Drop I3t: the `javascript`+`browser` absent-IP row red. Widen I3t to all `javascript`: the no-runtime row red. Apply I3t to I1: the `{{auto}}` row red.** Add `10.200.32.0/19` to an overlay: the emptiness assertion and H1 red. |
| **Criteria** | TASK-01 criterion 2 (unit half; the integration half at G3), TASK-01's `{{auto}}` / client-platform rules, TASK-01 wired to `main()` (integration (ii), at G3). |

### 3.3 C3: GeoLite2 lookup and the reloadable reader (TASK-03)

| | |
|---|---|
| **Files** | `go.mod` / `go.sum`: `github.com/oschwald/maxminddb-golang/v2`. `apps/ingestor-go/geoip/` (new): `Config` (the `GEOIP_*` env of §8.3, including `MaxAge` and an injectable `Now`), the S3 client (minio-go, `Region` from config, and the host-shape rule **duplicated** from `symbolicate.UsePathStyle`, §8.3), an exported test-only `Transport` field, the `ObjectSource` seam (`Stat`, `Get`), and `Loader` (`atomic.Pointer`, poll, the 256 MiB cap, `Verify`, the `-City` suffix rule, staleness on every tick, no `Close` on swap). The loader writes one log line per state change (§5.5, including `geoip.reloaded`) through an injected logger. `Lookup(netip.Addr) (validation.GeoInfo, bool)` satisfies `enrich.GeoLookup`. `enrich`: the geo fill (§5.2 mapping, §5.3 rules; `IsPublic` reused). `main.go`: build the loader from env and `Start(ctx)` it; it never blocks readiness. `tests/fixtures/geoip/`: both `.mmdb` files, `README.md`, `LICENSE-APACHE`, `LICENSE-MIT` (§5.6, §2 rule 7). |
| **Red** | `tests/unit/ingestor_geoip_test.go`: missing ×5, corrupt ×5 (the fixture truncated in-test), stale (`Now = BuildEpoch+721h`) against `+719h` ready, wrong type, and recovery, each giving **exactly one** line, with `IngestAt` succeeding and geo null after each. The lookup: `81.2.69.142` gives `GB` / `London` / `England` / `United Kingdom`, under an injected `Now`. Private, loopback, link-local and unspecified addresses are skipped. A sent `geo` (and `{}`) is untouched. An unparseable IP gets no geo. **Reload:** ETag `"a"` then `"b"` gives `DatabaseType == "GeoIP2-City"` and exactly one `geoip.reloaded` line with `etag` `"b"`. `tests/unit/ingestor_geoip_s3_test.go`: path-style against virtual-host addressing, the SigV4 `ap-southeast-1` scope, and the parity table against `symbolicate.UsePathStyle`. `enrichment_test.go` (second part, compiled): a fake S3 `httptest` server on `127.0.0.1` serving the fixture, and `GEOIP_MAX_AGE=876000h` through `StartIngestorWithEnv`. `81.2.69.142` is stored with that geo. A second container whose object 404s stores null geo, and its `/health` is 200. |
| **Mutations** | Log on every poll: count ≠ 1, red. Return a lookup error into `IngestAt`: red. Map `region` to the subdivision: red. Skip the `geo == nil` check: red. Skip the swap on an ETag change: the reload case red. Hard-code `us-east-1`: the region case red. |
| **Criteria** | TASK-03 "missing or corrupt mmdb logs once and ingestion continues", TASK-03 lookup rules, TASK-03 wired to `main()` (at G3), and TASK-05 "OBS → AWS S3 is values/secret only" (the S3 client test lives here because the client does). |

### 3.4 C4: User-Agent → contexts (TASK-04)

| | |
|---|---|
| **Files** | `go.mod` / `go.sum`: `github.com/ua-parser/uap-go`, pinned by pseudo-version. `validation/bounds.go`: exported `ContextFits`, which reuses `objectSize` / `entrySize`, with no constant or table change. `enrich`: UA source selection (§6.2: the body's `request.headers` first, case-insensitive and lexically first; else the connection UA for client-platform events only, tunnel-only items included), the 1024-byte cap, the fill (§6.3: `os` or `client_os`, no `type`, Relay's version and frozen-OS rules, `Other` fills nothing), and the fit rule. `main.go`: `uaparser.New(uaparser.WithCacheSize(1024))` once, passed through `service.WithEnricher`. |
| **Red** | `tests/unit/ingestor_useragent_test.go` (spec §10.2 TASK-04 source and fill row). `tests/unit/ingestor_enrich_order_test.go`: JSON equality with every field sent, **zero S3 calls** from 100 `IngestAt` calls over a counting `ObjectSource` (a 5 s deadline as a hang guard only), and enrich-before-bounds with a stub `GeoLookup` returning a 200-rune city → 128 runes plus `server_truncated`. `tests/unit/fingerprint_golden_test.go`: **one added case**, every golden fixture through `enrich.Apply` with `81.2.69.142`, a Chrome UA and the fixture DB under an injected `Now`. It asserts that geo **was** filled and that the fingerprint is unchanged. The existing cases are untouched. |
| **Mutations** | Write `os` for server events: red. Remove the fit rule: the eviction case red. Run `enrich.Apply` after `ValidatePayload`: the truncation case red. Fetch from S3 on a lookup miss: the zero-calls case red. Add `contexts` to the fingerprint input (a scratch edit, reverted): the golden case red. |
| **Criteria** | TASK-04 source and fill rules; the compatibility rows (sent fields byte for byte, no network I/O, fingerprints unchanged). TASK-04 criterion 1 (a tunnelled browser event gets browser and OS) completes at C6, because its proof replays `tunnel-clean.json`. |

### 3.5 C5: the refresh subcommand and the chart (TASK-05)

| | |
|---|---|
| **Files** | `go.mod` / `go.sum`: `github.com/maxmind/geoipupdate/v8`. `apps/ingestor-go/geoip/refresh/` (new): §8.2 steps 1-6, including the case-insensitive `maxmind-md5` metadata lookup, the MD5/`Verify`/type validation before any `PutObject`, and the 10-minute context. The key is never logged. `main.go`: `geoip-refresh` dispatch **before** the DB and NATS connections (§8.1). Chart: `templates/geoip-refresh-cronjob.yaml` (new, shaped like `retention-cronjob.yaml`, §8.4), and `templates/ingestor.yaml` `GEOIP_*` container env when `geoip.enabled`. `values.yaml`: the `geoip` block (with `refreshCron.*SecretKey`) and ingestor `requests.memory: 256Mi`. Both overlays: `geoip.enabled`, `refreshCron.enabled`, pinned `storage.{endpoint,bucket,region,prefix}`, `256Mi`, and the `secrets.sealedData` comment naming TASK-07's two keys, **with no placeholder ciphertext**. `RUNBOOK.md`: §1 key rows, §2 "GeoIP refresh", and the §4 troubleshooting rows (§8.4). |
| **Red** | `tests/unit/ingestor_geoip_refresh_test.go`: (b) through (f) of spec §10.2 TASK-05's zero-downloads row. Case (b) serves `X-Amz-Meta-Maxmind-Md5` through a **real minio-go client** against an `httptest` fake. `tests/unit/ingestor_geoip_imports_test.go`: `go list -deps` over `geoip`, `enrich`, `clientip` and `service` contains nothing under `github.com/maxmind/geoipupdate` and not `geoip/refresh`, and `geoip/refresh`'s only importer is the ingestor main package. `tests/unit/helm_geoip_test.go` (second part): overlay pins, no `MAXMIND_*` in `secrets.data` or outside `secrets.sealedData`, no plaintext licence key, `256Mi`, exactly two day-of-week values, `backoffLimit: 0`, and `Forbid` in the template text. |
| **Mutations** | Construct the MaxMind client in the loader (import `geoipupdate` from `geoip`): the imports test red. Drop the md5 pass-through, or look up the key case-sensitively: (b) red. Put the licence key in `secrets.data`: red. Put a range in `trustedProxyCidrs`: red. **G8** (§4): add a scratch Service template: exit 1. Revert every mutation before committing. |
| **Chart checks** | G4-G8 (§4) pass on this commit. |
| **Criteria** | TASK-05 "helm template renders the refresh CronJob and geo wiring; the licence key only through the sealed secret" (G5, G6, G7, G2), and TASK-05 "pod starts and rollouts cause zero MaxMind downloads" (G2, G6; P3 post-deploy). TASK-05 "OBS → AWS" is proved at C3. The chart side of TASK-01 (the empty list, and no Service change through G8) holds here too. |

### 3.6 C6: the tunnel (TASK-06) and the cross-language proofs

| | |
|---|---|
| **Files** | `packages/sdk-js/src/nextjs/route.ts`: `trustedProxyHops`, `clientAddress` (§4.2, including the canonical IPv4/IPv6 rules and unmapping; step 3 yields `null` under the App Router), the written IP (§4.2: `clientKey` first), `applyClientIp` (**exported**, the `USER_KEYS` rebuild, B1-B5, §4.4), step 8's headers (`user-agent` when 1-1024 bytes with no control characters, and `x-sentinel-derive-ip: off` always; never `x-forwarded-for`), `defaultClientKey` on the same derivation, and the top-of-file comment (§4.4). `README.md` and `CHANGELOG.md`: §4.3's text **word for word or stronger**, including what `0` does under the App Router (revision 5). `tests/route/route.test.ts`, `tests/route/bucket.test.ts` (T14 and its header comment), `tests/tunnel-fixtures.test.ts` (new; it never writes under `CI`), and `tests/tunnel-fixtures/*.json` (five route fixtures and `tunnel-user-rebuild.json`, **generated** by the suite, never hand-written). `tests/unit/sdk_js_tunnel_fixture_test.go` (new). `tests/integration/enrichment_test.go` (third part, compiled). `docs/sdk-specification.md`: the "Server-side enrichment" subsection for all cards (§9), including the marker, I3t and its residual. |
| **Red** | The sdk-js suite first: §4.5's derivation cases (including both hops-0 shapes: a plain `Request` gives no `user`, and a socket request gives `203.0.113.9`), the helper cases with whole-item deep equality, the through-the-route overwrite and `USER` cases, `clientKey`, the header set, and the rate limit (T14 updated, plus 61 requests sharing a rightmost hop → 429). Run with `npx -y pnpm@10.34.5 --dir packages/sdk-js test`: red on `main`'s route. Then green. Then the fixture suite writes the six files locally and they are **committed**. Then the Go side: `sdk_js_tunnel_fixture_test.go`. It runs the Huawei replay, the **public-peer replay in its four variants** (§4.5: marker plus runtime → none; runtime only → none, **I3t alone**; marker only → none, **the marker alone**; neither → `198.51.100.200`), the fixture-shape assertions, and the `encoding/json` decode of `tunnel-user-rebuild.json` through `enrich.Apply` with the fixture DB under an injected `Now`. `enrichment_test.go` (compiled) posts every route fixture's `forwarded` request and every rebuild output (§4.5 G3 bullet). |
| **Mutations** (spec §10.2 TASK-06 and TASK-01 criterion 1 rows) | Key on the leftmost hop: the derivation case red. Keep a browser-sent IP (B2): the helper case red. Overwrite only the exact `ip_address` key: the `IP_ADDRESS` / `Ip_Address` / `ip_addreſſ` cases red. Keep `geo`: `browser-geo` red. Rebuild to `{ip_address}` only: the `id: "u1"` case red. Case-insensitive allow-list: the `ID` / `Username` case red. Leave the browser's IP when none is derived: B4 red. Skip `{{auto}}` in `applyClientIp`: the `{{auto}}` helper case red. Stop sending `x-sentinel-derive-ip`: the header-set case red. Forward `x-forwarded-for`: the header-set case red. Stop forwarding `user-agent`: the header-set case red. Then, **after reverting**, run the CI-mode signal once: change a route output without regenerating the fixtures and run `CI=1 npx -y pnpm@10.34.5 --dir packages/sdk-js test`. The fixture test is red and names the file. Revert. |
| **Criteria** | TASK-06 criterion 1 ("stored with the browser's IP, not the web server's") and criterion 2 ("never forwards a client-supplied forwarding header verbatim") in G2 (sdk-js and the Go replay) and at G3. TASK-01 criterion 1 (the tunnel-to-ingestor mapping, spec §1.3 and §10.2) in G2 and at G3. TASK-04 criterion 1 (`tunnel-clean` → `Chrome 128.0.0` / `Windows >=10`) in G2 and at G3. |
| **Extra checks** | `npx -y pnpm@10.34.5 --dir packages/sdk-js typecheck`, `… build` and `… check:no-dependencies`, which CI's `sdk-js-node` runs on a PR. `CI=1 npx -y pnpm@10.34.5 --dir packages/sdk-js test` is green on the committed fixtures. |

### 3.7 How the integration rows' mutations are covered

Rule 2 of §2 allows one container run. The G3 cases are therefore written in C2, C3 and C6, compiled by `go vet ./...` at every
commit, and **executed once**, in §4's final step. No G3 mutation is run. Every G3 row in spec §10.2 shares its production line with a
unit or sdk-js case whose mutation **is** run in §3. The one exception is "remove the `clientip.Middleware` wrap in `main.go`", which
only integration (ii) sees. For that one, the seat greps a call path from both ingest routes in `main.go` to `clientip.Middleware` (B3)
and cites it. The seat's report states plainly that G3 mutations were not executed, and why. It does not claim them.

---

## 4. The gate: exact commands, in this order, before reporting

Run from the repository root on the final commit, with `GOCACHE` set (§2). Stop at the first failure, fix inside the declared paths,
and restart the sequence **from G1**. Never re-run G3 after a failure without first re-passing G1 and G2, and never run it twice in
parallel with anything.

```bash
export GOCACHE=/home/fitrapujo/.cache/go-build-sentinel-event-enrichment

# G1 — build (root, and as CI's go-root sees it)
rtk task build
GOWORK=off rtk go build ./... && GOWORK=off rtk go vet ./...

# G2 — the widened unit gate (contract decision 9): tests/unit, ingestor packages, dashboard, sdk-js under pnpm 10.34.5,
#      and the tunnel-fixture drift checks
rtk task test-unit
GOWORK=off rtk go test ./tests/unit/... -count=1                 # what CI's go-root runs
CI=1 npx -y pnpm@10.34.5 --dir packages/sdk-js test              # the CI-mode fixture test never writes
npx -y pnpm@10.34.5 --dir packages/sdk-js typecheck && npx -y pnpm@10.34.5 --dir packages/sdk-js build
git status --porcelain                                           # must be empty: no fixture or lockfile rewrite

# G4 — lint
helm lint deploy/helm/sentinel

# G5 — render + kube-linter, both Huawei overlays (the lint-helm-huawei job)
for f in values-huawei-staging.yml values-huawei-production.yml; do
  helm template sentinel deploy/helm/sentinel -f deploy/helm/sentinel/$f > /tmp/$f.yaml &&
  kube-linter lint --config deploy/helm/sentinel/.kube-linter.yaml /tmp/$f.yaml || { echo "G5 FAILED: $f"; break; }
done

# G6 — CronJob renders; MAXMIND only in the CronJob or the SealedSecret; licence key is a secretKeyRef   (bash; spec §10.1 G6 verbatim)
# G7 — plain values.yaml: geoip off, no CronJob, TRUSTED_PROXY_CIDRS rendered empty on the ingestor container   (spec §10.1 G7)
# G8 — every Service / Ingress / Endpoints of the FULL render equals 1e5c6e2's, both overlays   (bash; spec §10.1 G8 script verbatim)

# G3 — ONCE, ALONE, LAST (machine rule): builds both images and runs tests/integration with FORCE_TESTCONTAINERS=1
rtk task test-integration
```

G6, G7 and G8 are run **exactly as spec §10.1 writes them**, under `bash`. G6(b) must print `0` for both overlays. G6(c)'s window must
hold `secretKeyRef:`, `key: MAXMIND_LICENSE_KEY` and `optional: true`, with `0` `value:` lines. G8 must exit 0. The seat also re-runs
the G8 mutation once (a scratch Service template in the working tree gives exit 1, then it is removed) and records it.

**G3 expectations.** Every new `enrichment_test.go` case passes. The baseline elsewhere is spec-recorded: **78 passed, 0 failed,
9 skipped** on the testcontainers path (CLAUDE.md, 2026-07-31), plus the new cases. On rootless podman, nine
`TestProcessorService_*` tests are known to fail with `postgres ping unavailable … connection refused` from a testcontainers
lifecycle problem, while passing in CI (CLAUDE.md). If exactly those fail and every `enrichment_test.go` case passes, the seat
reports them as **pre-existing, unrelated** (quoting the output) and does **not** re-run the suite or "fix" them. Any other failure
is this change's to repair, followed by the §4 sequence from G1.

---

## 5. Criterion → step → gate

| Card / criterion (spec §10.2) | Made to pass in | Proved by |
|---|---|---|
| TASK-01 c1: `{{auto}}` stored with the caller's real IP (mapped per §1.3) | C6 (C2 supplies the resolver) | G2 (`applyClientIp` `{{auto}}` case, `tunnel-auto-ip` Go replay), G3 (`tunnel-auto-ip` through the container); post-deploy P1 now, P5 after web adopts 0.2.0 |
| TASK-01 c2: a spoofed XFF from an untrusted hop is not trusted | C2 | G2 (C1-C19, H1-H3, M1-M5, `IsPublic`), G3 (integration (i), (ii)) |
| TASK-01 `{{auto}}` / client-platform rules, including I3t | C2 | G2 (`ingestor_enrich_ip_test.go`) |
| TASK-01 wired to `main()` | C2 | G3 (ii); grep of both wrapped chains (B3) |
| TASK-03 missing / corrupt mmdb logs once, ingestion continues | C3 | G2 |
| TASK-03 lookup rules and reload | C3 | G2 |
| TASK-03 wired to `main()` | C3 | G3 |
| TASK-03 staging event from a known IP stores country and city | — | post-deploy P2 (after TASK-07 and one job run) |
| TASK-04 c1: sdk-js browser event gets browser and OS from its UA | C6 (C4 supplies the parser) | G2 (`tunnel-clean` replay, header-set case), G3 |
| TASK-04 source and fill rules | C4 | G2 |
| TASK-05 CronJob and geo wiring render; key only via the sealed secret | C5 | G5, G6, G7, G2 (`helm_geoip_test.go`); G8 (no network object changed) |
| TASK-05 zero MaxMind downloads from pods | C5 | G2 (imports test, refresh (b)-(f)), G6; post-deploy P3 |
| TASK-05 OBS → AWS S3 is values/secret only | C3 | G2 (`ingestor_geoip_s3_test.go`) |
| TASK-05 pods have a readable, current mmdb | — | post-deploy P4 (after TASK-07) |
| TASK-06 c1: browser's IP, not the web server's | C6 | G2 (sdk-js, Go replay including the four-variant public-peer replay, `encoding/json` decode), G3 |
| TASK-06 c2: no client forwarding header forwarded verbatim | C6 | G2 (header set, spoof case, fixture shape), G3 (`tunnel-spoofed-xff`) |
| Compatibility: fingerprints unchanged | C4 | G2 |
| Compatibility: sent fields byte for byte; no network I/O | C4 | G2 |
| Build | every commit | G1 |

---

## 6. What the seat reports, and what it does not do

- **Report** (on its return channel): the outcome, the PR URL, the final commit SHA, each gate's command and result (the G2 counts,
  G3 pass/fail/skip counts, G6's printed values, G8's exit code), the mutation log per commit, the explicit note that G3 mutations were
  not run (§3.7), and any pre-existing failure quoted verbatim with its classification.
- **Not this seat's:**
  - TASK-07: the MaxMind account and sealed keys.
  - Post-deploy P1-P5: operator steps after merge and deploy. P5 is recorded under `sentinel-sdk-js-rich-context/TASK-06`.
  - Any sdk-js tag or release, and any web bump (contract decision 5).
  - The dashboard's pnpm pin (spec §12).
  - Any Service, Ingress or load-balancer change (contract decision 11; G8 enforces it).
- **Stop conditions:**
  - `NEEDS_DECISION`: the MaxMind fixture metadata differs from spec §5.6, or a spec rule cannot be implemented as written.
  - `BLOCKED_EXTERNAL`: a new Go module cannot be fetched through the image's `GOPROXY`, or the fixture source is unreachable.
  - Everything else inside the declared paths is repaired, not asked about.
