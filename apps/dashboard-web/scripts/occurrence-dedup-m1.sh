#!/usr/bin/env bash
# sentinel-occurrence-dedup-project §13 M1 — the LABELLED FALLBACK take on the local compose stack
# (contract decision 10). Evidence about order of magnitude, never the criterion met on the venue
# the policy names.
#
# Primary — occurrence_dedup_cross_issue_stored_rows (flows P1 → P3 → D1–D4): on the FEATURE
# processor, after occurrence-dedup-seed.sql (≥ 1,000,000 rows) and M2's migration:
#
#   pass one   10,000 `javascript` events for release R1 whose frames name the chunks of the
#              committed sentinel-sourcemaps fixture bundle (tests/fixtures/sourcemaps/, build A,
#              sourcemaps-seed.sh's events-a posted ten times — the ingestor mints a fresh event_id
#              per posting, so the ten postings are 10,000 distinct events) through /ingest/batch,
#              WITH NO MAPS uploaded: every frame is `no_map`, the fingerprints hash the minified
#              names, N = 60 issues appear (the generator's TRIPLES);
#   the maps   build A's 8 maps + the cap-size map uploaded for R1 (POST /api/sourcemaps), then
#              SOURCEMAP_NEGATIVE_TTL waited out;
#   pass two   THE SAME 10,000 MESSAGES REDELIVERED BYTE-FOR-BYTE. The instrument (contract §13
#              names both candidates; this is the second): the processor's durable pull consumer
#              `processor-consumer` is deleted and re-created on ERROR_EVENTS with
#              `--deliver=<the first pass's starting stream sequence>` WHILE THE PROCESSOR KEEPS
#              RUNNING — its pull subscription simply continues against the re-created consumer,
#              which redelivers stream sequences (S0, S1] (the pass-one messages, and nothing else,
#              because nothing else was published after them). The stream messages themselves are
#              what is redelivered — same bytes, same headers, same traceparent — which is exactly
#              what a JetStream redelivery in production is. Every event now symbolicates and hashes
#              to a DIFFERENT fingerprint (the real trigger, decision 24 proves it happened).
#
#   and reads: SELECT count(*) FROM error_occurrences  +10,000 after pass one, UNCHANGED after pass two
#              SELECT count(*) FROM issues              unchanged after pass two (the phantom issues never created)
#              outcome="duplicate"                      = 10,000 for pass two
#              sentinel_symbolication_events_total{outcome=~"applied_.*"}  Δ ≈ 0 on pass one, ≥ 10,000 on pass two
#              D1  the project's `/` (the real page, signed in as the seed's engineer) lists N issues, one per error
#              D2  each issue's `count` sums to 10,000 — one occurrence per event
#              D3  releases.occurrence_count for R1 moved by 10,000 on pass one and by 0 on pass two
#              D4  the alert channel: alerts dispatched on pass one, ZERO on pass two (the processor's
#                  notifier log, event=alert.telegram.sent, and sentinel_alert_dispatch_total)
#
# Secondary — storeevent_insert_mean_delta_ms: the mean of sentinel_process_duration_seconds over
# pass one (10,000 fresh events with BOTH unique indexes present) minus the same mean over 10,000
# of the same events on the PREVIOUS tag's processor with ONE index (the `baseline` phase, run
# before M2's migration on the same ≥ 1,000,000-row table). Both bucket tables are pasted; NO p95
# is claimed — the histogram's 5/10/25 ms buckets cannot resolve a sub-millisecond index cost.
#
#   usage: apps/dashboard-web/scripts/occurrence-dedup-m1.sh baseline > m1-baseline.md   # v0.2.18 processor, BEFORE 1725200000
#          apps/dashboard-web/scripts/occurrence-dedup-m1.sh run      > m1.md            # feature processor, AFTER M2
#          apps/dashboard-web/scripts/occurrence-dedup-m1.sh restore                     # put the consumer back to --deliver=all
#
# Both phases share $DEDUP_WORK (the baseline's mean and the generated events are kept there).
# `restore` matters: a consumer created with --deliver=<seq> cannot be UPDATEd to deliver=all, which
# is what the processor's ensureConsumer does on every start — so a processor RESTART after `run`
# fails until the consumer is re-created (the stream is purged first so deliver=all does not replay
# the run). `run` prints the reminder.
#
# D4 needs the processor to have somewhere to send: the alert sink below listens on the host
# ($M1_SINK_PORT, default 18025) and the processor must carry
#   ALERT_TELEGRAM_BOT_TOKEN=m1  ALERT_TELEGRAM_CHAT_ID=m1
#   ALERT_TELEGRAM_API_BASE_URL=http://host.containers.internal:18025   (host.docker.internal on Docker)
# (a compose override file for the processor service — never an edit to docker-compose.yml). Without
# them pass one dispatches nothing and D4 reads as "not exercised", which the report says.
#
# Output is markdown on stdout, progress on stderr.
set -euo pipefail
. "$(dirname "${BASH_SOURCE[0]}")/occurrence-dedup-measure-lib.sh"

