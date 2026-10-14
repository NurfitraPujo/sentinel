# Sentinel grouping v2: design note and evidence

Date: 2026-10-14 (repair pass; first version 2026-10-13). Contract: `prd/sentinel-grouping-v2/contract.md` (light tier: this
note is the spec stage). Code: `apps/processor-go/fingerprint/{fingerprint,grouping}.go`, `symbolicate/{service,cache}.go`,
`service/processor_service.go`, `packages/shared-go/nats/subscriber.go`. Evidence is re-runnable:

```
go test ./tests/unit -run TestGroupingV2Evidence -v                              # fixtures + named failure modes
GROUPING_CORPUS=<corpus.jsonl> go test ./tests/unit -run TestGroupingV2Corpus -v # real occurrences (skips when unset)
```

## 1. The v2 input

SHA-256, first 16 hex, of the `|`-joined string

```
v2 | environment | normalised error_class | frame label (x up to 5) [| msg=<normalised message>]
```

* **Version.** `fingerprint.AlgorithmVersion = "v2"`, one constant, first token. A client `fingerprint` is returned verbatim, unprefixed (D3).
* **Frame label.** `file:function`. For a frame symbolication did not rewrite on a platform in `SYMBOLICATE_PLATFORMS`
  (default `javascript`, `browser`) the file is build-hash-normalised and a function name the minified-name detector flags (one or
  two characters per dotted segment: `e`, `t`, `Ab`, `a1`) folds to `*`, because a minifier's name identifies nothing and changes between
  builds. Any other name is kept: it is stable across builds, and it is what keeps two callers behind one wrapper frame apart
  (contract §5.1). History: the first version folded such names; R1 dropped the function of every unsymbolicated frame (to separate two
  errors in functions `e,t` and `n,r` of one bundle, which the message now does); repair #2 narrowed R1's drop back to the detector's
  names, see section 3. Line and column are never inputs.
* **Frames.** Library/platform frames are removed first (whatever `in_app` says); of the rest the `in_app` ones are used, else any;
  a library-only stack is hashed as it is. The first **5** (crash-first) are hashed.
* **Library rule.** `dart:` always. A `package:` frame is library code iff its package is on a fixed, code-level list (flutter,
  dio, http, provider, riverpod, bloc, get, sentry, firebase_*, drift, the supabase family: supabase, supabase_flutter, postgrest,
  gotrue, realtime_client, storage_client, functions_client, yet_another_json_isolate, mutex; and widely used plugins such as
  intl, cached_network_image, shared_preferences, url_launcher, path_provider, connectivity_plus, package_info_plus, device_info_plus,
  flutter_secure_storage). The decision depends only on the frame's own package, never on the rest of the stack. Go: stdlib import
  paths (also in `-trimpath` form; `internal/` and `vendor/` are not stdlib roots), `@v` module-cache paths, `/pkg/mod/`, `vendor/`
  for `.go` files only, GOROOT paths; `node_modules`.
* **Message.** `msg=<normalised message>` is appended when (a) no usable frame exists, (b) the top chosen frame is a *wrapper*
  (file base name in {logger, log, errorhandler, interceptor(s), httpclient, apiclient, fetcher, request(s), axios, client, wrapper}
  or function last segment in {log, error, warn, logError, captureException, captureMessage, handleError, reportError, request,
  fetch, send, wrapper, middleware, interceptor, onError}), or (c) on a gated platform no hashed frame is symbolicated (R1: the
  file alone cannot say which error it was). Normalisation = the existing `normalizer`, then quoted strings `<STR>`, hex runs
  `<HEX>`, digit runs `<N>`, whitespace collapsed, 256 runes max.
* **Unchanged:** environment in the input, the class normalisation, the file-path normaliser, `fingerprint_override` semantics,
  every platform outside the gate set (a Go or Dart frame keeps `file:function`).

### Why N = 5 (re-confirmed on the real corpus, section 3)

On the 599 real web occurrences the issue counts are 64 (N=3), 64 (N=5), 64 (N=8); mobile is 14/14/14 and 3/3/3. The corpus is
completely insensitive to N because its web frames are mostly unsymbolicated and collapse to a few normalised files, so the choice
rests on the named cases (section 2): N=3 gets one wrong (a four-frame client wrapper hides the caller, which is frame 5) and N=8
gets two wrong (it splits one failure site whose entry path differs only beyond frame 5, and the same failure with and without a trailing `main.dart` frame). **N=5 is the smallest value that gets every
named case right, and the corpus gives no number that argues for another.** Unchanged.

