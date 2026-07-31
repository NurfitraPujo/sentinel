# Observability Plan (P9-2) — v2, after adversarial review

> [!IMPORTANT]
> **Status: IMPLEMENTED and verified 2026-07-31.** Every work package (W0–W4) landed. The acceptance bar
> in §7 is met and gated by matrix row **U35** (`tests/e2e/tracing_test.go`), which asserts one trace
> containing spans from both Go services, read back out of the trace backend.
>
> - What it actually does at runtime, with the commands that proved it: `docs/memory/VERIFIED_STATE.md`.
> - The decisions, as decisions: `docs/memory/DECISIONS.md` **D15**.
> - Why this needed a deployment-level test at all: `docs/memory/BUGS.md` **B11** — OTel's
>   missing-registration failure mode is *silence*, not an error.
> - Findings deliberately left unfixed: **P9-5** in [E2E_RECOVERY_PLAN.md](E2E_RECOVERY_PLAN.md).
>
> Each work package was reviewed adversarially by a separate reviewer that reproduced findings at
> runtime rather than reading the diff. That review found five defects worth fixing — including an
> unauthenticated cardinality-growth vector on the public ingest port, a `target_info` leak of argv and
> hostname, the DLQ severing the trace in both directions, and magic-link tokens reaching the trace
> backend from the dashboard. All were fixed before this plan was marked done. The text below is the
> plan as written; it has NOT been rewritten to match the outcome, so the two can be compared.

*Originally: v2 draft. v1 proposed request-ID correlation and deferred OpenTelemetry; the review below
found real flaws in v1 beyond that, and the deferral itself was overruled: **OTel is in scope for this
pass.** Executes P9-2 of [E2E_RECOVERY_PLAN.md](E2E_RECOVERY_PLAN.md); the P9-2 acceptance bar is
restated in §7 and extended, not weakened.*

## 0. Review of v1 — what was wrong with it

Findings from reviewing v1 exhaustively, most consequential first. Kept because the *reasons* are what
stop the next plan repeating them.

1. **v1's central trade-off (D-b, "IDs now, OTel later") bought the wrong thing.** It optimized for a
   small diff, but the expensive part of this work is not the OTel SDK — it is touching all 80+ log call
   sites, changing the subscriber's handler signature, and threading context through the pipeline. v1 did
   ALL of that work and then attached only a homemade ID to it. Doing it twice — once for IDs, again for
   OTel — would mean paying the ripple cost twice, including a second signature change through every test
   double. The marginal cost of real OTel *on top of the plumbing v1 already required* is small; the cost
   of retrofitting it later is the whole ripple again. Verdict: v1 had the cost model backwards.
2. **v1 invented a parallel standard.** `X-Sentinel-Request-Id` over NATS is a bespoke correlation scheme
   when W3C `traceparent` exists, is what the SDK-side `ErrorEvent.trace_id` already speaks (D8's
   context-aware telemetry correlation), and is what every future tool understands. Bespoke correlation
   headers are how a codebase ends up with three of them.
3. **v1's metrics choice created two instrumentation APIs.** Prometheus `client_golang` for metrics plus
   (eventual) OTel for traces means every instrumented function imports two ecosystems. The OTel metric
   SDK with a Prometheus *exporter* keeps the scrape endpoint identical while using one API for both
   signals.
4. **v1 had no answer for "where do I look?"** Logs with request IDs still require grepping two
   containers. A trace UI is the difference between correlation existing and correlation being usable.
   v1 shipped the former and called it observability.
5. **v1's U35 asserted less than it could.** Echoing a header and grepping a log proves propagation into
   *logs*; querying the trace backend for one trace containing spans from BOTH services proves the whole
   pipeline — collector reachable, exporters working, context propagated over NATS. That is the assertion
   worth having, and v1 could not express it.
6. **Smaller real defects in v1, all carried into v2's design:**
   - No shutdown/flush story: without `TracerProvider.Shutdown` on SIGTERM, the processor's last spans
     (exactly the interesting ones during an incident) are dropped.
   - No degradation story: services must come up and stay up when the collector is absent; export
     failures are a warning, never a crash and never backpressure on ingest.
   - No version constraint noted: `go.mod` already pins `otel v1.43.0` transitively via testcontainers;
     direct deps MUST align with it or the module graph fights.
   - Host ports for the trace backend were unconsidered; on this machine other stacks squat on default
     ports (the NATS 4222 lesson). 4317/4318/16686 are verified free *today*, but they get the same
     `${VAR:-default}` parameterization as NATS anyway.
   - v1 forgot `sentinel_ingest_publish_failures_total` had a natural twin in the dashboard's
     `api_key.invalidated` publish failure — the exact counter that would have caught the NATS_URL gap.
     Out of scope for Go metrics, but the dashboard's structured log line for that failure is now named
     explicitly in W3.

