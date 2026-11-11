# Sentinel event enrichment (client IP, geo, User-Agent): implementation specification

**Feature:** `sentinel-event-enrichment` · **Repository:** `sentinel` (`NurfitraPujo/sentinel`) · **Base:** `main` @ `1e5c6e2` (v0.2.36)
**Built against:** `prd/sentinel-event-enrichment/contract.md` @ `774039e` (decisions 1-11) and `be.md` @ `e07441f`. If this document
and the contract disagree, the contract wins, except where §1.3 records that the contract's description of the code or of Sentry is
wrong. Each such case says what this spec does about it.
**Work items:** `sentinel-event-enrichment/TASK-01`, `TASK-03`, `TASK-04`, `TASK-05`, `TASK-06`. `TASK-07` belongs to the operator, and
nothing here depends on it to build or test. §10 maps every acceptance criterion to the command that proves it.
**Outcome of the open questions:** none are left open, and there is no `NEEDS_DECISION`. Every rule below is derived from the contract,
the code, Sentry's own implementation (Relay), K8s's or MaxMind's documentation, or a coordinator/operator ruling, and §11 names the
source of each. §11.2 lists the trust-boundary consequences the spec reviewer should see before merge.
**Revision 2 (2026-09-24):** repairs the 13 findings of the first spec review (2 major, 7 minor, 4 note) and records the coordinator's
rulings R1-R4, which the contract now carries as decisions 8-10.
**Revision 3 (2026-09-24):** follows **contract decision 11** (operator, after the second spec review), which supersedes decision 2's
mechanism and all of decision 10. The tunnel now writes the browser's IP into each forwarded event's `user.ip_address`; the ingestor
trusts no forwarding header on K8s (`trustedProxyCidrs` is empty in both overlays) and never stores a non-public address as a
derived caller IP. The `…-ingestor-elb` Service, `externalTrafficPolicy: Local`, the `ClusterIP` conversion, the sync waves, the
health-check analysis and the security-group fallback are **removed**: this feature makes no Service, NodePort or load-balancer change.
It also repairs the 9 findings of the second spec review (2 major, 4 minor, 3 note). §11.4 lists every revision-3 change, and §13 maps
each finding id, from both reviews, to the sections it changed.
**Revision 4 (2026-09-24):** repairs the 10 findings of the third spec review (1 major, 3 minor, 6 note), under the coordinator's
ruling on the major `enr-rev3-casefold-ip-key`. The contract stays pinned at `774039e`, and decision 11 still binds. The tunnel's
`applyClientIp` now **rebuilds** `user` from an exact-key allow-list (`id`, `username`, `email`, string values only), so no key that
Go's case-folding JSON decoder could read as `ip_address` or `geo` survives, and a browser-sent `geo` is dropped (§4.4). The tunnel
also sends a no-derive marker, `X-Sentinel-Derive-IP: off`, which the ingestor honours, so a tunnel that derived no IP can never let
the ingestor fill the web server's address (§3.8). §11.5 lists every revision-4 change, and §13.1 maps each third-review finding to it.
**Revision 5 (2026-09-24):** repairs the 4 findings of the fourth spec review (1 minor, 3 note). The contract stays pinned at
`774039e`. G8 now diffs **every** Service, Ingress and Endpoints document of the **full** render against `1e5c6e2`, so a Service added
in any template file turns it red (dry-run with a scratch `-ingestor-elb` template: exit 1, §10.1). I3 no longer fills a
connection-derived IP for a `javascript` item whose `metadata.runtime` is `browser`, because that runtime only ever arrives through a
tunnel, and a pre-0.2.0 tunnel sends no marker (§3.2, §3.9). The hops-0 README text and test now say what the Next.js App Router
really provides, which is no socket (§4.3, §4.5). P5's reference address is taken over IPv4 with no proxy (§10.3). §11.6 lists every
revision-5 change, and §13.0 maps each fourth-review finding to it.

Every "today" claim cites `path:line` at `1e5c6e2` unless it names another source. Facts about the running cluster were read on
2026-09-24 with read-only `kubectl` against the shared CCE cluster that hosts both `sentinel-staging` and `sentinel-production`.

---

## 1. The problem

### 1.1 In the words of whoever has it

> Whoever triages a Sentinel issue can see where and on what an error happened only if the SDK happened to send it. Sentry fills
> three things in on the server; Sentinel fills in none of them. — contract §1

The contract states the hypothesis like this. Say a tunnelled browser event is stored on staging. It must carry the browser's IP, the
country and city for that IP, and a browser and OS parsed from the browser's User-Agent. None of that may need an SDK upgrade for geo
and UA.

### 1.2 What is true today (verified)

| # | Fact | Evidence |
|---|---|---|
| T1 | The ingestor reads exactly two request headers, `X-API-Key` and `X-Project-Key`. No `RemoteAddr`, `X-Forwarded-For` or `User-Agent` is read anywhere in `apps/ingestor-go`. | `apps/ingestor-go/auth/apikey.go:114`, `:158`. `grep -rn "RemoteAddr\|X-Forwarded\|User-Agent" apps/ingestor-go` returns nothing else. |
| T2 | `otelhttp` records `client.address` from `RemoteAddr` / `X-Forwarded-For` on spans, and the metric view strips it. Nothing reads it back. | `packages/shared-go/obs/provider.go:175-195` |
| T3 | The live pipeline for one item is: decode, then `applyAuthenticatedScope`, then `IngestAt`. `IngestAt` runs `resolveEventID`, `resolveTimestamp`, the `ReceivedAt` stamp, `ValidatePayload` (the bounds step), `MapPayloadToEvent`, protovalidate and publish, in that order. The receipt time is read once per HTTP request and shared by every batch item. | `apps/ingestor-go/main.go:413-433` (single), `:476-508` (batch); `apps/ingestor-go/service/service.go:234-285` (`:239`, `:249`, `:256`, `:265`, `:274`) |
| T4 | `user` and `contexts` are copied verbatim from the body. `UserInfo{id, username, email, ip_address, geo}`, `GeoInfo{city, region, country_code, subdivision}`, `Contexts map[string]map[string]interface{}`. | `apps/ingestor-go/validation/validator.go:94-126`; `apps/ingestor-go/mapping/mapper.go:60-61`, `:99-125` |
| T5 | The bounds step's user and contexts rules: `ip_address` ≤ 45 code points, **dropped** if over; `geo.city` / `region` / `subdivision` ≤ 128 code points, truncated; `geo.country_code` ≤ 8, dropped; contexts ≤ 32 items and ≤ 65536 serialized bytes. When over, it removes other names before the well-known `device os app runtime browser culture`, largest first. Every cut is recorded in `metadata.server_truncated`. | `apps/ingestor-go/validation/bounds.go:58-66` (constants), `:122-125` (well-known), `:306-335` (`applyBounds`), `:337-351` (`boundUser`), `:416-466` (`boundContexts`), `:734` (`objectSize`) |
| T6 | `request.headers` is a `map[string]string` with case-sensitive keys, bounded to 64 entries, 256-code-point keys and 4096-code-point values. | `validator.go:129-136`; `bounds.go:469-520` |
| T7 | `platform` must match `^[a-z0-9]+$` and be ≤ 50 characters, so **a hyphen is a 400**. | `packages/proto/sentinel/v1/error_event.proto:20-22` |
| T8 | Two SDKs produce events. `sdk-go` sends `platform: "go"`. `sdk-js` sends `platform: "javascript"` from **every** runtime (browser, node and edge) and records the runtime in `metadata.runtime` ∈ {`browser`, `node`, `edge`}. No other platform value is produced in the repo (repo-wide grep: `go`, `javascript`, plus test literals `golang`/`python`/`node`). | `packages/sdk-go/event.go:319`; `packages/sdk-js/src/core/capture.ts:260`, `packages/sdk-js/src/core/types.ts:45`, `packages/sdk-js/src/core/event.ts:178`; `docs/sdk-specification.md:34` |
| T9 | The sdk-js tunnel forwards `content-type`, `x-api-key` and `x-project-key` and nothing else. A test pins exactly that header set against a client that sends `X-Forwarded-For`, `User-Agent`, `Cookie`, `Authorization` and `traceparent`. | `packages/sdk-js/src/nextjs/route.ts:427-440`; `packages/sdk-js/tests/route/route.test.ts:571-590` |
| T10 | The tunnel's body filter `ITEM_KEYS` keeps only the **pre-v2** keys (`event_id` … `fingerprint_override`). `user`, `contexts`, `request` and every other schema-v2 key are **deleted** from every tunnelled item. It forces `platform = 'javascript'`. `metadata` passes through, so `metadata.runtime` survives. The filtered items are re-serialized (`JSON.stringify(shape.items)`) and posted as one array to `<endpoint>/batch`, so the tunnel already writes every forwarded body itself. | `route.ts:64-79`, `:270-295` (`normaliseBody`), `:427-441` (step 8) |
| T11 | The tunnel's default rate-limit key `defaultClientKey` is the **leftmost** non-empty `X-Forwarded-For` hop, which the browser writes. The only adopter, `web`, overrides it with `clientKey: getClientIp`. | `route.ts:197-220`, `:42-43`; `web` `src/app/api/sentinel/route.ts:10-16` @ `183f390a6` |
| T12 | `web`'s `getClientIp` takes the XFF entry at `len − TRUSTED_PROXY_HOPS` (default 1, i.e. the **rightmost** hop) and returns it only if `isIP` accepts it. `TRUSTED_PROXY_HOPS` is unset in `example-fe-staging`, so it is 1. | `web` `src/app/api/client-ip.ts:7-31`, `.env.example:69-70` @ `183f390a6`; `kubectl -n example-fe-staging get cm example-fe-zasri` (no such key) |
| T13 | `web`'s ingress chain has one proxy. `app-staging.example.com`, `app.example.com` and `zasri.com` resolve to `114.119.184.248`, a K8s ELB (`Server: elb`, no CDN header). It is a **shared** load balancer: `web`'s Ingresses carry `kubernetes.io/elb.class: union`, `elb.id: f9076b2b-71cd-4564-a7f5-2a894e1755fe`, `elb.port: 443`, and their 443 listener's master is another team's Ingress, `kubernetes.io/elb.listener-master-ingress: argocd/argocd-public-production`, which sets no `X-Forwarded-For` annotation. On a shared load balancer, "Transfer Client IP Address" for **HTTP and HTTPS** listeners is "Enabled by default" and disabling it is "Not supported". Client IP transfer at layer 7 is the `X-Forwarded-For` header. So this listener always inserts the caller's address into `X-Forwarded-For`, and the rightmost entry is the ELB's. K8s does not document whether an existing header is appended to or replaced. The rightmost-hop rule (§4.1) gives the ELB's entry in both cases, so the difference does not matter. P5(b) (§10.3) checks the result end to end. The ELB forwards `app-staging.example.com` to NodePort Service `example-fe` (`32003:31476`), and `app-staging.zasri.com` to `example-fe-zasri` (`31115:31768`). Those pods run in the **same** cluster as Sentinel, pod IPs `172.16.x.x`. | `dig @1.1.1.1`, `curl -sI https://app-staging.example.com`; `kubectl -n example-fe-staging get ingress,svc,pods -o wide`, `kubectl -n argocd get ingress argocd-public-production -o jsonpath='{.metadata.annotations}'` (2026-09-24); K8s ELB user guide, "Transfer Client IP Address (Shared Load Balancers)" (`usermanual-elb/elb_ug_jt_0515.html`, table "Transfer client IP address support": "HTTP and HTTPS \| Enabled by default \| Not supported"); ELB FAQ `elb_faq_0090` ("Layer 7 load balancing … The real IP address is placed in the X-Forwarded-For header field by the load balancer") |
| T14 | The ingest host `api-sentinel(-stg).example.com` resolves to the private shared ELB `10.200.7.29` (`kubernetes.io/elb.class: union`, `ingressClassName: cce`). The ELB routes to a **NodePort** Service (staging `31123`, production `31316`) with `externalTrafficPolicy: Cluster`; so is every NodePort Service in the cluster. Staging and production share one ELB id. So the ingestor's `RemoteAddr` for any caller that comes through the ELB is an infrastructure address (a node, a node's container gateway, or the ELB's `100.125.x.x` backend range), never the caller. **This feature changes none of it** (contract decision 11). | `deploy/helm/sentinel/templates/ingestor-ingress.yaml:1-29`; `values-k8s-staging.yml:128-139`; `values-k8s-production.yml:128-139`; `kubectl -n sentinel-staging get svc sentinel-sentinel-ingestor`; `kubectl get svc,ingress -A` (2026-09-24) |
| T15 | The cluster is CCE Standard with `vpc-router` networking in the production VPC `10.200.0.0/16`. Nodes are in the private subnets `10.200.32.0/19`, `10.200.64.0/19`, `10.200.96.0/19` and `10.200.128.0/19`. Pods are in `172.16.0.0/16`, and Services in `10.247.0.0/16`. Both `sentinel-staging` and `sentinel-production` deploy to this cluster (`destination.server: https://kubernetes.default.svc`). The node subnets are **not node-only**: `infra` places the non-node ECS `ai-node-production-0` (`allow_all_ingress_from_vpc`) in the same subnet id the CCE cluster names. Every one of these ranges is private (RFC 1918), and K8s's ELB backend range `100.125.0.0/16` lies inside the carrier-grade-NAT block `100.64.0.0/10` (RFC 6598). | `infra` `terraform/provisioning/hcs/ap-southeast-3/production/vpc/example-production/main.tf:58-78` @ `360420d`; `infra` `ecs/ai-node-production-0/main.tf:8`, `cce/example-production/main.tf:46` @ `360420d`; `values-k8s-staging.yml:67-69`; `deploy/helm/sentinel/argocd-app/{staging,production}.yml:17-19`; `kubectl get nodes -o wide`, `kubectl -n sentinel-staging get pods -o wide` |
| T15a | **The ingest path is IPv4-only**, although the VPC is dual-stack (`ipv6_enable = true`). Every node reports only an IPv4 `InternalIP` and no pod CIDR of either family; every ingestor pod has one IPv4 `podIP`; both ingestor Services are `ipFamilies: ["IPv4"]`, `ipFamilyPolicy: SingleStack`; `api-sentinel-stg.example.com` and `api-sentinel.example.com` have an `A` record (`10.200.7.29`) and **no `AAAA`**. Since revision 3 this fact carries no trust weight: nothing is trusted on K8s, and the public-caller rule (§3.5) covers both families. | `infra` `vpc/example-production/main.tf` (`ipv6_enable`); `kubectl get nodes -o custom-columns=…status.addresses[*].address,…spec.podCIDRs`, `kubectl -n sentinel-{staging,production} get pods -o custom-columns=…status.podIPs[*].ip`, `kubectl -n sentinel-{staging,production} get svc sentinel-sentinel-ingestor -o jsonpath='{.spec.ipFamilies} {.spec.ipFamilyPolicy}'`, `dig +short AAAA … @1.1.1.1`, all 2026-09-24 |
| T16 | Object storage is reached through `minio-go` v7.1.0, already in `go.mod`. The processor's client takes the endpoint URL (the scheme decides TLS) and the bucket. It hard-codes region `us-east-1` and picks path-style or virtual-host by host shape, overridable by `S3_FORCE_PATH_STYLE`. K8s OBS **requires** virtual-host. | `go.mod:14`; `apps/processor-go/symbolicate/fetcher.go:18-121` (`:53-72` rule, `:113` region) |
| T17 | The chart's S3 values are `config.s3.{endpoint, publicEndpoint, bucket}`, rendered into the shared ConfigMap as `S3_ENDPOINT` / `S3_PUBLIC_ENDPOINT` / `S3_BUCKET`. The credentials are the Secret keys `S3_ACCESS_KEY` / `S3_SECRET_KEY`. The ingestor container already receives `S3_ENDPOINT`, `S3_PUBLIC_ENDPOINT` and `S3_BUCKET` through its `envFrom` of the shared ConfigMap, but **no S3 credential**. The `GEOIP_*` names this spec adds do not collide with any of them. | `deploy/helm/sentinel/values.yaml:97-105`; `templates/configmap.yaml:16-18`; `templates/_helpers.tpl:86-92`; `templates/ingestor.yaml:55-57` (`envFrom`), `:58-74` (`env`, no S3 key) |
| T18 | K8s secrets are a `SealedSecret` rendered from `secrets.sealedData`: one ciphertext per key, sealed for namespace `sentinel-<env>` and name `sentinel-sentinel-secrets`. | `templates/secret.yaml:1-17`; `values-k8s-staging.yml:46-64` |
| T19 | Every pod runs with `readOnlyRootFilesystem: true`, UID 65532 and all capabilities dropped. Scheduled jobs are `batch/v1 CronJob`s with `concurrencyPolicy: Forbid`, and chart convention is that they "ship OFF". | `values.yaml:540-549`; `templates/retention-cronjob.yaml:1-61`; `values.yaml:394` |
| T20 | The ingestor image is Alpine with `ca-certificates`, `ENTRYPOINT ["/app/ingestor"]`, built from the repo root. Every CI and release image build is a matrix row. | `apps/ingestor-go/Dockerfile`; `.github/workflows/ci.yml:725-777`, `release-k8s-staging.yml:65-100` |
| T21 | The chart gate renders both K8s values files and lints them with `kube-linter` v0.6.5 and the waivers in `.kube-linter.yaml`. | `.github/workflows/ci.yml:678-718`; `deploy/helm/sentinel/.kube-linter.yaml` |
| T22 | `task test-unit` runs `go test ./tests/unit/...` and the **dashboard** vitest suite only. It runs neither `apps/ingestor-go/...` package tests nor the sdk-js suite **today**; §10.1 widens it (contract decision 9). CI's `sdk-js-node` job installs pnpm **10** (`pnpm/action-setup@v4`, `version: 10`) and runs `pnpm test` only on push, never on a pull request. `task test-integration` builds `localhost/sentinel_ingestor:latest` and `…processor:latest` and runs `go test ./tests/integration/...` with `FORCE_TESTCONTAINERS=1`. | `Taskfile.yml:184-190`, `:203-210`; `.github/workflows/ci.yml:361-363`, `:399-401` |
| T23 | The integration suite runs the ingestor container with `NetworkMode: "host"`, so its `RemoteAddr` for a test client is `127.0.0.1`. It passes a fixed env map. | `tests/integration/testcontainers/ingestor.go:132-170` |
| T24 | The fingerprint input is `Environment \| ErrorClass \| file:function` of up to three frames. `user` and `contexts` are not inputs. The golden test decodes every JSON in `packages/sdk-js/tests/fixtures/*.json`, plus two Go fixtures, and pins their fingerprints. | `apps/processor-go/fingerprint/fingerprint.go:59-116`; `tests/unit/fingerprint_golden_test.go:36-46`, `:73-95` |
| T25 | The processor stores `user` unmasked and runs the masker over `contexts`. The masker's patterns are SSN, passport, `api_key=`, `password=`, `token=`, credential and bearer, so none of them can match a browser or OS name or version. | `apps/processor-go/event/event.go:125-130`, `:297-298`; `apps/processor-go/masker/masker.go:10-16` |
| T26 | Chart-value assertions under `task test-unit` are Go tests that parse the values files and deep-merge the overlays. They never exec `helm`, because `go-root` runs `tests/unit` with no helm on the runner. | `tests/unit/helm_dashboard_cap_test.go:1-57`, `tests/unit/helm_processor_memory_test.go` |
| T27 | Cross-language proof precedent: the sdk-js vitest suite **writes** committed fixtures, and a `tests/unit` Go test consumes them, so the Go side never needs node. | `tests/unit/sdk_js_fixture_test.go:18-30`; `packages/sdk-js/tests/fixtures.test.ts:1-8` |
| T28 | The ingestor decodes a batch with `encoding/json` into `[]validation.ErrorPayload`. That decoder matches object keys to struct fields **case-insensitively, with Unicode simple folding**, and when two keys match one field **the last one wins**. Measured with go1.26.4 against a struct shaped like `UserInfo`: `{"user":{"ip_address":"198.51.100.23","IP_ADDRESS":"6.6.6.6"}}`, `{"user":{"Ip_Address":"6.6.6.6"}}` and `{"user":{"ip_addreſſ":"6.6.6.6"}}` (U+017F, long s) all decode `ip_address` as `6.6.6.6`. `{"user":{…,"GEO":{"city":"Fake"}}}` decodes a `geo`, and `{"user":{…},"USER":{"ip_address":"6.6.6.6"}}` decodes the second object as `user`. The tunnel's top-level filter is an exact-key `Set.has` over `ITEM_KEYS`, so no top-level key that only folds to an allowed one survives it. | `apps/ingestor-go/main.go:475-476`; `validator.go:94`, `:114-120`; `route.ts:64-79`, `:283-286`; measured 2026-09-24 |

### 1.3 Where the contract does not match the code or Sentry

Every correction below goes upward. The last column says what this spec does about each one.

| Contract says | What is true | Consequence for this spec |
|---|---|---|
| §4.1: client platforms are "browser/javascript, dart/flutter, cocoa, java-android". | `java-android` can never arrive: the proto rejects a hyphen (T7). Sentinel receives only `go` and `javascript`, and `javascript` is also what sdk-js **node and edge** send (T8). Sentry's own list is `javascript`, `cocoa` and `objc` (Relay `relay-event-normalization/src/event.rs`, `normalize_ip_addresses`, "In an ideal world all SDKs would set {{auto}} explicitly"). | §3.1 fixes the list. `javascript` counts as a client platform unless `metadata.runtime` is `node` or `edge`. Android is served by `{{auto}}` only. |
| §4.3: "The tunnel forwards no User-Agent today, so the body is the source for tunnelled browser events." | The body **cannot** be the source. The tunnel deletes `request` from every item (T10), and sdk-js sends no `request` at all (grep: no `user_agent` / `navigator` / `request` in `packages/sdk-js/src`). The only UA the ingestor would see for a tunnelled event is the web server's Node `fetch` UA. So as written, the hypothesis in §1 (browser and OS on a tunnelled staging event) is unreachable. | §4.4: the tunnel forwards the browser's `User-Agent` on its forward. The ingestor's "ingest request's UA, client platforms only" rule (contract §4.3) then yields the browser's UA. No `ITEM_KEYS` change. |
| §4.3: fill `contexts.os`. | Sentry fills `contexts.os` only for `javascript`. For every other platform it writes `contexts.client_os`, "to avoid conflicts with OS-context sent by a serverside SDK" (Relay `normalize/user_agent.rs`, `normalize_user_agent_info_generic`). A Go event whose `request.headers` carries the end user's UA would otherwise record the browser's OS as the server's OS. | §6.3: `contexts.os` for client-platform events and `contexts.client_os` otherwise. It is never written over an existing key. |
| TASK-06 goal: forward the caller's IP "derived the same way as its rate-limit key". | The default rate-limit key is the leftmost hop, which the browser controls (T11). Deriving the IP that way would store any address the browser chooses, which fails TASK-06's own spoof criterion. | §4.2: one derivation, rightmost-trusted-hop, feeds both the IP the tunnel writes into the body (§4.4, contract decision 11) and the default rate-limit key. The default key therefore changes (§4.3). |
| §1: "The ingestor's only peer is the web server." | The ingestor's immediate peer is infrastructure, not the web server: the ELB forwards to NodePort `31123`/`31316` under `externalTrafficPolicy: Cluster` (T14), so kube-proxy rewrites the source to a node-side address. | Contract decision 11 already accounts for it. On K8s no forwarding header is believed, the peer is always private or CGNAT, and so the ingestor never derives a caller IP there (§3.5, §3.6). The browser's IP arrives in the body, written by the tunnel (§4.4). |
| TASK-01 criterion 1: "An event with `user.ip_address={{auto}}` is stored with the caller's real IP behind the cluster ingress (staging check)." | Under decision 11 this holds for a **tunnelled** `{{auto}}` event, because the tunnel replaces the `{{auto}}` with the browser's IP before the ingestor sees it. It cannot hold for a caller that posts `{{auto}}` **directly** through the private ELB: the ingestor believes no header on K8s and its peer is infrastructure, so the stored value is `""` (§3.2 I2). That is the decision's intended result, not a defect. | Per the coordinator's brief for revision 3, criterion 1 is proved by the tunnel-to-ingestor integration case (§10.2), and the staging observation is post-deploy step P5, once web adopts sdk-js 0.2.0 (`sentinel-sdk-js-rich-context/TASK-06`). P1 checks now that a direct `{{auto}}` is stored as `""`, never as an infrastructure address. The criterion's wording goes upward. |

---

## 2. What this change does, and does not do

**Does:**
1. It resolves the caller's IP per HTTP request. It believes `X-Forwarded-For` only through a configured trusted-proxy CIDR list, and
   fills `user.ip_address` for `{{auto}}` on any platform and for an absent IP on client platforms (TASK-01). A derived caller IP is
   used only if it is **public**: a private, loopback, link-local, carrier-grade-NAT or shared-ELB address is never stored as one, and
   `{{auto}}` with no public caller is stored as `""` (§3.5). On K8s the trusted list is **empty** in both overlays, so no forwarding
   header is believed there; the rule stays in code for Phase 4's public endpoint (contract decision 11, §3.6). A request that carries
   the tunnel's no-derive marker has no caller IP on any deployment (§3.8), and a `javascript` item whose runtime is `browser`, which
   only a tunnel can deliver, never gets a connection-derived IP, whatever the tunnel's version (§3.9).
2. It looks the IP up in a GeoLite2-City database held in memory and fills an absent `user.geo`. It reloads a newer database from S3
   without a restart. Any failure means no geo (TASK-03).
3. It parses a User-Agent into `contexts.browser` and `contexts.os` / `contexts.client_os` when those are absent (TASK-04).
4. The sdk-js tunnel derives the browser's IP by the rightmost-trusted-hop rule (`trustedProxyHops`) and **writes it into each
   forwarded event's `user.ip_address`**. It overwrites any value the browser sent, and adds a `user` object where `ITEM_KEYS` removed
   it. A `user` that survives the filter is **rebuilt** from the exact keys `id`, `username` and `email`, with string values only, so a
   browser can smuggle neither an IP nor a `geo` past Go's case-folding decoder (§4.4). The tunnel forwards no `X-Forwarded-For`. It
   forwards the browser's `User-Agent` and the no-derive marker `X-Sentinel-Derive-IP: off` (§3.8). The default rate-limit key moves to
   the same derivation (TASK-06, contract decision 11).
5. A `geoip-refresh` subcommand of the ingestor binary runs from a twice-weekly CronJob. It downloads GeoLite2-City only when MaxMind
   has a newer file and writes it through the S3 API. The chart gains the storage, poll, trusted-CIDR (empty on K8s) and sealed-key
   wiring, plus RUNBOOK entries (TASK-05).
6. `task test-unit` also runs the ingestor's package tests and the sdk-js suite under pnpm **10.34.5**, pinned exactly in both the
   Taskfile and CI, and fails on tunnel-fixture drift (contract decision 9, §10.1).