## 2. Fixture and named-case evidence

The 25 committed web/Go fixtures and the 2 real Flutter events in `tests/fixtures/sentry/`, plus constructed pairs for every named failure mode.
Every fixture web frame is an unsymbolicated gated frame, so under R1 the message joins and the fixtures that v1 merged only
because they shared a stack now split by message; no fixture pair merges.

```
| corpus | events | v1 issues | v2 N=3 | v2 N=5 | v2 N=8 |
|---|---|---|---|---|---|
| mobile (sentry-compat fixtures) | 2 | 2 | 2 | 2 | 2 |
| web (sdk-js/sdk-go fixtures) | 25 | 19 | 24 | 24 | 24 |

fixture pairs whose verdict differs between v1 and v2 (N=5):
  SPLIT: component-stack-100kib.json <-> node-modules-frame.json
  SPLIT: message-cut-code-point.json <-> metadata-near-the-cap.json
  SPLIT: message-cut-code-point.json <-> v2-estimate-strings.json
  SPLIT: message-cut-code-point.json <-> v2-hostile-before-send.json
  SPLIT: message-cut-code-point.json <-> v2-rich-context.json
  SPLIT: metadata-near-the-cap.json <-> v2-estimate-strings.json
  SPLIT: metadata-near-the-cap.json <-> v2-hostile-before-send.json
  SPLIT: metadata-near-the-cap.json <-> v2-rich-context.json
  SPLIT: v2-estimate-strings.json <-> v2-hostile-before-send.json
  SPLIT: v2-estimate-strings.json <-> v2-rich-context.json
  SPLIT: v2-hostile-before-send.json <-> v2-rich-context.json

named failure modes (expected: same issue? / v1 says / v2 N=3,5,8 say):
  (1) stackless events, different messages                                                        want same=false v1 same=true  v2: N=3 same=false N=5 same=false N=8 same=false
  (1) stackless events, same failure, different ids                                               want same=true  v1 same=true  v2: N=3 same=true  N=5 same=true  N=8 same=true
  (2) two callers behind one HTTP-client wrapper                                                  want same=false v1 same=true  v2: N=3 same=true  N=5 same=false N=8 same=false
  (N) same failure site, entry point differs only beyond frame 5                                  want same=true  v1 same=true  v2: N=3 same=true  N=5 same=true  N=8 same=false
  (2b) one wrapper-top failure, different ids in the message                                      want same=true  v1 same=true  v2: N=3 same=true  N=5 same=true  N=8 same=true
  (3) Dart: dio frames flagged in_app, different app callers                                      want same=false v1 same=true  v2: N=3 same=false N=5 same=false N=8 same=false
  (3) Go -trimpath: stdlib and module frames flagged in_app, different app callers                want same=false v1 same=true  v2: N=3 same=false N=5 same=false N=8 same=false
  (5) minified function renamed across builds (e -> t)                                            want same=true  v1 same=false v2: N=3 same=true  N=5 same=true  N=8 same=true
  (1b) minified: different errors in functions e,t vs n,r of one bundle                           want same=false v1 same=false v2: N=3 same=false N=5 same=false N=8 same=false
  (3b) Dart: postgrest/supabase/gotrue above visits vs profile (list only, no main.dart rule)     want same=false v1 same=true  v2: N=3 same=false N=5 same=false N=8 same=false
  (3c) Dart: same failure with and without a trailing main.dart frame and a sibling app_core frame want same=true  v1 same=true  v2: N=3 same=true  N=5 same=true  N=8 same=false
  (2c) two callers behind one wrapper, no source map: same file, same message, real names         want same=false v1 same=false v2: N=3 same=false N=5 same=false N=8 same=false
  (4) same bug, symbolicated vs not -> still two until the retry resolves it (not decided by Compute) want same=false v1 same=false v2: N=3 same=false N=5 same=false N=8 same=false
  line numbers only differ                                                                        want same=true  v1 same=true  v2: N=3 same=true  N=5 same=true  N=8 same=true

wrong verdicts of 14 named cases: v1=6 N=3=1 N=5=0 N=8=2

```

## 3. Evidence on the real corpus

