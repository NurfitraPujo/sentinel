#!/usr/bin/env bash
# sentinel-sourcemaps §13 M1 — the LABELLED FALLBACK take on the local compose stack (decision 15).
# Evidence about shape and order of magnitude, never the criterion met; the criterion's venue is
# sentinel-staging-http via the held task, once a rig is registered.
#
# Primary — the fingerprint-set equality (flows S1/S2/S5). Drives sourcemaps-seed.sh phase by
# phase and snapshots the seed project's `issues.fingerprint` set between them:
#
#   A   = the set after build A's 1,000 events under R1 (maps uploaded)
#   A∪B = the set after build B's 1,000 events under R1 (build B's maps uploaded, same source)
#   R0A = the NEW fingerprints after build A's events under R0 (no maps — normalisation alone)
#   R0B = the NEW fingerprints after build B's events under R0
#
# and reports |A|, whether A∪B = A (build B created no issue), |A| = N (the README's 60), and the
# same for R0A/R0B. Secondary — the mean of sentinel_process_duration_seconds over the R1 events
# WITH A WARM CACHE (build A's events posted a second time, after every map was fetched once)
# minus the mean over the R0 events, from the histogram's Δ_sum/Δ_count, with every run's bucket
# table pasted. No p95 is claimed: the buckets are 5/10/25 ms and there is no Prometheus on
# compose, so a millisecond-level p95 is not resolvable here.
#
#   usage: apps/dashboard-web/scripts/sourcemaps-m1.sh > m1.md
#
# Output is markdown on stdout, progress on stderr. Re-seeds the project (phase setup) first.
set -euo pipefail
. "$(dirname "${BASH_SOURCE[0]}")/sourcemaps-measure-lib.sh"

WORK="$(mktemp -d -t sourcemaps-m1.XXXXXX)"
export SEED_DIR="$WORK/seed"
N=$(node -e 'process.stdout.write(String(require(process.argv[1]).TRIPLES))' "$(dirname "$SEED")/../../../tests/fixtures/sourcemaps/generate-events.mjs" 2>/dev/null || echo 60)

log "M1: work dir $WORK"
"$SEED" setup generate upload-a >&2

phase() {
  # phase <label> <seed phase> <expected occurrences after> — run, wait, snapshot.
  local label="$1" p="$2" want="$3"
  metrics_snapshot "$WORK/$label.before"
  "$SEED" "$p" >&2
  wait_occurrences "$want" 600 >/dev/null
  sleep 2
  metrics_snapshot "$WORK/$label.after"
  fingerprint_set "$WORK/$label.set"
  log "M1: $label — $(wc -l < "$WORK/$label.set") fingerprints, $(wait_occurrences "$want" 1) occurrences"
}

phase a-cold events-a 1000
phase a-warm events-a 2000
"$SEED" upload-b >&2
phase b events-b 3000
phase r0a events-r0a 4000
phase r0b events-r0b 5000

# --- the sets ---
setsize() { wc -l < "$1" | tr -d ' '; }
A=$(setsize "$WORK/a-cold.set"); AW=$(setsize "$WORK/a-warm.set"); AB=$(setsize "$WORK/b.set")
R0A_ALL=$(setsize "$WORK/r0a.set"); R0B_ALL=$(setsize "$WORK/r0b.set")
comm -13 "$WORK/b.set" "$WORK/r0a.set" > "$WORK/r0a.new"      # fingerprints R0A added beyond A∪B
comm -13 "$WORK/r0a.set" "$WORK/r0b.set" > "$WORK/r0b.new"    # fingerprints R0B added beyond that
R0A=$(setsize "$WORK/r0a.new"); R0B_NEW=$(setsize "$WORK/r0b.new")
A_UNION_B_EQ_A=$([ "$AB" -eq "$A" ] && cmp -s "$WORK/a-cold.set" "$WORK/b.set" && echo yes || echo NO)
R0_EQ=$([ "$R0B_NEW" -eq 0 ] && echo yes || echo NO)

cat <<MD
### M1 — fingerprint-set equality and the per-event cost (labelled fallback, local compose, $(date -u +%Y-%m-%dT%H:%MZ))

| Run | Events | Fingerprints in the project after it | Claim |
| --- | --- | --- | --- |
| build A under R1, maps for A (cold) | 1,000 | \|A\| = **$A** | \|A\| = N = $N: **$([ "$A" -eq "$N" ] && echo yes || echo NO)** |
| build A under R1 again (warm cache) | 1,000 | $AW | unchanged: **$([ "$AW" -eq "$A" ] && echo yes || echo NO)** |
| build B under R1, maps for A and B | 1,000 | \|A ∪ B\| = **$AB** | **A ∪ B = A: $A_UNION_B_EQ_A** (build B created no issue) |
| build A under R0 (no maps) | 1,000 | $R0A_ALL (= \|A\| + \|R0A\|) | \|R0A\| = **$R0A** = N: **$([ "$R0A" -eq "$N" ] && echo yes || echo NO)** |
| build B under R0 (no maps) | 1,000 | $R0B_ALL | **R0A ∪ R0B = R0A: $R0_EQ** ($R0B_NEW new) — the normalisation alone (S5) |

sentinel_process_duration_seconds, Δ per run (the histogram is process-wide; nothing else was posting during the run):

**build A under R1, cold** (the first event on each map fetches it)

$(histogram_table "$WORK/a-cold.before" "$WORK/a-cold.after" sentinel_process_duration_seconds)

**build A under R1, warm cache** — the R1 mean

$(histogram_table "$WORK/a-warm.before" "$WORK/a-warm.after" sentinel_process_duration_seconds)

**build B under R1** (build B's maps cold on the first event each)

$(histogram_table "$WORK/b.before" "$WORK/b.after" sentinel_process_duration_seconds)

**build A under R0, no maps** — the R0 mean (every frame \`no_map\`, negative-cache hits after the first per path)

$(histogram_table "$WORK/r0a.before" "$WORK/r0a.after" sentinel_process_duration_seconds)

**build B under R0, no maps**

$(histogram_table "$WORK/r0b.before" "$WORK/r0b.after" sentinel_process_duration_seconds)

MD
warm_mean=$(calc "1000 * ($(metric "$WORK/a-warm.after" sentinel_process_duration_seconds_sum) - $(metric "$WORK/a-warm.before" sentinel_process_duration_seconds_sum)) / ($(metric "$WORK/a-warm.after" sentinel_process_duration_seconds_count) - $(metric "$WORK/a-warm.before" sentinel_process_duration_seconds_count))")
r0_mean=$(calc "1000 * ($(metric "$WORK/r0a.after" sentinel_process_duration_seconds_sum) - $(metric "$WORK/r0a.before" sentinel_process_duration_seconds_sum)) / ($(metric "$WORK/r0a.after" sentinel_process_duration_seconds_count) - $(metric "$WORK/r0a.before" sentinel_process_duration_seconds_count))")
cat <<MD
**symbolication_process_mean_delta_ms = R1 warm mean − R0 mean = $warm_mean − $r0_mean = $(calc "$warm_mean - $r0_mean") ms** (a number, not a bound; no p95 claimed).

Cache counters after the R1 runs (S9 — after warm-up, hit ≫ miss):

\`\`\`
$(grep -E '^sentinel_sourcemap_cache_events_total|^sentinel_symbolication_(events|frames)_total' "$WORK/b.after" | sed -E 's/otel_scope_[a-z_]+="[^"]*",?//g; s/\{,?/{/; s/,\}/}/')
\`\`\`

Sets and snapshots kept under \`$WORK\`.
MD
