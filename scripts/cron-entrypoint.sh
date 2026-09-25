#!/bin/sh
# Entrypoint for the scheduled sentinel-cron service (see scripts/Dockerfile.cron and the
# `sentinel-cron` service in docker-compose.yml).
#
# N9 (docs/plans/AGENT_WORKER_PLAN.md, contract correction C11): the reference stack shipped the
# retention endpoint (apps/dashboard-web/src/routes/api/cron/retention/+server.ts) but NOTHING invoked
# it, so `reapStaleClaims` never ran — claims held by a crashed agent were never force-released, and the
# `claim_released(reason=stale)` event path was untestable. This wraps a `curl` of that endpoint in a
# sleep-loop so it runs periodically without an external cron/scheduler in the image, mirroring the
# dlq-drainer precedent (tools/dlq/entrypoint.sh).
#
# sentinel-user-report-issue §4.4b adds a SECOND loop for POST /api/cron/oncall-escalation, and the
# contract says why it is not folded into the first: retention runs hourly, and a 15-minute
# acknowledgement deadline swept hourly is a 75-minute deadline. The two loops have independent
# gates, intervals and targets, and they run as background jobs of this one process so a single
# compose service still covers both — the same shape the Helm chart expresses as two CronJobs.
#
# One safety gate per loop, matching repo convention (dlq-drainer ships gated OFF):
#
#   RETENTION_CRON_ENABLED must be "true", or the retention loop never calls its endpoint.
#   ONCALL_CRON_ENABLED    must be "true", or the escalation loop never calls its endpoint.
#
# With BOTH gates off this script sleeps forever, which is exactly what `wait-healthy.sh` expects of
# it (see OPTIONAL_SERVICES there).
#
# Unlike the dlq-drainer there is no second "execute" gate on either loop: each endpoint is itself
# the authority on what it does (retention honours DATA_RETENTION_DAYS / MANUAL_ISSUE_RETENTION_DAYS;
# the escalation sweep honours ESCALATION_ACK_MINUTES / ESCALATION_MAX_HOPS), there is no dry-run
# mode, and the caller only ever POSTs — the destructiveness lives server-side, not here.
#
# All knobs are environment variables so interval/target can be tuned per-deployment without rebuilding.
set -eu

RETENTION_ENABLED="${RETENTION_CRON_ENABLED:-false}"
RETENTION_INTERVAL_SECONDS="${RETENTION_CRON_INTERVAL_SECONDS:-3600}"
RETENTION_TARGET_URL="${RETENTION_CRON_URL:-http://dashboard:3000/api/cron/retention}"
RETENTION_TIMEOUT_SECONDS="${RETENTION_CRON_TIMEOUT_SECONDS:-180}"

# §4.4b: "This one runs every minute." 60s is the contract's interval, not a tuning default that
# happens to be small — a sweep interval above ESCALATION_ACK_MINUTES makes the deadline meaningless.
ONCALL_ENABLED="${ONCALL_CRON_ENABLED:-false}"
ONCALL_INTERVAL_SECONDS="${ONCALL_CRON_INTERVAL_SECONDS:-60}"
ONCALL_TARGET_URL="${ONCALL_CRON_URL:-http://dashboard:3000/api/cron/oncall-escalation}"
ONCALL_TIMEOUT_SECONDS="${ONCALL_CRON_TIMEOUT_SECONDS:-20}"

if [ -z "${CRON_SECRET:-}" ]; then
  echo "sentinel-cron: CRON_SECRET is unset — every endpoint would reject every call with 401. Refusing to start."
  exit 1
fi

# One loop, parameterised. Each background job runs this with its own label, interval and target;
# a failure inside a run is logged and the loop continues, because a transient non-2xx must not
# take the schedule down until the next container restart.
run_loop() {
  label="$1"
  interval="$2"
  target="$3"
  timeout="$4"

  echo "sentinel-cron[$label]: starting schedule. interval=${interval}s target=$target"

  while true; do
    echo "sentinel-cron[$label]: run starting at $(date -u +%FT%TZ)"
    # -sS: quiet but still print errors. -f: non-2xx becomes a non-zero exit. --max-time bounds a hung call.
    # -w: append the final HTTP status so a healthy run is visible in the logs.
    if ! curl -sS -f --max-time "$timeout" \
        -X POST \
        -H "x-cron-secret: ${CRON_SECRET}" \
        -w "\nsentinel-cron[$label]: HTTP %{http_code}\n" \
        "$target"; then
      echo "sentinel-cron[$label]: run failed (curl non-zero) — will try again next interval, not stopping the schedule."
    fi
    echo "sentinel-cron[$label]: run finished at $(date -u +%FT%TZ), sleeping ${interval}s"
    sleep "$interval"
  done
}

LOOP_PIDS=""
started=0

if [ "$RETENTION_ENABLED" = "true" ]; then
  run_loop retention "$RETENTION_INTERVAL_SECONDS" "$RETENTION_TARGET_URL" "$RETENTION_TIMEOUT_SECONDS" &
  LOOP_PIDS="$LOOP_PIDS $!"
  started=$((started + 1))
else
  echo "sentinel-cron[retention]: RETENTION_CRON_ENABLED=$RETENTION_ENABLED (not \"true\") — not scheduling."
  echo "sentinel-cron[retention]: set RETENTION_CRON_ENABLED=true to activate the schedule."
fi

if [ "$ONCALL_ENABLED" = "true" ]; then
  run_loop oncall-escalation "$ONCALL_INTERVAL_SECONDS" "$ONCALL_TARGET_URL" "$ONCALL_TIMEOUT_SECONDS" &
  LOOP_PIDS="$LOOP_PIDS $!"
  started=$((started + 1))
else
  echo "sentinel-cron[oncall-escalation]: ONCALL_CRON_ENABLED=$ONCALL_ENABLED (not \"true\") — not scheduling."
  echo "sentinel-cron[oncall-escalation]: set ONCALL_CRON_ENABLED=true to activate the schedule."
fi

if [ "$started" -eq 0 ]; then
  echo "sentinel-cron: no loop enabled — sleeping forever without calling any endpoint."
  exec sleep infinity
fi

echo "sentinel-cron: $started loop(s) running (pids:$LOOP_PIDS)."

# Liveness by polling, not by `wait -n`: this runs under busybox ash (alpine, see
# scripts/Dockerfile.cron), whose `wait` takes no `-n`, and a bare `wait` would block until EVERY
# job exits — so one dead loop would leave the container "up" with the other still running and
# nothing saying so. Each loop is an infinite `while true`, so a missing pid means it died in a way
# its own error handling did not cover: exit non-zero and let compose's `restart: unless-stopped`
# bring the whole service back.
while true; do
  for pid in $LOOP_PIDS; do
    if ! kill -0 "$pid" 2>/dev/null; then
      echo "sentinel-cron: scheduled loop pid $pid exited unexpectedly — exiting so the container is restarted."
      exit 1
    fi
  done
  sleep 30
done
