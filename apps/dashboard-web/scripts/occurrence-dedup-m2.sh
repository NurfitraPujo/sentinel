#!/usr/bin/env bash
# sentinel-occurrence-dedup-project §13 M2 — the LABELLED FALLBACK take on the local compose stack
# (contract decision 10). Evidence about order of magnitude, never the criterion met on the venue
# the policy names.
#
# What it measures, with the seed at ≥ 1,000,000 rows (occurrence-dedup-seed.sql, loaded BEFORE
# this) and the PREVIOUS tag's processor consuming (decision 17: v0.2.18, c5082e3 — the feature's
# arbiter without its index is 42P10 on every insert, so the feature build must NOT be the consumer
# while the file runs), while tests/load posts continuously at its default rate (200 events/s, 10
# workers — SENTINEL_LOAD_EVENTS_PER_SEC / SENTINEL_LOAD_WORKERS are left at their defaults; only
# the duration is raised so the harness outlasts the migration):
#
#   ingest_naks_during_build   Δ sentinel_process_events_total{outcome="retried"} + {outcome="deadlettered"}
#                              over the WHOLE `cmd/migrate up` run, first block to last (decision 17),
#                              with the M6-only slice (the CREATE UNIQUE INDEX CONCURRENTLY block)
#                              beside it — the primary: must be 0
#   dedup_backfill_seconds     wall-clock of block (b), the batched backfill, from the migrate
#                              session's statement log; batch size (10,000 rows by id range, one
#                              COMMIT per batch — the file's constant) and the per-batch UPDATE
#                              durations (min / mean / max) from auto_explain's nested-statement log
#                              (decision 20)
#   dedup_index_build_seconds  wall-clock of the CREATE UNIQUE INDEX CONCURRENTLY block, same log
#   indisvalid                 pg_index.indisvalid of uq_error_occurrences_project_event after the run
#
# both durations reported with the row count they were taken at, so the operator can scale them to
# production's 769,590 rows (decision 23) before the tag.
#
# HOW the block durations are read. cmd/migrate (goose) logs one line per FILE, not per block, and a
# per-batch duration is invisible to anything outside the DO block. So the migration is run as a
# dedicated role, `dedup_migrator` (created here, SUPERUSER — the compose database's `sentinel` is
# one too), whose per-role settings turn on `log_min_duration_statement = 0` (every top-level block
# logs its own duration) and auto_explain with `log_nested_statements = on` (every per-batch UPDATE
# inside the backfill's DO block logs its duration too). Nothing else's session is affected; the
# settings are read from the postgres container's log, FOLLOWED LIVE (`docker logs -f` from before the
# first block — on a journald-backed podman the journal rotates within minutes under this much
# output and `docker logs` afterwards no longer reaches the start of the run). A second watcher polls
# pg_stat_activity for that role's active statement every 0.5 s so the M6 slice can be cut from the
# /metrics samples taken at the build block's first and last sighting.
#
#   usage: apps/dashboard-web/scripts/occurrence-dedup-m2.sh > m2.md
#
# Preconditions (refused, not assumed): the migration is NOT yet applied (no project_id column, ledger
# below 1725200000); the dedup seed's ingest key exists; the stack answers. The migrate binary is
# built from THIS checkout (the feature branch — the file under test), run host-side against
# $M2_MIGRATE_DB_URL (default: $DATABASE_URL with sslmode=disable, as `dedup_migrator`).
#
#   M2_LOAD_DURATION   seconds the load harness runs for (default 1200; it is killed when the
#                      migration returns and the processor has drained)
#
# Output is markdown on stdout, progress on stderr. Snapshots and logs are kept under $DEDUP_WORK/m2.
set -euo pipefail
. "$(dirname "${BASH_SOURCE[0]}")/occurrence-dedup-measure-lib.sh"

