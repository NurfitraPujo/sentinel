#!/usr/bin/env bash
# sentinel-sourcemaps §13 M3 — the LABELLED FALLBACK take on the local compose stack (decision 15):
# §4.1 wall-clock for the whole-bundle request (16 committed maps + the cap-size map, ≈ 49.6 MiB
# of multipart body against BODY_SIZE_LIMIT=110M) and for the cap-size map alone, ≥ 10 runs each.
# Evidence about shape and order of magnitude, never the criterion met; the staging re-take must
# add one full-size upload through the CCE ELB ingress (decision 31), which compose cannot.
#
# Primary: every response is 201/200 with one files[] entry per map; the first run of each series
# is 201 (created — the release is this script's own, R1-m3, so the seed's rows are untouched) and
# every later run is 200 all-unchanged. The two series use two releases (R1-m3, R1-m3-capsize) so
# each has its own run 1. Secondary: p95 of time_total reported, no bound set.
#
#   usage: SEED_DIR=<dir> [RUNS=10] apps/dashboard-web/scripts/sourcemaps-m3.sh > m3.md
set -euo pipefail
. "$(dirname "${BASH_SOURCE[0]}")/sourcemaps-measure-lib.sh"
: "${SEED_DIR:?set SEED_DIR to the generated seed directory (sourcemaps-seed.sh prints it)}"
RUNS="${RUNS:-10}"
RELEASE="${RELEASE:-R1-m3}"
FIXTURES="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../tests/fixtures/sourcemaps" && pwd)"
WORK="$(mktemp -d -t sourcemaps-m3.XXXXXX)"

capsize_path=$(node -e 'process.stdout.write(require(process.argv[1]).capsize.minified_path)' "$SEED_DIR/events/manifest.json")
capsize_bytes=$(stat -c %s "$SEED_DIR/capsize.map")

# bundle_parts → the 16 committed maps as -F pairs (both builds), then the cap-size map.
bundle_parts=()
while IFS=$'\t' read -r path map; do bundle_parts+=(-F "minified_path=$path" -F "file=@$FIXTURES/$map"); done < <(
  node -e 'const m = require(process.argv[1]); for (const f of [...m.maps.a, ...m.maps.b]) process.stdout.write(`${f.minified_path}\t${f.map}\n`);' "$SEED_DIR/events/manifest.json")
bundle_parts+=(-F "minified_path=$capsize_path" -F "file=@$SEED_DIR/capsize.map")
capsize_parts=(-F "minified_path=$capsize_path" -F "file=@$SEED_DIR/capsize.map")

# series <label> <release> <expected files> <parts...> — RUNS uploads; one line per run: status
# files outcomes time_total.
series() {
  local label="$1" release="$2" want="$3"; shift 3
  local i out status secs body files outcomes
  : > "$WORK/$label.tsv"
  for i in $(seq 1 "$RUNS"); do
    out="$(curl -sS -o "$WORK/$label.$i.json" -w '%{http_code} %{time_total}' -X POST "$DASHBOARD_URL/api/sourcemaps" \
      -H "X-API-Key: $SEED_INGEST_KEY" -F "release_version=$release" "$@")"
    status="${out% *}"; secs="${out#* }"
    body="$WORK/$label.$i.json"
    files=$(node -e 'const b = require(process.argv[1]); process.stdout.write(String((b.files || []).length))' "$body")
    outcomes=$(node -e 'const b = require(process.argv[1]); const c = {}; for (const f of b.files || []) c[f.outcome] = (c[f.outcome] || 0) + 1; process.stdout.write(Object.entries(c).map(([k, v]) => `${k}×${v}`).join(" ") || JSON.stringify(b).slice(0, 120))' "$body")
    printf '%s\t%s\t%s\t%s\t%s\n' "$i" "$status" "$files" "$outcomes" "$secs" >> "$WORK/$label.tsv"
    log "M3 $label run $i: HTTP $status, $files files ($outcomes), ${secs}s"
    [ "$files" = "$want" ] || log "M3 $label run $i: expected $want files[] entries"
  done
}

p95() { sort -g "$1" | awk '{ v[NR] = $1 } END { i = int(NR * 0.95 + 0.999999); if (i < 1) i = 1; printf "%s", v[i] }'; }
table() {
  echo "| run | HTTP | files[] | outcomes | time_total (s) |"
  echo "| --- | --- | --- | --- | --- |"
  awk -F'\t' '{ printf "| %s | %s | %s | %s | %s |\n", $1, $2, $3, $4, $5 }' "$WORK/$1.tsv"
  cut -f5 "$WORK/$1.tsv" > "$WORK/$1.times"
  printf '| **p95** | | | | **%s** |\n| **max** | | | | %s |\n| **min** | | | | %s |\n' "$(p95 "$WORK/$1.times")" "$(sort -g "$WORK/$1.times" | tail -n1)" "$(sort -g "$WORK/$1.times" | head -n1)"
}

log "M3: $RUNS whole-bundle uploads (17 maps, $(calc "($capsize_bytes + 36084) / 1048576") MiB of maps) under $RELEASE"
series bundle "$RELEASE" 17 "${bundle_parts[@]}"
log "M3: $RUNS cap-size-only uploads under $RELEASE-capsize"
series capsize "$RELEASE-capsize" 1 "${capsize_parts[@]}"

cat <<MD
### M3 — upload wall-clock, whole bundle and cap-size map alone (labelled fallback, local compose, $(date -u +%Y-%m-%dT%H:%MZ))

Against the compose dashboard (BODY_SIZE_LIMIT=$(docker exec sentinel-dashboard env 2>/dev/null | awk -F= '/^BODY_SIZE_LIMIT=/ { print $2 }'), MinIO on the same host), releases \`$RELEASE\` (bundle) and \`$RELEASE-capsize\` (cap-size map alone), \`curl -w %{time_total}\` from the host. Run 1 of each series creates the rows (201); every later run is the identical bytes and must be 200 all-\`unchanged\`.

**Whole bundle — 17 maps (16 committed + the cap-size map), $capsize_bytes + 36,084 bytes of files:**

$(table bundle)

**Cap-size map alone ($capsize_bytes bytes), release \`$RELEASE-capsize\`:**

$(table capsize)

**sourcemap_upload_p95_ms**: whole bundle $(calc "1000 * $(p95 "$WORK/bundle.times")") ms, cap-size map alone $(calc "1000 * $(p95 "$WORK/capsize.times")") ms over $RUNS runs each — reported, no bound set (nobody has measured one; the ELB path is the held staging re-take's).

Rows after the runs:
\`\`\`
$(psql "$DATABASE_URL" -At -F' | ' -c "SELECT release_version, count(*) AS rows, sum(size_bytes) AS bytes FROM sourcemaps WHERE project_id = '$SEED_PROJECT_ID' AND release_version IN ('$RELEASE', '$RELEASE-capsize') GROUP BY 1 ORDER BY 1")
\`\`\`
Responses kept under \`$WORK\`.
MD
