# Changelog

All notable changes to `packages/sdk-go` are documented here.

## v0.4.0 — Rich context: typed fields, breadcrumbs, `BeforeSend`, `HTTPMiddleware`

### Added

- **Typed wire fields.** `Event` gains `Level`, `SDK` (`{Name: "sentinel.go", Version: "0.4.0"}`), `ServerName`,
  `Transaction`, `User *UserInfo`, `Tags map[string]string`, `Contexts map[string]map[string]interface{}`,
  `Request *RequestInfo` and `Breadcrumbs []Breadcrumb`. `NewEvent` fills `Level` (`"error"`), `SDK`, `ServerName`
  (from `Config.ServerName`, else a cached `os.Hostname()`), per-event `runtime`/`os`/`device` contexts, and
  non-nil `Tags`/`Metadata`.
- **Context helpers**: `WithUserInfo`, `WithLevel`, `NormalizeLevel`, `WithBreadcrumbBuffer`, `AddBreadcrumb`,
  `BreadcrumbsFromContext`, `UserFromContext`, `TagsFromContext`, `MetadataFromContext`, and `ApplyContext(ctx,
  ev)` — the one call that copies everything a context carries onto an event, on both the `Client` path and the
  `NewEvent` + `Transport.Push` path core uses.
- **`Config.BeforeSend func(*Event) *Event`** — runs after every automatic field is set (including the
  middleware's `request`/`transaction`); returning `nil` drops the event; a panic is recovered, logged and also
  drops the event.
- **`NewHTTPMiddleware(MiddlewareOptions)`** — `HTTPMiddleware` is now `NewHTTPMiddleware(MiddlewareOptions{})`.
  Fills `request` (method, url, query_string, filtered headers), `transaction` (the `ServeMux` route pattern on
  Go 1.23+, else the request path) and, on `MiddlewareOptions.CaptureClientIP` opt-in only, `user.ip_address`
  from `r.RemoteAddr`. Allocates only a scope and a breadcrumb buffer header eagerly; the request snapshot and
  client IP are built once, lazily, by the first capture.
- **`sentinelzerolog.LevelFromZerolog(zerolog.Level) string`** — maps zerolog's level enum to Sentinel's typed
  levels; the hook now sends a typed level via `CaptureErrorContext(WithLevel(...))` instead of writing
  `zerolog_level` into metadata, and fires on `Error`/`Fatal`/`Panic` only.

### Moved: metadata → typed fields

`user_id` → `user.id`; `tenant_id` → `tags.tenant_id`; other `WithTag` scalars → `tags`; `http_method`,
`http_url`, `http_user_agent`, `http_remote_addr` → `request` (no longer written to metadata at all). Stays in
metadata: the metadata-routed set (`fingerprint`, `error_class`, `release_version`, `request_id` and its
aliases, `trace_id`/`span_id` and aliases, `userId`/`user`, `tenantId`/`organization_id`, `runtime`,
`server_truncated`/`server_truncated_keys`, `truncated`/`truncated_keys`), every `WithContextMap` key, every
non-scalar `WithTag` value, and `panic`/`panic_stacktrace`.

**Tags are no longer free-form metadata.** Every `tags` entry is rolled up per issue into a tag facet
(`issue_tag_values`: one row per `(issue, key, value)`), shown in the tag panel and used by search by tag. A tag
whose value differs per request — a request id, an order or session id, a timestamp — creates one facet row per
request and makes the panel useless. Put per-request identifiers in `WithContextMap` (metadata); keep `WithTag`
for low-cardinality values (region, plan, feature flag). `request_id`/`requestId`/`request-id` passed to
`WithTag` stay in metadata for this reason, and for search by request id.

What is now **dropped client-side** where v0.3.x kept it in metadata: a `WithTag` scalar whose key fails
`^[a-zA-Z0-9_.:-]{1,32}$`, or whose value is empty, over 200 characters or multi-line, still goes to metadata as
in v0.3.x; a scalar is judged by its kind, so a named type such as `type Region string` is a tag. What is
dropped: an invalid entry assigned directly to `Event.Tags` (or by `BeforeSend`), an invalid `WithTenant` id, and,
over 50 tags, every tag beyond the 50 kept (`tenant_id` when present, plus the 49 lexically smallest others) —
v0.3.x kept all of those in metadata. A `WithTenant` id is never cut by the 50-tag cap.

The last write to a key still wins, across tags and metadata as on v0.3.x's one map: a `WithTag` or
`WithContextMap` write removes the key from the other store, so a tag later overwritten with a value that
cannot be a tag (empty, over 200 characters, non-scalar), or a `WithContextMap` key later set with `WithTag`,
reaches the wire once, with the last value. `WithTag(ctx, key, nil)` (or a nil pointer) removes the key from
both — v0.3.x sent it as `null` in metadata; under `user_id` it also clears the user id set with `WithUser`. One
exception: `WithContextMap({"user_id": …})` does not clear a user set with `WithUser`/`WithTag("user_id", …)`,
and Sentinel shows `user.id` first.

### Compatibility

Every existing function signature is unchanged, but `Event` and `Config` gain fields, so **unkeyed composite
literals** of either (`sentinel.Event{"id", "key", …}`) stop compiling and must be rewritten with field names
(`go vet`'s `composites` check already flags them); keyed literals and every other use compile unchanged.

### `transaction` and your `go` line

`transaction` is the `ServeMux` route pattern only when your program is built with Go 1.23+ **and** its main
module's `go.mod` declares `go 1.22` or newer (or sets `httpmuxgo121=0` with `//go:debug`); a main module
declaring `go 1.21` or older gets the request path by default, whatever toolchain builds it, because Go
derives `httpmuxgo121` from that line.

### Other behaviour changes

- The widened PII list (`api-key`, `cookie`, `credential`, `private_key`, `pwd` added to match
  `docs/sdk-specification.md` §5). `ScrubPII` now also applies inside lists (`[]interface{}`) in metadata —
  v0.3.x sent `{"items": [{"password": "x"}]}` unfiltered — and a metadata list is deep-copied per event, maps
  inside it included.
- Invalid UTF-8 in any string is sent as U+FFFD, one per invalid byte, exactly as `encoding/json` already wrote
  it, and every bound measures that form (a 255-byte `user.id` holding an invalid byte is dropped, as the server
  already dropped it).
- **Per-event marshalling and batching to `{endpoint}/batch`.** Every body, a lone event included, is now posted
  to `{endpoint}/batch` as a JSON array (v0.3.x posted a lone event to `{endpoint}` as an object; the batch
  endpoint answers a lone event's internal error with 400, not retried, as it already did for a batch of two or
  more). An event whose one-element array is over the batch endpoint's 5 MiB cap, or whose estimated protobuf
  size is over 1,000,000 bytes (NATS refuses a message over 1 MiB; the estimate is JSON **without HTML
  escaping**, except that `metadata`, `contexts`, `request.data` and breadcrumb `data` are counted as protobuf
  encodes them, where a number takes 11 bytes), now has `request.headers`/`env`, then `request.data`, then
  `breadcrumbs` removed until it fits, and is dropped with `OnError` only if it still does not. A value
  `encoding/json` cannot marshal (a `func`, a channel, `NaN`, a `time.Time` outside years 0-9999) costs only its
  own entry — a `request.data` or breadcrumb `data` holding it is removed, a `metadata` key or context holding it
  is removed and listed in `truncated_keys` — and the event is sent (v0.3.x lost the whole batch). The same holds
  for a value that marshals but that the ingestor cannot decode — a number too large for a `float64` or nesting
  deeper than 10,000 levels. A batch is sent as several requests when one would pass 5 MiB or 500 events.
- The SDK no longer writes into a `Tags` map, `*UserInfo` or other value the caller put on an event —
  `ApplyContext` and the transport build new ones.
- **Client-side bounds**: a `message` over 10,000 characters is truncated to 10,000 where v0.3.x sent it whole
  and the server rejected the event with 400 (grouping is unaffected: the message is not part of the
  fingerprint). Every other value the server rejected with 400 is now fixed client-side and the event sent: an
  `error_class` over 255 characters is truncated and an empty one becomes `Error`, a `fingerprint` over 64 is
  dropped, a `release_version` over 100 is truncated, a `trace_id`/`span_id` over 64 is dropped, an invalid
  `platform` becomes `go`, an invalid `environment` is normalized (`Production` → `production`, `Prod EU` →
  `prod-eu`, empty → `development`), and a stack trace over 100 frames keeps the first 100.
- `BeforeSend` may edit an event's own maps in place but must replace, not edit, the values §8's shared-reference
  table lists as shared.
- The zerolog hook sends a typed level and **no longer sends `logger.Log()` (`NoLevel`) lines or `Disabled`-level
  lines**, which v0.3.x captured as errors — only `Error`, `Fatal` and `Panic` lines are captured.
- `request.headers` never carries a credential header (the five named ones, any header whose name matches the
  PII list, any header whose name contains `jwt`, `assertion`, `oidc`, `principal` or `session`, which also
  removes a header that merely mentions a session, and any header whose name ends in `-key`, which also removes
  `Idempotency-Key` and `Sec-Websocket-Key`), nor a client-IP header (the seven named ones, any header whose name
  contains `forwarded`, `client-ip`, `clientip`, `real-ip`, `connecting-ip`, `connection-ip`, `originating-ip`,
  `remote-ip`, `remote-addr`, `external-address`, `user-ip` or `socketip`, which also removes
  `X-Forwarded-Host`/`-Proto`/`-Port`/`-Prefix`, and any header whose value is nothing but IP addresses). A URL
  in `Referer`, `Location`, `Content-Location`, `X-Original-Url`, `X-Original-Uri` or `X-Rewrite-Url` has its
  query filtered as `query_string` is, its fragment and any `user:password@` removed; a value that is not a URL
  drops the header, and `Link` is no longer recorded.
- **`"`, `\` and control characters count their UTF-8 bytes** in the publish-cost estimate, plus their JSON
  framing overhead, rather than the escaped length an `encoding/json` encoder with HTML escaping off still
  writes for them — an estimate that counted the escaped length would over-shed an event dense with those
  characters even though NATS would carry it.

### `metadata` and `contexts` are bounded client-side

The SDK now applies the server's own bounds to `metadata` and `contexts` before it sends them, with a 4 KiB
margin: over 32 contexts, or over 61,440 bytes of contexts, it removes contexts in the server's order (names
other than `device`, `os`, `app`, `runtime`, `browser`, `culture` first, largest first); over 61,440 bytes of
`metadata` it removes keys in the server's order (largest first, the keys Sentinel routes on last), and records
what it removed in `metadata.truncated`/`truncated_keys`, as the JS SDK does, replacing any value put under
those two keys. **The SDK keeps up to 4 KiB less of each than the server would** (the server keeps up to 64
KiB): in exchange the server never cuts an event this SDK sent, so the SDK can size each event exactly as it
sends it and never loses one at the NATS publish for a cut it mispredicted. It also removes U+0000 from
`contexts`, `request.data` and breadcrumb `data` (keys and strings, at every depth) and from tag, header and env
keys and values, resolving a key collision as the server would, and removes a `server_truncated`/
`server_truncated_keys` you put in `metadata` yourself (the server always did).

### Timestamps and ids

The event and breadcrumb timestamps are sent in UTC; an event timestamp outside years 0-9999, which
`encoding/json` cannot write, is replaced with the send time, and such a breadcrumb timestamp with the event's
(v0.3.x could not marshal a batch holding such an event timestamp, and lost all of it). An `event_id` that is
empty, over 64 characters or holds a control character is replaced with a new UUID, as the ingestor would
replace it.

### `HTTPMiddleware` and `Init`

The recovered-panic event is sent through the default client, so it reaches your project only if the program
called `sentinel.Init`.

Upgrade:

```bash
go get github.com/tuju-ai/sentinel/packages/sdk-go@packages/sdk-go/v0.4.0
```

## v0.3.2 — Error classification interfaces

### Added

- **`ClassifiedError`, `Classify(err error, class string) error`, and the `ErrorClass() string` interface** —
  error class extraction now honours a wrapped error implementing `interface{ ErrorClass() string }`, unwrapping
  through `errors.Unwrap`/joined errors to find one, before falling back to the type name (`ac7d18c`, `4c8763c`).

## v0.3.1 — Error class from wrapped errors

### Added

- **`WithFingerprint`, `WithErrorClass`, `ExtractFingerprint`, `ExtractErrorClass`** — a context-scoped custom
  fingerprint and error class, read by `NewEvent`/`ApplyContext`. Error class resolution now unwraps
  wrapped/joined errors looking for a classifier before falling back to the error's type name (`ac7d18c`).

## v0.3.0 — Module path

### Changed

- **The module path is now `github.com/tuju-ai/sentinel/packages/sdk-go`** — the same
  `sentinel/packages/sdk-go` path under the `tuju-ai` organisation instead of the `NurfitraPujo`
  account every release up to `v0.2.2` was published from. The old path named a repository a
  consumer's `GOPRIVATE=github.com/tuju-ai/*` credentials cannot reach, so no consumer outside this
  repo could ever `go get` the SDK. Nothing but the path changes: the code delta since `v0.2.2` is
  this entry, the `README.md` paths and a comment on `Event.Fingerprint` (already on `main` before
  the rename); there is **no behaviour change**. Consumers upgrade by changing the import path and
  pulling the new tag:

  ```bash
  go get github.com/tuju-ai/sentinel/packages/sdk-go@packages/sdk-go/v0.3.0
  ```

  The old path stops existing at `main`; tags `v0.1.0`…`v0.2.2` remain fetchable under it for
  anyone who pinned them. The SDK is major-zero, so a path change on a minor bump follows the same
  convention `v0.2.0` set for the wire-format change.

## v0.2.2 — Custom fingerprints

### Added

- **`Event.Fingerprint`** — an optional custom grouping key. Leave it empty (the normal case) and the
  server computes one from the error class, the stacktrace and the environment; set it and the server
  stores it verbatim, so two events sharing a value group together whatever their stacktraces look
  like. Maximum 64 characters, matching `issues.fingerprint VARCHAR(64)`; the ingestor rejects an
  oversized value with a 400 rather than truncating, because a shortened grouping key would silently
  merge issues that were meant to stay apart.

  **A custom fingerprint groups within one project and one environment, not across environments.**
  Issue identity is the tuple `(project_id, environment, fingerprint)` — a `UNIQUE` constraint on the
  `issues` table, not something the fingerprint carries — so the same custom value sent from
  `production` and from `staging` is two issues, each with its own status and count. Resolving a bug
  in staging must not mark it resolved in production. Setting this field does not opt out of that
  split.

  This needed a server change too: the proto has carried `fingerprint` since the beginning and the
  processor honoured it, but the ingestor never decoded it from JSON — so the capability was
  reachable only by a hypothetical protobuf client, and JSON is the only ingest path there is.

  The proto's companion `fingerprint_override` is deliberately **not** exposed. Its effect is the
  reverse of what the name suggests: it forces the server to recompute and discards whatever
  fingerprint was sent with it, so an SDK field for it would be a switch whose only function is to
  throw away the value beside it.

## v0.2.1 — Rate-limit retries and trace flags

Both fixes come from an audit of this SDK against what the server actually accepts
(`apps/ingestor-go/validation`, `mapping`, `middleware` and the processor).

### Fixed

- **429 responses are retried instead of discarded.** The ingestor answers a rate-limited
  client with `429` and `Retry-After: 60` (`middleware/ratelimit.go`). The send loop treated
  every 4xx as "the payload was rejected, resending cannot succeed" and dropped the batch — so
  a rate-limited client silently threw away telemetry it could have delivered a minute later.
  429 is now retryable, the server's `Retry-After` is honoured (both the delay-seconds and
  HTTP-date forms), and the wait is capped at 60s so a bad header cannot park a batch
  indefinitely. Every other 4xx keeps its existing drop-immediately behaviour.

- **`trace_flags` is now sent.** The server accepts, maps and stores it end to end, but
  `ExtractTraceIDs` read only the ids off the OpenTelemetry `SpanContext` and dropped the flags
  that sit on the same object, so every SDK-originated event arrived with its sampled bit lost.

### Added

- `ExtractTraceContext(ctx)` returns the trace id, span id **and** the W3C trace-flags byte.
  `ExtractTraceIDs` is unchanged and still returns two values — it is exported, so callers
  outside this repo may depend on its signature.

### Note on retries and `Flush`

The retry wait is deliberately not cancelled by the transport's context. `Flush` cancels that
context before draining, so waiting on it would mean **no** batch is retried during a flush —
silently disabling the existing 5xx retries too. The trade-off is that a batch parked on a 60s
`Retry-After` can outlast a short `Flush` timeout, which then returns `false`; that is what
`Flush`'s bool already means.

## v0.2.0 — Wire contract fix (BREAKING)

> [!IMPORTANT]
> This is a **breaking wire-format change**. It is being released as a breaking `v0.2.0` rather than a patch
> because the JSON field names on `Event` changed. In practice **this breaks nothing that was working**,
> because nothing was working: every version of the SDK prior to this one had its payload rejected by the
> ingestor's `platform` requirement alone (HTTP 400, on every single event), and the SDK never inspected the
> HTTP response, so the 100% rejection rate was completely silent. See
> [`docs/memory/VERIFIED_STATE.md`](../../docs/memory/VERIFIED_STATE.md) findings **S4** and **S11**.

### Fixed

- **`error_message` → `message`** on `Event`. The ingestor/proto field is `message`; the old name meant the
  server-side message column was always empty for every SDK-originated event.
- **`context` → `metadata`** on `Event`. The ingestor/proto field is `metadata`; the old name meant every
  user tag and every PII-scrubbed context value was silently dropped before it ever reached the database.
- **Added `platform` to `Event`, always `"go"`.** The ingestor requires this field (`^[a-z0-9]+$`) and
  rejects any request missing it with HTTP 400 — this alone caused the 100% rejection rate.
- **Added `in_app` to `Frame`, and it is now actually populated** (`ExtractStacktrace` / `isInAppFrame`). A
  frame is considered in-app when its source file is not under `GOROOT`, not under a Go module cache
  (`.../pkg/mod/...`), and not under a `vendor/` directory. Previously `Frame` had no `in_app` field at all,
  so the processor's fingerprinting algorithm (which only hashes in-app frames) degenerated to the error
  class alone — every error of a given class in a project collapsed into a single issue.
- **`sendBatch` now inspects the HTTP response.** Previously the status code was discarded entirely
  (`defer resp.Body.Close()` with no check), so a 100% rejection rate produced no error, log line, or metric
  anywhere on the client. Now:
  - 2xx: success.
  - 4xx: the batch is dropped immediately (not retried — resending an unchanged payload cannot change a
    validation outcome). If `Config.Debug` is set, the status and response body are logged.
  - 5xx / network error: retried up to 4 attempts total with capped exponential backoff
    (200ms → 400ms → 800ms, capped at 5s), then dropped.
  - Added `Config.OnError func(error)` — called whenever a batch is ultimately dropped (4xx, or 5xx/network
    error after retries are exhausted), so applications can alert on ingest failure instead of silently
    losing data. Retries and `OnError` always run on the SDK's internal worker goroutine, never on the
    caller's goroutine — capturing an error never blocks application code.

### Not changed

- `Config.ReleaseVersion` → `Event.release_version` was already wired correctly before this release; it is
  called out here only because it is part of the same S4/S5 field-name table in
  [`docs/plans/E2E_RECOVERY_PLAN.md`](../../docs/plans/E2E_RECOVERY_PLAN.md) P2-3.

### Migration

No application code changes are required to adopt this version unless you constructed `sentinel.Event`
literals directly (uncommon — most callers go through `sentinel.CaptureError` / `CaptureErrorContext`, which
build the event internally and are unaffected). If you did construct `Event` literals or depended on the
`error_message` / `context` JSON keys on the wire (e.g. a custom proxy or test fixture), update those field
names to `message` / `metadata`.