PHASE="${1:-}"
[ -n "$PHASE" ] || { echo "usage: $0 baseline|run|restore" >&2; exit 2; }
WORK="$DEDUP_WORK/m1"
mkdir -p "$WORK"
export SEED_DIR="$WORK/seed"
M1_SINK_PORT="${M1_SINK_PORT:-18025}"
PASSES=10   # × sourcemaps-seed.sh's 1,000 events-a = 10,000
EVENTS=$((PASSES * 1000))
N=$(node -e 'process.stdout.write(String(require(process.argv[1]).TRIPLES))' "$ROOT/tests/fixtures/sourcemaps/generate-events.mjs" 2>/dev/null || echo 60)
ALERT_THRESHOLD=100   # per issue, 1 h window: 10,000 / 60 ≈ 167 occurrences per issue → one alert per issue on pass one

nats_cli() { nats --server "$NATS_URL" "$@"; }

# post_pass <label> — events-a PASSES times; wait until the seed project holds <label>'s expected rows.
post_pass() {
  local i
  for i in $(seq 1 "$PASSES"); do "$SEED" events-a >&2; done
}

# hist_mean <before> <after> — mean (ms) of sentinel_process_duration_seconds over the interval.
hist_mean() {
  calc "1000 * ($(metric "$2" sentinel_process_duration_seconds_sum) - $(metric "$1" sentinel_process_duration_seconds_sum)) / ($(metric "$2" sentinel_process_duration_seconds_count) - $(metric "$1" sentinel_process_duration_seconds_count))"
}

# --- the alert sink: answers every POST with Telegram's {"ok":true} and appends one line per call ---
sink_start() {
  : > "$WORK/sink.log"
  python3 - "$M1_SINK_PORT" "$WORK/sink.log" >/dev/null 2>&1 <<'PY' &
import sys, http.server, time
port, path = int(sys.argv[1]), sys.argv[2]
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get('Content-Length') or 0)
        body = self.rfile.read(n)
        with open(path, 'a') as f:
            f.write(f"{time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime())} {self.path} {len(body)}B\n")
        out = b'{"ok":true,"result":{"message_id":1}}'
        self.send_response(200); self.send_header('Content-Type', 'application/json'); self.send_header('Content-Length', str(len(out)))
        self.end_headers(); self.wfile.write(out)
    def log_message(self, *a): pass
http.server.ThreadingHTTPServer(('0.0.0.0', port), H).serve_forever()
PY
  SINK_PID=$!
}
sink_count() { wc -l < "$WORK/sink.log" | tr -d ' '; }

# The processor's log, followed live from the start of the run into $WORK/processor.log: on a
# journald-backed podman the journal rotates within minutes under 10,000 events' worth of lines, so
# `docker logs` read afterwards no longer reaches pass one. Every log read below is from this file.
processor_log_follow() {
  docker logs -f --since "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$PROCESSOR_CONTAINER" > "$WORK/processor.log" 2>&1 &
  PLOG_PID=$!
  sleep 2
}
# notifier_sent_lines — the processor's notifier log lines that record a delivered alert.
notifier_sent_lines() { grep -c '"event":"alert.telegram.sent"' "$WORK/processor.log" || true; }
# plog_since <RFC3339> — the followed log's lines at or after a timestamp (the lines are JSON with "time" first).
plog_since() { awk -v t="$1" 'substr($0, 10, 19) >= substr(t, 1, 19)' "$WORK/processor.log"; }