**Does not:**
- It does not change the dashboard, the processor, the proto or the schema. There is no migration: schema v2's `event_user` and
  `contexts` JSONB columns already carry every derived value.
- The ingestor does not overwrite anything the body carries. The one exception is the literal `{{auto}}`. (The tunnel, which writes
  the body, does overwrite a browser-sent `user.ip_address`: contract decision 11, §4.4.)
- It does no per-event network I/O. Geo lookup and UA parsing run in-process, and the S3 fetch runs in a background goroutine.
- Ingestor pods never contact MaxMind. Only the CronJob does.
- It makes **no Service, `externalTrafficPolicy`, NodePort, Ingress or load-balancer change** (contract decision 11). Revision 2's
  `…-ingestor-elb` Service, the `ClusterIP` conversion, the sync waves and the security-group fallback are gone.
- It does not widen the tunnel's `ITEM_KEYS`, so tunnelled `contexts` and `request` stay deleted, and so does every key of a sent
  `user`. That belongs to `sentinel-sdk-js-rich-context`. Nothing here needs it: the tunnel adds back a `user` holding only the derived
  `ip_address` (§4.4), and the UA reaches the ingestor as a header.
- It does not read `request.env.REMOTE_ADDR`, Sentry's third IP source, which the contract does not list. It does not read `Forwarded`
  (RFC 7239), `X-Real-IP`, `X-Client-IP` or UA client hints (`Sec-CH-UA*`).
- It does not derive a `device` context. The contract names browser and OS only.
- No sdk-js release and no web bump (contract decision 5). Production browser events keep arriving without an IP or UA until
  web adopts sdk-js 0.2.0.

---

## 3. Client IP: ingestor (TASK-01)

### 3.1 Client platforms

An event is a **client-platform event** exactly when:

- `platform` ∈ {`javascript`, `browser`, `cocoa`, `objc`, `dart`, `flutter`}, and
- it is **not** the case that `platform == "javascript"` and `metadata.runtime` is the JSON string `"node"` or `"edge"`.

Derivation:
- `javascript` and `browser`: contract §4.1, plus the processor's existing notion of browser platforms (`values.yaml`
  `symbolicatePlatforms: "javascript,browser"`).