Source: a local export of 642 stored occurrences (`web` 599 in 50 issues, `zasri-mobile` 34 in 14, `tuju-mobile` 9 in 3), never
committed; the test reads it through `GROUPING_CORPUS`. Issues are identified below only by anonymised labels (W/Z/T + sequence
in sorted-id order) and error class; no message, URL or id from the corpus is in this repository.

**What the export can and cannot say (read this before the numbers).**
* **The export lost the `symbolicated` key.** 467 of the browser frames are `turbopack:///[project]/...` and
  `[turbopack]/browser/runtime/...` source paths, which only the symbolication stage produces. So "none of the 599 web occurrences
  has a symbolicated frame" is **not established**: those frames were symbolicated, the export does not say so, and the test
  therefore hashes them as unsymbolicated unless told otherwise (the third row of the variants table does tell it).
* **125 rows predate `error_occurrences.message`.** They have an empty message although the failure had one; a sibling occurrence of
  the same issue usually carries it. Ten of the twelve web splits below exist only because of those empty messages (measured: 12 split issues as exported, 2 once the messages are filled from siblings).
* **The v1 reconstruction needs one repair, and then reproduces today exactly: v1 = 50 / 14 / 3** (web / zasri / tuju). 68 of the 642
  occurrences carry an empty `errorClass` while a sibling of the same issue carries the class the issue was created with; those take
  their issue's class (without it the web reconstruction gives 62, because v1 hashed the class). This was re-run with the **real v1
  `fingerprint.Compute` from `origin/main`** (copied into a scratch package, not committed) over all 642 records: the same 50 / 14 / 3,
  and the same hash as the independent reimplementation in the test for every one of the 642 records. In the reconstruction no v1
  group owns two current issues and no current issue owns two v1 groups, so the pairs below are real differences between v1 and v2,
  not reconstruction error.