# --- D1 through the real page: sign in as the seed's engineer (the releases-snooze-seed.sql recipe,
# tests/e2e U24's cookie handling) and count the issue cards of /?project=<id>&status=all, page by page.
dashboard_issue_cards() {
  local email='sourcemaps-engineer@example.invalid' secret='dev-only-insecure-secret-change-me-please-32chars'
  local token hash jar total=0 page=1 n loc
  token=$(openssl rand -hex 16); hash=$(printf '%s%s' "$token" "$secret" | sha256sum | cut -d' ' -f1)
  sql "INSERT INTO verification_token (identifier, token, expires) VALUES ('$email', '$hash', now() + interval '15 minutes')" >/dev/null
  jar="$WORK/cookies.txt"; : > "$jar"
  loc=$(curl -sS -o /dev/null -w '%{http_code} %{redirect_url}' -c "$jar" "$DASHBOARD_URL/auth/callback/email?token=$token&email=$email&callbackUrl=/")
  local sess; sess=$(awk '$6 ~ /session-token/ { print $7 }' "$jar" | tail -n1)
  if [ -z "$sess" ]; then echo "sign-in failed ($loc)"; return 0; fi
  while :; do
    n=$(curl -sS -b "authjs.session-token=$sess" "$DASHBOARD_URL/?project=$SEED_PROJECT_ID&status=all&page=$page" | grep -o 'href="/issues/[0-9a-f-]*"' | sort -u | wc -l | tr -d ' ')
    [ "$n" -gt 0 ] || break
    total=$((total + n)); page=$((page + 1))
    [ "$page" -le 50 ] || break
  done
  echo "$total issue cards over $((page - 1)) page(s) of 20 (sign-in callback: $loc)"
}

case "$PHASE" in

baseline)
  if [ "$(sql "SELECT count(*) FROM information_schema.columns WHERE table_name = 'error_occurrences' AND column_name = 'project_id'")" != 0 ]; then
    echo "baseline: error_occurrences.project_id exists — the baseline is the PREVIOUS tag with ONE index, before 1725200000" >&2; exit 2
  fi
  log "baseline: processor $(processor_image); $(occurrences_total) rows in error_occurrences"
  "$SEED" setup generate >&2
  metrics_snapshot "$WORK/baseline.before"
  post_pass
  wait_occurrences "$EVENTS" 900 >/dev/null; sleep 3
  metrics_snapshot "$WORK/baseline.after"
  MEAN=$(hist_mean "$WORK/baseline.before" "$WORK/baseline.after")
  printf '%s\n' "$MEAN" > "$WORK/baseline.mean"
  printf '%s\n' "$(occurrences_total)" > "$WORK/baseline.rows"
  cat <<MD
#### M1 baseline — 10,000 fresh events on the previous tag, one unique index (labelled fallback, local compose, $(date -u +%Y-%m-%dT%H:%MZ))