What survives from v1 unchanged: slog (D-a), no per-project metric labels (cardinality), the fixed metric
name table, W0-first sequencing with contracts before fan-out, `tools/dlq`/migrations CLIs keeping plain
stdout, and the standing agent rules.

## 1. Decisions (v2)

**D-a: `log/slog`.** Unchanged from v1. JSON in containers, `LOG_FORMAT=text` locally. Every line carries
`service`, and `trace_id`/`span_id` when a span is in context — via a slog.Handler wrapper, so call sites
do not thread IDs by hand.

**D-b (reversed): full OpenTelemetry SDK in both Go services, this session.**
- **Traces**: `otelhttp` wraps the ingestor's handlers; a producer span wraps the NATS publish with
  context **injected into NATS headers via the W3C TraceContext propagator**; the processor's subscriber
  extracts it and opens a consumer span, with child spans for deserialize/store/index/alert-dispatch.
  Sampler `parentbased_always_on` (env-overridable via standard `OTEL_TRACES_SAMPLER`) — dev traffic is
  tiny; sampling is a production concern with standard knobs already.
- **Metrics**: OTel meter API with the **Prometheus exporter** — `/metrics` scrape endpoints exactly as
  v1 promised (ingestor `:8080/metrics`, processor `:8081/metrics`), one instrumentation API (finding 3).
- **Versions pinned to the transitive baseline**: `otel v1.43.0` / `contrib v0.68.0` (finding 6c).
- **Degradation**: OTLP exporter with short timeouts; collector absent ⇒ one warning line, service fully
  functional (finding 6b). `OTEL_SDK_DISABLED=true` honoured for anyone who wants it off.
- **Shutdown**: providers flushed on SIGTERM with a bounded context (finding 6a).

**D-c: Jaeger all-in-one as the trace backend in compose.** Accepts OTLP directly (4317 gRPC / 4318 HTTP),
UI on 16686, one container, no separate collector until there is a reason for one. Host ports
parameterized `${JAEGER_*_HOST_PORT:-…}` per the NATS lesson. It is dev/CI infrastructure, not a
production topology decision.

**The upgrade path is free, by construction.** Services emit standard OTLP and standard Prometheus
scrape format — nothing Jaeger-specific. Consequences, so nobody re-litigates this later:
- A standalone **otel-collector** can be inserted in front at any time (`services → collector → Jaeger`
  over OTLP) for fan-out, tail sampling, or multi-backend shipping — a compose change, zero code change.
  Modern Jaeger (v2) is itself a distribution of the OTel Collector framework, and the collector's old
  Jaeger-specific exporter was removed in favour of exactly this OTLP path.
- **Grafana** sits on top whenever wanted: it has a native Jaeger data source for traces, and reads any
  Prometheus that scrapes our `/metrics`. Swapping Jaeger for Grafana **Tempo** (also OTLP-ingesting) is
  likewise a compose-file decision, not an application one.

**D-d: correlation ID = the OTel trace ID.** No bespoke request-id scheme (finding 2). The ingestor
returns the trace ID to the caller as `X-Request-Id` (human-friendly name, standard value); an inbound
W3C `traceparent` is honoured, making the pipeline's trace a child of the caller's. The `ErrorEvent`'s
own `trace_id` field keeps its D8 meaning — the *caller's* trace — logged alongside, never conflated.

**D-e: the wire contract is `traceparent` in NATS headers.** Proto untouched, SDK untouched, missing
header degrades to a new root trace. The propagator carrier is the only bespoke code, and it lives in
`packages/shared-go/obs` next to its constants.

**D-f: dashboard scope.** `@opentelemetry/sdk-node` with OTLP export and http/pg auto-instrumentation in
the SvelteKit server, plus structured request logs carrying the trace id. If the JS SDK fights the
SvelteKit build (a known risk, §8), the fallback for this pass is: honour/emit `traceparent` on inbound
requests, structured logs with trace ids, **no spans** — recorded in P9 as a follow-up, not silently
dropped. The `api_key.invalidated` publish-failure log line becomes structured with a fixed `event` key
(finding 6e) so it is finally alertable-by-grep.

## 2. Contracts (W0 owns these; exported constants, both sides import — the B5 rule)

```go
// packages/shared-go/obs
const HTTPRequestIDHeader = "X-Request-Id"   // response header; value = hex trace id
const LogKeyTraceID  = "trace_id"            // slog attr, injected by the handler wrapper
const LogKeySpanID   = "span_id"
const LogKeyService  = "service"
const LogKeyEvent    = "event"               // fixed machine-greppable event names
// NATS propagation uses the W3C standard header ("traceparent") via the TraceContext propagator;
// the carrier adapter for nats.Header lives here. No custom header name to drift.
```

