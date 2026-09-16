# sentinel-occurrence-dedup-project §13 — helpers shared by occurrence-dedup-m1.sh and
# occurrence-dedup-m2.sh (sourced, never run). Sits on sourcemaps-measure-lib.sh (the processor's
# Prometheus text at $PROCESSOR_URL/metrics, the histogram Δ tables, the seed project's ids) and
# adds what §13 needs beyond it: the whole-table row count, the two unique indexes' catalog state,
# the ERROR_EVENTS stream's sequence (the redelivery instrument's input), and the counters that
# are read as deltas.
#
#   DATABASE_URL    postgres://sentinel:changeme@localhost:5432/sentinel   (psql, host side)
#   DASHBOARD_URL   http://localhost:3000
#   INGESTOR_URL    http://localhost:8080
#   PROCESSOR_URL   http://localhost:8081       (/metrics, /health)
#   NATS_URL        nats://localhost:4222       (nats CLI, host side — NATS_HOST_PORT if 4222 is taken)
#   PROCESSOR_CONTAINER  sentinel-processor     (docker logs for the notifier lines, docker inspect for the image)
#   POSTGRES_CONTAINER   sentinel-postgres      (docker logs for the migrate session's statement durations)
#   DEDUP_WORK      where snapshots go and where m1's baseline is kept between its two invocations
#                   (default /tmp/occurrence-dedup-m; m1 baseline and m1 run must share it)

. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/sourcemaps-measure-lib.sh"

NATS_URL="${NATS_URL:-nats://localhost:4222}"
POSTGRES_CONTAINER="${POSTGRES_CONTAINER:-sentinel-postgres}"
DEDUP_WORK="${DEDUP_WORK:-/tmp/occurrence-dedup-m}"
export NATS_URL

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"

# The dedup seed's ids (occurrence-dedup-seed.sql): the organization, project 01 and its ingest key —
# the load harness's credential for M2.
DEDUP_SEED_ORG_ID='00000000-0000-4000-8000-0000000000e1'
DEDUP_SEED_PROJECT_ID='00000000-0000-4000-8000-0000000000e2'
DEDUP_SEED_PROJECT_KEY='occurrence-dedup-seed-01'
DEDUP_SEED_INGEST_KEY='sk_seed_dedup_0123456789abcdef0123456789abcdef'

sql() { psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -At -c "$1"; }

# occurrences_total — SELECT count(*) FROM error_occurrences, the whole table (§13's primary reads the
# table, not the project: a duplicate stored on ANY issue of the project would show here).
occurrences_total() { sql "SELECT count(*) FROM error_occurrences"; }
issues_total() { sql "SELECT count(*) FROM issues"; }
project_issues() { sql "SELECT count(*) FROM issues WHERE project_id = '$1'"; }
project_occurrences() { sql "SELECT count(*) FROM error_occurrences eo JOIN issues i ON i.id = eo.issue_id WHERE i.project_id = '$1'"; }

# index_state <name> — "<indisvalid> <indexdef>" for one index of error_occurrences, or "absent".
index_state() {
  sql "SELECT COALESCE((SELECT indisvalid::text || ' ' || pg_get_indexdef(indexrelid) FROM pg_index WHERE indexrelid = to_regclass('$1')), 'absent')"
}

# stream_last_seq — the ERROR_EVENTS stream's last sequence: the messages a pass published are
# (last_seq before, last_seq after].
stream_last_seq() {
  nats --server "$NATS_URL" stream info ERROR_EVENTS --json | awk -F'[:,]' '/"last_seq"/ { gsub(/[^0-9]/, "", $2); print $2; exit }'
}

# consumer_info_field <field> — one numeric field of the processor's durable consumer.
consumer_info_field() {
  nats --server "$NATS_URL" consumer info ERROR_EVENTS processor-consumer --json | awk -F'[:,]' -v f="\"$1\"" '$1 ~ f { gsub(/[^0-9]/, "", $2); print $2; exit }'
}

# counter_delta <before> <after> <name> [label-substring] — the difference of one counter between two
# /metrics snapshots (0 when the series is absent from either, which is how OTel reports "never
# recorded").
counter_delta() { calc "$(metric "$2" "$3" "${4:-}") - $(metric "$1" "$3" "${4:-}")"; }

# naks <before> <after> — retried + deadlettered over the interval: §13 M2's ingest_naks_during_build.
naks() {
  calc "$(counter_delta "$1" "$2" sentinel_process_events_total 'outcome="retried"') + $(counter_delta "$1" "$2" sentinel_process_events_total 'outcome="deadlettered"')"
}

# wait_counter <name> <label-substring> <target> [timeout s] — wait_metric, but the target is an
# absolute counter value the caller computed from a snapshot.
wait_counter() { wait_metric "$1" "$2" "$3" "${4:-600}"; }

# wait_stable <name> <label-substring> [quiet s] — return once the counter has not moved for <quiet>
# seconds (the processor has drained what it was given).
wait_stable() {
  local name="$1" sub="$2" quiet="${3:-5}" last cur t=0
  last=$(curl -sS "$PROCESSOR_URL/metrics" | awk -v n="$name" -v s="$sub" '$1 ~ "^" n "(\\{|$)" && index($1, s) > 0 { v += $NF } END { printf "%s", (v == "" ? 0 : v) }')
  while [ "$t" -lt "$quiet" ]; do
    sleep 1
    cur=$(curl -sS "$PROCESSOR_URL/metrics" | awk -v n="$name" -v s="$sub" '$1 ~ "^" n "(\\{|$)" && index($1, s) > 0 { v += $NF } END { printf "%s", (v == "" ? 0 : v) }')
    if [ "$cur" = "$last" ]; then t=$((t + 1)); else t=0; last="$cur"; fi
  done
}

# processor_image — "<image> <created>" of the running processor container, so a report says which
# build consumed (M2: the previous tag's; M1: the feature's).
processor_image() {
  docker inspect --format '{{.Config.Image}} (image created {{.Image}})' "$PROCESSOR_CONTAINER" 2>/dev/null \
    | sed -E 's/\(image created (sha256:)?([0-9a-f]{12})[0-9a-f]*\)/(image \2)/'
}

# metrics_block <file> — the sentinel_* series of a snapshot with the otel_scope_* labels stripped,
# for pasting.
metrics_block() {
  grep -E "^sentinel_$2" "$1" | sed -E 's/otel_scope_[a-z_]+="[^"]*",?//g; s/\{,?/{/; s/,\}/}/'
}