Processor: **$(processor_image)**. error_occurrences: **$(occurrences_total)** rows after the run
(the seed's 1,000,000 + these 10,000); indexes on it: $(sql "SELECT string_agg(indexname, ', ' ORDER BY indexname) FROM pg_indexes WHERE tablename = 'error_occurrences'").
Events: sourcemaps-seed.sh events-a × $PASSES under R1 with no maps (same bytes as M1's pass one), $(project_issues "$SEED_PROJECT_ID") issues (N = $N).

sentinel_process_duration_seconds, Δ over the run:

$(histogram_table "$WORK/baseline.before" "$WORK/baseline.after" sentinel_process_duration_seconds)

**baseline mean = $MEAN ms** (kept in \`$WORK/baseline.mean\` for the run phase).
MD
  ;;

run)
  [ -s "$WORK/baseline.mean" ] || { echo "run: no $WORK/baseline.mean — run the baseline phase first (on the previous tag, before the migration)" >&2; exit 2; }
  NEW_INDEX=$(index_state uq_error_occurrences_project_event)
  [ "${NEW_INDEX%% *}" = true ] || { echo "run: uq_error_occurrences_project_event is not a valid index ($NEW_INDEX) — run M2 first" >&2; exit 2; }
  sink_start; processor_log_follow; trap 'kill $SINK_PID $PLOG_PID 2>/dev/null || true' EXIT
  log "run: processor $(processor_image); alert sink on :$M1_SINK_PORT"

  "$SEED" setup >&2
  [ -d "$SEED_DIR/events/events-a" ] || "$SEED" generate >&2
  # The project's alert channel (D4): one telegram config, threshold per issue per hour.
  sql "INSERT INTO alert_configs (project_id, organization_id, channel, channel_config, frequency_threshold, frequency_window_seconds, enabled)
       VALUES ('$SEED_PROJECT_ID', '00000000-0000-4000-8000-0000000000d1', 'telegram', '{\"chat_id\":\"m1\"}', $ALERT_THRESHOLD, 3600, true)" >/dev/null
  nats_cli pub alert_config.changed "{\"project_id\":\"$SEED_PROJECT_ID\",\"config_id\":\"m1\"}" >/dev/null 2>&1 || true
  sleep 3

  # --- before pass one ---
  OCC0=$(occurrences_total); ISS0=$(issues_total); PISS0=$(project_issues "$SEED_PROJECT_ID")
  REL0=$(sql "SELECT COALESCE((SELECT occurrence_count FROM releases WHERE project_id = '$SEED_PROJECT_ID' AND version = 'R1'), 0)")
  S0=$(stream_last_seq)
  SENT0=$(notifier_sent_lines); SINK0=$(sink_count)
  metrics_snapshot "$WORK/pass1.before"
  T1=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  log "run: pass one — $EVENTS events, no maps (stream last_seq $S0)"
  post_pass
  wait_occurrences "$EVENTS" 900 >/dev/null
  wait_stable sentinel_process_events_total 'outcome="stored"' 5
  metrics_snapshot "$WORK/pass1.after"
  OCC1=$(occurrences_total); ISS1=$(issues_total); PISS1=$(project_issues "$SEED_PROJECT_ID")
  REL1=$(sql "SELECT COALESCE((SELECT occurrence_count FROM releases WHERE project_id = '$SEED_PROJECT_ID' AND version = 'R1'), 0)")
  S1=$(stream_last_seq)
  sleep 5; SENT1=$(notifier_sent_lines); SINK1=$(sink_count)
  DUP1=$(counter_delta "$WORK/pass1.before" "$WORK/pass1.after" sentinel_process_events_total 'outcome="duplicate"')
  STORED1=$(counter_delta "$WORK/pass1.before" "$WORK/pass1.after" sentinel_process_events_total 'outcome="stored"')
  SYM1=$(counter_delta "$WORK/pass1.before" "$WORK/pass1.after" sentinel_symbolication_events_total 'outcome="applied_')
  NOMAP1=$(counter_delta "$WORK/pass1.before" "$WORK/pass1.after" sentinel_symbolication_frames_total 'outcome="no_map"')
  # A sample event for decision 24's second form: the first "Fingerprint computed" line of pass one,
  # keyed by its trace_id — the traceparent header travels with the stream message, so the same
  # trace_id names the same event on pass two.
  FP1_LINE=$(plog_since "$T1" | grep -F 'Fingerprint computed' | head -n1 || true)
  SAMPLE_TRACE=$(printf '%s' "$FP1_LINE" | grep -o '"trace_id":"[0-9a-f]*"' | cut -d'"' -f4 || true)
  FP1=$(printf '%s' "$FP1_LINE" | grep -o '"fingerprint":"[0-9a-f]*"' || true)
  log "run: pass one done — occurrences +$((OCC1 - OCC0)), issues +$((ISS1 - ISS0)), stream last_seq $S1, duplicate Δ $DUP1"

  # --- the maps, then the negative cache ---
  "$SEED" upload-a >&2
  NEG_TTL=$(docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$PROCESSOR_CONTAINER" | awk -F= '$1 == "SOURCEMAP_NEGATIVE_TTL" { print $2 }')
  log "run: maps uploaded for R1; waiting out SOURCEMAP_NEGATIVE_TTL=${NEG_TTL:-?} (+10 s)"
  sleep 15

  # --- pass two: the redelivery ---
  metrics_snapshot "$WORK/pass2.before"
  DUP_BEFORE=$(metric "$WORK/pass2.before" sentinel_process_events_total 'outcome="duplicate"')
  START_SEQ=$((S0 + 1))
  log "run: re-creating processor-consumer at stream sequence $START_SEQ (the processor keeps running)"
  nats_cli consumer rm ERROR_EVENTS processor-consumer -f >/dev/null
  nats_cli consumer add ERROR_EVENTS processor-consumer --pull --deliver="$START_SEQ" --ack=explicit --max-deliver=7 --wait=80s --max-pending=1000 --defaults >/dev/null
  T2=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  wait_counter sentinel_process_events_total 'outcome="duplicate"' "$(calc "$DUP_BEFORE + $EVENTS")" 900 || log "run: duplicate did not reach +$EVENTS in 900 s"
  wait_stable sentinel_process_events_total 'outcome="duplicate"' 5
  wait_stable sentinel_process_events_total 'outcome="stored"' 3
  metrics_snapshot "$WORK/pass2.after"
  OCC2=$(occurrences_total); ISS2=$(issues_total); PISS2=$(project_issues "$SEED_PROJECT_ID")
  REL2=$(sql "SELECT COALESCE((SELECT occurrence_count FROM releases WHERE project_id = '$SEED_PROJECT_ID' AND version = 'R1'), 0)")
  S2=$(stream_last_seq)
  sleep 5; SENT2=$(notifier_sent_lines); SINK2=$(sink_count)
  DUP2=$(counter_delta "$WORK/pass2.before" "$WORK/pass2.after" sentinel_process_events_total 'outcome="duplicate"')
  STORED2=$(counter_delta "$WORK/pass2.before" "$WORK/pass2.after" sentinel_process_events_total 'outcome="stored"')
  SYM2=$(counter_delta "$WORK/pass2.before" "$WORK/pass2.after" sentinel_symbolication_events_total 'outcome="applied_')
  FP2=$(plog_since "$T2" | grep -F "\"trace_id\":\"$SAMPLE_TRACE\"" | grep -F 'Fingerprint computed' | head -n1 | grep -o '"fingerprint":"[0-9a-f]*"' || true)
  DUP_LOG=$(plog_since "$T2" | grep -F 'Duplicate event skipped' | head -n1 || true)
  CONSUMER=$(nats_cli consumer info ERROR_EVENTS processor-consumer --json | grep -E '"(deliver_policy|opt_start_seq|num_pending|num_redelivered)"' | tr -d ' \n')
  CARDS=$(dashboard_issue_cards)
  COUNT_SUM=$(sql "SELECT sum(count) FROM issues WHERE project_id = '$SEED_PROJECT_ID'")
  ALERT_DISPATCH1=$(counter_delta "$WORK/pass1.before" "$WORK/pass1.after" sentinel_alert_dispatch_total 'outcome="sent"')
  ALERT_DISPATCH2=$(counter_delta "$WORK/pass2.before" "$WORK/pass2.after" sentinel_alert_dispatch_total 'outcome="sent"')
  BASE_MEAN=$(cat "$WORK/baseline.mean"); BASE_ROWS=$(cat "$WORK/baseline.rows" 2>/dev/null || echo '?')
  PASS1_MEAN=$(hist_mean "$WORK/pass1.before" "$WORK/pass1.after")
  PASS2_MEAN=$(hist_mean "$WORK/pass2.before" "$WORK/pass2.after")
  log "run: pass two done — occurrences +$((OCC2 - OCC1)), issues +$((ISS2 - ISS1)), duplicate Δ $DUP2, applied Δ $SYM2"

  cat <<MD
#### M1 — the cross-issue redelivery (labelled fallback, local compose, $(date -u +%Y-%m-%dT%H:%MZ))

Processor: **$(processor_image)** (the feature build). Indexes on error_occurrences:
\`${NEW_INDEX#* }\` (indisvalid = ${NEW_INDEX%% *}) and \`$(index_state uq_error_occurrences_issue_event | cut -d' ' -f2-)\`.
Events: sourcemaps-seed.sh events-a × $PASSES = $EVENTS \`javascript\` events under R1 (10 frames each on the
fixture bundle's chunks + the cap-size map), N = $N distinct fingerprints by construction.
Redelivery instrument: **the durable consumer re-created at the first pass's starting stream sequence**
— \`nats consumer rm ERROR_EVENTS processor-consumer\` then \`nats consumer add ERROR_EVENTS processor-consumer
--pull --deliver=$START_SEQ --ack=explicit --max-deliver=7 --wait=80s --max-pending=1000\` with the processor
left running; stream sequences $START_SEQ..$S1 are pass one (last_seq $S0 → $S1, exactly $((S1 - S0)) messages),
and last_seq after pass two is $S2 (nothing was published: the same stream messages were redelivered).
Consumer after pass two: \`$CONSUMER\`.

| Read | Before pass one | After pass one | After pass two | Claim |
| --- | --- | --- | --- | --- |
| **SELECT count(*) FROM error_occurrences** | $OCC0 | $OCC1 (**+$((OCC1 - OCC0))**) | $OCC2 (**+$((OCC2 - OCC1))**) | +$EVENTS then unchanged: **$([ $((OCC1 - OCC0)) -eq "$EVENTS" ] && [ "$OCC2" -eq "$OCC1" ] && echo yes || echo NO)** |
| **SELECT count(*) FROM issues** | $ISS0 | $ISS1 (+$((ISS1 - ISS0))) | $ISS2 (**+$((ISS2 - ISS1))**) | unchanged by pass two: **$([ "$ISS2" -eq "$ISS1" ] && echo yes || echo NO)** |
| issues in the seed project | $PISS0 | $PISS1 | $PISS2 | = N = $N after pass one: **$([ "$PISS1" -eq "$N" ] && echo yes || echo NO)**; no phantom after pass two: **$([ "$PISS2" -eq "$PISS1" ] && echo yes || echo NO)** |
| **outcome="duplicate"** Δ | | $DUP1 | **$DUP2** | = $EVENTS for pass two: **$([ "$(calc "$DUP2 == $EVENTS")" = 1 ] && echo yes || echo NO)** |
| outcome="stored" Δ | | $STORED1 | $STORED2 | $EVENTS then 0 |
| sentinel_symbolication_events_total{outcome=~"applied_.*"} Δ (decision 24) | | **$SYM1** | **$SYM2** | ≈ 0 then ≥ $EVENTS: **$([ "$(calc "$SYM1 < 100 && $SYM2 >= $EVENTS")" = 1 ] && echo yes || echo NO)** (pass one: $NOMAP1 \`no_map\` frames) |
| sample event (trace_id ${SAMPLE_TRACE:-?}), "Fingerprint computed" | | ${FP1:-not in the log} | ${FP2:-not in the log} | differs: **$([ -n "$FP1" ] && [ -n "$FP2" ] && [ "$FP1" != "$FP2" ] && echo yes || echo "not read")** |

Flows D1–D4, read after pass two:

| Flow | Read | Value | Claim |
| --- | --- | --- | --- |
| D1 | the project's \`/\` (GET /?project=$SEED_PROJECT_ID&status=all, signed in as the seed's engineer) | $CARDS | one issue per error: N = $N |
| D1 | SELECT count(*) FROM issues WHERE project_id = seed | $PISS2 | = $N: **$([ "$PISS2" -eq "$N" ] && echo yes || echo NO)** |
| D2 | SELECT sum(count) FROM issues WHERE project_id = seed | $COUNT_SUM | one occurrence per event, = $EVENTS: **$([ "$COUNT_SUM" -eq "$EVENTS" ] && echo yes || echo NO)** |
| D3 | releases.occurrence_count for R1 | $REL0 → $REL1 → $REL2 | moved once (+$((REL1 - REL0)), then +$((REL2 - REL1))): **$([ $((REL1 - REL0)) -eq "$EVENTS" ] && [ "$REL2" -eq "$REL1" ] && echo yes || echo NO)** |
| D4 | notifier log lines event=alert.telegram.sent (processor) | $((SENT1 - SENT0)) on pass one, **$((SENT2 - SENT1))** on pass two | no second alert: **$([ $((SENT2 - SENT1)) -eq 0 ] && [ $((SENT1 - SENT0)) -gt 0 ] && echo yes || echo "NO (pass one $((SENT1 - SENT0)), pass two $((SENT2 - SENT1)))")** |
| D4 | alert sink POSTs received (host :$M1_SINK_PORT) | $((SINK1 - SINK0)) on pass one, $((SINK2 - SINK1)) on pass two | same |
| D4 | sentinel_alert_dispatch_total{outcome="sent"} Δ | $ALERT_DISPATCH1 on pass one, $ALERT_DISPATCH2 on pass two | same |

The duplicate path's key-aware log line (first of pass two):

\`\`\`
${DUP_LOG:-none}
\`\`\`

Secondary — storeevent_insert_mean_delta_ms. sentinel_process_duration_seconds, Δ per run (nothing
else was posting during either run; both at ≥ 1,000,000 rows):

**baseline — 10,000 fresh events, previous tag, ONE unique index** ($BASE_ROWS rows after it)

$(histogram_table "$WORK/baseline.before" "$WORK/baseline.after" sentinel_process_duration_seconds)

**pass one — 10,000 fresh events, feature build, BOTH unique indexes** ($OCC1 rows after it)

$(histogram_table "$WORK/pass1.before" "$WORK/pass1.after" sentinel_process_duration_seconds)

**pass two — the same 10,000 redelivered, symbolicated, all duplicate** (for reference: the rolled-back path)

$(histogram_table "$WORK/pass2.before" "$WORK/pass2.after" sentinel_process_duration_seconds)

**storeevent_insert_mean_delta_ms = pass-one mean − baseline mean = $PASS1_MEAN − $BASE_MEAN = $(calc "$PASS1_MEAN - $BASE_MEAN") ms**
(a number, not a bound; no p95 claimed — every event sits in the same 25–100 ms buckets on both
sides, so the buckets cannot resolve the index's cost). Pass-two mean for reference: $PASS2_MEAN ms.

Symbolication and cache counters after pass two:

\`\`\`
$(metrics_block "$WORK/pass2.after" '(symbolication_(events|frames)_total|sourcemap_cache_events_total|process_events_total|alert_dispatch_total)')
\`\`\`

Snapshots, the generated events and the sink log are under \`$WORK\`. **Before restarting the processor,
run \`$0 restore\`** (the consumer is at deliver=by_start_sequence, which ensureConsumer cannot update).
MD
  ;;

restore)
  log "restore: purging ERROR_EVENTS and re-creating processor-consumer with --deliver=all (nats-init.sh's shape; the processor's ensureConsumer sets MaxDeliver/AckWait on its next start)"
  nats_cli stream purge ERROR_EVENTS -f >/dev/null
  nats_cli consumer rm ERROR_EVENTS processor-consumer -f >/dev/null
  nats_cli consumer add ERROR_EVENTS processor-consumer --pull --deliver=all --ack=explicit --max-deliver=7 --wait=80s --max-pending=1000 --defaults >/dev/null
  nats_cli consumer info ERROR_EVENTS processor-consumer --json | grep -E '"(deliver_policy|num_pending)"' >&2
  ;;

*) echo "unknown phase: $PHASE (baseline|run|restore)" >&2; exit 2 ;;
esac