- The runtime split: T8. Without it every sdk-js **server** event would get a pod address, which contract §4.1 forbids ("A server
  platform with no IP stays empty. Its connection address is a pod, not a user.").
- `cocoa` and `objc`: Sentry's own list (§1.3).
- `dart` and `flutter`: contract §4.1.
- Android: `java-android` is unrepresentable (T7), and `java` is also every JVM server, so it is excluded. An Android SDK gets its IP by
  sending `{{auto}}`, which works on every platform (§3.2).

`metadata.runtime` is read from the decoded body **before** the bounds step. The ingestor only ever sees raw metadata, so B6 does not
apply. A client that lies about its runtime changes only whether its own event gets its own connection address.

### 3.2 The `{{auto}}` rule

In order, per item, after the caller IP is resolved (§3.4) and filtered by the public-caller rule (§3.5). "A public caller" below
means the resolver returned an address **and** that address passed §3.5. "None" covers three cases: no address, a non-public one, and
a request that carries the no-derive marker (§3.8), which the resolver answers with no address before it walks anything.

| # | Body `user.ip_address` | Client-platform event? | Resolved caller IP | Result |
|---|---|---|---|---|
| I1 | exactly `{{auto}}` (case-sensitive, no trimming) | any | a public caller | `user.ip_address` = the resolved address |
| I2 | exactly `{{auto}}` | any | none | `user.ip_address` = `""`. The literal is never stored, and neither is an infrastructure address. |
| I3 | absent (`user` null, or the key absent/`""`) | yes, **and not a tunnel-only item** (§3.9) | a public caller | `user` is created if null. `user.ip_address` = the resolved address. |
| I3t | absent | a **tunnel-only item**: `platform == "javascript"` and `metadata.runtime` is the JSON string `"browser"` (§3.9) | any | unchanged. Its connection is a tunnel's server, never the browser, whatever the tunnel's version. |
| I4 | absent | yes | none | unchanged |
| I5 | absent | no | any | unchanged. This is a server event, and its connection is a pod. |
| I6 | any other value, valid IP or not, public or not | any | any | **unchanged, byte for byte** (the body wins, contract decisions 2 and 11). This is the row a tunnelled event takes: the tunnel has already written the browser's IP (§4.4). |

I1 is unaffected by I3t: an explicit `{{auto}}` is a request for the connection address, and on every tunnel it has already been
replaced (a 0.2.0 tunnel writes the IP, §4.4) or the item carries no `user` at all (a pre-0.2.0 tunnel's `ITEM_KEYS` removes it,
T10). The client-platform predicate (§3.1) is unchanged, and so is the connection-`User-Agent` rule (§6.2), which **needs** tunnel-only
items to count as client-platform events.

A value set here is `netip.Addr.String()` of an unmapped, zone-free address. That is ≤ 39 characters for IPv6 and ≤ 15 for IPv4, so
it always passes `MaxUserIPAddressLength` (45).

### 3.3 Trusted-proxy configuration

- **Env:** `TRUSTED_PROXY_CIDRS`, a comma-separated list of CIDR prefixes. A bare address is read as a single-address prefix (`/32` or
  `/128`). Whitespace around entries is ignored, and an empty entry (`a,,b`) is ignored.
- **Unset or empty: trust nothing.** No forwarding header is believed, and the caller IP is always the connection's `RemoteAddr`, which then
  still has to pass the public-caller rule (§3.5). This is the K8s configuration (§3.6).
- **An unparseable entry is a startup error.** `main` logs `TRUSTED_PROXY_CIDRS: "<entry>" is not a CIDR or IP address` and exits 1,
  before listening. This is the repo's config-typo convention (`fetcher.go:53-60`, a bad `S3_FORCE_PATH_STYLE` is fatal). A typo in a
  trust boundary must not become silent. A rollout with a bad value never becomes ready, and the previous ReplicaSet keeps serving.
- Parsed once at startup into `[]netip.Prefix` with `netip.ParsePrefix(...).Masked()`. IPv4-mapped IPv6 addresses are unmapped before
  every containment check.
- **An IPv4-mapped entry is a startup error**, whether it is a prefix (`::ffff:203.0.113.0/120`) or a bare address
  (`::ffff:203.0.113.10`, read as `/128`). An entry is IPv4-mapped when `prefix.Addr().Is4In6()` holds. `main` logs
  `TRUSTED_PROXY_CIDRS: "<entry>" is an IPv4-mapped IPv6 prefix; write it in IPv4 form` and exits 1, as for C12. The reason is that
  addresses are unmapped before the containment check, and `netip.Prefix.Contains` never matches an IPv4 address against an IPv6
  prefix (measured, go1.26.4). A mapped entry would therefore trust nothing, and in silence. If that proxy's address is public, it
  would then be stored as every caller's IP. Converting the entry to its IPv4 prefix (bits − 96) was rejected: a mapped prefix
  shorter than `/96` has no IPv4 equivalent, and one rule with a message is simpler than a conversion that must also reject some
  inputs (`enr-rev3-mapped-trusted-prefix`).
- Logged once at startup at Info: `trusted proxy ranges` with the parsed list, which is not secret.
- **Chart value:** `config.ingestor.trustedProxyCidrs` (a YAML list, default `[]`). It is rendered as plain env on the ingestor container
  only, not in the shared ConfigMap. The precedent is `config.processor.*`, which is rendered as env on the processor container
  (`values.yaml:158-165`).
- **Both K8s overlays set it to `[]` explicitly**, with a comment naming contract decision 11 (§3.6, §8.4). The explicit empty list
  is what the helm values test reads and asserts (§10.2 TASK-05), so re-adding a range there is a deliberate, test-visible change. The
  first deployment that sets a non-empty list is Phase 4's public endpoint (`sentinel-sentry-compat-ingest`), which must establish its
  own chain (who appends, who the peer is) when it does.

### 3.4 Which header, which hop

The ingestor reads **`X-Forwarded-For` only**. All header lines are taken in order (`r.Header.Values("X-Forwarded-For")`), joined with
`,` and split on `,`, and each entry is trimmed of spaces and tabs. The walk treats the chain as `[xff_1, …, xff_n, peer]`, where
`peer` is the host of `r.RemoteAddr`:

1. Start at the right end, which is `peer`, and move left.
2. Parse each entry as an address: `netip.ParseAddr`, else `netip.ParseAddrPort` for `ip:port` or `[v6]:port`. Then `Unmap()` it and
   drop any zone.
   - An empty or unparseable entry **stops the walk with no result**. A hop that cannot be read means the chain left of it cannot be
     vouched for.
3. An entry **outside** every trusted prefix is the caller, and the walk returns it.
4. An entry **inside** a trusted prefix is one of our own proxies. The walk continues left.
5. Running off the left end, or passing 32 entries (the cost cap), returns **no result**.

**An address inside a trusted range is never a caller address.** It is one of our own proxies, so the walk passes over it. Combined
with the public-caller rule (§3.5), this is what makes "a server platform's connection is a pod" hold even for `{{auto}}` from an
in-cluster server, and it stops the web server's own pod address from ever being stored as a browser's IP.

Given a proxy that appends, a spoofed left-hand entry is only believed if every hop to its right is trusted, which is the definition of
a trusted chain. **No K8s forwarding behaviour is relied on.** With the empty K8s list (§3.6) the walk returns the peer at step 3,
before it reads any header, so whether the shared ELB's listener appends to, replaces or drops `X-Forwarded-For` changes nothing that
is stored. Revision 2's append citations and its in-cluster append proof (P1(c)) are therefore removed. Phase 4, the first deployment
with a non-empty list, must establish its own proxy's behaviour when it configures one (§3.3).

The result is computed **once per HTTP request**, like `receivedAt` (T3), and shared by every item of a batch. It is carried on the
request context with the ingest request's `User-Agent`. See §7.2.

### 3.5 A derived caller IP must be public

Contract decision 11: the ingestor "never stores a private, loopback, link-local, carrier-grade-NAT or shared-ELB (`100.125.0.0/16`)
address as a DERIVED caller IP. `{{auto}}` with no public caller is stored as empty."

`clientip.IsPublic(a netip.Addr) bool` is the one definition. The address is `Unmap()`ped and its zone dropped first
(`a = a.Unmap().WithZone("")`). The function is built on `netip.Addr.IsGlobalUnicast()`, which already excludes invalid,
unspecified, loopback, link-local unicast, every multicast scope and the IPv4 broadcast address. It then subtracts the private ranges
and a fixed prefix list:

```go
return a.IsGlobalUnicast() && !a.IsPrivate() && !inAny(a, notPublic)
// notPublic: 0.0.0.0/8, 100.64.0.0/10, 100.125.0.0/16, 240.0.0.0/4,
//            fec0::/10, ::/96, 64:ff9b::/96, 64:ff9b:1::/48, 2002::/16
```

An address is **not** public when any row holds:

| # | Not public | Covers | Implemented by | Source |
|---|---|---|---|---|
| N1 | invalid or unspecified | `0.0.0.0`, `::` | `!IsGlobalUnicast()` | not an address a TCP peer can have |
| N2 | loopback | `127.0.0.0/8`, `::1` | `!IsGlobalUnicast()` | contract decision 11 ("loopback") |
| N3 | private | `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, `fc00::/7` | `IsPrivate()` | contract decision 11 ("private"); every K8s node, pod and Service range (T15) |
| N4 | link-local, zoned or not | `169.254.0.0/16`, `fe80::/10` (`fe80::1%eth0` too, zone dropped), `ff02::/16`, `ff01::/16` | `!IsGlobalUnicast()` | contract decision 11 ("link-local") |
| N5 | multicast, and IPv4 broadcast | `224.0.0.0/4`, `ff00::/8`, `255.255.255.255` | `!IsGlobalUnicast()` | not an address a TCP peer can have |
| N6 | carrier-grade NAT (RFC 6598) | `100.64.0.0/10`, including the mapped `::ffff:100.125.64.17` after `Unmap` | prefix | contract decision 11 ("carrier-grade-NAT") |
| N7 | K8s's shared-ELB backend range | `100.125.0.0/16` (`elb_faq_0090`: "Load balancers use IP addresses in 100.125.0.0/16 to communicate with backend servers") | prefix | contract decision 11 ("shared-ELB"). N7 lies inside N6, and is listed and tested on its own so that narrowing N6 can never re-admit it. |
| N8 | "this network", beyond the exact `0.0.0.0` | `0.0.0.0/8`, e.g. `0.1.2.3` | prefix | RFC 1122 §3.2.1.3: not a valid source address |
| N9 | reserved IPv4 | `240.0.0.0/4`, e.g. `240.0.0.1` (`255.255.255.255` is N5) | prefix | RFC 1112 §4 ("reserved for future addressing modes") |
| N10 | deprecated IPv6 site-local | `fec0::/10` | prefix | RFC 3879 (deprecated; `IsPrivate` covers only `fc00::/7`) |
| N11 | IPv6 forms that embed an IPv4 address | `::/96` IPv4-compatible (`::10.0.0.1`), `64:ff9b::/96` NAT64 (`64:ff9b::a00:1`), `64:ff9b:1::/48` local-use NAT64, `2002::/16` 6to4 (`2002:a00:1::1`) | prefix | RFC 4291 §2.5.5.1, RFC 6052, RFC 8215, RFC 3056 |

N1, N5, N8, N9 and N10 are not in the contract's list. None of them can be the source address of a real public caller, so excluding
them removes nothing a caller could have. N11 is a choice between two readings, and the conservative one is taken. The address
under the IPv6 wrapper is an IPv4 address the translator chose to encode, and in each measured example it was private (`10.0.0.1`).
Unwrapping it and testing the inner IPv4 address would store a few real NAT64 callers' addresses. Treating the whole range as not
public stores nothing for them. A derived IP that is dropped costs a label. A wrong one stores an infrastructure address as a
caller's, which is exactly what contract decision 11 forbids (`enr-rev3-ispublic-range-gaps`).

Measured with go1.26.4 on 2026-09-24: `IsGlobalUnicast()` is `false` for `255.255.255.255`, `fe80::1%eth0`, `224.0.0.1`, `::1` and
`0.0.0.0`. It is `true` for `0.1.2.3`, `240.0.0.1`, `::10.0.0.1`, `64:ff9b::a00:1`, `64:ff9b:1::a00:1`, `2002:a00:1::1`, `fec0::1`,
`100.64.0.1` and the unmapped `::ffff:100.125.64.17`, and `IsPrivate()` is `false` for all of those. That is why N6-N11 must be
explicit prefixes.

Everything else is public, **including the documentation ranges** (`192.0.2.0/24`, `198.51.100.0/24`, `203.0.113.0/24`,
`2001:db8::/32`). The contract does not list them, and the tests use them as stand-in public addresses.

Where it applies:
- **Only to a derived caller IP**, inside `clientip.Resolver.Resolve`: the walk's result is returned only if `IsPublic` holds, and
  otherwise the request has no caller IP. `{{auto}}` then becomes `""` (§3.2 I2), and an absent client-platform IP stays absent (I4).
- **Never to a body value.** A body `user.ip_address` is stored as sent, private or not (§3.2 I6, contract decision 2 "the body wins").
  A tunnelled event's IP is a body value (§4.4).
- The geo lookup (§5.3) uses the same predicate to decide what to skip, so there is one definition of "not a real caller".

### 3.6 K8s: no forwarding header is believed, and the Service does not change

Contract decision 11 fixes the K8s configuration: `trustedProxyCidrs` is **empty** in both `values-k8s-*.yml`, and this feature
makes **no Service, `externalTrafficPolicy`, NodePort or load-balancer change**. The reasons are the contract's:
- The only way to trust a proxy on K8s without also trusting whatever else connects straight to a node port was
  `externalTrafficPolicy: Local`. The second spec review showed that it drops live ingest traffic, both during the Service switch-over
  (`enr-rev2-rollout-wave-order`) and for about 15 s whenever a node loses its last ingestor pod (`enr-rev2-local-pod-churn-blackhole`).
- Every ingest call needs an API key, and a key holder can already put any `user.ip_address` in the body. Believing a forwarding
  header gives no caller a power it lacks, so the defence against a faked IP belongs in the tunnel, which holds the key and is the only
  party that sees the browser (§4).

**What the ingestor sees on K8s, after this change** (the Service is unchanged, T14):

| Caller | `RemoteAddr` at the pod | `X-Forwarded-For` at the pod | Walk result (empty list) | Derived caller IP |
|---|---|---|---|---|
| Tunnelled browser event (web pod → private ELB → NodePort) | a node-side address (`10.200.x.x`, or a node's `172.16.x.1` gateway) | whatever the ELB passes; ignored | the peer | **none** (N3). The stored IP is the body's, written by the tunnel. |
| Direct SDK caller through the ELB, with or without `X-Forwarded-For: 6.6.6.6` | a node-side address | ignored | the peer | **none**; `{{auto}}` → `""` |
| Anything in the VPC straight to `<node>:31123`, lying in its XFF | its own `10.200.x.x`, or a node address after SNAT | ignored | the peer | **none**; `6.6.6.6` is never believed |
| The non-node ECS (T15) through the ELB or straight to the NodePort | a node-side address, or its own `10.200.x.x` | ignored | the peer | **none** |
| A pod straight to the ClusterIP Service | the pod, `172.16.x.x` | ignored | the peer | **none** |
| A peer that shows up as the ELB's own backend address | `100.125.x.x` | ignored | the peer | **none** (N6, N7) |

So on K8s the ingestor **never** derives a caller IP. Every stored IP there comes from a body: an SDK that sent one, or the tunnel.
Which exact node-side address kube-proxy puts in `RemoteAddr` does not matter, because every candidate is private or CGNAT. The listener
the ingest Ingresses share is owned outside this repository, and nothing depends on its `X-Forwarded-For` behaviour (§11.2).

When Phase 4 exposes a public endpoint (`sentinel-sentry-compat-ingest`), it configures the list for its own proxy. Public callers are
outside every trusted range, so their `X-Forwarded-For` is ignored beyond that proxy's append, and the public-caller rule still stops an
infrastructure address from being stored.

### 3.7 Behaviour, case by case (unit-tested, §10)

Two lists are used:
- **"K8s list"** is **read from `deploy/helm/sentinel/values-k8s-staging.yml`** by the test (the T26 values parser), not copied
  into it. The test also asserts that it is empty and equal to the production overlay's. Adding any range back to an overlay therefore
  turns H1 and H3 red.
- **"Test list"** is a configured, non-empty list standing in for Phase 4: `203.0.113.0/24` (a public proxy tier) and `127.0.0.0/8`.

Walk cases (`clientip.Resolver.Resolve`, which includes §3.5):

| # | Trusted list | `RemoteAddr` | `X-Forwarded-For` | Resolved |
|---|---|---|---|---|
| C1 | empty | `198.51.100.77:4411` | `6.6.6.6` | `198.51.100.77`: **with an empty list a spoofed XFF is not trusted** |
| C2 | Test list | `198.51.100.77:4411` (untrusted peer) | `6.6.6.6` | `198.51.100.77`: **a spoofed XFF from an untrusted hop is not trusted** |
| C3 | Test list | `203.0.113.10:40022` (trusted) | `198.51.100.23` | `198.51.100.23` |
| C4 | Test list | `203.0.113.10:40022` | `6.6.6.6, 198.51.100.23` | `198.51.100.23`: the untrusted hop is the caller, and the spoof to its left is never reached |
| C5 | Test list | `203.0.113.10:40022` | `203.0.113.11` | none (every hop is infrastructure) |
| C6 | Test list | `203.0.113.10:40022` | absent | none |
| C7 | Test list | `203.0.113.10:40022` | `garbage, 203.0.113.11` | none (the walk stops at the unreadable hop) |
| C8 | Test list | `203.0.113.10:40022` | `198.51.100.23:61000, 203.0.113.11` | `198.51.100.23` (port stripped) |
| C9 | Test list | `[::ffff:203.0.113.10]:40022` | `2001:db8::1, 203.0.113.11` | `2001:db8::1` (peer unmapped before the check) |
| C10 | Test list | `203.0.113.10:40022` | two header lines `198.51.100.23` and `203.0.113.11` | `198.51.100.23` (lines joined in order) |
| C11 | Test list | `203.0.113.10:40022` | 40 trusted entries | none (cap) |
| C12 | `TRUSTED_PROXY_CIDRS=10.0.0.0/33` | — | — | startup exit 1 naming the variable |
| C13 | Test list | `127.0.0.1:5000` (trusted) | `10.1.2.3` | none: the walk's result is private (N3), so it is **not** a derived caller IP |
| C14 | Test list | `127.0.0.1:5000` | `100.125.64.17` | none (N7) |
| C15 | empty | `127.0.0.1:5000` | `198.51.100.23` | none: the peer is loopback (N2) and the header is ignored. This is the integration container's shape (T23). |
| C16 | `TRUSTED_PROXY_CIDRS=::ffff:203.0.113.0/120` | — | — | startup exit 1 naming the entry and "IPv4-mapped" (§3.3) |
| C17 | `TRUSTED_PROXY_CIDRS=::ffff:203.0.113.10` | — | — | startup exit 1, as C16 |
| C18 | Test list | `[fe80::1%eth0]:40022` | `198.51.100.23` | none: the zone is dropped, and `fe80::1` is neither trusted nor public (N4) |
| C19 | Test list | `203.0.113.10:40022` | `::ffff:100.125.64.17` | none: unmapped to `100.125.64.17` (N6/N7) |

No-derive marker cases (§3.8). "Marker" means the request carries `X-Sentinel-Derive-IP: off`:

| # | Trusted list | `RemoteAddr` | `X-Forwarded-For` | Marker | Resolved |
|---|---|---|---|---|---|
| M1 | empty | `198.51.100.200:4411` (a public peer standing in for a web server the ingestor sees directly) | absent | no | `198.51.100.200`. A **direct** client-platform caller with no IP gets its connection address (I3), unless the item is tunnel-only (I3t, §3.9). This is the shape `enr-rev3-null-ip-server-address` describes. |
| M2 | empty | `198.51.100.200:4411` | absent | yes | **none**: a tunnelled item with no IP is never stored under the tunnel server's address |
| M3 | Test list | `203.0.113.10:40022` (trusted) | `198.51.100.23` | yes | **none**: the marker wins over a trusted chain, because the tunnel has already written everything it knows |
| M4 | empty | `198.51.100.200:4411` | absent | header present with any other value, or empty | **none**: a malformed marker also means "do not derive" (§3.8) |
| M5 | Test list | `203.0.113.10:40022` (the tunnel server itself trusted, the reviewer's alternative) | absent | no | none (C6): listing the server in `trustedProxyCidrs` also prevents M1, but only where an operator remembers to do it. The marker does not depend on that. |

K8s cases (the list read from the overlay):

| # | `RemoteAddr` | `X-Forwarded-For` | Resolved |
|---|---|---|---|
| H1 | `10.200.32.66:51000` (a node-subnet host straight to the NodePort, `enr-rev-nodeport-xff-spoof`) | `6.6.6.6` | none: **a direct node-port caller's XFF is ignored**, and its own address is private |
| H2 | `100.125.64.17:40022` (the ELB's backend range) | `198.51.100.23, 172.16.3.117` | none: the header is ignored and the peer is CGNAT (N6/N7) |
| H3 | `172.16.3.1:40022` (a node's container gateway after SNAT) | `6.6.6.6, 10.200.32.20` (the non-node ECS through the ELB, `enr-rev-node-subnet-not-node-only`) | none; never `6.6.6.6` |

Predicate cases (`clientip.IsPublic`):
- `false`: `0.0.0.0`, `::`, `127.0.0.1`, `::1`, `10.0.0.1`, `172.16.3.117`, `192.168.1.1`, `fd00::1`, `169.254.169.254`, `fe80::1`,
  **`fe80::1%eth0`** (zoned), `224.0.0.1`, `100.64.0.1`, `100.127.255.254`, `100.125.64.17`, `::ffff:10.0.0.1` (mapped),
  **`::ffff:100.125.64.17`** (mapped CGNAT), **`0.1.2.3`** (N8), **`240.0.0.1`** (N9), **`255.255.255.255`** (N5), **`fec0::1`** (N10),
  **`::10.0.0.1`**, **`64:ff9b::a00:1`**, **`64:ff9b:1::a00:1`** and **`2002:a00:1::1`** (N11).
- `true`: `198.51.100.23`, `203.0.113.5`, `81.2.69.142`, `6.6.6.6`, `2001:db8::1`, **`2001:db8::1%eth0`** (zone dropped),
  `100.63.255.255` / `100.128.0.0` (just outside N6), **`1.0.0.1`** (just outside N8), **`223.255.255.255`** (just below N5 and N9),
  **`2003::1`** (just outside `2002::/16`) and **`64:ff9a::1`** (just outside `64:ff9b::/96`).

### 3.8 The tunnel's no-derive marker

**Rule.** When the ingest request carries the header `X-Sentinel-Derive-IP` (`r.Header.Values` non-empty, any value, including an
empty one), `clientip.Resolver.Resolve` returns **no caller IP** for that request. It does so before it reads `RemoteAddr` or any
forwarding header, whatever the trusted list says. The sdk-js tunnel sends `x-sentinel-derive-ip: off` on **every** forward (§4.4).
The value `off` is the only one the tunnel writes. The ingestor does not parse the value, so a typo or an empty value also means "do
not derive" and never turns derivation back on (M4).

**Effect per item** (§3.2): `{{auto}}` becomes `""` (I2), and an absent IP on a client-platform event stays absent (I4). A body IP is
stored as sent (I6), and the geo lookup and the connection `User-Agent` (§6.2) work as before. The marker withholds one input, the
connection address. It changes nothing else.

**Why** (`enr-rev3-null-ip-server-address`, coordinator ruling: "a tunnel that derived no IP can never let the ingestor fill the web
server's address"). When the tunnel derives no IP it forwards a client-platform item with no IP (B4/B5, §4.4). The ingestor's peer for
that forward is the web server, or infrastructure in front of it. On K8s the peer is always private, so nothing is stored (§3.6).
On any deployment where the ingestor sees the tunnel server at a public address, I3 would store the **web server's** address as the
browser's. That covers non-K8s installs, Phase 4, and every event from an adopter whose `clientKey` returns a non-IP. TASK-06's
first criterion forbids it, and it is the outcome decision 21 cites as its reason not to write `{{auto}}`. The marker makes "the
tunnel's forward has no connection-derived IP" a property of the request, not of each deployment's `trustedProxyCidrs`.

**Rejected alternative: the reviewer's configuration rule.** The finding proposed requiring such a deployment to list the tunnel
server's address in `trustedProxyCidrs` (M5). That is still true and harmless, but it relies on every operator remembering to do it.
It fails in silence when they forget, and it cannot cover an adopter's `clientKey` that returns a non-IP. The coordinator's ruling
asks for a rule that can **never** fail that way, so the marker replaces it as the guarantee. The RUNBOOK and README do not carry the
configuration rule.

**Rejected alternative: writing an explicit `""`.** `UserInfo.IPAddress` is a Go `string` (T4), so an explicit `""` and an absent key
decode identically. Telling them apart would change I3 for every direct SDK caller that sends `""` today.

**Trust analysis.** Any caller can send the header, and the only effect is that its own request gets no connection-derived IP. That
withholds information and never adds any, so the header needs no authentication. A browser cannot remove the marker from a
tunnelled forward, because the tunnel builds its headers from scratch (§4.4). A browser can only send the marker to the tunnel,
which never forwards an inbound header except `user-agent`. The header name follows the repo's `X-`-prefixed ingest headers
(`X-API-Key`, `X-Project-Key`, T1). `docs/sdk-specification.md`'s server-side enrichment subsection documents it (§9).

**Scope of the guarantee: a tunnel at ≥ 0.2.0.** Only a 0.2.0 tunnel sends the marker. Every older tunnel (sdk-js ≤ 0.1.x, web's
pinned `v0.1.2`) does not, and this ingestor ships before any of them upgrade. §3.9 is what covers them.

### 3.9 Tunnel-only items get no connection-derived IP (the pre-marker skew)

**The skew** (`enr-rev4-pre-marker-tunnel-skew`). This ingestor deploys first. The tunnels that exist when it does, sdk-js ≤ 0.1.x
including web's pinned `v0.1.2`, forward every browser item as `platform: "javascript"` with `metadata.runtime: "browser"`, **no
`user`** (T10) and **no marker**. Under revision 4's I3, on any deployment where the ingestor sees such a tunnel's server at a public
address, every tunnelled browser event would be stored under **that server's** address and its geo. Before this change it was stored
with none. K8s is unaffected, because its peer is always private (§3.6), but the chart and the ingestor are also used elsewhere,
and "the web server's address is never stored as a browser's" must not depend on a tunnel's version.

**Options.**

| | What it does | Verdict |
|---|---|---|
| A. Document it | The CHANGELOG and RUNBOOK tell a deployment that serves a pre-0.2.0 tunnel from a public address to list that server in `TRUSTED_PROXY_CIDRS` (M5) until the tunnel upgrades. | no: a rule that fails in silence when an operator forgets. §3.8 already rejected it for the same reason. |
| **B. A tunnel-only item gets no I3 fill** | I3 skips an item whose `platform` is `"javascript"` and whose `metadata.runtime` is the JSON string `"browser"` (row I3t, §3.2). | **recommended, and specified** |

**Why B loses nothing.** Such an item can reach the ingestor **only through a tunnel**, so its connection is never the browser:
- The browser build of sdk-js has **no `apiKey` and no `endpoint` option**. It can post only to a same-origin `tunnel`
  (`packages/sdk-js/src/browser.ts:1-3`, sdk-js decision 31 / B18; the same at `packages/sdk-js/v0.1.0`).
- The ingestor serves **no CORS**. `grep -rni "cors\|Access-Control\|Origin" apps/ingestor-go` finds no handler, so a browser's
  cross-origin POST carrying `X-API-Key` fails its preflight and never arrives.

So "unless the request is direct" has no case to keep for the sdk-js browser runtime. If a future change makes direct browser ingest
possible (CORS on the ingestor **and** an `endpoint` in the browser options), that change must revisit I3t, and until it does, such an
event is stored with no IP, which is safe.

**What B keeps.** I1 (`{{auto}}`) is unchanged. Every other client-platform event keeps I3: `cocoa`, `objc`, `dart`, `flutter`,
`browser`, and `javascript` with no runtime or any other non-server runtime. The connection `User-Agent` rule (§6.2) still treats
tunnel-only items as client-platform events, because for a 0.2.0 tunnel that UA is the browser's (§4.4). With a pre-0.2.0 tunnel it is
the server's Node `fetch` UA, which parses as `Other` and fills nothing (§6.3), exactly as before this change.

**Two independent defences.** For a 0.2.0 tunnel, I3t and the marker (§3.8) both stop the fill, and each is tested alone (§4.5, the
public-peer replay). The marker still matters on its own for a tunnelled item whose runtime is **not** `browser`, which only a browser
that rewrites its own `metadata.runtime` can produce.

**Residual, recorded.** Through a **pre-0.2.0** tunnel, on a deployment where the ingestor sees that tunnel's server at a public
address, a browser that rewrites its own `metadata.runtime` to something other than `browser`, `node` or `edge` gets **its own** event
stored under the tunnel server's address and that address's geo. It mislabels only its own event, which it could already fill with
junk, and it gains no power over anyone else's. K8s is unaffected (private peer), and a 0.2.0 tunnel closes it with the marker. A
deployment that wants it closed before its tunnels upgrade lists the tunnel server in `TRUSTED_PROXY_CIDRS` (M5).
`docs/sdk-specification.md` records this in one sentence (§9).


---

## 4. The sdk-js tunnel (TASK-06)

All changes are in `packages/sdk-js/src/nextjs/route.ts` and its tests.

### 4.1 Which hop is trustworthy on web's ingress chain

`web` has exactly one proxy in front of it: the public shared K8s ELB. Its HTTPS listener inserts the connecting address into
`X-Forwarded-For` on every request. On a shared load balancer this is enabled by default for HTTP/HTTPS listeners and cannot be
disabled (T13, K8s `elb_ug_jt_0515`). K8s does not say whether an existing header is appended to or replaced. Either way the
rightmost entry is the ELB's, so the rule below does not depend on it. The listener's master Ingress belongs to another team
(`argocd/argocd-public-production`, T13). Post-deploy P5(b) (§10.3) therefore checks the result end to end, with a spoofed header sent
through web's public host. Everything left of the rightmost `X-Forwarded-For` entry was written by the browser, or by whatever the
browser went through, and is attacker-controlled.
**The trustworthy hop is the entry at `len − hops`, where `hops` is the number of trusted proxies in front of the app. On web today that
is 1, the rightmost entry.** This is exactly web's `getClientIp` (T12). The tunnel must never use the leftmost entry.

### 4.2 One derivation for the IP and the rate-limit key

A new option joins `RouteHandlerOptions`:

```ts
/** Trusted reverse proxies in front of the app (web's TRUSTED_PROXY_HOPS). Default 1; an integer ≥ 0; anything else → default, one `warn`. */
trustedProxyHops?: number;
```

`clientAddress(req, hops)` returns a **canonical IP string**, or `null`:

1. `hops === 0`: skip to step 3.
2. If `x-forwarded-for` is present, split it on `,`, take `entries[entries.length − hops]` and trim it.
   - If that is a valid IP, return its canonical form.
   - Otherwise (out of range, empty or not an IP) return `null`. It must **not** fall back to the socket, because behind a proxy the
     socket is the proxy.
3. `hops` is 0, **or** there is no header: return the socket address (`req.ip` or `req.socket.remoteAddress`, as today at
   `route.ts:211-218`) in canonical form if it is a valid IP, else `null`. With `hops` 0 an `x-forwarded-for` header is never read.
   **Under the Next.js App Router neither property exists, so step 3 returns `null` there** (`enr-rev4-hops0-no-socket-under-next`).
   A route handler receives a `Request` with no socket, and `NextRequest.ip` was removed in Next 15. Next's own server does fill the
   header from the socket, but only when the request has none: `req.headers['x-forwarded-for'] ??= originalRequest?.socket?.remoteAddress`
   (`next@15.5.21` `dist/server/base-server.js:568`, read 2026-09-24). The route's own comment already says so (`route.ts:199-201`: "a
   plain `Request` has none — under Next the header is already filled from the socket"). Step 3 yields an address only on a runtime
   that exposes the socket on the request object, for example a custom Node server that passes it through.

(Revision 3 wrote step 3 as "if there is no header", so `hops` 0 with a header present returned nothing. That contradicted §4.3's
README text and §4.5's hops-0 test, `enr-rev3-hops0-step-text`.)

"Canonical" means parsed and re-serialized, never the substring as received. Anything that fails a step is not an IP and gives
`null`:
- A string containing `:` takes the IPv6 path. Any other string takes the IPv4 path.
- IPv4: four decimal octets 0-255, with no leading zeros, whitespace or port.
- IPv6:
  1. The trimmed string must match `/^[0-9A-Fa-f:.]+$/`. This rejects a zone (`%`), brackets, `@`, `/`, `?`, a port suffix and
     whitespace **before** any URL parsing.
  2. Parse `new URL('http://[' + s + ']')`. A throw means not an IP.
  3. `hostname` must start with `[` and end with `]`. The brackets are then stripped. A hostname with no brackets means the URL parser
     read something other than an IPv6 literal, and that is not an IP.
  4. **An IPv4-mapped address is unmapped.** If the stripped, lower-cased result matches `/^::ffff:([0-9a-f]{1,4}):([0-9a-f]{1,4})$/`
     (the WHATWG serializer writes `::ffff:198.51.100.23` as `::ffff:c633:6417`), the two groups become the dotted IPv4 form, so
     `::FFFF:C633:6417` → `198.51.100.23`. That is the form the ingestor stores for a derived address (§3.2), and one address then has
     one stored text whichever family it arrived in.

  Measured in node on 2026-09-24: without step 1, `s = '::1]@6.6.6.6:80/?['` parses, and its `hostname` is `6.6.6.6`, an address the
  entry's author chose. Step 1 rejects it on `]`, `@`, `/` and `?`, and step 3 would reject it on the missing brackets
  (`enr-rev3-canonical-ipv6-url`).

This parser works on the node and the edge runtime alike, where `node:net` is not available.

**The IP the tunnel writes** (§4.4) is computed once per request, before step 4, whether or not rate limiting is on:
- If `clientKey` is given, it is `canonical(clientKey(req))`. A throw, a non-string or a non-IP is `null`. Web's `getClientIp` already
  applies web's own hop count.
- Otherwise it is `clientAddress(req, trustedProxyHops)`.

### 4.3 The rate-limit key must change too

**Yes.** `defaultClientKey` becomes `clientAddress(req, trustedProxyHops) ?? SHARED_KEY`. There are two reasons.
- TASK-06's goal says the IP is derived the same way as the rate-limit key, and the leftmost-hop derivation lets the browser choose
  the IP it is stored under.
- The leftmost key lets any browser mint a fresh bucket per request by varying the header, so the default 60/min limit is a limit only
  for clients that do not try.

`clientKey`, when given, still decides the key exactly as today (`route.ts:356-364`), so web's behaviour does not change. Tests that pin
the leftmost behaviour change with it. `bucket.test.ts` T14 is one: `", 1.2.3.4"` must now key on the rightmost entry, and its header
comment at `bucket.test.ts:3` changes. The README and CHANGELOG entries for the route's rate-limit key change in the same commit.

**What the README and CHANGELOG entries must say about `trustedProxyHops`.** The default of 1 is right only for an app with exactly
one proxy in front of it, which is web's shape (T13). An adopter whose Next app has **no** proxy in front of it, but that leaves the
default, would take the rightmost `X-Forwarded-For` entry. The client wrote that entry. The tunnel re-serializes it, so no raw text
passes, but the client still chooses the address, and the ingestor stores it, because it arrives in the body that the tunnel wrote and
the body wins (§3.2 I6, §4.4). Web is unaffected, because it passes `clientKey: getClientIp`. The published package would still ship the hazard in 0.2.0. So the 0.2.0 README
section on the route and its CHANGELOG entry both state, in these words or stronger:

> `trustedProxyHops` must equal the number of reverse proxies in front of your app that append to `X-Forwarded-For`. The default, 1,
> assumes exactly one. A value larger than your real proxy count lets a client choose the IP your events are stored under.
>
> `0` means "never read `X-Forwarded-For`; use the socket address on the request". **The Next.js App Router exposes no socket**: a
> route handler gets a plain `Request`, and `NextRequest.ip` was removed in Next 15. So under the App Router, `0` makes the tunnel
> derive **no IP for any event**, and the default rate-limit key falls back to one shared bucket for every client. That is safe (no
> event is stored under a wrong address), but it turns the feature off. Next's own server fills `X-Forwarded-For` from the socket
> only when the request carries none, and keeps a client-sent header as it is. So with **no** proxy in front of the app, no hop count
> is trustworthy: `1` would store an address the client can choose. To get a real IP there, put a proxy that appends
> `X-Forwarded-For` in front of the app and count it, or pass a `clientKey` that reads an address you trust.

(Revision 4's text said that `0` makes the tunnel "use the socket address only", which steered adopters with no proxy to a setting
that silently derives nothing under Next, `enr-rev4-hops0-no-socket-under-next`.)

The README example is checked in review, not by a test. The rule itself is tested in §4.5 (hops 0 in both request shapes, 1 and 2).

### 4.4 What the forward carries

**The IP goes in the body (contract decision 11).** A new exported helper in `route.ts`,
`applyClientIp(items: Record<string, unknown>[], ip: string | null): void`, runs on the items `normaliseBody` returned, after step 7
and immediately before step 8 serializes them. The same `ip` (§4.2) applies to every item of the request.

**`user` is rebuilt, never edited in place** (coordinator ruling on `enr-rev3-casefold-ip-key`). The ingestor decodes `user` with
`encoding/json`, which matches keys case-insensitively with Unicode folding and lets the last duplicate win (T28). Deleting or
overwriting the one exact key `ip_address` therefore leaves `IP_ADDRESS`, `Ip_Address` or `ip_addreſſ` behind, and the ingestor reads
those as the IP. So when the item's `user` is a plain object, `applyClientIp` replaces it with a **new** object:
1. `const u: Record<string, string> = {}`.
2. For each name in the allow-list **`USER_KEYS = ['id', 'username', 'email']`**, in that order: if `user` has an **own** property whose
   key is **exactly** that name (`Object.prototype.hasOwnProperty.call(user, name)`, no case folding), and its value is a `string`,
   copy it to `u[name]`. A non-string value is dropped. The allow-list is the schema-v2 `UserInfo` string fields the tunnel may pass
   through (`validator.go:114-119`: `id`, `username`, `email`). It deliberately leaves out `ip_address` and `geo`.
3. If `ip` is an address, set `u.ip_address = ip` **last**.
4. `item.user = u`.

Every other key of the sent `user` is dropped: `ip_address` and `geo` in any spelling, and anything unknown. Only the four exact names
above can appear in a forwarded `user`, so no two of its keys fold to the same field, and no key folds to `ip_address` or `geo` unless
the tunnel wrote it. The object is created by the helper and assigned only allow-listed names, so an own `__proto__` key produced by
`JSON.parse` is never copied either. The top level needs no such step: `normaliseBody` already copies only keys that are exactly in
`ITEM_KEYS` (T28), so `USER` or `uſer` beside `user` never reaches the forward.

Per item:

| # | `ip` | The item's `user` after `ITEM_KEYS` | Result |
|---|---|---|---|
| B1 | an address | absent or `null` | `user` = `{ ip_address: ip }`. This is every item today, because `ITEM_KEYS` removes `user` (T10). |
| B2 | an address | a plain object | **rebuilt**: the exact-key string values of `id`, `username`, `email`, then `ip_address: ip`. Any value the browser sent under a key that folds to `ip_address` is gone, `{{auto}}` included, and so is any `geo`. |
| B3 | an address | present but not a plain object (string, number, boolean, array) | left as it is, and no IP is written. The route checks shape only, never item content (`route.ts:262-268`), and the ingestor's batch decode already rejects a non-object `user` with `400 Invalid JSON payload array` for the whole request (`apps/ingestor-go/main.go:475-481`, `validator.go:94` `User *UserInfo`), so the browser's own batch fails and nothing is stored under a chosen IP. |
| B4 | `null` | a plain object | **rebuilt** with the allow-listed keys only, and no `ip_address`. A browser-chosen IP is not evidence, so it must not survive just because no IP could be derived, whatever its spelling. The result may be `{}`. |
| B5 | `null` | absent or `null` | unchanged, and no `user` is created |

**A browser-sent `geo` never survives** (`enr-rev3-browser-geo-survives`). B2 and B4 drop it in every spelling. `user.geo` therefore
reaches the ingestor as null for every tunnelled item, and the only geo a tunnelled event can carry is the ingestor's lookup of the
tunnel-written IP (§5.3). Decision 11's reason ("a browser-chosen IP is not evidence") applies equally to a geo claimed for that IP.

B2 and B4 are unreachable through the route today, because `ITEM_KEYS` removes `user`. They become live when
`sentinel-sdk-js-rich-context` widens `ITEM_KEYS`, and that is why they are specified and unit-tested on the helper now (§4.5). When
that card widens `ITEM_KEYS`, it must keep `applyClientIp` after the filter and must not replace the exact-key top-level filter with a
case-insensitive one (§12).

**Defence in depth in the ingestor: none, deliberately.** Two options were considered, and both were rejected:
- **Decode `user` with exact key matching.** Today a direct SDK call that sends `IP_ADDRESS` or `Ip_Address` stores that value.
  Exact matching would silently stop storing it. That breaks the contract's binding compatibility rule ("events that already carry
  `user.ip_address` … are stored exactly as today").
- **Reject a `user` with duplicate fold-equal keys.** This turns requests accepted today into `400 Invalid JSON payload array`, for
  the whole batch.

Neither option removes a power from anyone. Under contract decision 11 any API-key holder can already put any IP in a body with the
exact key. The only writer that must not be tricked is the tunnel, and after the rebuild its output cannot contain fold-equal
duplicates by construction. §4.5's Go decode test proves that on the tunnel's real output. A future tunnel regression turns that test
red, which is where the defence belongs.

Why `null` does not become `{{auto}}`: `{{auto}}` asks the ingestor to use its own peer. On K8s that peer is infrastructure, so it
would be stored as `""` anyway (§3.6). On a deployment whose ingestor can see the tunnel's server directly, it would store the **web
server's** address as the browser's, which TASK-06's first criterion forbids. Writing nothing is not enough on its own either, because
of I3 on such a deployment. So every forward also carries the no-derive marker (§3.8), and a tunnelled item with no IP is stored with
none on every deployment. Tunnels too old to send the marker are covered by I3t (§3.9).

The tunnel does not apply the public-caller rule (§3.5) to what it writes. That rule is the ingestor's, for derived IPs, and the body
wins over it (§3.2 I6). On web the written IP is the address the public ELB appended, which is the browser's public address (T13). A
private address reaches the body only when the app's own derivation produced one, i.e. an intranet deployment, where it is the right
answer, or a wrong `trustedProxyHops`, which §4.3's README rule covers.

**Size.** Each item grows by at most `,"user":{"ip_address":"<≤39 chars>"}`, under 60 bytes. At the default 50 items that is under
3 KB on a body capped at 256 KiB on the way in (`DEFAULT_MAX_BODY_BYTES`), far below the ingestor's 5 MiB batch cap
(`apps/ingestor-go/main.go:42`).

**Headers.** Step 8 (`route.ts:427-441`) builds its headers **from scratch**, exactly as now, and adds **two** headers:

| Header | When | Value |
|---|---|---|
| `user-agent` | the request's `user-agent` is present, 1-1024 bytes, and contains no control character | the request's `user-agent`, unchanged |
| `x-sentinel-derive-ip` | **always**, on every forward, whether or not an IP was derived | the constant `off` (§3.8). Never copied from the inbound request. |

**No `x-forwarded-for` is forwarded, derived or not.** The IP's only channel is the body. A second channel would be ignored on K8s
(§3.6), and on a future deployment with a configured list it would make the tunnel's server a trusted proxy for a header that says the
same thing as the body, which is one more thing to keep consistent and nothing gained. `x-real-ip`, `forwarded`, `x-client-ip`,
`cookie`, `authorization`, `traceparent`, `host` and `origin` stay unforwarded. The top-of-file comment (`route.ts:8-16`) changes step 8
from "one forward, no client header" to "one forward: the browser's User-Agent is the only client header; the derived IP is written
into each item's `user.ip_address`, `user` is rebuilt from an exact-key allow-list, and `x-sentinel-derive-ip: off` tells the
ingestor never to derive an IP from this connection".

Why forward the UA (§1.3 row 2): the ingestor's second UA source is "the ingest request's own User-Agent, client platforms only"
(contract §4.3). Tunnelled items are `platform: "javascript"` with `metadata.runtime: "browser"` (T10), so they are client-platform
events. Without this header that UA is the web server's Node `fetch` UA. It parses as `Other` and fills nothing (§6.3), and the
contract's hypothesis cannot be met. The UA is labelled data, not a trust decision: the browser asserts it about itself exactly as it
does to Sentry, and its only consumer is a bounded parse (contract decision 11 keeps it).

### 4.5 Tests (sdk-js vitest) and the fixture bridge

In `packages/sdk-js/tests/route/`:
- **Derivation, observed in the forwarded body.** An inbound `x-forwarded-for: 6.6.6.6, 198.51.100.23` produces a forward whose
  every item has `user.ip_address: "198.51.100.23"`, never `6.6.6.6` and never the inbound string.
  - With `trustedProxyHops: 2`, `6.6.6.6, 198.51.100.23, 10.0.0.1` gives `198.51.100.23`.
  - `, 1.2.3.4` gives `1.2.3.4`.
  - An out-of-range hop count gives no `user` on any item.
  - `x-forwarded-for: 198.51.100.23 ` (trailing space) gives `198.51.100.23`. `::FFFF:C633:6417` and `::ffff:198.51.100.23` give
    `198.51.100.23` (unmapped, §4.2). `2001:DB8::1` gives `2001:db8::1`.
  - Non-IP hops give no `user`: `evil.example`, `fe80::1%eth0` (zone), `[2001:db8::1]`, `2001:db8::1]:80`, and
    **`::1]@6.6.6.6:80/?[`**, which must never yield `6.6.6.6` (`enr-rev3-canonical-ipv6-url`).
  - With `trustedProxyHops: 0`, a **plain `Request`** (no `ip`, no `socket`: the Next App Router's shape) with an inbound
    `x-forwarded-for: 6.6.6.6` gives **no `user` on any item**, and the bucket key is the shared one: the header is ignored and there
    is no socket to read (§4.2 step 3, §4.3's README text). This is the case the README describes.
  - With `trustedProxyHops: 0`, a request object that also carries `socket.remoteAddress = '203.0.113.9'` (a runtime that exposes
    the socket) and the same header gives `203.0.113.9`. This proves the header is ignored even when a socket exists, and it is
    named in the test as the non-Next path.
  - A three-item batch: all three items carry the same derived IP.
- **Rebuild, on the helper** (`applyClientIp`, B1-B5, §4.4). Each case asserts **deep equality** of the whole item, so an extra key
  in any spelling is red. With `ip = "198.51.100.23"`:
  - `{user: {ip_address: "6.6.6.6", id: "u1"}}` → `{user: {id: "u1", ip_address: "198.51.100.23"}}`, so `id` is proven carried.
  - `{user: {ip_address: "{{auto}}"}}` → `{user: {ip_address: "198.51.100.23"}}`.
  - **`{user: {ip_address: "6.6.6.6", IP_ADDRESS: "6.6.6.6"}}`** and the reviewer's shape **`{user: {ip_address: "", IP_ADDRESS:
    "6.6.6.6"}}`** → `{user: {ip_address: "198.51.100.23"}}`.
  - **`{user: {Ip_Address: "6.6.6.6"}}`** and **`{user: {"ip_addreſſ": "6.6.6.6"}}`** (U+017F) → `{user: {ip_address: "198.51.100.23"}}`.
  - **`{user: {id: "u1", geo: {country_code: "XX", city: "Fake"}}}`** and **`{user: {id: "u1", GEO: {country_code: "XX"}}}`** →
    `{user: {id: "u1", ip_address: "198.51.100.23"}}`, so a browser-sent `geo` is dropped (`enr-rev3-browser-geo-survives`).
  - `{user: {id: "u1", ID: "u2", Username: "n", email: 5, extra: "x"}}` → `{user: {id: "u1", ip_address: "198.51.100.23"}}`: only
    exact names and string values are copied.
  - No `user`, and `user: null`, each → `{user: {ip_address: "198.51.100.23"}}`. `user: "x"` is left as `"x"` (B3).
  - The serialized item's `user` keys are in the order `id`, `username`, `email`, `ip_address`, which makes the fixtures deterministic.

  With `ip = null`:
  - `{user: {ip_address: "6.6.6.6", id: "u1"}}` → `{user: {id: "u1"}}`.
  - `{user: {Ip_Address: "6.6.6.6"}}`, `{user: {"ip_addreſſ": "6.6.6.6"}}` and `{user: {IP_ADDRESS: "6.6.6.6", GEO: {}}}` each →
    `{user: {}}` (B4).
  - An item with no `user`, or `user: null`, is unchanged (B5).
- **Overwrite, through the route.** An inbound item carrying `user: {ip_address: "6.6.6.6"}` with `x-forwarded-for: 198.51.100.23`
  forwards `user: {ip_address: "198.51.100.23"}`. With no derivable IP it forwards no `user` at all. An inbound item carrying both `user`
  and **`USER: {ip_address: "6.6.6.6"}`** forwards neither `USER` nor any other top-level key outside `ITEM_KEYS` (T28's exact filter).
- **clientKey:** a `clientKey` returning `10.0.0.7` writes `10.0.0.7`. One returning `'bucket-a'` (not an IP) writes no IP but still
  keys the bucket on `'bucket-a'`. A throwing one writes no IP.
- **Header set:** `route.test.ts:571-590` is rewritten. Every forward's header names ⊆ {`content-type`, `x-api-key`, `x-project-key`,
  `user-agent`, `x-sentinel-derive-ip`}. **`x-forwarded-for` is never present**, including when the inbound request carries one.
  `user-agent` equals the inbound value. A 1025-byte UA is not forwarded. **`x-sentinel-derive-ip` is present on every forward with the
  value `off`**, both when an IP was derived and when none was, and an inbound `x-sentinel-derive-ip: on` does not change it.
- **Rate limit:** T14 is updated as described in §4.3. A new case: 61 requests whose leftmost hop varies and whose rightmost hop is
  fixed share one bucket, so the 61st is a 429.
- **Fixtures:** a new `packages/sdk-js/tests/tunnel-fixtures.test.ts` writes `packages/sdk-js/tests/tunnel-fixtures/*.json`. It is a
  **sibling** directory, not `tests/fixtures/`, whose `*.json` glob is the golden fingerprint corpus (T24).
  - It writes deterministically and only when content changes, the `fixtures.test.ts` convention (`fixtures.test.ts:45-46`),
    **with one difference: when `process.env.CI` is set it never writes.** It compares the freshly captured content with the
    committed file and fails the test on any difference, including a missing file, naming the file. **This CI-mode test is the red
    signal for drift in CI**: a route change that alters a forward turns the suite red there instead of silently rewriting the fixture
    the Go tests read. Locally (no `CI`) it rewrites as before, and the gate's `git` check below catches the rewrite.
  - The unit gate runs this suite (§10.1 G2, contract decision 9) from the **repository root** and then fails on any drift in the
    directory: `git diff --exit-code -- packages/sdk-js/tests/tunnel-fixtures` **and**
    `test -z "$(git ls-files --others --exclude-standard -- packages/sdk-js/tests/tunnel-fixtures)"`. The second command is needed
    because `git diff` does not see a new, untracked fixture file.
  - CI's `sdk-js-node` job gains the same two checks after its `pnpm test` step, under the same push-only guard. **That job runs with
    `defaults.run.working-directory: packages/sdk-js`** (`ci.yml:351-353`), and a git pathspec resolves against the current directory,
    so there the pathspec is **`tests/tunnel-fixtures`**: `git diff --exit-code -- tests/tunnel-fixtures` and
    `test -z "$(git ls-files --others --exclude-standard -- tests/tunnel-fixtures)"`. Revision 2's `packages/sdk-js/tests/tunnel-fixtures`
    would have resolved to `packages/sdk-js/packages/sdk-js/…` and matched nothing, so both checks would pass without checking anything
    (`enr-rev2-ci-drift-pathspec`; the same trap `ci.yml`'s consumer-path comment records for `git archive`). The step carries a
    comment saying so. Under `CI` the fixture test never writes, so these two CI checks are a backstop; the CI-mode test above is the
    primary signal.
  - Each file holds `{ name, inbound: {headers, body}, forwarded: {path, headers, body}, expect: {client_ip, user_agent} }`, captured
    from the real handler with a stub upstream. `inbound.body` is produced by a **browser-runtime** sdk-js client, with a `user` object
    added to the item where the case needs one. `expect.client_ip` is `""` when no IP may be stored.
  - At least five route fixtures. Every one has `forwarded.headers["x-sentinel-derive-ip"] == "off"`:

| File | Inbound | Expected forward (every item) | `expect.client_ip` |
|---|---|---|---|
| `tunnel-clean.json` | XFF `198.51.100.23`, a Chrome/Windows UA, no `user` | `user: {ip_address: "198.51.100.23"}`, `user-agent` forwarded | `198.51.100.23` |
| `tunnel-spoofed-xff.json` | XFF `6.6.6.6, 198.51.100.23` | `user: {ip_address: "198.51.100.23"}` | `198.51.100.23` |
| `tunnel-auto-ip.json` | XFF `198.51.100.23`; item `user: {ip_address: "{{auto}}"}` | `user: {ip_address: "198.51.100.23"}` | `198.51.100.23` |
| `tunnel-browser-sent-ip.json` | XFF `198.51.100.23`; item `user: {ip_address: "6.6.6.6"}` | `user: {ip_address: "198.51.100.23"}` | `198.51.100.23` |
| `tunnel-no-ip.json` | no XFF and no socket; item `user: {ip_address: "6.6.6.6"}` | no `user` | `""` |

  - **One helper fixture, `tunnel-user-rebuild.json`** (`enr-rev3-casefold-ip-key`). Through the route today, `ITEM_KEYS` removes
    `user` before `applyClientIp` runs (T10), so no route fixture can carry a browser-sent `user` to the Go side. This file carries
    the **helper's** real output instead. Its shape is `{ name, cases: [{ name, ip, input, output }] }`. `input` is an item array,
    and `output` is the **string** `JSON.stringify(items)` after the exported `applyClientIp(items, ip)` has run, byte for byte what
    step 8 would post. The cases:

| Case | `ip` | `input` item's `user` |
|---|---|---|
| `dup-exact-and-upper` | `81.2.69.142` | `{ip_address: "", IP_ADDRESS: "6.6.6.6"}` |
| `dup-both-set` | `81.2.69.142` | `{ip_address: "6.6.6.6", IP_ADDRESS: "6.6.6.6"}` |
| `mixed-case` | `81.2.69.142` | `{Ip_Address: "6.6.6.6"}` |
| `long-s` | `81.2.69.142` | `{"ip_addreſſ": "6.6.6.6"}` |
| `browser-geo` | `81.2.69.142` | `{id: "u1", geo: {country_code: "XX", city: "Fake"}, GEO: {country_code: "XY", city: "Fake2"}}` |
| `no-ip-folded` | `null` | `{Ip_Address: "6.6.6.6", GEO: {country_code: "XX"}}` |

  `81.2.69.142` is the fixture database's London address (§5.6), so geo from the ingestor's own lookup is distinguishable from the
  browser's `XX` / `Fake`.

The Go side consumes them twice:
- **`tests/unit/sdk_js_tunnel_fixture_test.go` (G2).**
  - **K8s replay.** It replays each route fixture's `forwarded` request through the ingestor's real resolver and enrichment, with
    K8s's shape: the list read from the staging overlay (empty, §3.7), `RemoteAddr = 10.200.32.66:51000` (a node-side peer, T14),
    and an ELB-style appended header `X-Forwarded-For: 6.6.6.6, 172.16.3.117` that must be ignored. It checks the resulting
    `user.ip_address` against `expect.client_ip`, and `contexts.browser` / `contexts.os` for `tunnel-clean`.
  - **Public-peer replay** (`enr-rev3-null-ip-server-address`). It replays `tunnel-no-ip`'s forward again with an empty list and
    `RemoteAddr = 198.51.100.200:4411`, a public peer standing in for a web server the ingestor sees directly. The resulting
    `user.ip_address` must be empty, never `198.51.100.200`. Since revision 5 two defences stop the fill, the marker (§3.8) and I3t
    (§3.9), so the replay runs in four variants, which test each defence alone and prove the case is not vacuous:

    | Variant | Marker header | Items' `metadata.runtime` | Stored |
    |---|---|---|---|
    | as forwarded | present | `browser` | none |
    | marker removed (a pre-0.2.0 tunnel's shape) | absent | `browser` | **none** (I3t alone) |
    | runtime removed from every item | present | absent | **none** (marker alone) |
    | both removed | absent | absent | `198.51.100.200` (M1): the fill really runs when neither defence applies |
  - **Fixture shape.** On the fixture itself it asserts that `forwarded.headers` has no `x-forwarded-for` and has
    `x-sentinel-derive-ip: off`, that every forwarded item's `user.ip_address` equals `expect.client_ip` (or that `user` is absent
    when it is `""`), and that no forwarded item's IP equals the inbound item's when the two differ.
  - **Decode of the tunnel's output with `encoding/json`** (the coordinator's ruling). For each case in `tunnel-user-rebuild.json`,
    `json.Unmarshal([]byte(output), &items)` into `[]validation.ErrorPayload`, the same type and decoder as the ingestor's batch
    handler (`main.go:475-476`). Then the items run through `enrich.Apply` with the K8s replay's `Info` and marker, and the fixture
    geo database under an injected `Now` (§5.4). Every item must have `User.IPAddress == ip` (`""` for `no-ip-folded`), never
    `6.6.6.6`. `User.Geo` must be the lookup's `{country_code: "GB", city: "London", …}` for the `81.2.69.142` cases, never `XX`, `XY`
    or `Fake`, and null for `no-ip-folded`. Before decoding, it also asserts on the raw `output` that every `user` object's keys are
    a subset of `{id, username, email, ip_address}` and that no two keys of one object are equal under `strings.EqualFold`.
- **`tests/integration/enrichment_test.go` (G3), the tunnel's output through the real ingestor.** For each route fixture it POSTs
  `forwarded.body` to the container's `forwarded.path` (`/batch`) with `forwarded.headers`, the API key replaced by the test project's.
  The container runs with `TRUSTED_PROXY_CIDRS` unset, as on K8s. The stored `error_occurrences.event_user->>'ip_address'` must
  equal `expect.client_ip`: the browser's IP for `tunnel-clean`, `tunnel-auto-ip` and `tunnel-browser-sent-ip` (never `6.6.6.6`), and
  none for `tunnel-no-ip`, i.e. never the tunnel server's own address. (Its peer here is `127.0.0.1`, T23, which §3.5 excludes
  anyway, so the public-peer replay above is the proof that the marker works.) `tunnel-clean`'s stored `contexts` must carry the
  browser and OS (§10.2 TASK-04). For each `tunnel-user-rebuild.json` case, it POSTs `output` to `/batch` with `tunnel-clean.json`'s
  `forwarded.headers`, against the geo-configured container (§10.2 TASK-03 wiring row). The stored `ip_address` is the case's `ip`
  (none for `no-ip-folded`). The stored `event_user->'geo'` is the lookup's `GB` / `London`, or null, and its `country_code` is never
  `XX` or `XY`.

---

## 5. Geo lookup: ingestor (TASK-03)

### 5.1 Library

**`github.com/oschwald/maxminddb-golang/v2`** (ISC licence; v2.6.0 at the time of writing).
- `OpenBytes([]byte)` opens a database already in memory.
- `(*Reader).Lookup(netip.Addr) Result` and `Result.Found()` / `Result.Decode(&rec)` do the lookup.
- `Reader.Verify()` checks integrity, and `Metadata.{DatabaseType, BuildEpoch}` describes the file.
- Lookups are safe for concurrent use. `Close` must not run concurrently with a lookup (package docs).

Only the four fields used are decoded, through a local struct with `maxminddb` tags (`city.names.en`, `country.iso_code`,
`country.names.en`, `subdivisions[].names.en`). The v1 `geoip2` package is not needed.

### 5.2 What is filled

The mapping is Sentry's, from Relay `relay-event-normalization/src/geo.rs`, `GeoIpLookup::try_lookup`:

| `user.geo` field | GeoLite2-City record | Bound (T5) |
|---|---|---|
| `country_code` | `country.iso_code` | ≤ 8, dropped if over (never happens: ISO codes are 2) |
| `city` | `city.names.en` | ≤ 128, truncated |
| `subdivision` | `subdivisions[0].names.en` | ≤ 128, truncated |
| `region` | `country.names.en` (Sentry puts the **country name** here) | ≤ 128, truncated |

Absent record fields stay `""`. When all four are empty, `user.geo` stays null. `docs/sdk-specification.md`'s example
(`"region": "Berlin", "subdivision": "BE"`) shows an SDK-sent value, and §9 adds a sentence saying what the server writes.

### 5.3 When

After §3.2, per item:

- `user.geo` is null (not sent): the lookup may run. **A `geo` object the SDK sent, including `{}`, is never touched.** A tunnelled
  event never carries one, because the tunnel drops a browser-sent `geo` in every spelling (§4.4). Its geo is always this lookup's,
  of the tunnel-written IP.
- `user.ip_address` must parse with `netip.ParseAddr` exactly as stored, with no trimming. A sent address that does not parse gets no
  geo.
- The address is unmapped, then **skipped** without a lookup when `clientip.IsPublic` is false (§3.5: unspecified, loopback,
  private, link-local, multicast, carrier-grade NAT, shared ELB). One predicate serves both, so "not a real caller" has one definition.
- A lookup with `!Found()`, a decode error or no current database gives no geo.

The lookup applies to IPs the SDK sent as well as derived ones (contract §4.2, "when `user.ip_address` is known").

### 5.4 The reloadable reader

Package `apps/ingestor-go/geoip`, type `Loader`:

- **State** is an `atomic.Pointer[db]`, where `db` holds `{reader, etag, buildTime}`.
  - Lookups load the pointer and never block.
  - A replaced reader is **never `Close`d**, only dropped. It was opened with `OpenBytes`, so there is no mapping to release, and
    closing it could race an in-flight lookup (§5.1). The garbage collector frees it.
- **Start:** `Loader.Start(ctx)` returns immediately, and one goroutine does the first fetch and then polls. Readiness and `/health`
  never wait for it. Until the first successful load, lookups return nothing.
- **Poll:** every `GEOIP_POLL_INTERVAL` (default `1h`; any Go duration ≥ `1m`, otherwise the default plus one Warn line).
  1. `StatObject(bucket, prefix+"GeoLite2-City.mmdb")`.
  2. If the ETag equals the loaded one, only the staleness check (below) runs.
  3. Otherwise `GetObject`, read at most 256 MiB (over that is `corrupt`), then `OpenBytes` and `Verify()`. `Metadata.DatabaseType`
     must end in `-City`.
  4. Then the staleness check, then the pointer swap.
- **Stale:** the loaded file is stale when `BuildTime()` is older than `GEOIP_MAX_AGE` (default `720h`). A stale database answers
  nothing, so the contract's "stale means no geo" (§4.2) holds. 30 days is MaxMind's own rule: the GeoLite EULA requires licensees to
  "cease use of and destroy any old versions … within thirty (30) days following the release of the updated GeoLite Databases".
  Staleness is re-evaluated on every poll tick, so a database that ages past the limit while running stops answering within one poll
  interval.
- **Injectable clock and age.** `geoip.Config` carries `MaxAge time.Duration` (from `GEOIP_MAX_AGE`, any Go duration ≥ `1h`, else the
  default plus one Warn line) and `Now func() time.Time` (nil means `time.Now`; not reachable from env). Staleness is
  `cfg.Now().Sub(buildTime) > cfg.MaxAge`. Tests need both: MaxMind's test database is built on 2026-02-04 (§5.6), so under the
  default 720 h it is already stale today and would answer nothing. Unit tests inject `Now`; the integration container sets
  `GEOIP_MAX_AGE=876000h` (100 years) through `StartIngestorWithEnv` (§10.2).
- **Never a failure.** Any of these means no geo, a pod that stays ready, and events that are still accepted:
  - no configuration (`GEOIP_S3_BUCKET` empty),
  - incomplete configuration (bucket set but endpoint or credentials empty, or a bad `GEOIP_S3_FORCE_PATH_STYLE`),
  - object not found, an S3 error, a truncated body, `Verify` failure, the wrong database type, or stale.

  This is the opposite of `TRUSTED_PROXY_CIDRS` (§3.3) on purpose. A geo misconfiguration costs a feature, while a trust misconfiguration
  costs correctness.
- **A newer upload replaces the database without a restart**, within one poll interval. A rollout also loads the current file at start.
  Pods never contact MaxMind: the `geoip` package the ingestor imports for serving has no MaxMind client in it (§8.2 keeps the
  downloader in a separate package).

### 5.5 Log once per state change

The loader has one state: `disabled` | `loading` | `ready` | `unavailable` | `stale`. `unavailable` carries a fixed reason:
`config`, `not_found`, `fetch_error`, `corrupt` or `wrong_type`.

- One log line is written **only when (state, reason) changes**, with one addition: a successful swap from one `ready` database to a
  different object (new ETag) writes one Info line with event key `geoip.reloaded` and `build_time`, `database_type` and `etag`. The
  operator check in §10.3 P4 reads the newest `build_time` from these lines, which it could not do if a reload were silent. It uses the event key `obs.LogKeyEvent="geoip.state"` with attributes
  `from`, `to`, `reason`, `build_time` (for `ready` and `stale`) and `object` (bucket/key, not secret).
- Levels: Info for `disabled`, `loading` and `ready`; Warn for `unavailable` and `stale`.
- A poll that fails the same way as the last one writes nothing. Recovery writes one `ready` line.
- The error text of an S3 failure goes on the transition line as `error`. minio-go errors carry no credentials.
- **Nothing is logged per event.** The logger is injected (`NewLoader(cfg, logger)`), so tests capture it.

### 5.6 Test fixtures and licence

Two databases are committed, both from `github.com/maxmind/MaxMind-DB` `test-data/` at commit
**`0eef25a46e20f4e96d27b951d0228efabe21323f`** (2026-09-22). Their metadata was read with `maxminddb-golang` v2.6.0 on 2026-09-24:

| File | `DatabaseType` | `BuildEpoch` | Size | `81.2.69.142` |
|---|---|---|---|---|
| `tests/fixtures/geoip/GeoLite2-City-Test.mmdb` | `GeoLite2-City` | 2026-02-04 22:49:29 UTC | 21088 B | city `London`, country `GB` / `United Kingdom`, subdivision `England` |
| `tests/fixtures/geoip/GeoIP2-City-Test.mmdb` | `GeoIP2-City` | 2026-02-04 22:49:29 UTC | 22569 B | the same |

- The **first** file is the one every lookup test uses. The **second** exists only to prove a reload (§10.2 TASK-03). It passes the
  loader's `-City` suffix rule (§5.4), and it is a different object with a different `DatabaseType`. Both files have the **same build
  epoch**, so a reload test asserts the swap through `DatabaseType` and the `geoip.reloaded` line's `etag` / `database_type`, never
  through `build_time`.
- Both are older than the default `GEOIP_MAX_AGE` (720 h) today. Every test that expects an answer therefore injects `Now` or sets
  `GEOIP_MAX_AGE` (§5.4). The staleness test sets `Now = BuildEpoch + 721h` (stale) and `BuildEpoch + 719h` (ready).
- Beside them, commit:
  - `tests/fixtures/geoip/README.md`, naming the source URL, the commit SHA, the source JSONs (`source-data/GeoLite2-City-Test.json`,
    `source-data/GeoIP2-City-Test.json`), the two build epochs above, and the addresses the tests use;
  - `LICENSE-APACHE` and `LICENSE-MIT` copied from that commit (both exist at its root). The repository README states: "licensed under
    the Apache License, Version 2.0 or the MIT License, at your option".

  The files are about 21 KB and 22 KB. Nothing is downloaded at test time.
- Tests pick addresses present in the source JSON at the pinned commit, and assert exactly the values that JSON states for them.
  `81.2.69.142` (`81.2.69.142/31` in `source-data/GeoLite2-City-Test.json`) gives the row above. A test also asserts on
  `Metadata.DatabaseType == "GeoLite2-City"`.
- A **corrupt** fixture is produced inside the test (for example the valid file truncated to half its length). No broken binary is
  committed.
- **The over-long city is not an mmdb.** Neither test database has a 200-rune city, and none is built. The enrich-before-bounds case
  (§10.2 compatibility row) uses a stub lookup instead: `enrich` reaches geo through a one-method interface,
  `type GeoLookup interface { Lookup(netip.Addr) (validation.GeoInfo, bool) }`, which `*geoip.Loader` implements, and the test passes a
  stub that returns a 200-rune `City` for any address.

---

## 6. User-Agent → contexts: ingestor (TASK-04)

### 6.1 Library

**`github.com/ua-parser/uap-go/uaparser`** (Apache-2.0 / MIT). It carries the `uap-core` regexes, the same family database Sentry's
`relay_ua` uses, so family names match Sentry's (`Chrome`, `Mobile Safari`, `Windows`, `Mac OS X`, `iOS`, `Android`).
- It is built once at startup with `uaparser.New(uaparser.WithCacheSize(1024))` over the embedded definitions (`NewFromSaved`
  semantics), and shared.
- The module has no tagged release, so it is pinned by pseudo-version in `go.mod`.

### 6.2 Which User-Agent

1. `request.headers[k]` from the body, where `k` is the first key in lexical order with `strings.EqualFold(k, "User-Agent")`. This
   applies to **any** platform, because a server SDK's `request` describes its end user's HTTP request (Sentry reads it the same way).
2. Only when (1) gives nothing, and **only for a client-platform event** (§3.1): the ingest request's own `User-Agent` header, captured
   per request with the IP (§7.2). A server SDK's HTTP client UA is therefore never parsed as a browser. For a tunnelled browser event
   this is the browser's UA (§4.4).

A UA that is empty or longer than 1024 bytes is not parsed. The regex cost grows with length, and no real UA is that long.

### 6.3 What is filled

- `ua := parser.ParseUserAgent(s)` and `os := parser.ParseOs(s)`.
- A family of `"Other"` means "unknown" and fills nothing for that context (Relay `is_known`).
- Version: `major`, then `.minor` if present, then `.patch` if minor is present (Relay `get_version`). No major means no version key.
  For the OS only, Relay's frozen-version rule applies: `Windows` version `10` becomes `>=10`, and `Mac OS X` `10.15.7` becomes
  `>=10.15.7`.

| Context | Written when | Keys written |
|---|---|---|
| `contexts.browser` | `contexts["browser"]` is absent and the UA family is known | `name`, and `version` if it has one |
| `contexts.os` (client-platform event) or `contexts.client_os` (any other event) | that key is absent and the OS family is known | `name`, and `version` if it has one |

No `type` key is written, because the contract fixes `{name, version}`. **A context the SDK sent is never touched**, including one with
a missing `name`, **and neither is any key inside it**. The only operation is inserting an absent top-level key.

**Fit rule.** A derived context is inserted only if the `contexts` object with it still fits `MaxContexts` (32) and `MaxContextsSize`
(65536 bytes), measured the way `boundContexts` measures it. Otherwise it is skipped. Without this, adding a derived context could make
the bounds step evict a context the SDK sent. A new exported helper
`validation.ContextFits(p *ErrorPayload, name string, v map[string]interface{}) bool` reuses `objectSize` / `entrySize` (`bounds.go:299`,
`:734`), so the two measurements cannot drift.

---

## 7. Ordering and compatibility

### 7.1 Where enrichment runs

`IngestAt` becomes:

`resolveEventID` → `resolveTimestamp` → `ReceivedAt` stamp → **`enrich.Apply`** → `ValidatePayload` (bounds) →
`MapPayloadToEvent` → protovalidate → publish.

- Enrichment runs **before** the bounds (contract §4 Compatibility), so a derived value is truncated, dropped and marked
  (`server_truncated`: `user.geo`, `contexts`) exactly as a sent one would be.
- Enrichment runs after `applyAuthenticatedScope`, so a scope-refused batch item costs no lookup.
- `enrich.Apply` does IP (§3.2), then geo (§5.3), then UA (§6), in that order, because geo reads the IP that IP enrichment produced.

### 7.2 How the per-request data reaches `IngestAt`

- Package `apps/ingestor-go/clientip` owns `Resolver{trusted []netip.Prefix}`, `Resolve(*http.Request) (netip.Addr, bool)` and a typed
  context accessor pair: `WithRequestInfo(ctx, Info{ClientIP netip.Addr; HasClientIP bool; UserAgent string})` and
  `RequestInfoFromContext`.
  - `Resolve` returns `(addr, true)` only when the walk's result passes `IsPublic` (§3.5), so `HasClientIP` is never true for an
    infrastructure address, and nothing downstream has to re-check it.
  - `Resolve` returns `(netip.Addr{}, false)` first thing when the request carries `X-Sentinel-Derive-IP` (§3.8). `UserAgent` is
    still captured.
  - It uses a private key type, never a string literal (CLAUDE.md working convention; R1 in `VERIFIED_STATE.md`).
- `main.go` wraps both ingest handlers (`main.go:216-235`) in `clientip.Middleware(resolver)`, **inside** `observeMiddleware`, so the
  data exists before the handler runs. Auth, rate limit and handler order are unchanged.
- `IngestAt`'s signature does **not** change, because `tests/unit` is one flat package (B4). A context with no `Info` (every existing
  unit test, and `Ingest`) means "no caller IP, no connection UA": the body rules still apply and connection-derived fills do not.
- The geo loader and the UA parser reach `IngestService` through a variadic option, `NewIngestService(publisher, opts ...Option)` with
  `service.WithEnricher(e)`. Existing callers compile unchanged.

### 7.3 Compatibility guarantees and how each is held

| Guarantee | Held by |
|---|---|
| An event already carrying `user.ip_address` (≠ `{{auto}}`), `user.geo`, `contexts.browser`, `contexts.os` or `contexts.client_os` is stored byte for byte in those fields. | §3.2 I6, §5.3, §6.3. Test: a payload with every field set is JSON-equal before and after `enrich.Apply`. |
| Fingerprints do not change. | No enriched field is a fingerprint input (T24). `fingerprint_golden_test.go` stays green unchanged. A new case runs each golden fixture through `enrich.Apply` with a full `Info` (public IP `81.2.69.142`, Chrome UA) and the fixture geo DB under an injected `Now` (§5.4), asserts that geo **was** filled (so the case is not vacuous), and asserts the same fingerprint. |
| No network I/O on the request path. | Lookup and parse are in-process. S3 runs only in the loader goroutine. Test: the loader runs over a **counting fake object source** (the `geoip` package's `ObjectSource` seam: `Stat`, `Get`); after the first load completes the test snapshots the counters, calls `IngestAt` 100 times with public IPs, and asserts both counters are unchanged, i.e. **zero** S3 calls from the request path. A 5 s context deadline on the loop is a hang guard only, never the assertion. No wall-clock bound is asserted (it would be flaky under `-race` on the shared runners and would not prove the absence of I/O). |
| No enrichment on the processor or the dashboard. | Nothing under `apps/processor-go` or `apps/dashboard-web` changes. |

---

## 8. Chart and refresh (TASK-05)

### 8.1 The shape of the refresh job: options and recommendation

| Option | What it costs | Verdict |
|---|---|---|
| A. A new image (`tools/geoip-refresh` + Dockerfile) | Three new matrix rows (`ci.yml` build-images, both release workflows) and a new image tag parameter in both ArgoCD apps. It pulls MaxMind's client into a new module or into root anyway. | no |
| B. `ghcr.io/maxmind/geoipupdate` + an uploader container | `geoipupdate` decides "newer" by the MD5 of the **local** file. An emptyDir starts empty every run, so every run would **download**, burning the free quota. Avoiding that needs a third container to pre-fetch the current file from S3. Two third-party images must be mirrored to SWR (the cluster's international egress is flaky, `Dockerfile` GOPROXY note), and two `readOnlyRootFilesystem` exceptions are needed. | no |
| **C. A subcommand of the ingestor binary: `/app/ingestor geoip-refresh`** | No new image, no CI or release change. The tag is pinned with the ingestor's own (`images.ingestor`), and the S3 config code is shared with the pod-side loader. | **recommended** |

With C, the MaxMind client lives in its own package `apps/ingestor-go/geoip/refresh`, reachable only from the subcommand branch in
`main()`, not from the serving path. The ingestor **Deployment** never receives `MAXMIND_*` env (§8.4, asserted).

`main()` dispatches before anything else: `if len(os.Args) > 1 && os.Args[1] == "geoip-refresh" { os.Exit(refresh.Main(ctx, logger)) }`,
ahead of the DB and NATS connections (`main.go:52-121`).

### 8.2 The refresh algorithm (the only code that contacts MaxMind)

Client: **`github.com/maxmind/geoipupdate/v8/client`** (Apache-2.0 / MIT).
- `client.New(accountID int, licenseKey string, opts...)` builds it.
- `Download(ctx, editionID, md5) (DownloadResponse, error)`. When `md5` matches the server's, `UpdateAvailable` is false and **no
  download is performed** (package docs). MaxMind: "Requests to check for updated databases do not count against the daily download
  limit". `Reader` yields the `.mmdb` itself.
- `WithEndpoint` exists, and is the test seam.

The run:

1. Read `MAXMIND_ACCOUNT_ID` and `MAXMIND_LICENSE_KEY`.
   - Either empty: log Error `geoip-refresh: MAXMIND_ACCOUNT_ID / MAXMIND_LICENSE_KEY not set; MaxMind not contacted` and exit 1.
     This is contract decision 6: with no key, the job fails, logs it, and geo stays empty.
   - The account id is not a positive integer: the same, exit 1.
   - The key is never logged.
2. Build the S3 client from the same `GEOIP_S3_*` configuration as the pods (§8.3). Invalid: exit 1, and MaxMind is not contacted.
3. `StatObject(<prefix>GeoLite2-City.mmdb)`.
   - Not found: `md5 = ""`.
   - Found: `md5` = the user-metadata value whose key equals `maxmind-md5` **case-insensitively** (`strings.EqualFold` over
     `ObjectInfo.UserMetadata`), or `""` if there is none. A literal map lookup is wrong: minio-go v7.1.0 keys `UserMetadata` by the
     canonical header name with `X-Amz-Meta-` trimmed, so the key arrives as `Maxmind-Md5` (`minio-go/v7@v7.1.0/utils.go:354-355`).
     A miss would make every run download and re-upload.
   - Any other error: exit 1 **before** contacting MaxMind, so a download that could not be stored is never spent.
4. `Download(ctx, "GeoLite2-City", md5)`.
   - `!UpdateAvailable`: log Info `geoip-refresh: up to date` (md5) and exit 0.
5. Read `Reader` (cap 256 MiB) into memory, then validate:
   - hex MD5 of the bytes == `resp.MD5`,
   - `maxminddb.OpenBytes` and `Verify()` succeed,
   - `DatabaseType == "GeoLite2-City"`.

   Any failure: exit 1 with nothing uploaded. The object in storage stays the last good one.
6. `PutObject(<prefix>GeoLite2-City.mmdb)` with content type `application/octet-stream` and user metadata `maxmind-md5`,
   `maxmind-build-epoch` and `maxmind-last-modified`. The object replacement is atomic, so pods read the old file or the new one. Log
   Info `geoip-refresh: uploaded` (md5, build time, bytes). Exit 0.

The whole run is bounded by a 10-minute context, and by `activeDeadlineSeconds` (§8.4).

**Download budget:** one run does at most one metadata check and at most one download. The schedule allows two runs a week, and
`backoffLimit: 0` with `concurrencyPolicy: Forbid` removes retries and overlaps. That is **≤ 2 downloads a week per environment**,
≤ 4 a week for one account shared by both, against 30 per 24 h. Pod starts, rollouts, autoscaling and crash loops cost none (§5.4).

### 8.3 Storage configuration (pods and job)

| Env | Chart value (`geoip.storage.*`) | Default | Meaning |
|---|---|---|---|
| `GEOIP_S3_ENDPOINT` | `endpoint` | `""` → `sentinel.s3Endpoint` (`config.s3.endpoint`, else bundled MinIO) | URL with scheme. `https` means TLS. |
| `GEOIP_S3_REGION` | `region` | `us-east-1` | SigV4 region, passed to `minio.Options.Region`. |
| `GEOIP_S3_BUCKET` | `bucket` | `""` → `config.s3.bucket` | Empty after defaulting means geo `disabled`. |
| `GEOIP_S3_PREFIX` | `prefix` | `geoip/` | Key prefix, used verbatim. The object is `<prefix>GeoLite2-City.mmdb`. |
| `GEOIP_S3_FORCE_PATH_STYLE` | `forcePathStyle` | `""` | `""` uses the processor's host-shape rule (`fetcher.go:53-72`). A `strconv.ParseBool` value overrides it. |
| `GEOIP_S3_ACCESS_KEY` / `GEOIP_S3_SECRET_KEY` | `accessKeySecretKey` / `secretKeySecretKey` (**names of keys** in the chart Secret) | `S3_ACCESS_KEY` / `S3_SECRET_KEY` | `secretKeyRef` to the named key. |
| `GEOIP_POLL_INTERVAL` | `geoip.pollInterval` | `1h` | pods only |
| `GEOIP_MAX_AGE` | `geoip.maxAge` | `720h` | pods only (§5.4) |

The host-shape rule is **duplicated** into `apps/ingestor-go/geoip`, not imported from `apps/processor-go/symbolicate`. That keeps the
processor untouched and keeps the sourcemap libraries out of the ingestor binary. A parity test runs both functions over the same host
table (`minio`, `localhost`, `127.0.0.1`, `[::1]:9000`, `obs.example.com`, `s3.us-east-1.amazonaws.com`, plus
override values) and asserts equal results, so they cannot drift silently.

**OBS → AWS S3 is a values-and-secret change only.** For example:
- `geoip.storage.endpoint: https://s3.<region>.amazonaws.com`
- `region: <region>`
- `bucket: <bucket>`
- `forcePathStyle: ""` (the dotted host gives virtual-host)
- new sealed keys `GEOIP_S3_ACCESS_KEY` / `GEOIP_S3_SECRET_KEY`, with `accessKeySecretKey` / `secretKeySecretKey` pointed at them.

No code path names OBS.

**Credentials until then: the existing read-write key, as an accepted risk** (contract decision 8, operator, 2026-09-24). On OBS the
ingestor and the CronJob both use the defaults `S3_ACCESS_KEY` / `S3_SECRET_KEY`, the key that also writes attachments and sourcemaps.
No read-only OBS key is created now. At the AWS S3 move the key is split: the ingestor's `accessKeySecretKey` / `secretKeySecretKey`
point at a **read-only key scoped to `<prefix>*` (`geoip/*`)**, and the CronJob keeps a key that can write that prefix. The chart
already renders the two workloads' key names from one value pair; the split needs a second pair for the CronJob,
`geoip.refreshCron.accessKeySecretKey` / `secretKeySecretKey` (default: the `geoip.storage.*` names), added now so that the split is a
values-and-secret change only (contract decision 4).

The K8s region value is `us-east-1`, the region the processor already signs with successfully against this OBS endpoint
(`fetcher.go:113`). It is set explicitly in both overlays, so the AWS move edits one visible line.

### 8.4 Chart changes

**New template `templates/geoip-refresh-cronjob.yaml`**, rendered when `geoip.refreshCron.enabled`. Shaped like
`retention-cronjob.yaml`:

- `batch/v1 CronJob` `<fullname>-geoip-refresh`, component label `geoip-refresh`.
- `schedule`, and `timeZone` (GA since Kubernetes 1.27; the cluster is 1.29).
- `concurrencyPolicy: Forbid`, `startingDeadlineSeconds`, history limits, `backoffLimit: 0`, `activeDeadlineSeconds`,
  `restartPolicy: Never`.
- `serviceAccountName`, `imagePullSecrets`, `nodeSelector` / `tolerations` / `affinity` as the Deployments.
- `.Values.podSecurityContext` and a container `securityContext: .Values.securityContext`. The job writes nothing to disk, so no
  volume is needed.
- Image `images.ingestor.repository:{tag | default .Chart.AppVersion}` with `args: ["geoip-refresh"]`.
- Env: `GEOIP_S3_*` (above), with `GEOIP_S3_ACCESS_KEY` / `GEOIP_S3_SECRET_KEY` taken from the keys named by
  `geoip.refreshCron.accessKeySecretKey` / `secretKeySecretKey` (each defaulting to the `geoip.storage.*` name, so the two workloads
  share one key until the AWS move splits them, §8.3), plus `MAXMIND_ACCOUNT_ID` and `MAXMIND_LICENSE_KEY` as `secretKeyRef` to the chart Secret with
  **`optional: true`**. With the keys absent the pod still starts, and the job fails loudly at step 1 (§8.2). Build and render pass
  either way.

**`templates/ingestor.yaml`:** when `geoip.enabled`, add `GEOIP_S3_ENDPOINT`, `GEOIP_S3_REGION`, `GEOIP_S3_BUCKET`, `GEOIP_S3_PREFIX`,
`GEOIP_S3_FORCE_PATH_STYLE`, `GEOIP_POLL_INTERVAL` and `GEOIP_MAX_AGE` as plain env. `GEOIP_S3_ACCESS_KEY` and `GEOIP_S3_SECRET_KEY`
are `secretKeyRef` to the configured key names, **not** optional: those keys already exist in both envs (T18). Always add
`TRUSTED_PROXY_CIDRS` (`join "," .Values.config.ingestor.trustedProxyCidrs`). **No `MAXMIND_*` variable ever appears on the ingestor
Deployment.**

**No Service, Ingress or load-balancer template changes** (contract decision 11, §3.6). `templates/ingestor-ingress.yaml` and the
Service in `templates/ingestor.yaml` are untouched, and no `argocd.argoproj.io/sync-wave` annotation is added. Revision 2's
`templates/ingestor-elb-service.yaml`, `ingestor.elbService.*` values, `ClusterIP` conversion and wave ordering are withdrawn.

**`values.yaml` additions** (every key has a comment giving its syntax, as `config.processor.*` does):

```yaml
config:
  ingestor:
    trustedProxyCidrs: []          # TRUSTED_PROXY_CIDRS; empty = trust nothing (§3.3)
geoip:
  enabled: false                   # renders the ingestor's GEOIP_* env
  pollInterval: "1h"
  maxAge: "720h"
  storage:
    endpoint: ""
    region: "us-east-1"
    bucket: ""
    prefix: "geoip/"
    forcePathStyle: ""
    accessKeySecretKey: S3_ACCESS_KEY
    secretKeySecretKey: S3_SECRET_KEY
  refreshCron:
    enabled: false                 # ships OFF (chart convention, values.yaml:394)
    schedule: "0 6 * * 3,6"        # Wed + Sat 06:00 UTC: after MaxMind's Tue/Fri GeoLite releases
    timeZone: "Etc/UTC"
    startingDeadlineSeconds: 3600
    activeDeadlineSeconds: 900
    backoffLimit: 0                # a retry could be a second download; the next run is 3-4 days away
    successfulJobsHistoryLimit: 3
    failedJobsHistoryLimit: 3
    resources:
      requests: { cpu: 50m, memory: 256Mi }
      limits: { cpu: 500m, memory: 512Mi }
ingestor:
  resources:
    requests: { cpu: 100m, memory: 256Mi }   # was 128Mi: +~60 MiB resident DB, ~2x transiently during a swap
geoip:
  refreshCron:
    accessKeySecretKey: ""         # "" = geoip.storage.accessKeySecretKey; the write key at the AWS move (§8.3)
    secretKeySecretKey: ""
```

(The second `geoip.refreshCron` block is shown separately for readability; it merges into the one above.)

**Both `values-k8s-*.yml`:**
- `config.ingestor.trustedProxyCidrs: []`, **explicitly empty**, under a comment: "Contract decision 11: no forwarding header is
  believed on K8s. The tunnel writes the browser's IP into the body. Phase 4 (public endpoint) configures this; do not add node,
  pod or ELB ranges here." (§3.3, §3.6)
- `ingestor.service` is **unchanged** (`NodePort`, `31123` / `31316`).
- `geoip.enabled: true` and `geoip.refreshCron.enabled: true`.
- `geoip.storage.endpoint` / `bucket` are pinned explicitly to the same values as the file's `config.s3`, with
  `region: "us-east-1"` and `prefix: "geoip/"`.
- `ingestor.resources.requests.memory: 256Mi` (both overlays pin `128Mi` today at line 149).
- A comment block under `secrets.sealedData` names the two keys TASK-07 adds, `MAXMIND_ACCOUNT_ID` and `MAXMIND_LICENSE_KEY`, sealed
  for `sentinel-<env>` / `sentinel-sentinel-secrets`.

**The keys themselves are NOT added as placeholders.** A `SealedSecret` with one undecryptable entry fails to unseal as a whole,
which would take every Sentinel secret down with it.

**kube-linter:** no waiver is added. The CronJob follows `retention-cronjob.yaml`'s shape, with resources, security context and a
non-root pod. If `lint-helm-k8s` flags it, the manifest is fixed, not `.kube-linter.yaml`.

**`RUNBOOK.md`:**
- §1's key table gains two rows: `MAXMIND_ACCOUNT_ID` (the numeric account id) and `MAXMIND_LICENSE_KEY` (a GeoLite licence key from
  the MaxMind account; never paste it anywhere but the encryptor).
- A new subsection **"GeoIP refresh"** under §2 covers:
  - what the job does and its budget (§8.2);
  - a manual run: `kubectl -n sentinel-<env> create job --from=cronjob/sentinel-sentinel-geoip-refresh geoip-refresh-manual-$(date +%s)`,
    which costs at most one download;
  - how to read the job log (`uploaded` / `up to date` / the step-1 error);
  - how to confirm the pods loaded it (the ingestor's `geoip.state … to=ready` line with `build_time`);
  - the move to AWS S3 as the value and secret edits in §8.3.
- §4 Troubleshooting gains:

| Symptom | Likely cause | Check |
|---|---|---|
| Job fails at start | Keys not sealed (TASK-07) | The step-1 log line |
| Ingestor logs `geoip.state to=unavailable reason=not_found` | The job has not run yet | Run the job manually |
| `to=stale` | The job has been failing for ~30 days | The job history |
| `{{auto}}` events posted straight to the ingestor (not through the tunnel) are stored with an empty IP | Expected on K8s: no forwarding header is believed and the ingestor's peer is infrastructure, which is never stored (contract decision 11, spec §3.6) | Not a fault. Send the real IP in `user.ip_address`, or send browser events through the sdk-js ≥ 0.2.0 tunnel. Do **not** add ranges to `trustedProxyCidrs` to "fix" it. |
| Browser events from web carry no IP | web still runs sdk-js < 0.2.0, whose tunnel writes no IP (contract decision 5) | web's `@sentinel/sdk-js` version |
| A stored IP is a private, `100.64.0.0/10` or `100.125.x.x` address | It came from a body, which is stored as sent. The ingestor never derives such an address (spec §3.5) | The sender: an SDK that set it, or a tunnel whose `trustedProxyHops` does not match its proxy count |
| Tunnelled browser events (web on sdk-js ≥ 0.2.0) are stored with an IP the browser chose, e.g. P5(b)'s `6.6.6.6` | web's public ELB listener (master Ingress `argocd/argocd-public-production`, owned by another team) stopped inserting `X-Forwarded-For`, or a proxy was added in front of web without changing web's `TRUSTED_PROXY_HOPS` | Re-run P5(b) (spec §10.3). Until it passes, roll web back to the sdk-js version before 0.2.0, whose tunnel writes no IP (`sentinel-sdk-js-rich-context/TASK-06` owns web's pin). Then get the listener's owner to restore the insertion, or set the hop count to the real proxy count. |

---

## 9. Files touched

| Path | Change | Card |
|---|---|---|
| `apps/ingestor-go/clientip/` (new) | CIDR parsing (IPv4-mapped entries fatal), `Resolver.Resolve` (no-derive marker first, §3.8), `IsPublic` (§3.5), typed context accessors, `Middleware` | 01 |
| `apps/ingestor-go/enrich/` (new) | client-platform predicate, `{{auto}}` rule with the tunnel-only exclusion I3t (§3.9), geo fill, UA parse and fill, fit rule | 01, 03, 04 |
| `apps/ingestor-go/geoip/` (new) | S3 config and client, `Loader` (poll, swap, state log), `Lookup` | 03, 05 |
| `apps/ingestor-go/geoip/refresh/` (new) | `geoip-refresh` subcommand (§8.2) | 05 |
| `apps/ingestor-go/service/service.go` | `Option` / `WithEnricher`; call `enrich.Apply` between `:256` and `:265` | 01, 03, 04 |
| `apps/ingestor-go/validation/bounds.go` | exported `ContextFits` (no constant or table change) | 04 |
| `apps/ingestor-go/main.go` | subcommand dispatch; `TRUSTED_PROXY_CIDRS` (fatal on typo); geo loader start; UA parser; `clientip.Middleware` around both chains | 01, 03, 04, 05 |
| `go.mod` / `go.sum` | `oschwald/maxminddb-golang/v2`, `ua-parser/uap-go`, `maxmind/geoipupdate/v8` | 03-05 |
| `packages/sdk-js/src/nextjs/route.ts`, `README.md`, `CHANGELOG.md` | §4: `trustedProxyHops` (README text per §4.3, including what `0` does under the App Router), `clientAddress` (canonical IPv6 with the character check and unmapping), `applyClientIp` (rebuilds `user` from `USER_KEYS`, writes the IP), the `user-agent` and `x-sentinel-derive-ip: off` forwards, the rate-limit key | 06 |
| `packages/sdk-js/tests/route/{route,bucket}.test.ts`, `tests/tunnel-fixtures.test.ts`, `tests/tunnel-fixtures/*.json` (new, including `tunnel-user-rebuild.json`) | §4.5 | 06 |
| `deploy/helm/sentinel/templates/geoip-refresh-cronjob.yaml` (new), `templates/ingestor.yaml` (the ingestor container's `GEOIP_*` and `TRUSTED_PROXY_CIDRS` env only; its Service is untouched) | §8.4 | 05, 01 |
| `deploy/helm/sentinel/values.yaml`, `values-k8s-staging.yml`, `values-k8s-production.yml` (`trustedProxyCidrs: []` explicitly), `RUNBOOK.md` | §8.4, §3.3 | 05, 01 |
| `tests/fixtures/geoip/{GeoLite2-City-Test.mmdb,GeoIP2-City-Test.mmdb,README.md,LICENSE-APACHE,LICENSE-MIT}` (new) | §5.6 | 03 |
| `Taskfile.yml` (`test-unit`, pnpm `10.34.5`), `.github/workflows/ci.yml` (`sdk-js-node` only: `pnpm/action-setup` `version: 10.34.5`, and the two drift checks after `pnpm test` with the pathspec `tests/tunnel-fixtures`) | §10.1, §4.5 | 06 (gate, contract decision 9) |
| `tests/unit/*` (new files, §10, including `pnpm_pin_test.go`), `tests/unit/fingerprint_golden_test.go` (one added case) | §10 | all |
| `tests/integration/enrichment_test.go` (new), `tests/integration/testcontainers/ingestor.go` (new `StartIngestorWithEnv`; `StartIngestor` unchanged) | §10 | 01, 03, 04, 06 |
| `docs/sdk-specification.md` | a "Server-side enrichment" subsection: `{{auto}}`, the client-platform list, what the server fills and under which key, "a value you send always wins", and that a derived IP is stored only if public (§3.5), and that a request carrying `X-Sentinel-Derive-IP` gets no connection-derived IP (§3.8), and neither does a `javascript` item whose `metadata.runtime` is `browser` (§3.9), with the residual §3.9 records. A sentence on the Next.js tunnel: it writes the browser's IP into `user.ip_address`, replacing any value the browser sent; it forwards only `id`, `username` and `email` of a sent `user`, and never a sent `geo`. The `user` "stored as sent" row (`:310`) gains "except `{{auto}}`". | 01, 03, 04, 06 |

Nothing under `apps/processor-go`, `apps/dashboard-web`, `packages/proto`, `gen/` or `packages/db-migrations` changes, and neither do
`templates/ingestor-ingress.yaml` or any Service definition (contract decision 11).

---

## 10. Verification

The plane's gates are `task test-unit` and `task test-integration` (T22), plus `helm template` for the chart. **This change widens
`task test-unit`** (contract decision 9, coordinator ruling R1 on `enr-rev-tunnel-no-gate`), so the ingestor's package tests and the
sdk-js suite are gate-run from now on. Every Go proof below still lives in `tests/unit` or `tests/integration` unless it says otherwise,
because those are what the other tasks' reviewers read. `tests/unit` is one flat package, so a compile error in one new file silences
all of it (B4). Run `go vet ./tests/unit/...` before trusting a green run.

Every test must be seen to **fail** with its production line removed (CLAUDE.md working convention). The column "Mutation" names the
line.

### 10.1 Commands

**The widened `test-unit` task** (`Taskfile.yml`, replacing `:184-190`; the comment block above it names contract decision 9 and the
`sentinel-event-schema-v2` precedent, as the existing comment does):

```yaml
    test-unit:
        desc: Run unit tests (Go tests/unit, ingestor packages, dashboard and sdk-js vitest suites)
        cmds:
            - go test ./tests/unit/... -v
            - go test ./apps/ingestor-go/...
            - pnpm --dir apps/dashboard-web install --frozen-lockfile
            - pnpm --dir apps/dashboard-web test
            - npx -y pnpm@10.34.5 --dir packages/sdk-js install --frozen-lockfile
            - npx -y pnpm@10.34.5 --dir packages/sdk-js test
            - git diff --exit-code -- packages/sdk-js/tests/tunnel-fixtures
            - test -z "$(git ls-files --others --exclude-standard -- packages/sdk-js/tests/tunnel-fixtures)"
```

**pnpm is pinned to the exact version `10.34.5`, in the Taskfile and in CI together** (`enr-rev2-npx-pnpm-floating`). pnpm 11
rewrites `packages/sdk-js/pnpm-lock.yaml` and fails `--frozen-lockfile` (coordinator, 2026-09-24), so the gate must not use whatever
`pnpm` is on `PATH`; the authoring machine's is `11.1.2`. Revision 2 used `npx -y pnpm@10`, arguing that a floating major matched CI's
`version: 10`. That was wrong on its own terms: npx resolves the `pnpm@10` range against the registry on every run, so two gate runs of
the same commit could use different pnpm versions, and the gate could not run offline even with a warm store. The argument for
floating also only held while CI floated, and this change already edits `ci.yml`. So:
- `Taskfile.yml`'s two sdk-js lines use `npx -y pnpm@10.34.5`. An exact version resolves to one tarball, which npx caches, so a second
  run needs no registry lookup.
- `ci.yml`'s `sdk-js-node` job pins `pnpm/action-setup@v4` `with: version: 10.34.5` (today `version: 10`, `ci.yml:361-363`). The step
  gains a comment: "Pinned with Taskfile.yml test-unit's `npx -y pnpm@10.34.5`; bump both together."
- **The two are bumped together, and a test holds them together.** `tests/unit/pnpm_pin_test.go` parses `Taskfile.yml` and
  `.github/workflows/ci.yml` (YAML, the T26 approach; no exec). It asserts that every `pnpm@` in `test-unit`'s commands names one exact
  `MAJOR.MINOR.PATCH` version, that the `sdk-js-node` job's `pnpm/action-setup` `version` is exactly that string, and that neither is a
  bare major or a range. Mutation: set either back to `10`, or bump one alone, and it is red.
- `ci.yml`'s other `pnpm/action-setup` (the dashboard job, `ci.yml:285-287`, `version: 10`) is **not** changed. The dashboard's pnpm is
  outside contract decision 9, as are the Taskfile's dashboard lines (§12).
- Why not corepack: a `packageManager` field changes `packages/sdk-js/package.json`, which is what a git-dependency consumer installs
  (the `sdk-js-node` job's consumer-path step), and corepack must be enabled per machine and is no longer bundled from Node 25. `npx`
  ships with every Node install.

Measured on this branch (`56dfdf6`, code identical to `main` `1e5c6e2`) on 2026-09-24: `npx -y pnpm@10 --version` resolved to
`10.34.5`, which is the version now pinned; install `--frozen-lockfile` → done, lockfile untouched; `test` → 26 files, 459 tests
passed; `git status --porcelain` → empty; `go test ./apps/ingestor-go/...` → all six packages `ok`. This matches the coordinator's
measurement on `main` `1e5c6e2`.

The dashboard lines keep using the `pnpm` on `PATH`, as before. They are not this change's ruling (§12).

| Id | Command |
|---|---|
| G1 | `task build` |
| G2 | `task test-unit`: the task above, i.e. `go test ./tests/unit/...`, `go test ./apps/ingestor-go/...`, the dashboard suite, the sdk-js suite under pnpm 10.34.5, and the tunnel-fixture drift check |
| G3 | `task test-integration` |
| G4 | `helm lint deploy/helm/sentinel` |
| G5 | for `f` in `values-k8s-staging.yml values-k8s-production.yml`: `helm template sentinel deploy/helm/sentinel -f deploy/helm/sentinel/$f > /tmp/$f.yaml && kube-linter lint --config deploy/helm/sentinel/.kube-linter.yaml /tmp/$f.yaml` (the `lint-helm-k8s` job, T21) |
| G6 | for each K8s file `$f`, with `R="helm template sentinel deploy/helm/sentinel -f deploy/helm/sentinel/$f"`: (a) `$R --show-only templates/geoip-refresh-cronjob.yaml` succeeds (it fails if the CronJob does not render); (b) every `MAXMIND` match is inside the CronJob **or the `SealedSecret`**: `$R \| awk '/^---/{k=""} /^kind: /{k=$2} /MAXMIND/ && k!="CronJob" && k!="SealedSecret"{n++} END{print n+0}'` prints `0`. A `SealedSecret` has only `spec.encryptedData`, so a `MAXMIND_*` key there holds ciphertext once TASK-07 seals it (`templates/secret.yaml:9-16`); whether a value is plaintext is asserted on the values files instead (`helm_geoip_test.go`, §10.2 TASK-05). Every other kind, the ingestor Deployment and the ConfigMap included, must have none. (c) the licence key is a secret reference, never a literal: in `$R --show-only templates/geoip-refresh-cronjob.yaml \| grep -A5 -- '- name: MAXMIND_LICENSE_KEY'` the window contains `secretKeyRef:`, `key: MAXMIND_LICENSE_KEY` and `optional: true`, and piping it on to `grep -cE '^ +value:'` prints `0`. |
| G7 | `helm template sentinel deploy/helm/sentinel` (plain `values.yaml`): renders with `geoip` off, no CronJob, and `TRUSTED_PROXY_CIDRS` rendered empty on the ingestor container |
| G8 | **No Service, Ingress or Endpoints change anywhere in the chart** (contract decision 11): every `Service`, `Ingress` and `Endpoints` document of the **full** render, per K8s overlay, is diffed against the same set rendered at `1e5c6e2` and must be identical. The script is below. |

**G8, the script** (`enr-rev4-g8-template-scoped`, replacing revision 4's per-template render, which itself replaced revision 3's four
markers, `enr-rev3-g8-not-render-as-today`):

```bash
B=$(mktemp -d); git archive 1e5c6e2 deploy/helm/sentinel | tar -x -C "$B"
net() { awk 'function flush(){ if (k=="Service" || k=="Ingress" || k=="Endpoints") printf "%s", buf; buf=""; k="" }
             /^---/{flush()} /^kind: /{k=$2} {buf=buf $0 "\n"} END{flush()}' \
        | grep -vE '^ *(helm\.sh/chart|app\.kubernetes\.io/version):'; }
render() { helm template sentinel "$1" -f "$1/$2" | net; }
rc=0
for f in values-k8s-staging.yml values-k8s-production.yml; do
  diff <(render "$B/deploy/helm/sentinel" "$f") <(render deploy/helm/sentinel "$f") || rc=1
done; rm -rf "$B"; test "$rc" -eq 0
```

- **Baseline.** The baseline is `1e5c6e2`'s own chart and its own overlay, i.e. production as it renders today, extracted with
  `git archive` (no worktree, nothing checked out). A shallow CI clone runs `git fetch --depth=1 origin 1e5c6e2` first.
- **Scope: the full render, no `--show-only`.** The `kind` tracker from G6 keeps every document whose top-level `kind` is `Service`,
  `Ingress` or `Endpoints`, **from any template file**, with its `# Source:` line. Revision 4 rendered only `templates/ingestor.yaml`'s
  Service and `templates/ingestor-ingress.yaml`, so a Service or Ingress added in any other file passed. The reviewer showed it with
  revision 2's `-ingestor-elb` Service as a new `templates/ingestor-elb.yaml`: exit 0 on both overlays. Now:
  - **A new template file** that renders a Service, Ingress or Endpoints on K8s adds a document, so the diff is non-empty.
  - **`templates/ingress.yaml`**, the generic Ingress that already routes to the ingestor, is covered too. It renders nothing on K8s
    because its guard is `ingress.enabled` **and** `global.provider != "k8s"` (`templates/ingress.yaml:1`). The finding said it was
    `ingress.enabled` alone, but `values.yaml:455` sets that to `true`, and it is the provider test that keeps the file empty. Anything
    that makes it render turns G8 red.
  - Every field of each document is compared: type, selector, ports, `targetPort`, `nodePort`, both traffic policies, annotations
    (`elb.*`, `argocd.argoproj.io/sync-wave`), the Ingress host, and its backend service and port.
  - `Endpoints` is included because the chart renders two on K8s, and they route traffic as much as a Service does. This feature adds
    none, so including them costs nothing.
  - **Nothing this feature does change enters the set.** The ingestor Deployment's env and the new CronJob are other kinds. This
    feature adds no Service, Ingress or Endpoints anywhere (§8.4), so the diff stays empty.
- **The one exclusion** is the two label lines generated from `Chart.yaml` (`helm.sh/chart`, `app.kubernetes.io/version`,
  `_helpers.tpl:19-21`). A chart or app version bump by a release is not a Service change, and without the exclusion G8 would go red on
  every bump.
- **Why a diff against the commit rather than a committed golden file.** The golden file would be a second copy of the same render,
  and it would have to be regenerated in exactly the case G8 exists to catch. The commit cannot drift.
- When a later feature changes a Service, Ingress or Endpoints on purpose, it moves the baseline commit in G8 in the same change.

G6 and G8 are written for `bash` (`$R` relies on word splitting, and G8 uses process substitution). They need only `helm`, `git`,
`tar`, `grep`, `diff` and POSIX `awk`. No `yq` is needed, and neither the authoring machine nor CI has it. G6 was dry-run on
2026-09-24 with `helm` v3 and `bash`. G6(b): with `--set secrets.sealedData.MAXMIND_LICENSE_KEY=AgBxyz` standing in for TASK-07's
ciphertext, revision 2's `awk` printed `1` on both overlays (`enr-rev2-g6-sealed-keys`), and revision 3's prints `0`.

**G8 dry-run, revision 5** (`helm` v3.16.4, `bash`, 2026-09-24, on this branch, whose chart equals `1e5c6e2`'s). Each K8s overlay
renders 5 `Service`, 2 `Ingress` and 2 `Endpoints` documents, 212 lines, and the diff is empty. The script run against a scratch copy
of the chart as the new side:

| Change on the new side | G8 |
|---|---|
| none | exit 0 on both overlays |
| **a scratch `templates/ingestor-elb.yaml`** holding revision 2's `…-ingestor-elb` Service (`NodePort` `31125`, `externalTrafficPolicy: Local`) | **exit 1**; the diff shows the added document under `# Source: sentinel/templates/ingestor-elb.yaml` |
| `--set global.provider=generic` (makes `templates/ingress.yaml` render its Ingress) | **exit 1** |
| `--set ingestor.service.nodePort=31999` | **exit 1** |

S0 (`pnpm --dir packages/sdk-js test` as a supporting command) is **gone**. The sdk-js suite is part of G2, and CI's `sdk-js-node` job
runs the same suite and the same drift checks on push (with the pathspec `tests/tunnel-fixtures`, §4.5).

### 10.2 Criterion → proof

| Card / criterion | Proof | Test (what it asserts) | Mutation that must turn it red |
|---|---|---|---|
| **TASK-01** criterion 2, "A spoofed X-Forwarded-For from an untrusted hop is not trusted (test)" | G2, G3 | **Unit** (`tests/unit/ingestor_clientip_test.go`, `httptest.NewRequest` with `RemoteAddr` set): C1-C19, H1-H3 and M1-M5 (§3.7), and the `IsPublic` predicate table, including the revision-4 rows (N8-N11, zoned and mapped-CGNAT inputs). With an **empty list**: C1 (a public peer's `6.6.6.6` is ignored), C15, H1-H3 (K8s, the list read from the overlay), M1-M2 and M4 (the no-derive marker). With a **configured list**: C2 (untrusted peer), C4 (spoof left of the untrusted hop), C13-C14 and C18-C19 (a non-public walk result is not a caller), and M3 (the marker wins over a trusted chain). Startup parse errors for C12, C16 and C17 (IPv4-mapped entries). **Integration** (`tests/integration/enrichment_test.go`, two containers via `StartIngestorWithEnv`, peer `127.0.0.1`, T23): (i) `TRUSTED_PROXY_CIDRS` unset, POST `{{auto}}` with `X-Forwarded-For: 6.6.6.6`: the stored IP is empty (`coalesce(event_user->>'ip_address', '') = ''`), never `6.6.6.6` and never `127.0.0.1`; (ii) `TRUSTED_PROXY_CIDRS=127.0.0.0/8,::1/128`, POST `{{auto}}` with `X-Forwarded-For: 6.6.6.6, 198.51.100.23`: stored `198.51.100.23`, never `6.6.6.6`. | Believe XFF always (skip the trust check): C1, C2, H1 and integration (i) red. Walk left-to-right: C4 and integration (ii) red. Drop `IsPublic` from `Resolve`: C13-C15, H1-H3 and integration (i) (which would store `127.0.0.1`) red. Drop N7 while narrowing N6: the `100.125.64.17` predicate case red. Build `IsPublic` without the N8-N11 prefixes: the `0.1.2.3`, `240.0.0.1`, `fec0::1`, `::10.0.0.1`, `64:ff9b::a00:1` and `2002:a00:1::1` cases red. Accept an IPv4-mapped trusted entry: C16/C17 red. Ignore the marker: M2-M4 red. Add any range to an overlay's `trustedProxyCidrs`: the overlay-emptiness assertion red, and `10.200.32.0/19` also turns H1 red. |
| TASK-01, `{{auto}}` / client-platform rules | G2 | `tests/unit/ingestor_enrich_ip_test.go`: I1-I6 and I3t (§3.2) × {`javascript`+`browser` runtime, `javascript` with no runtime, `javascript`+`node`, `javascript`+`edge`, `go`, `cocoa`, `dart`}. `{{auto}}` with no public caller becomes `""`. An existing value, including a private one such as `10.1.2.3`, is byte-equal (I6: the public-caller rule never touches a body value). **I3t:** an absent IP on `javascript`+`browser` with a public caller stays absent, while `javascript` with no runtime and `cocoa` with the same caller are filled (I3), and `javascript`+`browser` with `{{auto}}` and a public caller is filled (I1). | Drop the runtime split: the `javascript`+`node` row red. Store `{{auto}}` through: I2 red. Apply `IsPublic` to body values: the `10.1.2.3` case red. Drop I3t: the `javascript`+`browser` absent-IP row red, and so is the public-peer replay's "marker removed" variant (§4.5). Widen I3t to every `javascript` event: the no-runtime I3 row red. Apply I3t to I1: the `{{auto}}` row red. |
| TASK-01 wired to `main()` | G3 | Integration (ii) above: it can only store `198.51.100.23` if `main.go` wraps both ingest chains in `clientip.Middleware` and parses `TRUSTED_PROXY_CIDRS`. | Remove the `clientip.Middleware` wrap in `main.go`: (ii) red. |
| TASK-01 criterion 1, "An event with user.ip_address={{auto}} is stored with the caller's real IP behind the cluster ingress (staging check)" | G3; G2; **post-deploy P1 now, P5 after web adopts sdk-js 0.2.0** | Mapped per §1.3 (the caller behind the ingress is the browser, and its `{{auto}}` is replaced by the tunnel). **G3:** `enrichment_test.go` sends `tunnel-auto-ip.json`'s `forwarded` request, the tunnel's real output, through the real ingestor container (§4.5): stored `198.51.100.23`. **G2:** `sdk_js_tunnel_fixture_test.go` replays the same fixture with K8s's shape (empty list, node-side peer, ignored ELB header): `198.51.100.23`. **P1** (§10.3) checks on staging now that a direct `{{auto}}` through the ELB is stored as `""`, never as an infrastructure address. **P5** is the staging observation of a real browser event, once web runs sdk-js 0.2.0 (`sentinel-sdk-js-rich-context/TASK-06`). | Make `applyClientIp` skip `{{auto}}` (leave a `{{auto}}` value in place), or drop the body write. **The immediate red signals** are the `applyClientIp` `{{auto}}` helper case in the sdk-js suite, the gate's fixture drift check (`tunnel-auto-ip.json` regenerates differently), and, under `CI`, the CI-mode fixture test. G3 and the Go replay read the **committed** `tunnel-auto-ip.json`, and `task test-unit` runs `go test ./tests/unit/...` before the sdk-js suite regenerates it, so they turn red (stored `""`) only once a regenerated fixture is committed. They prove the criterion, but they are not the mutation's first signal (`enr-rev3-crit1-mutation-signal`; the TASK-06 row already cites the same way). |
| **TASK-03** "Missing or corrupt mmdb logs once and ingestion continues (test)" | G2 | `tests/unit/ingestor_geoip_test.go` with a fake object source and an injected `Now = BuildEpoch + 24h` (§5.4): not-found ×5 polls gives exactly one `unavailable/not_found` line; corrupt (the fixture truncated) ×5 gives one `unavailable/corrupt` line; stale (`Now = BuildEpoch + 721h`, while `+ 719h` is ready); wrong type; recovery gives one `ready` line. After each, `IngestAt` succeeds and `user.geo` is null. | Log on every poll: count ≠ 1, red. Return an error from lookup into `IngestAt`: red. |
| TASK-03, lookup rules | G2 | Same file, injected `Now` as above: `81.2.69.142` (fixture) fills `country_code: "GB"`, `city: "London"`, `subdivision: "England"`, `region: "United Kingdom"` (§5.2, §5.6). Private, loopback, link-local and unspecified are skipped. A sent `geo` (including `{}`) is untouched. A sent unparseable IP gets no geo. **Reload:** the fake source serves `GeoLite2-City-Test.mmdb` under ETag `"a"`, then `GeoIP2-City-Test.mmdb` under ETag `"b"`. After the next poll the loader reports `DatabaseType == "GeoIP2-City"`, and exactly one `geoip.reloaded` line has `etag` `"b"`, with no restart. `build_time` is not the signal, because the two files share a build epoch (§5.6). The over-128 truncation is proven in the ordering row below, through the stub lookup. | Map `region` to the subdivision: red. Skip the `geo == nil` check: red. Skip the swap when the ETag changes: the reload case red. |
| TASK-03 wired to `main()` | G3 | `enrichment_test.go`: the container's `GEOIP_S3_*` points at an `httptest` fake S3 on `127.0.0.1` serving the fixture, with `GEOIP_MAX_AGE=876000h` passed through `StartIngestorWithEnv`. Without it the 2026-02-04 fixture is stale and the test cannot pass (§5.4). An event with `user.ip_address: "81.2.69.142"` is stored with the expected `geo`. A second container whose object 404s stores the event with null geo, and `/health` is 200. | — |
| TASK-03 "Staging event from a known IP stores the expected country/city" | **Post-deploy P2** (after TASK-07 and one job run) | POST with body `user.ip_address` = a public IP whose GeoLite2 City answer the operator reads from MaxMind's GeoLite2 demo at the same build date. The stored `country_code` and `city` equal it. | — |
| **TASK-04** "An sdk-js browser event without contexts is stored with browser+os derived from its UA (test)" | G2, G3 | `tests/unit/sdk_js_tunnel_fixture_test.go`: `tunnel-clean.json`'s forward (UA `Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36`) gives `contexts.browser = {name:"Chrome", version:"128.0.0"}` and `contexts.os = {name:"Windows", version:">=10"}`. `enrichment_test.go` replays the same forward against the container and asserts the stored `contexts` JSONB. The tunnel half, i.e. that the forward carries the browser's `user-agent` at all, is §4.5's header-set case in the sdk-js suite, now inside G2. | Drop the connection-UA source: red. Stop forwarding `user-agent` in `route.ts`: the header-set case red; under `CI`, the CI-mode fixture test red (`tunnel-clean`'s forward changed), which is the red signal there (§4.5); locally, the gate's `git diff` drift check red. |
| TASK-04, source and fill rules | G2 | `tests/unit/ingestor_useragent_test.go`: body `request.headers` UA wins over the connection UA, and matches case-insensitively. The connection UA is ignored for `go` and `javascript`+`node`. `go` + body UA writes `client_os`, not `os`. A sent `browser` (even `{}`) and a sent `os` are untouched. `Other` fills nothing. A 1025-byte UA is not parsed. The fit rule: 32 sent contexts means no derived one, and a sent context is never evicted. | Write `os` for server events: red. Remove the fit rule: the eviction case red. |
| **TASK-05** "helm template renders the refresh CronJob and the ingestor's geo wiring for staging and production; the license key is referenced only through the sealed secret (test)" | G5, G6, G7, and G2 | G6 as written (its (b) now admits the `SealedSecret`'s ciphertext keys, §10.1). `tests/unit/helm_geoip_test.go` parses and merges values (T26): both overlays pin `geoip.enabled`, `geoip.refreshCron.enabled` and `geoip.storage.{endpoint,bucket,region,prefix}`; both overlays **declare** `config.ingestor.trustedProxyCidrs` and it is **empty** (contract decision 11), so a missing key and a non-empty list are both red. `secrets.data` in `values.yaml` has no `MAXMIND_*`. No values file has a `MAXMIND_*` key outside `secrets.sealedData`, and none has a plaintext `MAXMIND_LICENSE_KEY` anywhere. The ingestor memory request is `256Mi`. G8 diffs every `Service`, `Ingress` and `Endpoints` document of the **full** render, per overlay, against the render at `1e5c6e2`, so no network object anywhere in the chart changes (§10.1). | Put the license key in `secrets.data`: red. Put any range in an overlay's `trustedProxyCidrs`: red. Change anything in a rendered Service or Ingress (selector, `targetPort`, `nodePort`, either traffic policy, an `elb.*` or sync-wave annotation, the Ingress backend service or port): G8 red. **Add revision 2's `…-ingestor-elb` Service as a new template file: G8 red** (revision-5 dry-run with a scratch `templates/ingestor-elb.yaml`: exit 1). Make `templates/ingress.yaml` render on K8s (`global.provider=generic`): G8 red (exit 1). |
| TASK-05 "Pod starts and rollouts cause zero MaxMind downloads: only the CronJob contacts MaxMind, at most twice a week per env (test + staging job log)" | G2, G6; **post-deploy P3** | (a) `tests/unit/ingestor_geoip_imports_test.go` runs `go list -deps` over `./apps/ingestor-go/geoip`, `./apps/ingestor-go/enrich`, `./apps/ingestor-go/clientip` and `./apps/ingestor-go/service`. It asserts that **no** listed path starts with `github.com/maxmind/geoipupdate` or equals `…/apps/ingestor-go/geoip/refresh`. It also asserts, with `go list -f '{{.ImportPath}} {{join .Imports " "}}' ./apps/ingestor-go/...`, that the only importer of `geoip/refresh` is the `apps/ingestor-go` main package. (This replaces the first revision's "loader start plus 3 polls: 0 MaxMind requests". That test could not fail: the counting fake was reachable only through `client.WithEndpoint`, which the loader never receives.) `tests/unit/ingestor_geoip_refresh_test.go` uses a counting fake MaxMind (`client.WithEndpoint`) and a fake S3. (b) Refresh with a matching stored MD5: one metadata request, zero downloads, no `PutObject`. The fake S3 is an `httptest` server reached through a **real minio-go client**. It returns the object's metadata as the header `X-Amz-Meta-Maxmind-Md5`, which minio-go turns into the key `Maxmind-Md5` (§8.2), so a case-sensitive lookup of `maxmind-md5` fails this case. (c) Newer file: exactly one download and one `PutObject` with metadata. (d) No credentials: 0 MaxMind requests and exit 1. (e) A stat error: 0 MaxMind requests. (f) An MD5 mismatch: no `PutObject`. `helm_geoip_test.go`: the schedule has exactly two day-of-week values; `backoffLimit` is 0; `concurrencyPolicy` renders `Forbid` (template text). G6: no `MAXMIND` on the ingestor Deployment. P3: `kubectl -n sentinel-staging rollout restart deploy/sentinel-sentinel-ingestor`; the new pods log `geoip.state to=ready`; job history over 7 days shows ≤ 2 runs. | Construct the MaxMind client in the loader (the `geoip` package imports `geoipupdate`): (a) red. Drop the md5 pass-through, or look the key up case-sensitively: (b) red. |
| TASK-05 "Switching storage from OBS to AWS S3 is a values/secret change only: the S3 client takes endpoint, region, bucket, prefix and path-style from config (test)" | G2 | `tests/unit/ingestor_geoip_s3_test.go`: the geo S3 client against an `httptest` fake with a capturing transport (an exported `Transport` field, documented test-only). Path-style config gives `GET /<bucket>/<prefix>GeoLite2-City.mmdb`. Virtual-host (`forcePathStyle=false`, dotted host) gives `Host: <bucket>.<host>` with path `/<prefix>GeoLite2-City.mmdb`. `region=ap-southeast-1` appears in the SigV4 `Credential=…/ap-southeast-1/s3/aws4_request` scope. The path-style parity table (§8.3). | Hard-code `us-east-1`: the region case red. |
| TASK-05 "The ingestor pod on staging and prod has a readable, current GeoLite2-City.mmdb (staging + prod check)" | **Post-deploy P4** (after TASK-07) | For each env: `kubectl -n sentinel-<env> logs deploy/sentinel-sentinel-ingestor \| grep geoip.state` shows `to=ready` with `build_time` ≤ 7 days old, on every replica. | — |
| **TASK-06** "A tunnelled browser event with user.ip_address={{auto}} or empty is stored with the browser's IP, not the web server's (test)" | G2, G3 | **sdk-js suite, inside G2** (`task test-unit`, contract decision 9): §4.5's derivation cases (the IP in every forwarded item's `user.ip_address`), the `applyClientIp` helper cases B1-B5 (including the fold cases `IP_ADDRESS`, `Ip_Address`, `ip_addreſſ` and a folded `GEO`), the through-the-route overwrite case, the header-set case (`x-sentinel-derive-ip: off` on every forward), and the fixture test, which fails on drift. **Go, inside G2:** `sdk_js_tunnel_fixture_test.go` replays `tunnel-clean` (empty IP), `tunnel-auto-ip` (`{{auto}}`), `tunnel-spoofed-xff` and `tunnel-browser-sent-ip` with K8s's shape (§4.5): the result is `198.51.100.23`, never `6.6.6.6`, `172.16.3.117` or the node-side peer `10.200.32.66`. `tunnel-no-ip` stores **no** IP, and its **public-peer replay** (`RemoteAddr` `198.51.100.200`, §4.5) stores none either, in its four variants: none with only the marker, none with only I3t (`metadata.runtime: browser`, marker removed, the pre-0.2.0 tunnel's shape), and the peer with both removed (M1), so the replay is not vacuous. The **`encoding/json` decode** of `tunnel-user-rebuild.json`'s outputs gives `User.IPAddress` = the tunnel's `ip` for every fold case and the lookup's geo, never the browser's. **Integration, G3:** `enrichment_test.go` sends each fixture's `forwarded` request, the tunnel's real output, through the real ingestor container: the stored IP is `198.51.100.23` for `tunnel-clean`, `tunnel-auto-ip` and `tunnel-browser-sent-ip` (**a browser-sent `6.6.6.6` is overwritten**), and `tunnel-no-ip` stores none. Each `tunnel-user-rebuild.json` output posted as a batch stores the tunnel's IP and the lookup's geo (`GB`), never `6.6.6.6` or `XX`. | Tunnel keys on the leftmost hop: the derivation case red in the sdk-js suite, and if the fixtures are regenerated and committed anyway, the Go and integration `tunnel-spoofed-xff` cases red. Keep a browser-sent `user.ip_address` when present (B2): the helper case red. Overwrite or delete only the exact key `ip_address` in place (revision 3's B2/B4): the `IP_ADDRESS`, `Ip_Address` and `ip_addreſſ` helper cases red, and once the fixture is regenerated and committed, the Go decode case red (it decodes `6.6.6.6`). Keep `geo`: the `browser-geo` helper case red. Drop the allow-listed keys (rebuild to `{ip_address}` only): the helper's `id: "u1"` case red. Match the allow-list case-insensitively: the `ID`/`Username` case red. Leave the browser's IP when none is derived (B4): the helper case red. Stop sending `x-sentinel-derive-ip`: the header-set case red, and under `CI` the fixture test red; the public-peer replay's "runtime removed" variant (marker alone) needs the marker in the committed fixture, so once committed it stores `198.51.100.200`, red. Write `{{auto}}` instead of nothing when `ip` is null: `tunnel-no-ip` stores `""` rather than omitting it, and its fixture assertion (`user` absent) red. An uncommitted regeneration: the gate's drift check red; in CI the CI-mode fixture test red. |
| TASK-06 "The tunnel never forwards a client-supplied forwarding header verbatim; a spoofed X-Forwarded-For from the browser is not what reaches the ingestor as the client IP (test)" | G2, G3 | **sdk-js suite, inside G2:** the header-set case (`x-forwarded-for` is **never** on a forward, even when the inbound request carries one) and the spoof derivation case (the written IP is never `6.6.6.6` nor the inbound string). **Go, inside G2:** the fixture assertions in §4.5 (no `x-forwarded-for` in `forwarded.headers`; the forwarded IP never equals a differing inbound IP), and `tunnel-spoofed-xff` resolves to `198.51.100.23`. **G3:** `tunnel-spoofed-xff` through the real ingestor stores `198.51.100.23`, not `6.6.6.6`. | Forward `req.headers.get('x-forwarded-for')`: the header-set case red, and under `CI` the fixture test red (the forward's headers changed). Write the leftmost hop into the body: the spoof case red and, once committed, the Go and G3 spoofed cases red. |
| Compatibility: fingerprints unchanged | G2 | `fingerprint_golden_test.go` unchanged and green, plus the enriched-fixture case (§7.3). | Add `contexts` to the hash input: red. |
| Compatibility: sent fields byte for byte; no network I/O | G2 | `tests/unit/ingestor_enrich_order_test.go`: the JSON-equality case; the zero-S3-calls case (§7.3, counting fake object source); and enrichment before bounds, where a **stub `GeoLookup`** (§5.6) returns a 200-rune city and the payload after `IngestAt` has it at 128 runes with `user.geo` in `server_truncated`. | Run `enrich.Apply` after `ValidatePayload`: the truncation case red. Fetch from S3 on a lookup miss: the zero-calls case red. |
| Build | G1 | — | — |

### 10.3 Post-deploy steps (staging first, then production)

**P1, the K8s ingest path believes no header and stores no infrastructure address** (after this change deploys to staging; a staging
check of the rule TASK-01 criterion 2 tests, and the "now" part of criterion 1's mapping, §1.3). Nothing about the Service changed, so P1 has no
rollout ordering to follow. Each case posts `{"platform":"go",…}` with the staging key and a unique `message` from a host outside the
cluster (VPN or bastion) to `https://api-sentinel-stg.example.com/ingest`, and reads the occurrence back through the agent occurrence
endpoint or `psql` (`error_occurrences.event_user->>'ip_address'`).
- **(a)** `user: {ip_address: "{{auto}}"}`, no forwarding header: the stored IP is **empty**. Not a `10.200.x.x`, `172.16.x.x` or
  `100.125.x.x` address, and not the literal `{{auto}}`.
- **(b)** The same with `X-Forwarded-For: 6.6.6.6`: the stored IP is **empty**, never `6.6.6.6`.
- **(c)** `user: {ip_address: "198.51.100.23"}` (a body value): stored as `198.51.100.23`. The body wins.
- **(d)** Every ingestor replica's startup log (`kubectl -n sentinel-staging logs <pod>` for each pod of
  `deploy/sentinel-sentinel-ingestor`) has the Info line `trusted proxy ranges` with an **empty** list (§3.3).

Production repeats (a), (b) and (d) against `api-sentinel.example.com`. If (a) or (b) stores any address, the overlay's list is not empty
or the public-caller rule is not running: that is a defect in this change, not a values tweak. **Never** make it "work" by adding a
range to `trustedProxyCidrs` (contract decision 11).

- **P2** and **P3** are as in §10.2.
- **P4** is as in §10.2, reading the newest `build_time` from either `geoip.state … to=ready` or `geoip.reloaded` (§5.5).
- **P5, tunnel on staging: the staging observation for TASK-01 criterion 1 and for TASK-06.** It becomes possible only once web runs
  sdk-js with this route (contract decision 5), which is `sentinel-sdk-js-rich-context/TASK-06` (web adopts sdk-js 0.2.0), and it is
  recorded there. Until then this spec's proof of criterion 1 is the G3 tunnel-to-ingestor case (§10.2). Each case reads the
  occurrence back as in P1:
  - **(a)** A browser error from `app-staging.example.com` is stored with the browser's public **IPv4** address (the one
    `https://api.ipify.org` reports in that browser; that host answers over IPv4 only, and web's hosts have only an `A` record, T13, so
    the browser reaches web over IPv4 too), its geo, and `contexts.browser` / `contexts.os`. One sent from the browser console with
    `user: {ip_address: "6.6.6.6"}` is stored with the browser's IP, not `6.6.6.6`.
  - **(b) Spoofed header through web's public host** (`enr-rev3-public-elb-append-unverified`). From a host outside the cluster,
    take the reference address as `ME=$(curl -4 -s --noproxy '*' https://ifconfig.me)`, then post one browser-shaped event with a
    unique `message` straight to the tunnel: `curl -4 -s --noproxy '*' https://app-staging.example.com/api/sentinel -H 'content-type: application/json' -H 'sec-fetch-site: same-origin' -H 'X-Forwarded-For: 6.6.6.6' --data
    '{"platform":"javascript","message":"p5b-<uuid>","metadata":{"runtime":"browser"},…}'`. The stored `ip_address` must equal `$ME`.
    **Both requests use `-4 --noproxy '*'`** (`enr-rev4-p5-ifconfig-family`). A dual-stack host would otherwise ask `ifconfig.me`
    over IPv6, while web's hosts have only an `A` record (`114.119.184.248`, T13), so the tunnel sees the host's IPv4 address and a
    correct result would read as the stop below. A shell `HTTP(S)_PROXY` applied to one request and not the other would do the same.
    The `sec-fetch-site: same-origin` header passes the tunnel's same-origin step (step 2, `route.ts:162-176`), which a bare curl
    would fail with 403. This is the end-to-end check of T13's documented behaviour: the public listener inserts the caller's address, and the tunnel reads
    the rightmost entry.
  - **(c)** The same as (b), with the same `-4 --noproxy '*'` flags and `$ME`, and a two-entry header
    `X-Forwarded-For: 6.6.6.6, 7.7.7.7`: stored `$ME`, never `6.6.6.6` or `7.7.7.7`.

  **If (b) or (c) stores `6.6.6.6` or `7.7.7.7`,** the public listener did not insert the caller's address, or a proxy in front of
  web is missing from web's hop count. Every tunnelled browser IP is then browser-chosen, and so is web's rate-limit bucket (T12). This
  is a **stop**. Web's sdk-js 0.2.0 adoption (`sentinel-sdk-js-rich-context/TASK-06`) must not proceed, or it is rolled back, until a
  re-run passes, because the older tunnel writes no IP. The listener's X-Forwarded-For setting belongs to its master Ingress's owner
  (`argocd/argocd-public-production`, T13), and the hop count belongs to web's `TRUSTED_PROXY_HOPS`. Nothing in this repository changes
  to "fix" it, and the ingestor has nothing to add: it stores the body the tunnel wrote. The RUNBOOK row in §8.4 says the same. If
  (b) stores **no** IP, web's `getClientIp` rejected the rightmost entry. That is a web defect to report, and it is not a spoof.
  Production repeats (b) and (c) against `https://app.example.com/api/sentinel` once web's production runs sdk-js 0.2.0. Both are re-run
  after any change to the public listener (§8.4 RUNBOOK row).

Revision 2's P1(b)-(e) are **withdrawn** with the design they checked: the direct-to-NodePort spoof (b) is now C1/H1 plus P1(b)
above, since no header is believed; the in-cluster append proof (c) checked a listener behaviour nothing depends on (§3.4); the ELB
member-health check (d) belonged to `externalTrafficPolicy: Local`; and the IPv4-only check (e) protected an IPv4-only trusted list,
which is now empty.

---

## 11. Decisions this spec made, and what each was derived from

### 11.1 Decisions

| # | Decision | Derived from |
|---|---|---|
| 1 | Client platforms are `javascript` (except runtime `node`/`edge`), `browser`, `cocoa`, `objc`, `dart` and `flutter`. | Contract §4.1; T7 (no `java-android`); T8 (runtime split); Relay's `javascript\|cocoa\|objc` |
| 2 | `{{auto}}` with no **public** caller is stored as `""`. | Contract §2 ("`{{auto}}` … is replaced") and **decision 11** ("`{{auto}}` with no public caller is stored as empty"). Storing the literal would be a value nobody sent as data. |
| 3 | An address in a trusted range is never a caller, and an unreadable hop stops the walk with no result. | Contract §4.1 ("a server platform … connection address is a pod"); TASK-06 "not the web server's" |
| 4 | `X-Forwarded-For` only, rightmost-untrusted walk. **No K8s forwarding behaviour is relied on**: with the empty list the walk stops at the peer (§3.4). Revision 2's append citations and P1(c) are withdrawn. | Contract §4.1; **decision 11** (empty list on K8s); the rule is kept in code for Phase 4 (`sentinel-sentry-compat-ingest`) |
| 5 | `TRUSTED_PROXY_CIDRS` typo is fatal; geo misconfiguration is not. | Repo convention (`fetcher.go:53-60`); contract §4.2 "never an error" |
| 6 | The tunnel forwards the browser's `User-Agent`. | Contract §1 hypothesis and §4.3 source 2; T10 (the body path is closed); **decision 11** keeps it ("the UA is labelled data, not a trust decision", coordinator brief for revision 3) |
| 7 | The tunnel's written IP and its default rate-limit key share the rightmost-trusted-hop derivation (`trustedProxyHops`, default 1). | TASK-06 goal and criterion 2; T11-T13; **decision 11** ("rightmost untrusted `X-Forwarded-For` hop, `trustedProxyHops`") |
| 8 | `os` for client-platform events and `client_os` otherwise. No `type` key. | Relay `normalize_user_agent_info_generic`; contract §4.3 `{name, version}` |
| 9 | Geo field mapping, with `region` = country name. | Relay `geo.rs`; TASK-03 goal "like Sentry's user.geo.*" |
| 10 | Stale is older than 720 h (30 days) by build time, and a stale database answers nothing. | Contract §4.2 "stale … means no geo"; GeoLite EULA 30-day clause |
| 11 | The refresh is a subcommand of the ingestor binary, with `backoffLimit: 0`, running Wed and Sat. | §8.1 comparison; contract decision 3 budget; TASK-05 criterion 2 |
| 12 | MaxMind keys are `secretKeyRef … optional: true`, and there are no placeholder ciphertexts. | Contract decision 6; `SealedSecret` all-or-nothing unseal |
| 13 | **Decision, accepted risk:** the ingestor (and the CronJob) use the existing read-write `S3_ACCESS_KEY` / `S3_SECRET_KEY` on OBS. At the AWS S3 move the ingestor gets a read-only `geoip/*` key and the CronJob keeps a write key, a values-and-secret change through `geoip.storage.*SecretKey` and `geoip.refreshCron.*SecretKey` (§8.3, §8.4). | **Contract decision 8** (operator decision, 2026-09-24, coordinator ruling R3 on `enr-rev-ingestor-rw-bucket-key`); contract decision 4 |
| 14 | In-memory reader (`OpenBytes`), no `Close` on swap, and the ingestor memory request goes to 256 Mi. | `readOnlyRootFilesystem` (T19); maxminddb `Close` concurrency rule |
| 15 | The UA parse cap is 1024 bytes, and the parse is cached (1024 entries). | Bounded per-request cost (contract "no added latency" intent); no real UA approaches it |
| 16 | **The unit gate widens (R1).** `task test-unit` also runs `go test ./apps/ingestor-go/...` and the sdk-js suite (`install --frozen-lockfile`, then `test`) under **pnpm `10.34.5` exactly** (`npx -y pnpm@10.34.5`), and fails on tunnel-fixture drift (`git diff --exit-code` plus an untracked-file check). CI's `sdk-js-node` pins the same `10.34.5`, the two are bumped together, and `tests/unit/pnpm_pin_test.go` holds them equal. Under `CI` the fixture test fails on drift instead of writing. | **Contract decision 9** (coordinator ruling R1, 2026-09-24, on `enr-rev-tunnel-no-gate`; measured on `main` `1e5c6e2`: six ingestor packages pass, sdk-js 26 files / 459 tests under pnpm 10.34.5); the `sentinel-event-schema-v2` precedent (dashboard vitest in the same gate); `enr-rev2-npx-pnpm-floating` (exact pin in both places, as the reviewer wrote and the coordinator's brief ordered) |
| 17 | ~~Trust only the ELB and the pods; `…-ingestor-elb` Service with `externalTrafficPolicy: Local`~~ **Superseded.** `trustedProxyCidrs` is **empty** in both K8s overlays, declared explicitly, and this feature makes **no Service, `externalTrafficPolicy`, NodePort, Ingress or load-balancer change**. The security-group fallback is gone with it. | **Contract decision 11** (operator, 2026-09-24), which supersedes contract decision 10 in full, after `enr-rev2-rollout-wave-order` and `enr-rev2-local-pod-churn-blackhole`; §3.6 |
| 18 | The ingest path is IPv4-only (T15a). Since revision 3 this is **not load-bearing**: the K8s list is empty, and the public-caller rule covers both families. P1(e) is withdrawn. | T15a; decision 11; §3.5 |
| 19 | **The reviewer's fixes for the remaining minors and notes are applied as written (R4)**, with two recorded deviations. First, the reload fixture is `GeoIP2-City-Test.mmdb`, as suggested, but the reload is asserted by `DatabaseType` and ETag, because both test databases have the same build epoch. Second, the drift check adds an untracked-file check to `git diff --exit-code`, because `git diff` does not see a new file. | Coordinator ruling R4; §5.6 (measured build epochs); §4.5 |
| 20 | **The tunnel writes the IP into the body.** `applyClientIp` (B1-B5, §4.4): with an IP, create `user` if absent or `null`, else **rebuild** it (decision 28) with `ip_address` last; leave a non-object `user` alone (the ingestor rejects it); with no IP, rebuild a plain-object `user` without any IP and create nothing. It runs after `ITEM_KEYS`, on every item of the request. *(Revision 4: "set only `user.ip_address`" and "delete a browser-sent `ip_address`" were replaced by the rebuild.)* | **Contract decision 11** ("writes it into each forwarded event's `user.ip_address`. It overwrites any value the browser sent, because a browser-chosen IP is not evidence"). Revision 3 also quoted "adds a user object if ITEM_KEYS stripped it, touching nothing else in user". That phrase is from the revision-3 brief, not the contract text at `774039e`. The coordinator's revision-4 ruling supersedes its second half with the rebuild (decision 28). B3 and B4 are derived: decision 11's reason ("a browser-chosen IP is not evidence") rules out keeping the browser's value when none is derived, and the route's shape-only rule (`route.ts:262-268`) rules out repairing a malformed `user`. |
| 21 | With no derivable IP the tunnel writes **nothing**, never `{{auto}}`, and every forward carries the no-derive marker (decision 29), so the ingestor cannot fill the item from its peer either. | TASK-06 criterion 1 ("not the web server's"): `{{auto}}` would make an ingestor that can see the tunnel's server store that server's address; on K8s it would be `""` anyway (§4.4). Revision 3 stopped there, and I3 still allowed the same outcome (`enr-rev3-null-ip-server-address`). |
| 22 | The tunnel forwards **no `x-forwarded-for`**; the body is the IP's only channel. | Decision 11 ("the tunnel writes the client IP into the event body"); a header would be ignored on K8s and would make the tunnel a trusted proxy for a redundant claim elsewhere (§4.4); TASK-06 criterion 2 then holds trivially for headers |
| 23 | **The public-caller rule** (N1-N11 since revision 4, §3.5; decision 31) applies to derived caller IPs only, inside `clientip.Resolver.Resolve`. N1 (unspecified) and N5 (multicast) are added to the contract's list, and N7 is kept separate from N6. The documentation ranges count as public. The geo lookup reuses the predicate. The tunnel does not apply it. | **Contract decision 11** (private, loopback, link-local, CGNAT, shared-ELB); N1/N5 cannot be a TCP peer, so adding them excludes no real caller; the documentation ranges are not in the contract's list and the tests depend on them; the body wins over derived rules (decision 2) |
| 24 | TASK-01 criterion 1 is proved by the tunnel-to-ingestor integration case (G3) and its Go replay (G2); P1 checks now that a direct `{{auto}}` stores `""`; the staging observation is P5 after web adopts sdk-js 0.2.0. | The coordinator's brief for revision 3; §1.3 (the criterion's literal wording cannot hold for a direct caller under decision 11, and that goes upward) |
| 25 | TASK-01 criterion 2 is proved by unit cases **and** integration cases, each with an empty list and with a configured list (C1/C15/H1-H3 and integration (i); C2/C4/C13-C14 and integration (ii)). | The coordinator's brief for revision 3; §3.7, §10.2 |
| 26 | CI's drift checks use the pathspec `tests/tunnel-fixtures` because `sdk-js-node` runs in `packages/sdk-js`; the CI-mode fixture test is the red signal cited for CI. | `enr-rev2-ci-drift-pathspec`, fix as the reviewer wrote; `ci.yml:351-353` |
| 27 | G6(b) admits `MAXMIND` matches in the `SealedSecret`, and the values test asserts no plaintext key and no `MAXMIND_*` outside `secrets.sealedData`. | `enr-rev2-g6-sealed-keys`, the reviewer's first option; dry-run in §10.1 |
| 28 | **`applyClientIp` rebuilds `user`** from an exact-key allow-list `USER_KEYS = ['id', 'username', 'email']`, with string values only, never `ip_address` or `geo` in any spelling, and then writes `ip_address` itself when it derived one. A browser-sent `geo` is therefore always dropped. | **Coordinator ruling** on `enr-rev3-casefold-ip-key`, which also resolves `enr-rev3-browser-geo-survives`; T28 (Go's folding decoder, measured); the list is `validator.go:114-119` `UserInfo`'s string fields minus `ip_address` |
| 29 | **The no-derive marker.** The tunnel sends `x-sentinel-derive-ip: off` on every forward. The ingestor derives no caller IP for any request that carries the header, whatever its value. | The coordinator's brief for `enr-rev3-null-ip-server-address` ("pick a rule where a tunnel that derived no IP can never let the ingestor fill the web server's address"). A header, not a body field, because the resolver works per request (§7.2) and an explicit `""` cannot be told from an absent key (T4). Unauthenticated, because it can only withhold information (§3.8). The reviewer's `trustedProxyCidrs` rule is kept as a tested fact (M5), not as the guarantee. |
| 30 | **No fold-related defence in the ingestor.** `user` keeps being decoded by `encoding/json` as today. | The coordinator asked for a recommendation. Exact-key decoding breaks the contract's compatibility rule for direct callers that send `IP_ADDRESS`. Rejecting fold-equal duplicates turns accepted batches into 400s. Neither removes a power, because a key holder can write any IP with the exact key (contract decision 11). The tunnel's output is fold-safe by construction, and the Go decode test proves it (§4.4, §4.5). |
| 31 | `IsPublic` is `IsGlobalUnicast() && !IsPrivate()` minus a prefix list, and the IPv4-embedding IPv6 ranges are **not public** rather than unwrapped. | `enr-rev3-ispublic-range-gaps`, the reviewer's fix with its first option for N11; RFCs 1122, 1112, 3879, 4291, 6052, 8215, 3056; measured with go1.26.4 (§3.5) |
| 32 | An IPv4-mapped `TRUSTED_PROXY_CIDRS` entry is a **startup error**, not converted. | `enr-rev3-mapped-trusted-prefix`, the reviewer's second option; §3.3 gives the reason (a mapped prefix shorter than `/96` has no IPv4 form) |
| 33 | The tunnel's canonical IPv6 form requires `/^[0-9A-Fa-f:.]+$/` before parsing and a bracketed `hostname` after, and an IPv4-mapped address is **unmapped** to dotted IPv4. | `enr-rev3-canonical-ipv6-url`, the reviewer's fix; unmapping matches the ingestor's stored form for derived addresses (§3.2), and the finding asked the spec to choose |
| 34 | G8 diffs every `Service`, `Ingress` and `Endpoints` document of the **full** render against `1e5c6e2` (`git archive`), excluding only the two version-label lines. A committed golden file was not chosen. *(Revision 5: the full render replaced revision 4's two `--show-only` templates.)* | `enr-rev3-g8-not-render-as-today`, the coordinator's brief ("a committed golden file, or a diff"); `enr-rev4-g8-template-scoped`, the reviewer's fix ("apply the kind tracker to the full render") with `Endpoints` added; dry-run in §10.1 |
| 35 | Web's public listener is **documented** to insert `X-Forwarded-For`: shared ELB, HTTP/HTTPS, enabled by default, and it cannot be disabled. Post-deploy P5(b)/(c) still checks it end to end with a spoofed header, and a failure stops web's 0.2.0 adoption. | `enr-rev3-public-elb-append-unverified`; K8s `elb_ug_jt_0515` and `elb_faq_0090`; T13 (the listener's master Ingress is owned outside this repository, so the documented default can change without a Sentinel change) |
| 36 | **I3t: a `javascript` item whose `metadata.runtime` is `"browser"` gets no I3 fill** (§3.2, §3.9). I1 and the UA rule are unchanged. Recommended over documenting a `TRUSTED_PROXY_CIDRS` rule for pre-0.2.0 tunnels. | `enr-rev4-pre-marker-tunnel-skew`, the coordinator's brief ("make I3 not fill … unless the request is direct, or record why the K8s-only exposure is acceptable; recommend one"). The "direct" case does not exist for that runtime: the sdk-js browser build has no `apiKey` / `endpoint` (`browser.ts:1-3`) and the ingestor serves no CORS. Contract §4.1's own reason ("its connection address is … not a user") and TASK-06 ("not the web server's"). A documented rule fails in silence, which §3.8 already rejected. |
| 37 | The README says `trustedProxyHops: 0` derives **no IP** under the Next.js App Router, and the hops-0 test asserts `null` for a plain `Request`. | `enr-rev4-hops0-no-socket-under-next`, the reviewer's fix; `next@15.5.21` `base-server.js:568` (`x-forwarded-for ??= socket.remoteAddress`); `route.ts:199-201` |
| 38 | P5 takes `$ME` with `curl -4 -s --noproxy '*'` and posts with the same flags; P5(a) reads the browser's IPv4 address. | `enr-rev4-p5-ifconfig-family`, the reviewer's fix; T13 (web's hosts have only an `A` record) |

### 11.2 Trust-boundary notes for the spec reviewer

- **On K8s no forwarding header is believed, and the ingestor never derives a caller IP there** (§3.6, contract decision 11).
  Every stored IP comes from a body. A server SDK that posts `{{auto}}` directly gets `""`, which differs from Sentry, where the
  server's view of the connection is used. That is the decision's intended cost: on this ingress the connection is never the caller.
- **The trust boundary for a browser's IP is now the tunnel.** The ingestor stores whatever the tunnel writes, because it is a body
  value (§3.2 I6). The tunnel's derivation (`trustedProxyHops`, or web's `clientKey: getClientIp`) is what keeps a browser from choosing
  its stored IP, and §4.3's README rule is what keeps an adopter from misconfiguring it. A holder of the tunnel's API key can write any
  IP, exactly as any key holder can in any body (contract decision 11's premise).
- **A browser cannot set its stored IP or geo.** A browser-sent `user` is removed by `ITEM_KEYS` today. Once `ITEM_KEYS` widens,
  `applyClientIp` **rebuilds** it from the exact keys `id`, `username` and `email` (B2, B4). No key that Go's case-folding decoder
  reads as `ip_address` or `geo` survives (`IP_ADDRESS`, `Ip_Address`, `ip_addreſſ`, `GEO`; T28). Revision 3's in-place overwrite
  left those behind, which was `enr-rev3-casefold-ip-key`. Its `X-Forwarded-For` entries left of the trusted hop are never read, and
  the tunnel forwards no `X-Forwarded-For` at all (§4.4). The top-level filter is exact-key too, so a second `USER` object never
  reaches the ingestor. This relies on web's public listener inserting the caller's address, which K8s documents as always on
  (T13) and P5(b)/(c) check.
- **The web server's own address is never stored as a browser's, whatever the tunnel's version.** A 0.2.0 tunnel's forward carries
  `x-sentinel-derive-ip: off`, so the ingestor derives nothing from that connection on any deployment (§3.8). Any caller can send that
  header, but it only withholds its own connection address, and a browser cannot strip it, because the tunnel builds its forward
  headers from scratch. **Older tunnels send no marker** (`enr-rev4-pre-marker-tunnel-skew`), so revision 4's form of this guarantee
  held only for a tunnel at ≥ 0.2.0. Revision 5 adds I3t (§3.9): an item that is `javascript` with runtime `browser`, which reaches the
  ingestor only through a tunnel, never gets a connection-derived IP. That covers every tunnel version. The one residual is a browser
  that rewrites its own `metadata.runtime` behind a pre-0.2.0 tunnel whose server the ingestor sees at a public address: its **own**
  event gets that server's address (§3.9). K8s is unaffected.
- **Nothing in the cluster network can set a stored IP through a header any more.** Revision 2 trusted the pod network
  (`172.16.0.0/16`), so any pod in any of the cluster's ~45 namespaces, including third-party software (sonarqube, grafana, qdrant,
  databend, milvus-development, the paddler LLM proxy, bedrock-access-gateway), could assert a caller IP (`enr-rev2-pod-cidr-population`).
  With the empty list that is gone. The same holds for the node subnets and the non-node ECS in them (T15), and for any direct
  node-port caller (`enr-rev-nodeport-xff-spoof`, `enr-rev-node-subnet-not-node-only`): their header is ignored, and their own address
  is private, so it is never stored either (H1, H3, P1(b)). What any of them can still do is what every API-key holder can do: put an
  IP in a body. The impact of that is labelled data only, because ingest rate limiting is keyed on the API key, not the IP.
- **The shared 443 listener is owned elsewhere, and nothing depends on it** (`enr-rev2-listener-owned-elsewhere`). Both Sentinel
  ingestor Ingresses sit on a shared listener whose master is another team's Ingress
  (`kubernetes.io/elb.listener-master-ingress: paddler-proxy-production/halyard-paddler-management-production-ingress`, shared by 27
  Ingresses; `kubectl`, 2026-09-24). Its `X-Forwarded-For` handling (append, replace or drop) is set outside this repository and can
  change with no Sentinel change. Since revision 3 no stored value depends on it, because the K8s list is empty and the walk never
  reads the header (§3.4). No RUNBOOK row is added, because there is nothing to troubleshoot. If Phase 4 ever configures a list on this
  listener, it must establish the listener's behaviour then, and record its ownership as a dependency.
- **Web's public listener is owned elsewhere too.** Web's HTTPS listener on the shared public ELB (`f9076b2b…`) has another team's
  master Ingress, `argocd/argocd-public-production` (T13). Its `X-Forwarded-For` insertion is K8s's documented default for shared
  HTTP/HTTPS listeners and cannot be disabled there. The tunnel's rightmost-hop rule still depends on it, so P5(b)/(c) check it end
  to end, and a RUNBOOK row names the owner and the stop (§8.4, §10.3).
- When Phase 4 (`sentinel-sentry-compat-ingest`) exposes a public endpoint, public callers are outside every trusted range, so their
  `X-Forwarded-For` is ignored beyond that endpoint's own proxy, and the public-caller rule still refuses infrastructure addresses.
  These rules are what that endpoint relies on, as the contract requires.
- **The ingestor gains read-write credentials to the environment bucket, and that is an accepted risk, not an open item**
  (decision 13, contract decision 8). The existing OBS key also writes attachments and sourcemaps, and the ingestor is the only
  externally exposed service, so a compromise of an ingestor pod can now overwrite sourcemaps and attachments and the geo object. The
  operator accepted this on 2026-09-24 over creating a read-only OBS key now. It ends at the AWS S3 move, where the ingestor's key
  becomes read-only on `geoip/*` (§8.3), a values-and-secret change with no code.
- **The MaxMind licence key reaches only the CronJob pod.** It is never on the ingestor Deployment (asserted by G6) and never logged.
- **A browser can still claim any `User-Agent`,** exactly as it can to Sentry. The derived contexts are labelled data, not evidence,
  which is why decision 11 keeps the UA forward.

### 11.3 NEEDS_DECISION

None. Contract decision 11 answered the one operator question the second review raised (`enr-rev2-sg-fallback-undecided`: whether to
accept node-subnet trust under a security-group fallback), by removing the design that needed a fallback. Revision 2's remaining
empirical uncertainties (whether the shared ELB appends, whether pod traffic to the VIP keeps the pod address, whether pod-less nodes
fail the health check) no longer bear on any stored value, so they need neither a check nor a decision. The new edge cases this
revision had to settle (a non-object `user`, a browser-sent IP when none is derived, what to write when no IP is derived, whether the
tunnel filters private addresses, N1/N5, the documentation ranges) are each derived in §11.1 decisions 20-23 from decision 11's own
reasons and the existing code. The wording of TASK-01 criterion 1 goes upward as a correction (§1.3); its proof mapping was set by the
coordinator's brief, so it is not a decision left open.

Revision 4 raises no new product question. The two choices the third review needed were made by the coordinator's ruling (the
`user` rebuild and its allow-list, decision 28) or delegated with an explicit rule (the no-derive marker, decision 29, and the
ingestor defence-in-depth recommendation, decision 30). The remaining choices (N11 not public rather than unwrapped, mapped trusted
entries fatal rather than converted, mapped addresses unmapped by the tunnel, a diff rather than a golden file for G8) each picked
between options the reviewer or coordinator offered, and each is derived in §11.1 decisions 31-34.

Revision 5 raises no new product question either. The one choice the fourth review offered (a code rule or a documented exposure for
pre-0.2.0 tunnels) was delegated by the coordinator with "recommend one". The code rule is recommended and specified, and the reason is
derived from the code (§3.9, decision 36).

### 11.4 Revision 3: every change, and why

| # | Change | Sections | Driver |
|---|---|---|---|
| R3-1 | Built against contract `774039e` (decisions 1-11). | header | contract re-pin |
| R3-2 | The tunnel writes the derived IP into each forwarded item's `user.ip_address` (`applyClientIp`, B1-B5), overwrites a browser-sent value, deletes it when none is derived, and forwards no `x-forwarded-for`. The derivation and the rate-limit key change are revision 2's, unchanged. | §2, §4.2, §4.3, §4.4, §9 | contract decision 11; §11.1 decisions 20-22 |
| R3-3 | The ingestor's public-caller rule (N1-N7), applied to derived caller IPs only; `{{auto}}` with no public caller → `""`; geo reuses the predicate. | §2, §3.2, §3.5 (new), §5.3, §9 | contract decision 11; decision 23 |
| R3-4 | `trustedProxyCidrs` is empty and declared explicitly in both K8s overlays; the walk stays in code for Phase 4; the K8s append citations are dropped. | §3.3, §3.4, §3.6 (rewritten), §8.4 | contract decision 11 |
| R3-5 | **Removed:** the `…-ingestor-elb` Service, `externalTrafficPolicy: Local`, ports 31125 / 31318, the `ClusterIP` conversion, the sync-wave annotations and rollout steps, the health-check analysis, the security-group fallback, the `ingestor.elbService.*` values, and the RUNBOOK rows about them. No Service, Ingress, NodePort or load-balancer change remains. | §1.3, §2, old §3.5, §8.4, §9, §10.1 G7/G8, §10.3 | contract decision 11; `enr-rev2-rollout-wave-order`, `enr-rev2-local-pod-churn-blackhole`, `enr-rev2-sg-fallback-undecided`, `enr-rev2-healthcheck-default-unverified`, `enr-rev2-pod-cidr-population` |
| R3-6 | New case tables: C1-C15 with an empty and a configured "Test list", H1-H3 with the overlay's (empty) list, and the `IsPublic` predicate cases. | §3.7 | brief (criterion 2 with an empty and a configured list) |
| R3-7 | Fixtures now carry the IP in the body; a fifth fixture `tunnel-browser-sent-ip.json`; `tunnel-no-ip.json` carries a browser-sent IP that must not survive; the Go replay uses K8s's shape (empty list, node-side peer, ignored header); a new integration case sends every fixture's forward through the real ingestor. | §4.5, §10.2 TASK-06 | brief (TASK-06 proofs) |
| R3-8 | TASK-01 criterion 1 mapped to the tunnel-to-ingestor integration case and its Go replay, with P5 as the post-deploy staging observation once web adopts sdk-js 0.2.0; the criterion's wording for direct callers goes upward. | §1.3, §10.2, §10.3 P5 | brief; decision 24 |
| R3-9 | TASK-01 criterion 2 proved by unit and integration cases, each with an empty and a configured list. | §10.2 | brief; decision 25 |
| R3-10 | P1 rewritten for the new design (direct `{{auto}}` → `""`, a spoofed header ignored, a body IP kept, the empty list logged); revision 2's P1(b)-(e) withdrawn. | §10.3 | contract decision 11 |
| R3-11 | CI drift checks use the pathspec `tests/tunnel-fixtures`; the CI-mode fixture test is the cited red signal in CI. | §4.5, §9, §10.2 TASK-04 and TASK-06 | `enr-rev2-ci-drift-pathspec` |
| R3-12 | G6(b) admits the `SealedSecret`'s `MAXMIND_*` keys; the values test forbids `MAXMIND_*` outside `secrets.sealedData`. Dry-run recorded. | §10.1 G6, §10.2 TASK-05 | `enr-rev2-g6-sealed-keys` |
| R3-13 | pnpm pinned to `10.34.5` in `Taskfile.yml` (`npx -y pnpm@10.34.5`) and in `ci.yml`'s `sdk-js-node` `pnpm/action-setup`, bumped together and held equal by `tests/unit/pnpm_pin_test.go`. | §2, §9, §10.1 | `enr-rev2-npx-pnpm-floating` |
| R3-14 | The shared listener's ownership recorded, noting nothing depends on it; the pod-network population recorded accurately and shown to be no longer trusted. | §11.2 | `enr-rev2-listener-owned-elsewhere`, `enr-rev2-pod-cidr-population` |
| R3-15 | First-review findings re-checked under decision 11 (all still fixed; three by a different mechanism). | §13.3 (numbered §13.2 in revision 3) | brief |

### 11.5 Revision 4: every change, and why

| # | Change | Sections | Driver |
|---|---|---|---|
| R4-1 | `applyClientIp` rebuilds `user` from `USER_KEYS = ['id','username','email']` (exact keys, string values), writes `ip_address` last, and drops `ip_address` / `geo` in every spelling. B2/B4 rewritten. The ingestor gets no fold defence, and the reason is recorded. | §2, §4.4, §9, §11.1 decisions 20, 28, 30, §11.2 | `enr-rev3-casefold-ip-key` (coordinator ruling), `enr-rev3-browser-geo-survives` |
| R4-2 | T28 records Go's case-folding, last-wins decode and the tunnel's exact top-level filter, measured on go1.26.4. | §1.2 | `enr-rev3-casefold-ip-key` |
| R4-3 | Helper cases for `{ip_address, IP_ADDRESS}`, `Ip_Address`, `ip_addreſſ`, folded `GEO`, non-exact and non-string allow-list keys, and `ip = null`. A through-the-route `USER` case. A helper fixture `tunnel-user-rebuild.json`, decoded in Go with `encoding/json` into `[]validation.ErrorPayload` and posted to the real ingestor in G3. | §4.5, §10.2 TASK-06 | `enr-rev3-casefold-ip-key` (the coordinator's case list and Go decode test) |
| R4-4 | The no-derive marker `X-Sentinel-Derive-IP`: sent as `off` on every forward, honoured by `Resolve` before anything else, and effective with any value. Cases M1-M5, a public-peer Go replay of `tunnel-no-ip`, and header-set assertions. | §2, §3.2, §3.8 (new), §4.4, §4.5, §7.2, §9, §10.2, §11.1 decisions 21, 29, §11.2 | `enr-rev3-null-ip-server-address` (coordinator: "can never let the ingestor fill the web server's address") |
| R4-5 | T13 cites K8s: shared-ELB HTTP/HTTPS listeners insert `X-Forwarded-For` by default, and it cannot be disabled. It also names web's listener master Ingress and corrects the backend Service (`example-fe` for `app-staging.example.com`). P5 gains (b) and (c): a spoofed header through web's public host, with a stop rule and a RUNBOOK row. | §1.2 T13, §4.1, §8.4, §10.3, §11.1 decision 35, §11.2 | `enr-rev3-public-elb-append-unverified` |
| R4-6 | G8 is a full-render diff of the ingestor Service and Ingress against `1e5c6e2`, with the dry-run and a mutation recorded. | §10.1, §10.2 TASK-05, §11.1 decision 34 | `enr-rev3-g8-not-render-as-today` |
| R4-7 | `IsPublic` is rebuilt on `IsGlobalUnicast()`, with N8-N11 added (`0.0.0.0/8`, `240.0.0.0/4`, `fec0::/10`, IPv4-embedding IPv6 not public). The predicate table gains zoned, mapped-CGNAT and boundary inputs, and walk cases C18/C19 are added. | §3.5, §3.7, §10.2 TASK-01, §11.1 decision 31 | `enr-rev3-ispublic-range-gaps` |
| R4-8 | An IPv4-mapped trusted entry is a startup error (C16, C17). | §3.3, §3.7, §9, §11.1 decision 32 | `enr-rev3-mapped-trusted-prefix` |
| R4-9 | Canonical IPv6 in the tunnel: a character check, a bracketed `hostname`, mapped → IPv4. `::1]@6.6.6.6:80/?[` and other non-IP cases are added. | §4.2, §4.5, §11.1 decision 33 | `enr-rev3-canonical-ipv6-url` |
| R4-10 | `clientAddress` step 3 reads "`hops` is 0, or there is no header". | §4.2 | `enr-rev3-hops0-step-text` |
| R4-11 | The TASK-01 criterion 1 mutation cites its real red signals: the helper case, the drift check and the CI-mode fixture test. | §10.2 | `enr-rev3-crit1-mutation-signal` |

### 11.6 Revision 5: every change, and why

| # | Change | Sections | Driver |
|---|---|---|---|
| R5-1 | G8 renders the **full** chart per overlay and diffs every `Service`, `Ingress` and `Endpoints` document against `1e5c6e2`. The dry-run is recorded: a scratch `templates/ingestor-elb.yaml` with revision 2's Service gives exit 1, and so do `global.provider=generic` and a changed `nodePort`. The finding's reason for `templates/ingress.yaml` rendering nothing on K8s is corrected: it is the provider guard. | §10.1 G8, §10.2 TASK-05, §11.1 decision 34 | `enr-rev4-g8-template-scoped` |
| R5-2 | The skew is stated: pre-0.2.0 tunnels send no marker. I3t: a `javascript` + runtime `browser` item gets no connection-derived IP. It is recommended over a documented rule, with a recorded residual. The public-peer replay runs in four variants, and the I1-I6 unit table gains I3t rows. The §11.2 guarantee is qualified and then restored for every tunnel version. | header, §3.2, §3.7 M1, §3.8, §3.9 (new), §4.5, §9, §10.2 TASK-01 and TASK-06, §11.1 decision 36, §11.2 | `enr-rev4-pre-marker-tunnel-skew` |
| R5-3 | §4.2 step 3 records that the App Router exposes no socket. The README text says `0` derives no IP there, and what to do instead. The hops-0 test asserts `null` for a plain `Request`, and keeps a separate socket case as the non-Next path. | §4.2, §4.3, §4.5, §9, §11.1 decision 37 | `enr-rev4-hops0-no-socket-under-next` |
| R5-4 | P5 uses `curl -4 -s --noproxy '*'` for `$ME` and for the tunnel posts. P5(a) reads the browser's IPv4 address from an IPv4-only service. | §10.3 P5, §11.1 decision 38 | `enr-rev4-p5-ifconfig-family` |

---

## 12. Findings outside this scope (recorded, not fixed)

| Finding | Classification | Evidence |
|---|---|---|
| ~~`task test-unit` runs neither `apps/ingestor-go/...` package tests nor the sdk-js suite.~~ **Resolved in this change** by contract decision 9 (§10.1, decision 16). | Was a pre-existing gate gap; now in scope by ruling. | `Taskfile.yml:184-190` |
| `task test-unit`'s dashboard lines use whatever `pnpm` is on `PATH` (`11.1.2` on the authoring machine), while `ci.yml`'s dashboard job still floats `pnpm/action-setup` `version: 10`. Whether pnpm 11 also rewrites `apps/dashboard-web/pnpm-lock.yaml` was **not** measured here. Revision 3 pins only the sdk-js pair (`10.34.5`, §10.1). | Pre-existing; outside decision 9, which covers the sdk-js lines only. Reported to the coordinator, not changed. | `Taskfile.yml:188-189`; `ci.yml:285-287` |
| Every other NodePort Service in the cluster is `externalTrafficPolicy: Cluster` behind the same shared ELB, so any of them that trusts `X-Forwarded-For` from a node address has the direct-to-NodePort spoof. This spec avoids it for the ingestor by trusting no range on K8s (§3.6). | Unrelated pre-existing exposure in other workloads. Recorded, not investigated. | `kubectl get svc -A` (2026-09-24) |
| RUNBOOK §1's encryptor example seals with `"secretName":"sentinel-secrets"`, while the chart's Secret and both overlays' comments say `sentinel-sentinel-secrets`. A strict-scope seal against the example name would not decrypt. | Pre-existing documentation defect. TASK-07's operator should seal with `sentinel-sentinel-secrets` (the card already says so). | `RUNBOOK.md:36-39`; `templates/_helpers.tpl:35-41`; `values-k8s-staging.yml:62-63` |
| The tunnel deletes every schema-v2 key (`user`, `tags`, `contexts`, `request`, `breadcrumbs`, `exception`, …) from browser events. Schema v2's fields are unreachable from any tunnelled browser event until `ITEM_KEYS` widens. When it widens to keep `user`, `applyClientIp` must still run **after** the filter, and its B2/B4 cases (§4.4, already unit-tested) become live. The top-level filter must stay exact-key (T28). The same case-folding hazard applies to any other `user`-like object a widened filter passes through. `contexts.browser` / `os` are safe, because the ingestor decodes `Contexts` as a map, which is exact-key, and never overwrites a sent context (§6.3). | Pre-existing; belongs to `sentinel-sdk-js-rich-context`. | `route.ts:64-79` |

---

## 13. Review findings and where each was fixed

Closing a finding is the reviewer's act, not this document's. §13.0 names what revision 5 changed. §13.1 names what revision 4 changed. §13.2 and §13.3 name what revision 3
changed, and revision 4 left those repairs in place.

### 13.0 Fourth spec review (against revision 4, `dd010ed`)

| Finding | Severity | Repair in revision 5 | Sections changed |
|---|---|---|---|
| `enr-rev4-g8-template-scoped` | minor | Fixed as the reviewer wrote, and widened by one kind. G8 applies the `kind` tracker to the **full** render (no `--show-only`) and keeps every `Service`, `Ingress` and `Endpoints` document from any template file, diffed against `1e5c6e2`. Dry-run: exit 0 unchanged; **exit 1** with a scratch `templates/ingestor-elb.yaml` holding revision 2's `…-ingestor-elb` Service, the reviewer's exact case; exit 1 with `templates/ingress.yaml` made to render (`global.provider=generic`); exit 1 with a changed `nodePort`. §10.2's mutation column now cites the new-template case. | §10.1 G8, §10.2 TASK-05, §11.1 decision 34 |
| `enr-rev4-pre-marker-tunnel-skew` | note | **Fixed in code, the recommended option.** The skew is stated (§3.8, §3.9). I3 no longer fills a `javascript` item whose `metadata.runtime` is `browser` (I3t), because that runtime reaches the ingestor only through a tunnel: the browser build has no `apiKey` / `endpoint`, and the ingestor serves no CORS. So no tunnel version can get its server's address stored as a browser's. The reviewer's documented-rule option was not chosen, because it fails in silence (decision 36). It is kept as an optional mitigation for the one recorded residual, a browser rewriting its own runtime behind a pre-0.2.0 tunnel. The §11.2 guarantee is qualified and restored. The unit table and the public-peer replay test I3t and the marker each alone. | header, §3.2, §3.7, §3.8, §3.9, §4.5, §9, §10.2, §11.1 decision 36, §11.2 |
| `enr-rev4-hops0-no-socket-under-next` | note | Fixed as the reviewer wrote. The README text says `0` yields an IP only where the runtime exposes the socket, and **not** under the Next App Router, where it derives no IP and the default rate-limit key becomes the shared bucket. It also says why no hop count is trustworthy with no proxy in front (Next's `x-forwarded-for ??=`). The hops-0 test asserts no `user` for a plain `Request` with no `ip` or socket. The socket case is kept, labelled as the non-Next path. | §4.2, §4.3, §4.5, §11.1 decision 37 |
| `enr-rev4-p5-ifconfig-family` | note | Fixed as the reviewer wrote: `ME=$(curl -4 -s --noproxy '*' https://ifconfig.me)`, and both tunnel posts use `curl -4 -s --noproxy '*'`. P5(a) reads the browser's address from the IPv4-only `api.ipify.org`. | §10.3 P5, §11.1 decision 38 |

### 13.1 Third spec review (against revision 3, `f62e06a`)

| Finding | Severity | Repair in revision 4 | Sections changed |
|---|---|---|---|
| `enr-rev3-casefold-ip-key` | major | **Fixed per the coordinator's ruling.** `applyClientIp` no longer edits `user` in place. It rebuilds `user` from the exact keys `id`, `username` and `email` (string values only, the schema-v2 `UserInfo` string fields), then writes `ip_address` itself when it derived one. So no key that Go's case-folding, last-wins decoder reads as `ip_address` or `geo` can survive (T28, measured on go1.26.4 with the reviewer's three inputs). Helper cases for `{ip_address, IP_ADDRESS}` (both the reviewer's `""` shape and both set), `Ip_Address`, `ip_addreſſ` and a folded `GEO`, with and without an IP. A helper fixture `tunnel-user-rebuild.json` carries the helper's real output, which a Go test decodes with `encoding/json` into `[]validation.ErrorPayload`, asserting the stored IP is the tunnel's. G3 posts the same outputs to the real ingestor. **Ingestor defence in depth: none, with the reason** (decision 30): exact-key decoding breaks the compatibility rule for direct callers, rejecting duplicates turns accepted batches into 400s, and neither removes a power a key holder lacks. | §1.2 T28, §2, §4.4, §4.5, §9, §10.2 TASK-06, §11.1 decisions 20, 28, 30, §11.2, §12 |
| `enr-rev3-null-ip-server-address` | minor | **Fixed with the no-derive marker** (coordinator: pick a rule where this can never happen). Every tunnel forward carries `x-sentinel-derive-ip: off`, and `Resolve` returns no caller IP for any request carrying the header, before it reads the peer (§3.8). A tunnelled item with no IP is therefore never filled from the web server's address, on any deployment. The reviewer's `trustedProxyCidrs` rule is kept as a tested fact (M5) but is not the guarantee, so the RUNBOOK and README do not require it (**recorded deviation**, from the coordinator's instruction). The reviewer's unit case is added in both forms: M1, a client-platform item with no IP behind a public untrusted peer, stores the peer; M5, with that peer trusted, stores none; and M2, with the marker, stores none. There is also a public-peer Go replay of `tunnel-no-ip`, which G3's loopback peer could not show. | §2, §3.2, §3.7, §3.8, §4.4, §4.5, §7.2, §10.2, §11.1 decisions 21, 29, §11.2 |
| `enr-rev3-public-elb-append-unverified` | minor | **Cited, and checked.** K8s documents that a shared load balancer's HTTP/HTTPS listener transfers the client IP in `X-Forwarded-For`, "Enabled by default", and that disabling it is "Not supported" (`elb_ug_jt_0515`; `elb_faq_0090`). Web's listener is on a shared ELB (`elb.class: union`, T13). K8s does not say append versus replace, and the rightmost-hop rule gives the ELB's entry either way. The listener's master is another team's Ingress (`argocd/argocd-public-production`), so the reviewer's P5 case is added as P5(b), with a two-entry variant (c): a spoofed `X-Forwarded-For` through `https://app-staging.example.com/api/sentinel` must store the curl host's public IP. On failure, web's 0.2.0 adoption stops or rolls back, and a RUNBOOK row says who owns the fix. T13's backend Service is also corrected (`example-fe`). | §1.2 T13, §4.1, §8.4, §10.3 P5, §11.1 decision 35, §11.2 |
| `enr-rev3-g8-not-render-as-today` | minor | Fixed as the reviewer wrote. Per overlay, G8 renders `templates/ingestor-ingress.yaml` and the Service document from `templates/ingestor.yaml` (the `awk` kind tracker), on this change and on `git archive 1e5c6e2`, and requires an empty `diff`. Only the two `Chart.yaml`-derived version labels are excluded. Dry-run: exit 0 on both overlays, and exit 1 with a changed `nodePort` and host. | §10.1 G8, §10.2 TASK-05, §11.1 decision 34 |
| `enr-rev3-ispublic-range-gaps` | note | Fixed as the reviewer wrote. `IsPublic` is built on `IsGlobalUnicast()` (which excludes broadcast), with `0.0.0.0/8`, `240.0.0.0/4` and `fec0::/10` added, and `::/96`, `64:ff9b::/96`, `64:ff9b:1::/48` and `2002::/16` **not public** (the reviewer's first option, chosen because it is conservative, decision 31). The predicate table gains every input the reviewer listed, plus zoned `fe80::1%eth0` and mapped CGNAT `::ffff:100.125.64.17`, and boundary cases. The go1.26.4 measurements are recorded. | §3.5, §3.7, §10.2 TASK-01, §11.1 decision 31 |
| `enr-rev3-mapped-trusted-prefix` | note | Fixed with the reviewer's second option: an IPv4-mapped entry (prefix or bare address) is a startup error naming the entry, and C16/C17 cover it. Conversion was rejected because a mapped prefix shorter than `/96` has no IPv4 form. | §3.3, §3.7, §11.1 decision 32 |
| `enr-rev3-canonical-ipv6-url` | note | Fixed as the reviewer wrote. `/^[0-9A-Fa-f:.]+$/` is checked before parsing, and a bracketed `hostname` is required after. The spec now says a mapped address **is** unmapped to dotted IPv4 (`::FFFF:C633:6417` → `198.51.100.23`), matching the ingestor's stored form. `::1]@6.6.6.6:80/?[` is added to the non-IP cases, and the node measurement is recorded. | §4.2, §4.5, §11.1 decision 33 |
| `enr-rev3-hops0-step-text` | note | Fixed as the reviewer wrote: step 3 reads "`hops` is 0, or there is no header: return the socket address if it is a valid IP, else `null`". | §4.2 |
| `enr-rev3-browser-geo-survives` | note | **Resolved by the `enr-rev3-casefold-ip-key` rebuild** (the coordinator's ruling). `geo` is never in the allow-list, so a browser-sent `geo` is dropped in every spelling, and a tunnelled event's geo comes only from the ingestor's lookup of the tunnel-written IP. There are helper cases (`geo`, `GEO`) and fixture cases (`browser-geo`, `no-ip-folded`) decoded in Go and stored through G3. | §4.4, §4.5, §5.3, §11.1 decision 28 |
| `enr-rev3-crit1-mutation-signal` | note | Fixed as the reviewer wrote. The TASK-01 criterion 1 row now cites the `applyClientIp` `{{auto}}` helper case, the gate's drift check and the CI-mode fixture test as the red signals. G3 and the Go replay turn red only once a regenerated fixture is committed. | §10.2 |

### 13.2 Second spec review (against revision 2, `2deca8b`)

| Finding | Severity | Repair in revision 3 | Sections changed |
|---|---|---|---|
| `enr-rev2-rollout-wave-order` | major | **Resolved by removal.** Contract decision 11 withdraws the `…-ingestor-elb` Service, the Ingress backend move, the `ClusterIP` conversion and the sync-wave ordering. With no Service or Ingress change there is no switch-over, so there is no window in which ports close while the ELB still sends to them, and no rollback ordering to get wrong. G8 now asserts that the ingestor Service and Ingress render exactly as today. | §2, old §3.5 (removed), §3.6, §8.4, §9, §10.1 G8, §10.3, §11.1 decision 17 |
| `enr-rev2-local-pod-churn-blackhole` | major | **Resolved by removal.** `externalTrafficPolicy: Local` is gone (contract decision 11), so kube-proxy keeps rerouting under `Cluster` as today, and a node losing its last ingestor pod drops nothing. The cost moves to where decision 11 puts it: the ingestor believes no header on K8s, and the browser's IP arrives in the body from the tunnel. | §2, §3.6, §4.4, §8.4, §10.3, §11.1 decisions 17 and 20 |
| `enr-rev2-sg-fallback-undecided` | minor | **Resolved by removal.** No fallback exists, because nothing it would fall back from exists. Contract decision 11 is the operator's ruling that the finding asked for, and it keeps the node subnets untrusted (every range is untrusted). No `infra` change is needed, and §11.3's "none" is now true. | §3.6, §10.3, §11.1 decision 17, §11.3 |
| `enr-rev2-healthcheck-default-unverified` | minor | **Resolved by removal.** The health-check analysis existed only for `Local`. No backend group, health check or listener setting changes, so there is nothing to record before a switch. | old §3.5 (removed), §10.3 (P1(d) withdrawn) |
| `enr-rev2-ci-drift-pathspec` | minor | Fixed as the reviewer wrote. CI's `sdk-js-node` job runs in `packages/sdk-js`, so its drift checks use the pathspec `tests/tunnel-fixtures`, with a comment saying why. §10.2 cites the CI-mode fixture test as the red signal in CI; the git checks there are a backstop. | §4.5, §9, §10.1 (S0 note), §10.2 TASK-04 and TASK-06 rows, §11.1 decision 26 |
| `enr-rev2-g6-sealed-keys` | minor | Fixed with the reviewer's first option: G6(b) counts `MAXMIND` matches outside the CronJob **and** the `SealedSecret`. Plaintext is asserted on the values files instead (`helm_geoip_test.go`: no `MAXMIND_*` outside `secrets.sealedData`, no plaintext key anywhere). Dry-run: with a sealed key set, revision 2's `awk` printed `1` on both overlays; revision 3's prints `0`. | §10.1 G6 and dry-run note, §10.2 TASK-05 row, §11.1 decision 27 |
| `enr-rev2-listener-owned-elsewhere` | note | Recorded in §11.2: the shared listener's master Ingress is named, and **nothing now depends on the listener's `X-Forwarded-For` behaviour**, because the K8s list is empty and the walk never reads the header. So no RUNBOOK row is added (there is no symptom to troubleshoot), and revision 2's one-time P1(c) is withdrawn. Phase 4 must establish the listener's behaviour if it ever configures a list on it. | §3.4, §3.6, §10.3, §11.2 |
| `enr-rev2-pod-cidr-population` | note | **Resolved by removal.** The pod network is no longer trusted, so no pod in any of the ~45 namespaces can set a stored IP through a header. §11.2 names the population the finding listed, and says what remains: any API-key holder can put an IP in a body, with impact limited to labelled data. | §3.6, §11.2 |
| `enr-rev2-npx-pnpm-floating` | note | Fixed as the reviewer wrote: pnpm `10.34.5` pinned exactly in `Taskfile.yml` (`npx -y pnpm@10.34.5`) and in `ci.yml`'s `sdk-js-node` `pnpm/action-setup` (`version: 10.34.5`), bumped together. `tests/unit/pnpm_pin_test.go` fails if either floats or they differ. Revision 2's floating-major argument is withdrawn in §10.1. | §2, §9, §10.1, §11.1 decision 16, §12 row 2 |

### 13.3 First spec review (against revision 1, `56dfdf6`): re-checked under contract decision 11

| Finding | Severity | Revision 2 fix | Status under revision 3 |
|---|---|---|---|
| `enr-rev-nodeport-xff-spoof` | major | `…-ingestor-elb` with `Local`; node subnets untrusted; C13, P1(b). | **Still fixed, by a different mechanism.** A direct node-port caller's `X-Forwarded-For` is ignored because the K8s list is empty, and its own address is private, so it is never stored (§3.6; H1; P1(b)). The `Local` Service is removed (contract decision 11). |
| `enr-rev-tunnel-no-gate` | major | The unit gate widens to the ingestor packages and the sdk-js suite under pnpm 10, with a drift check. | **Still fixed**, and tightened: pnpm pinned to `10.34.5` in both places (`enr-rev2-npx-pnpm-floating`), and the CI pathspec corrected (`enr-rev2-ci-drift-pathspec`). |
| `enr-rev-elb-append-unverified` | minor | K8s docs cited; P1(c) as the in-cluster append proof. | **Moot, and closed by the design change.** Nothing depends on whether the shared ELB appends: the empty list means the header is never read (§3.4). The citations and P1(c) are removed. |
| `enr-rev-node-subnet-not-node-only` | minor | Node subnets untrusted; T15 records the non-node ECS; C14. | **Still fixed.** No subnet is trusted at all. The ECS's header is ignored and its address is private (H3), so it can set a stored IP only as any API-key holder can, in a body (§11.2). |
| `enr-rev-geo-fixture-stale` | minor | Injectable `Now` / `MaxAge`; second fixture `GeoIP2-City-Test.mmdb`; stub `GeoLookup`. | Unaffected; still fixed. |
| `enr-rev-md5-metadata-case` | minor | Case-insensitive metadata lookup; test (b) through a real minio-go client. | Unaffected; still fixed. |
| `enr-rev-ingestor-rw-bucket-key` | minor | Accepted risk (contract decision 8). | Unaffected; still recorded as a decision. |
| `enr-rev-g6-grep` | minor | G6 `awk` kind tracker plus the `secretKeyRef` window check. | Still fixed, and extended for the `SealedSecret` (`enr-rev2-g6-sealed-keys`). |
| `enr-rev-zero-download-test-decoration` | minor | `go list -deps` import assertion. | Unaffected; still fixed. |
| `enr-rev-t17-configmap` | note | T17 says "no S3 credential". | Unaffected; still fixed. |
| `enr-rev-hops-default-adopters` | note | README/CHANGELOG rule for `trustedProxyHops`; hops-0 test. | **Still fixed, and now more important:** the tunnel's derivation is the trust boundary for browser IPs (§11.2). §4.3's wording is updated: a wrongly-defaulted adopter's client-chosen IP is stored because it arrives in the body. |
| `enr-rev-latency-wallclock-test` | note | Counting fake object source; zero S3 calls. | Unaffected; still fixed. |
| `enr-rev-ipv6-trusted-list` | note | IPv4-only established; P1(e). | **Moot.** The K8s list is empty, so it has no prefix of either family to be missing. The public-caller rule is family-agnostic (N2-N5 cover `::1`, `fc00::/7`, `fe80::/10`, `ff00::/8`). T15a stays as a fact; P1(e) is withdrawn. |
