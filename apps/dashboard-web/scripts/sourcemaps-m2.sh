#!/usr/bin/env bash
# sentinel-sourcemaps §13 M2 — the LABELLED FALLBACK take on the local compose stack (decision 15):
# the cold-path cost and the processor's RSS with the cap-size map parsed and held. Evidence about
# shape and order of magnitude, never the criterion met.
#
# Preconditions: the seed's R1 maps uploaded (sourcemaps-seed.sh setup generate upload-a upload-b,
# or a finished sourcemaps-m1.sh), SEED_DIR pointing at that seed's generated files, the processor
# running with GOMEMLIMIT as the chart sets it (values.yaml: 400MiB) and SOURCEMAP_MAX_PARSE_BYTES
# at its default so the cap-size map IS parsed. `docker compose restart processor` needs the
# compose files the stack was started with — pass them in COMPOSE_ARGS (e.g. "-f docker-compose.yml
# -f override.yml"); default: docker-compose.yml alone.
#
# What it does:
#   1. restarts the processor (cold cache), waits for /health, reads RSS₀;
#   2. posts touch-small.json — one frame on each of the 16 committed maps — waits for 16 cold
#      fetches, reads RSS₁;
#   3. posts touch-capsize.json — one frame on the cap-size map — waits for the fill (the event
#      itself may pass through `timeout`, decision 37: reported as a number), samples RSS for 20 s
#      after it lands and reads the max and the last as RSS₂;
#   4. reports the fetch-duration bucket table over the 17 cold fetches (p95 ≈ the bucket the 17th
#      fetch falls in — 17 × 0.95 rounds to the max), RSS against 512Mi, and the parsed:raw ratio
#      (RSS delta ÷ raw bytes) for the cap-size map alone and for the whole bundle.
#
#   usage: SEED_DIR=<dir> apps/dashboard-web/scripts/sourcemaps-m2.sh > m2.md
set -euo pipefail
. "$(dirname "${BASH_SOURCE[0]}")/sourcemaps-measure-lib.sh"
: "${SEED_DIR:?set SEED_DIR to the generated seed directory (sourcemaps-seed.sh prints it)}"
COMPOSE_ARGS="${COMPOSE_ARGS:--f docker-compose.yml}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
WORK="$(mktemp -d -t sourcemaps-m2.XXXXXX)"

capsize_bytes=$(stat -c %s "$SEED_DIR/capsize.map")
small_bytes=$(node -e 'const m = require(process.argv[1]); process.stdout.write(String([...m.maps.a, ...m.maps.b].reduce((s, f) => s + f.bytes, 0)))' "$SEED_DIR/events/manifest.json")

log "M2: restarting the processor (cold cache)"
# shellcheck disable=SC2086
(cd "$ROOT" && docker compose $COMPOSE_ARGS restart processor >&2)
for _ in $(seq 1 60); do curl -sf "$PROCESSOR_URL/health" >/dev/null 2>&1 && break; sleep 1; done
sleep 3
rss0=$(processor_rss_kib)
metrics_snapshot "$WORK/0.metrics"
fetch0=$(metric "$WORK/0.metrics" sentinel_sourcemap_fetch_duration_seconds_count)
timeout0=$(metric "$WORK/0.metrics" sentinel_symbolication_frames_total 'outcome="timeout"')
log "M2: RSS₀ = $rss0 KiB (fetch count $fetch0)"

post() { curl -sS -X POST "$INGESTOR_URL/ingest" -H "X-API-Key: $SEED_INGEST_KEY" -H 'Content-Type: application/json' --data-binary "@$1"; echo; }

log "M2: posting touch-small (16 committed maps)"
post "$SEED_DIR/events/touch-small.json" >&2
wait_metric sentinel_sourcemap_fetch_duration_seconds_count '' "$(calc "$fetch0 + 16")" 120
sleep 3
rss1=$(processor_rss_kib)
metrics_snapshot "$WORK/1.metrics"
log "M2: RSS₁ = $rss1 KiB after the 16 small maps"