M2_LOAD_DURATION="${M2_LOAD_DURATION:-1200}"
MIGRATOR_ROLE='dedup_migrator'
MIGRATOR_PASSWORD='dedup_migrator'
WORK="$DEDUP_WORK/m2"
mkdir -p "$WORK"
rm -f "$WORK"/*.before "$WORK"/*.after "$WORK"/sample.* "$WORK"/samples.tsv "$WORK"/activity.tsv "$WORK"/*.log

# --- preconditions ---
if [ "$(sql "SELECT count(*) FROM information_schema.columns WHERE table_name = 'error_occurrences' AND column_name = 'project_id'")" != 0 ]; then
  echo "M2: error_occurrences.project_id already exists — 1725200000 has run; M2 measures the run itself. Reset the database (docker compose down -v) and reload the seed." >&2
  exit 2
fi
if [ "$(sql "SELECT count(*) FROM project_api_keys WHERE key_hash = encode(digest('$DEDUP_SEED_INGEST_KEY'::bytea, 'sha256'), 'hex') AND status = 'active'")" != 1 ]; then
  echo "M2: the dedup seed's ingest key is not in project_api_keys — load occurrence-dedup-seed.sql first." >&2
  exit 2
fi
curl -sSf "$PROCESSOR_URL/health" >/dev/null
curl -sSf "$INGESTOR_URL/health" >/dev/null
ROWS_BEFORE=$(occurrences_total)
log "M2: error_occurrences has $ROWS_BEFORE rows; processor is $(processor_image)"

# --- the migrate role and its per-session logging ---
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -q <<SQL
DO \$\$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '$MIGRATOR_ROLE') THEN
    CREATE ROLE $MIGRATOR_ROLE LOGIN SUPERUSER PASSWORD '$MIGRATOR_PASSWORD';
  END IF;
END \$\$;
ALTER ROLE $MIGRATOR_ROLE SET log_min_duration_statement = 0;
ALTER ROLE $MIGRATOR_ROLE SET session_preload_libraries = 'auto_explain';
ALTER ROLE $MIGRATOR_ROLE SET auto_explain.log_min_duration = 0;
ALTER ROLE $MIGRATOR_ROLE SET auto_explain.log_nested_statements = on;
ALTER ROLE $MIGRATOR_ROLE SET auto_explain.log_analyze = off;
ALTER ROLE $MIGRATOR_ROLE SET auto_explain.log_timing = off;
SQL
M2_MIGRATE_DB_URL="${M2_MIGRATE_DB_URL:-$(printf '%s' "$DATABASE_URL" | sed -E "s#//[^@]*@#//$MIGRATOR_ROLE:$MIGRATOR_PASSWORD@#; s#\?.*##")?sslmode=disable}"

# --- the migrate binary, from this checkout ---
log "M2: building cmd/migrate from $ROOT/packages/db-migrations"
(cd "$ROOT/packages/db-migrations" && GOWORK=off go build -o "$WORK/migrate" ./cmd/migrate)

# --- background watchers ---
sampler() {
  # every second: unix time, retried, deadlettered, stored, duplicate — the M6 slice is cut from these.
  while :; do
    local f="$WORK/sample.$(date +%s)"
    curl -sS "$PROCESSOR_URL/metrics" > "$f" 2>/dev/null || true
    printf '%s\t%s\t%s\t%s\t%s\n' "$(date +%s)" \
      "$(metric "$f" sentinel_process_events_total 'outcome="retried"')" \
      "$(metric "$f" sentinel_process_events_total 'outcome="deadlettered"')" \
      "$(metric "$f" sentinel_process_events_total 'outcome="stored"')" \
      "$(metric "$f" sentinel_process_events_total 'outcome="duplicate"')" >> "$WORK/samples.tsv"
    sleep 1
  done
}
activity_watcher() {
  # every 0.5 s: what the migrate role is running (the block), so each block's window is known.
  while :; do
    psql "$DATABASE_URL" -At -F $'\t' -c "SELECT extract(epoch FROM now())::bigint, state, left(regexp_replace(query, '\s+', ' ', 'g'), 90) FROM pg_stat_activity WHERE usename = '$MIGRATOR_ROLE' AND state <> 'idle'" 2>/dev/null >> "$WORK/activity.tsv" || true
    sleep 0.5
  done
}
# kill_tree <pid> — the harness is `go test` with the compiled load.test binary as its child; only
# THIS run's tree is killed, never another load.test on the machine.
kill_tree() { local c; for c in $(pgrep -P "$1" 2>/dev/null); do kill_tree "$c"; done; kill "$1" 2>/dev/null || true; }
sampler & SAMPLER_PID=$!
activity_watcher & WATCHER_PID=$!
LOAD_PID=
PGLOG_PID=
cleanup() {
  kill "$SAMPLER_PID" "$WATCHER_PID" 2>/dev/null || true
  if [ -n "$PGLOG_PID" ]; then kill "$PGLOG_PID" 2>/dev/null || true; fi
  if [ -n "$LOAD_PID" ]; then kill_tree "$LOAD_PID"; fi
}
trap cleanup EXIT

# --- the load harness, at its default rate ---
log "M2: starting tests/load (default rate: 200 events/s, 10 workers) for up to ${M2_LOAD_DURATION}s"
(cd "$ROOT" && SENTINEL_LOAD_INGESTER_URL="$INGESTOR_URL" SENTINEL_LOAD_API_KEY="$DEDUP_SEED_INGEST_KEY" \
  SENTINEL_LOAD_PROJECT_KEY="$DEDUP_SEED_PROJECT_KEY" SENTINEL_LOAD_DURATION_SEC="$M2_LOAD_DURATION" \
  go test ./tests/load/ -run '^TestLoadIngest$' -v -count=1 -timeout "$((M2_LOAD_DURATION + 300))s" > "$WORK/load.log" 2>&1) &
LOAD_PID=$!
# Let it reach the processor before the migration starts: stored must be moving.
stored0=$(curl -sS "$PROCESSOR_URL/metrics" | awk '$1 ~ /^sentinel_process_events_total\{/ && index($1, "outcome=\"stored\"") { v += $NF } END { print v + 0 }')
wait_metric sentinel_process_events_total 'outcome="stored"' "$((stored0 + 500))" 120 || { echo "M2: the load harness is not reaching the processor — see $WORK/load.log" >&2; exit 1; }
log "M2: load flowing (stored moved by ≥ 500)"

# --- the migration ---
docker logs -f --since "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$POSTGRES_CONTAINER" > "$WORK/postgres.log" 2>&1 &
PGLOG_PID=$!
sleep 2
metrics_snapshot "$WORK/migrate.before"
T0=$(date +%s.%N)
log "M2: cmd/migrate up -target=processor as $MIGRATOR_ROLE"
(cd "$ROOT/packages/db-migrations" && DB_URL_PROCESSOR="$M2_MIGRATE_DB_URL" "$WORK/migrate" up -target=processor > "$WORK/migrate.log" 2>&1) && MIGRATE_RC=0 || MIGRATE_RC=$?
T1=$(date +%s.%N)
metrics_snapshot "$WORK/migrate.after"
MIGRATE_SECONDS=$(calc "$T1 - $T0")
log "M2: cmd/migrate exited $MIGRATE_RC after ${MIGRATE_SECONDS}s"

# Let the harness keep posting 30 s past the run (a nak caused by the last block but classified
# after it must not be missed), then stop it and let what is in flight settle. The processor is NOT
# waited for to drain: on this one-host stack it consumes ~20–30 events/s against a 200 events/s
# harness, so the stream backlog it leaves behind is reported (num_pending), not waited out.
sleep 30
metrics_snapshot "$WORK/tail.after"
kill_tree "$LOAD_PID"; LOAD_PID=
sleep 15
metrics_snapshot "$WORK/drained.after"
kill "$SAMPLER_PID" "$WATCHER_PID" 2>/dev/null || true
PENDING=$(consumer_info_field num_pending)
ROWS_AFTER=$(occurrences_total)

# --- the postgres log: the migrate session's statements (the follower has everything since before block one) ---
sleep 2; kill "$PGLOG_PID" 2>/dev/null || true; PGLOG_PID=
# block_seconds <statement substring> — the duration (s) of the top-level statement whose text
# contains the substring, from `duration: X ms  statement: ...` (log_min_duration_statement). A
# multi-line statement (the DO blocks) continues on tab-indented lines, so the log is read as
# records: a record starts on a timestamped line and the substring is matched against the whole record.
block_seconds() {
  awk -v s="$1" '
    function flush() { if (rec != "" && index(rec, s) > 0 && match(rec, /duration: [0-9.]+ ms  statement:/)) { split(substr(rec, RSTART + 10), a, " "); printf "%.3f\n", a[1] / 1000; found = 1 } rec = "" }
    /^[0-9]{4}-[0-9]{2}-[0-9]{2} / { if (found) exit; flush() }
    { rec = rec $0 "\n" }
    END { if (!found) flush() }' "$WORK/postgres.log"
}
# The backfill's DO block and the build, by their text; the gate and the check by theirs.
BACKFILL_S=$(block_seconds 'batch_size CONSTANT integer := 10000' || true)
BUILD_S=$(block_seconds 'CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_error_occurrences_project_event' || true)
GATE_S=$(block_seconds '1725200000 gate (c)' || true)
CHECK_S=$(block_seconds '1725200000 check (e)' || true)
ALTER_S=$(block_seconds 'ALTER TABLE error_occurrences ADD COLUMN IF NOT EXISTS project_id' || true)
# Per-batch UPDATE durations: auto_explain logs `duration: X ms  plan:` and, on the next line,
# `Query Text: UPDATE error_occurrences o ...` for every nested statement.
awk '/duration: [0-9.]+ ms  plan:/ { for (i = 1; i <= NF; i++) if ($i == "duration:") d = $(i + 1); next }
     /Query Text: UPDATE error_occurrences o/ { if (d != "") print d; d = "" }
     { d = "" }' "$WORK/postgres.log" > "$WORK/batches.ms"
BATCHES=$(wc -l < "$WORK/batches.ms" | tr -d ' ')
if [ "$BATCHES" -gt 0 ]; then
  read -r BATCH_MIN BATCH_MEAN BATCH_MAX BATCH_SUM < <(awk 'NR == 1 { min = $1; max = $1 } { s += $1; if ($1 < min) min = $1; if ($1 > max) max = $1 } END { printf "%.1f %.1f %.1f %.1f\n", min, s / NR, max, s / 1000 }' "$WORK/batches.ms")
else
  BATCH_MIN=- BATCH_MEAN=- BATCH_MAX=- BATCH_SUM=-
fi

# --- the M6 slice: the build block's first and last sighting in pg_stat_activity, and the samples there ---
BUILD_T0=$(awk -F'\t' 'index($3, "CREATE UNIQUE INDEX CONCURRENTLY") { print $1; exit }' "$WORK/activity.tsv" || true)
BUILD_T1=$(awk -F'\t' 'index($3, "CREATE UNIQUE INDEX CONCURRENTLY") { t = $1 } END { print t }' "$WORK/activity.tsv" || true)
slice_naks() {
  # naks between the sample at or before $1 and the sample at or after $2.
  awk -F'\t' -v a="$1" -v b="$2" '
    $1 <= a { ra = $2; da = $3 }
    $1 >= b && !done { rb = $2; db = $3; done = 1 }
    END { if (!done) { rb = $2; db = $3 } ; printf "%d", (rb - ra) + (db - da) }' "$WORK/samples.tsv"
}
if [ -n "$BUILD_T0" ] && [ -n "$BUILD_T1" ]; then
  M6_NAKS=$(slice_naks "$BUILD_T0" "$BUILD_T1")
  M6_WINDOW="$(date -u -d @"$BUILD_T0" +%H:%M:%S)–$(date -u -d @"$((BUILD_T1 + 1))" +%H:%M:%S) UTC (pg_stat_activity sightings, 0.5 s resolution)"
else
  M6_NAKS='-'; M6_WINDOW='the build block was never sighted in pg_stat_activity (shorter than the 0.5 s poll)'
fi

NAKS_RUN=$(naks "$WORK/migrate.before" "$WORK/migrate.after")
NAKS_TAIL=$(naks "$WORK/migrate.before" "$WORK/tail.after")
NAKS_DRAINED=$(naks "$WORK/migrate.before" "$WORK/drained.after")
STORED_RUN=$(counter_delta "$WORK/migrate.before" "$WORK/migrate.after" sentinel_process_events_total 'outcome="stored"')
STORED_TAIL=$(counter_delta "$WORK/migrate.before" "$WORK/tail.after" sentinel_process_events_total 'outcome="stored"')
STORED_DRAINED=$(counter_delta "$WORK/migrate.before" "$WORK/drained.after" sentinel_process_events_total 'outcome="stored"')
NEW_INDEX=$(index_state uq_error_occurrences_project_event)
OLD_INDEX=$(index_state uq_error_occurrences_issue_event)
NULL_ROWS=$(sql "SELECT count(*) FROM error_occurrences WHERE project_id IS NULL")
LEDGER=$(sql "SELECT max(version_id) FROM schema_migrations")
HEALTH=$(curl -sS "$PROCESSOR_URL/health")
LOAD_SUMMARY=$(grep -E '^(Total Sent|Total Accepted|Total Rejected|Total Errors|Success Rate|Events/sec):' "$WORK/load.log" | tr '\n' ';' || true)

cat <<MD
### M2 — the migration under load (labelled fallback, local compose, $(date -u +%Y-%m-%dT%H:%MZ))

Consumer during the run: **$(processor_image)** (the previous tag's processor, decision 17).
Migrate binary: \`cmd/migrate\` built from this checkout, run host-side as \`$MIGRATOR_ROLE\` with
\`up -target=processor\` — exit **$MIGRATE_RC**, ledger now at **$LEDGER**. Load: \`tests/load\` at its
default rate (200 events/s, 10 workers) from before the first block to 30 s past the last
(harness: ${LOAD_SUMMARY:-killed after the run; see load.log}).

| Metric | Value | Taken at |
| --- | --- | --- |
| **ingest_naks_during_build** (whole \`cmd/migrate up\` run, first block to last) | **$NAKS_RUN** | retried + deadlettered Δ over ${MIGRATE_SECONDS}s; $STORED_RUN stored in the same window |
| ingest_naks, run + 30 s more under load | $NAKS_TAIL | $STORED_TAIL stored since the run began |
| ingest_naks, 15 s after the harness was stopped | $NAKS_DRAINED | $STORED_DRAINED stored since the run began; $PENDING messages still pending on the consumer (the processor consumes ~$(calc "$STORED_DRAINED / ($(date +%s) - ${T0%.*})") events/s here against the 200 events/s harness — the backlog is the venue's, not the migration's) |
| ingest_naks, M6-only slice (the \`CREATE UNIQUE INDEX CONCURRENTLY\` block) | $M6_NAKS | $M6_WINDOW |
| **dedup_backfill_seconds** (block (b), one DO block) | **${BACKFILL_S:-not found in the log}** | $ROWS_BEFORE rows before the run, $NULL_ROWS with project_id NULL after it |
| backfill batch size | 10,000 rows by id range, one COMMIT per batch (the file's constant) | $BATCHES batches logged |
| per-batch UPDATE duration (ms) min / mean / max | $BATCH_MIN / $BATCH_MEAN / $BATCH_MAX | sum ${BATCH_SUM}s of the block's ${BACKFILL_S:-?}s |
| **dedup_index_build_seconds** (the CONCURRENTLY block) | **${BUILD_S:-not found in the log}** | $ROWS_AFTER rows when the build finished |
| gate (c) / check (e) / ADD COLUMN (s) | ${GATE_S:-?} / ${CHECK_S:-?} / ${ALTER_S:-?} | |
| whole \`cmd/migrate up\` wall-clock (s) | $MIGRATE_SECONDS | includes goose's ledger bookkeeping |
| **pg_index.indisvalid, uq_error_occurrences_project_event** | **${NEW_INDEX%% *}** | \`${NEW_INDEX#* }\` |
| uq_error_occurrences_issue_event (kept, decision 5) | ${OLD_INDEX%% *} | \`${OLD_INDEX#* }\` |
| processor /health after the run | \`$HEALTH\` | |

migrate log:

\`\`\`
$(cat "$WORK/migrate.log")
\`\`\`

The migrate session's top-level statements from the postgres log (log_min_duration_statement = 0
for \`$MIGRATOR_ROLE\`; the backfill's per-batch lines are summarised above):

\`\`\`
$(grep -E 'duration: [0-9.]+ ms  statement:' "$WORK/postgres.log" | sed -E 's/^([^ ]+ [^ ]+) \[[0-9]+\] /\1 /; s/(statement: .{0,110}).*/\1 …/' || true)
\`\`\`

Process counters, Δ over the run (before → at the migrate exit → 30 s later under load → 15 s after the harness stopped):

\`\`\`
$(for o in stored duplicate retried deadlettered; do printf 'sentinel_process_events_total{outcome="%s"}  %s → +%s → +%s → +%s\n' "$o" "$(metric "$WORK/migrate.before" sentinel_process_events_total "outcome=\"$o\"")" "$(counter_delta "$WORK/migrate.before" "$WORK/migrate.after" sentinel_process_events_total "outcome=\"$o\"")" "$(counter_delta "$WORK/migrate.before" "$WORK/tail.after" sentinel_process_events_total "outcome=\"$o\"")" "$(counter_delta "$WORK/migrate.before" "$WORK/drained.after" sentinel_process_events_total "outcome=\"$o\"")"; done)
\`\`\`

Snapshots, the per-second samples, the pg_stat_activity trace, the harness and postgres logs are
under \`$WORK\`.
MD