v2 as the code stands (repair #2: a minified function name folds, a real one is kept; the Dart library list, now with `intl`), N = 3 / 5 / 8:

| project | occurrences | issues today | v1 reconstruction | v2 N=3 | v2 N=5 | v2 N=8 |
|---|---|---|---|---|---|---|
| tuju-mobile | 9 | 3 | 3 | 3 | 3 | 3 |
| web | 599 | 50 | 50 | 64 | 64 | 64 |
| zasri-mobile | 34 | 14 | 14 | 14 | 14 | 14 |

68 of 642 occurrences had an empty errorClass and took their issue's class; 125 have an empty message.

#### tuju-mobile

v1 reconstruction vs today's issues: 0 v1 groups own more than one current issue; 0 current issues own more than one v1 group.

Merged (several current issues become one v2 issue):

- none

Split (one current issue becomes several v2 issues):

- none

#### web

v1 reconstruction vs today's issues: 0 v1 groups own more than one current issue; 0 current issues own more than one v1 group.

Merged (several current issues become one v2 issue):

- none

Split (one current issue becomes several v2 issues):

- W02 (AxiosError, 25 occ.) -> 2 issues, occurrences 10/15
- W04 (AbortError, 23 occ.) -> 2 issues, occurrences 10/13
- W09 (NotAllowedError, 6 occ.) -> 2 issues, occurrences 3/3
- W13 (NotAllowedError, 25 occ.) -> 2 issues, occurrences 16/9
- W15 (TypeError, 25 occ.) -> 2 issues, occurrences 17/8
- W29 (Error, 13 occ.) -> 2 issues, occurrences 4/9
- W36 (Error, 4 occ.) -> 2 issues, occurrences 1/3
- W44 (Error, 6 occ.) -> 4 issues, occurrences 1/1/1/3
- W45 (AxiosError, 2 occ.) -> 2 issues, occurrences 1/1
- W47 (Error, 8 occ.) -> 2 issues, occurrences 3/5
- W48 (TypeError, 21 occ.) -> 2 issues, occurrences 17/4
- W50 (AxiosError, 3 occ.) -> 2 issues, occurrences 1/2

#### zasri-mobile

v1 reconstruction vs today's issues: 0 v1 groups own more than one current issue; 0 current issues own more than one v1 group.

Merged (several current issues become one v2 issue):

- none

Split (one current issue becomes several v2 issues):

- none

#### v2 N=5 under the export's known losses (issues / widest split of one current issue):

| variant | web | zasri-mobile | tuju-mobile |
|---|---|---|---|
| as exported | 64 / 4 | 14 / 1 | 3 / 1 |
| messages repaired from siblings | 53 / 3 | 14 / 1 | 3 / 1 |
| messages repaired + turbopack frames symbolicated | 53 / 3 | 14 / 1 | 3 / 1 |

(Message repair here takes the first non-empty message of the same issue; the reviewer's own repair gives 49 and 53 for the blanket-drop
rules, this one 48 and 52 for the same rules, so the two repairs differ by one issue at most. The comparison that matters below is made
with one method.)

**Reading.**
* **v1 = 50 / 14 / 3 is confirmed.** **Mobile does not move** (14 and 3 issues before and after, in every variant): the Dart events
  already carry distinct app frames, and the library-frame skip changes the hash value, not the partition. For mobile the reset is a
  one-time change of values, not of grouping.
* **Web, as exported: 50 -> 64 at N=5; 53 with the empty messages repaired from sibling occurrences; 53 with turbopack frames also
  treated as symbolicated.** The headline figure is mostly an export artefact: 11 of the 14 extra issues disappear when the messages are
  repaired, and what is left is real message-aware separation (R1: with no symbolicated frame the message decides). At least one split is
  one issue holding two unrelated failures of the same class (a mixed `NotAllowedError` issue), which v2 separates correctly.
* **The acceptance bound holds in every variant:** no current issue splits into more than N = 5 v2 issues (the widest is 4 as exported,
  3 once repaired), and mobile is flat. The 12 splits listed above are the same twelve, with the same fan-outs, as under the blanket drop
  this section reported before repair #2.
* **No merge remains.** Under the blanket drop of the function name the web partition had five merge lines (the `AxiosError` triple
  W02 + W22 + W30, `W29 + W47`, `W03 + W27`, `W20 + W40`); the independent review inspected the v2 merges and found them all the same failure class
  (chunk-load, `Failed to fetch`, axios `Network Error`), i.e. v1 had kept apart issues that differ only by a function name. Keeping non-minified names in the key
  (repair #2, below) un-merges all of them, which is why the web count rises from 58 to 64 as exported (48 to 53 repaired) while the
  split list does not change. The rise is the price of contract §5.1 ("two callers behind the same wrapper frame are two issues"), not a
  new instability: a real name is identical across builds.
* **Caveat.** Everything above is measured on frames the stage never saw. When source maps are uploaded for a release the frames hash as
  `file:function` of the source, so those releases will group on source-level names (which is the intended end state).

### R5 on the corpus
R5 originally also took the app's package from a `package:<pkg>/main.dart` frame and skipped every other package when it was
present. **That heuristic was removed** (review of PR #112, major 1): it made the hash depend on stack shape, so the same bug
split by whether `main.dart` happened to be in the stack, and a sibling package of a multi-package app (`app_core`) was app code
without `main.dart` and library code with it. The decision is now the deterministic list alone. The cost is that a library
missing from the list counts as app code and can over-split (never merge); extend the list in code when one is observed.

The corpus holds no frame whose file ends `/main.dart` and no supabase-family frames, so the mobile counts above come from the list.
The named cases are constructed: postgrest, supabase and gotrue frames above `package:zasri/visits.dart` versus
`package:zasri/profile.dart` give **different** fingerprints (case 3b), and the same failure with and without a trailing
`main.dart` frame, with a sibling `app_core` frame, gives the **same** fingerprint at N=5 (case 3c;
`TestCompute_DartDecisionIsStackIndependent`).

No own-package signal exists in the real mobile data either (coordinator decision, section 6): 0 of 43 events carry a release name or a `main.dart` frame that identifies the app package (the code lives in `daya`; the releases are `zasri.zasri` and `tuju.tuju`). `intl`, carried in-app by one corpus event, was added to the list; the mobile counts do not change.

### Repair #2: the function name on unsymbolicated gated frames

R1 dropped the function name of every frame the stage did not rewrite. That contradicts contract §5.1 (two callers behind one wrapper
frame must be DIFFERENT): on a `no_map` JavaScript project `loadCart` and `loadFeed` calling one `request` in the same file, with the same
message, hashed alike, which v1 kept apart. The ruling: the contract wins. The drop is narrowed to the names the minified-name detector
flags (one or two characters per dotted segment, `foldMinifiedFunction`, the same detector the first version of this change used); every
other name is kept, because it is stable across builds and so cannot create a ghost issue. Accepted on the numbers above: the per-issue
split bound is unchanged and the only movement is the five merge lines un-merging. Tests:
`TestCompute_TwoCallersBehindOneWrapperAreDifferentWithoutASourceMap`, `TestCompute_UnsymbolicatedFunctionFoldsOnlyWhenMinified`, named
case (2c). Known residue, accepted: a real one- or two-character name loses its identity (its file still counts); folding numbers and
quoted strings in the message merges `status 401` with `status 500` at one unsymbolicated place, and Safari's `Load failed` versus Chrome's
`Failed to fetch` is still one failure per browser.


## 4. Symbolication retry

When the stage ran and any frame ended `timeout` or `fetch_error`, and the identity will be server-computed, `processEventInternal`
returns `errSymbolicationRetry` (a non-Permanent error) before anything is written. The subscriber's existing
`NakWithDelay(retryBackoff(n))` redelivers after 1 s (delivery 1) and 5 s (delivery 2); delivery 3 is processed with whatever hash
the frames give. Total retries <= 2. The subscriber puts the delivery count on the handler context
(`nats.WithDeliveryCount`), the only change to shared code; no stream, table, AckWait or MaxDeliver change.

* **`fetch_error` can now succeed (R2).** The first version retried into the negative cache: a failed object GET is cached as
  `fetch_error` for `SOURCEMAP_NEGATIVE_TTL` (60 s), so the retries at 1 s and 5 s were answered from that entry and could never
  succeed. On **delivery 2 only** (the first retry) the service marks the context `symbolicate.WithRetryFetchError`, and the stage drops
  a cached `fetch_error` for the triples it looks up and fetches again. `no_map`, `map_error` and `oversize` stay cached (permanent);
  `timeout` is never cached; a retry that fails again is cached afresh, and delivery 3 uses the normal cache. `timeout` still gets up
  to 2 retries; a persistent storage outage costs **at most ONE uncached fetch per event** (the delivery-2 retry), not one per
  redelivery (review major 2; `TestSymbolicate_FetchErrorCacheBypassOnlyOnDeliveryTwo`). Tests: a store that fails on delivery 1 and a fetcher that
  fails on delivery 1 and succeeds on delivery 2, asserting the second delivery is symbolicated and hashed on source-level frames.
* **Worst-case added latency per event:** 1 s + 5 s redelivery delay (these do not occupy a consumer) plus up to two extra stage
  runs of `SYMBOLICATE_DEADLINE` (5 s default) each, so **<= 16 s** end to end over today's single run for a `timeout`. For a
  `fetch_error` only delivery 2 is uncached, so a slow-failing store adds at most one extra deadline-bound fetch (delivery 3 is
  answered from the negative cache): **<= 11 s** over the single run.
* **Head-of-line cost.** The delay is free; the stage runs are not. A batch is processed sequentially per pod: the subscriber handles the
  messages of a fetched batch one after another inside the pull loop, so every attempt that waits out the deadline holds the loop for up to
  `SYMBOLICATE_DEADLINE` (5 s) while every message queued behind it, of any project, waits. A project whose map fetch keeps timing out
  therefore costs up to **3 x SYMBOLICATE_DEADLINE = 15 s of consumer time per event** across its three deliveries, instead of the 5 s it
  cost before this change, delaying the events queued behind it; a `fetch_error` costs at most 2 x 5 s (first delivery plus the one uncached
  retry) and almost nothing if it fails fast. A burst of events on one slow map shares one background fill, so after the first the rest
  resolve from the cache; the pathological case is many distinct slow maps at once. The cost is bounded (the budget is per message, never
  indefinite) and is deliberately NOT mitigated here: there is no skip-the-retry-after-a-timeout logic, because the contract asks for a
  bounded retry. It shares MaxDeliver (7) with database retries.
* **Attribution.** A retry is the design working, not a store failure: `withSpan` leaves the `processor.store` span's status alone
  and sets `processor.outcome=symbolication_retry`; so does the consumer span in `main.go` (`service.MarkSymbolicationRetry`), which is the
  span a dashboard reads and which used to mark every returned error. The log line carries `outcome`, `delivery` and `max_retries`. The
  `processed_total` metric still counts the delivery as `retried` (it was returned to NATS), which keeps that metric's invariant.

## 5. Reset
As accepted (D1/D5): web and mobile issues regroup once at the first release carrying this; core is unaffected (it sends its own
fingerprint, D3). On the real corpus the value of every web and mobile fingerprint moves (the `v2` token), the partition moves for
web only (section 3). The coordinator lists unresolved web and mobile issue ids before the tag.

* **Rollout.** The processor runs `replicaCount` 2 (`deploy/helm/sentinel/values.yaml`), so during the rolling update v1 and v2 pods
  interleave and an ongoing failure feeds both its old and its new issue until the rollout ends; a rollback is a second reset. A resolved v1
  issue loses its regression reopen once (the recurrence opens a new v2 issue with a new-issue alert). Under #109's retention, assigned or
  commented v1 issues are kept indefinitely. There is no migration or schema change, and the fingerprint length is unchanged (16 hex).
* **Compute cost.** `fingerprint.Compute` went from about 3.7 us to about 33 us per event (13 KB and 67 allocations against 1 KB and 24)
  when the message joins, which is most unsymbolicated web events, dominated by `NormalizeMessage`'s regex passes (benchmark on a 40-frame
  stack). That is negligible next to the `StoreEvent` transaction.
* **Integration suite.** Not run by this change; the plane's integration gate was NO_WORSE at the reviewed head.

## 6. Decisions taken here
* Retry applies only to server-computed identity; a client fingerprint without override is never delayed.
* `middleware` is a wrapper by function name only, not by file name, because Next's `middleware.js` is a real entry file used in the normalisation fixtures.
* **Coordinator decision (repair #2, B2): contract §1.3 is narrowed to a maintained library list.** The contract asks for every `package:`
  library other than the app's own to be skipped. That cannot be implemented from the data we have: an own-package signal (the release
  name, or a `main.dart` frame) fires on **0 of 43** real mobile events, because the releases are bundle ids (`zasri.zasri`, `tuju.tuju`)
  while the code lives in a differently named package (`daya`), and no stack reaches `main.dart`. A stack-shape heuristic is also wrong
  where it does fire: a multi-package app's sibling package (`app_core`) is app code without `main.dart` in the stack and library code with
  it, so the same bug split by stack shape (the reason PR #112's review removed it). The decision is the code-level list alone,
  stack-independent. **Accepted limit:** a dependency missing from the list counts as app code, so five frames of an unlisted package above
  one app frame fill the 5-frame budget and two different app frames hash alike (`TestCompute_DartUnlistedDependencyIsAcceptedLimit`);
  extend the list in code when one is observed (`intl` was added from the corpus; the supabase family was added in #112). For listed
  dependencies the reviewer's probe holds (`TestCompute_DartListedDependenciesProperty`). No per-project configuration (contract §2).
* On an unsymbolicated gated frame only minified function names fold; real names are kept (contract §5.1 over R1's blanket drop).
* The retry bypass is a context mark set by the service, not a stage configuration, so the stage's default behaviour (and the negative-cache tests) are unchanged.

## 7. Stated limits
* **Wrapper list can swallow real app frames.** File bases such as `client`, `request`, `log`, `fetcher` and functions such as `send`,
  `fetch`, `error`, `request` also match ordinary app code. When one is the top frame the normalised message joins the input, so the
  effect is over-split only (two messages differing in a digit-free word split), bounded to top-of-stack frames. Accepted.
* **Unminified web frames keep their function name; a real one- or two-character name does not.** Repair #2 (coordinator ruling, contract
  §5.1): on a gated-platform frame symbolication did not rewrite, only names the minified-name detector flags fold to `*`. Residue: a
  genuine short name (`go`, `Ab`) is indistinguishable from a minifier's, and its file still contributes.
* **A Dart dependency missing from the library list counts as app code** (coordinator decision, section 6): it can fill the 5-frame budget
  and merge two different app frames. `TestCompute_DartUnlistedDependencyIsAcceptedLimit` documents it; extend the list when one is seen.
* **The reset is wider than web and mobile.** Fingerprint-less Go and other SDK projects regroup once too (version prefix, 5 frames,
  library skipping); only a client-supplied fingerprint is untouched. The coordinator's pre-release list of unresolved issues
  should include every such project.
* **The 599-event corpus numbers are not reproducible from committed tests.** `TestGroupingV2Corpus` skips without `GROUPING_CORPUS`
  and the export is not committed. The 25 + 2 fixture numbers and the named cases are reproducible
  (`go test ./tests/unit -run TestGroupingV2Evidence -v`).