log "M2: posting touch-capsize (the cap-size map, $capsize_bytes bytes)"
t_start=$(date +%s.%N)
post "$SEED_DIR/events/touch-capsize.json" >&2
wait_metric sentinel_sourcemap_fetch_duration_seconds_count '' "$(calc "$fetch0 + 17")" 300
t_fill=$(calc "$(date +%s.%N) - $t_start")
rss_max=0; rss_last=0
for _ in $(seq 1 20); do
  r=$(processor_rss_kib); rss_last=$r; [ "$r" -gt "$rss_max" ] && rss_max=$r
  sleep 1
done
metrics_snapshot "$WORK/2.metrics"
timeout2=$(metric "$WORK/2.metrics" sentinel_symbolication_frames_total 'outcome="timeout"')
cache_bytes=$(metric "$WORK/2.metrics" sentinel_sourcemap_cache_bytes)
log "M2: RSS₂ max $rss_max KiB, last $rss_last KiB; fill landed ≈ ${t_fill}s after the post"

mib() { calc "$1 / 1024"; }
cat <<MD
### M2 — cold fetch and processor RSS with the cap-size map parsed and held (labelled fallback, local compose, $(date -u +%Y-%m-%dT%H:%MZ))

Processor restarted cold; GOMEMLIMIT=$(docker exec "$PROCESSOR_CONTAINER" env 2>/dev/null | awk -F= '/^GOMEMLIMIT=/ { print $2 }'); SOURCEMAP_MAX_PARSE_BYTES unset (default = the upload cap); MinIO on the same host.

sentinel_sourcemap_fetch_duration_seconds (object GET + parse, cold path) over the 17 cold fetches — 16 committed maps ($small_bytes bytes) and the cap-size map ($capsize_bytes bytes):

$(histogram_table "$WORK/0.metrics" "$WORK/2.metrics" sentinel_sourcemap_fetch_duration_seconds)

**sourcemap_cold_fetch_p95_ms**: with 17 samples the 95th percentile is the largest sample — the bucket the cap-size map's fetch + parse fell in above; the 16 committed maps alone:

$(histogram_table "$WORK/0.metrics" "$WORK/1.metrics" sentinel_sourcemap_fetch_duration_seconds)

| RSS (VmRSS of the processor process) | KiB | MiB | against 512Mi |
| --- | --- | --- | --- |
| RSS₀ — after restart, cache empty | $rss0 | $(mib "$rss0") | $(calc "100 * $rss0 / 524288")% |
| RSS₁ — 16 committed maps parsed and cached | $rss1 | $(mib "$rss1") | $(calc "100 * $rss1 / 524288")% |
| RSS₂ (max over 20 s) — the cap-size map parsed and held as well | $rss_max | $(mib "$rss_max") | $(calc "100 * $rss_max / 524288")% |
| RSS₂ (last sample) | $rss_last | $(mib "$rss_last") | $(calc "100 * $rss_last / 524288")% |

sentinel_sourcemap_cache_bytes (raw-byte accounting) = $cache_bytes.

| parsed:raw ratio (RSS delta ÷ raw bytes) | raw bytes | RSS delta (KiB, max) | ratio (max) | RSS delta (KiB, last) | ratio (last) |
| --- | --- | --- | --- | --- | --- |
| the cap-size map alone (RSS₂ − RSS₁) | $capsize_bytes | $(calc "$rss_max - $rss1") | $(calc "1024 * ($rss_max - $rss1) / $capsize_bytes") | $(calc "$rss_last - $rss1") | $(calc "1024 * ($rss_last - $rss1) / $capsize_bytes") |
| the whole bundle (RSS₂ − RSS₀) | $((capsize_bytes + small_bytes)) | $(calc "$rss_max - $rss0") | $(calc "1024 * ($rss_max - $rss0) / ($capsize_bytes + $small_bytes)") | $(calc "$rss_last - $rss0") | $(calc "1024 * ($rss_last - $rss0) / ($capsize_bytes + $small_bytes)") |

**First-event timeout count** (frames_total{outcome="timeout"} before → after): $timeout0 → $timeout2, i.e. **$(calc "$timeout2 - $timeout0")** frame(s) passed through \`timeout\` on the cap-size map's first event; the fill landed ≈ ${t_fill} s after the post (SYMBOLICATE_DEADLINE default 5s).

Snapshots kept under \`$WORK\`.
MD