Metric names as in v1's table (they were right), emitted through the OTel meter:
`sentinel_ingest_requests_total{outcome}`, `sentinel_ingest_publish_failures_total`,
`sentinel_process_duration_seconds` (explicit buckets 5ms→10s),
`sentinel_process_events_total{outcome}`, `sentinel_dlq_depth`, `sentinel_dlq_publish_failures_total`,
`sentinel_alert_dispatch_total{channel,outcome}`.

## 3. Work packages

**W0 (me, alone, first): contracts + plumbing.** `obs` package: slog setup, trace-aware handler wrapper,
provider bootstrap (traces+metrics, resource attrs, flush-on-shutdown, degradation), NATS carrier;
`Publisher` gains a header-carrying publish; `Subscriber` handler signature becomes
`func(ctx context.Context, data []byte, headers nats.Header) error` — every call site and test double in
`tests/unit` + `tests/integration` audited in the same change (B4). Jaeger service in compose. Nothing
fans out until `GOWORK=off go vet ./...` and the unit suite are clean.

**W1: ingestor** — otelhttp, producer span + inject on publish, `X-Request-Id` response header, 16 log
sites to slog, metrics, `/metrics`. Typed context keys only (R1).

**W2: processor** — extract + consumer span in the subscriber path, child spans around store/index/
dispatch, 64 log sites, metrics incl. `sentinel_dlq_depth` from the existing DLQStats, `/metrics` on
`:8081`.

**W3: dashboard** — per D-f.

**W4 (me): the proving row.** **U35**: POST `/ingest` with a caller-supplied `traceparent`; assert
(a) `X-Request-Id` in the response equals that trace id; (b) the processor logged a line carrying it for
the same event; (c) **Jaeger's API returns one trace containing spans from BOTH `ingestor-go` and
`processor-go`** (finding 5); (d) both `/metrics` endpoints parse and `sentinel_ingest_requests_total`
moved. Plus matrix + docs updates.

W1–W3 parallel after W0, disjoint services, standing rules (no containers, no `tests/e2e/`, no files
outside their service; I rebuild once and run everything).

## 4. Log-migration ground rules (the strings tests grep for are load-bearing)

U20 greps ingestor output for `refus`/`disab`/`unreachable`; U26/U27 grep processor logs for
`Email attempt` / `Email sent successfully` / `Email failed after` / `reloaded alert configs after
alert_config.changed`; the DLQ path's `DEAD-LETTER` lines are diagnostic surface. Migrating a call site
keeps its message text; structure is added via attrs, not by rewording. Each brief lists these verbatim;
changing one is a test break, not a style choice.

## 5. Explicitly out of scope (decided, not forgotten)

Log shipping/aggregation (compose logs remain the sink); dashboard `/metrics`; per-project metric labels;
sampling policy beyond env defaults; `tools/dlq` and migration-CLI stdout (operator interfaces);
production trace-backend topology (Jaeger all-in-one is a dev/CI tool).

## 6. Sequencing note

The subscriber signature change is the ripple point (v1 finding, unchanged): it is why W0 is solo and
first, and why it lands with the full test sweep before any parallel agent exists to collide with.

## 7. Acceptance

P9-2's bar, extended by the OTel mandate:

> `slog` with request IDs across ingestor/processor/dashboard; `/metrics` on the Go services exposing at
> least ingest rate, processing latency, DLQ depth and publish failures; one e2e row asserting the
> correlation id propagates from `/ingest` to the processor's log line for the same event — **and** that
> one trace with spans from both Go services is retrievable from the trace backend.

Done means: U35 green; existing 61 e2e + 282 unit + integration all green; `GOWORK=off go vet ./...`
clean; `grep -rn "log\.\(Print\|Fatal\)" apps/ingestor-go apps/processor-go` returns only `main()`'s
fatal-on-boot lines (verified by running it, not promised).

## 8. Risks

- **OTel JS vs the SvelteKit build** (D-f) — fallback defined above; the decision point is W3's, with the
  fallback recorded in P9 if taken.
- **Version skew**: all direct OTel deps pinned to the transitive `v1.43.0`/`v0.68.0` baseline; `go mod
  tidy` diff reviewed in W0, not discovered in CI.
- **Exporter backpressure**: batch span processor with drop-on-full — ingest latency must never wait on
  the collector; U19's rate-limit timings would catch a regression here, which is a nice accident.
- **CI cost**: one more container in the e2e job (~200MB image). If job time grows past ~2min extra,
  Jaeger gets a CI-only memory-limited config before anything else is considered.
- **Two metric registries confusion**: the processor's existing `/health` JSON keeps its fields (tests
  decode them); `/metrics` is additive. Nothing migrates off `/health` in this pass.
