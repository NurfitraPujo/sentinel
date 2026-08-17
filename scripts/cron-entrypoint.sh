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
# One safety gate, matching repo convention (dlq-drainer ships gated OFF):
#
#   RETENTION_CRON_ENABLED must be "true", or this script sleeps forever and never calls the endpoint.
#
# Unlike the dlq-drainer there is no second "execute" gate: the retention endpoint is itself the
# authority on what it deletes (it honours DATA_RETENTION_DAYS / MANUAL_ISSUE_RETENTION_DAYS windows),
# there is no dry-run mode, and the caller only ever POSTs — the destructiveness lives server-side, not
# here.
#
# All knobs are environment variables so interval/target can be tuned per-deployment without rebuilding.
set -eu

ENABLED="${RETENTION_CRON_ENABLED:-false}"
INTERVAL_SECONDS="${RETENTION_CRON_INTERVAL_SECONDS:-3600}"
TARGET_URL="${RETENTION_CRON_URL:-http://dashboard:3000/api/cron/retention}"
TIMEOUT_SECONDS="${RETENTION_CRON_TIMEOUT_SECONDS:-30}"

if [ -z "${CRON_SECRET:-}" ]; then
  echo "sentinel-cron: CRON_SECRET is unset — the endpoint would reject every call with 401. Refusing to start."
  exit 1
fi

if [ "$ENABLED" != "true" ]; then
  echo "sentinel-cron: RETENTION_CRON_ENABLED=$ENABLED (not \"true\") — sleeping forever without calling the endpoint."
  echo "sentinel-cron: set RETENTION_CRON_ENABLED=true to activate the schedule."
  exec sleep infinity
fi

echo "sentinel-cron: starting schedule. interval=${INTERVAL_SECONDS}s target=$TARGET_URL"

while true; do
  echo "sentinel-cron: run starting at $(date -u +%FT%TZ)"
  # -sS: quiet but still print errors. -f: non-2xx becomes a non-zero exit. --max-time bounds a hung call.
  # -w: append the final HTTP status so a healthy run is visible in the logs.
  if ! curl -sS -f --max-time "$TIMEOUT_SECONDS" \
      -X POST \
      -H "x-cron-secret: ${CRON_SECRET}" \
      -w '\nsentinel-cron: HTTP %{http_code}\n' \
      "$TARGET_URL"; then
    echo "sentinel-cron: run failed (curl non-zero) — will try again next interval, not stopping the schedule."
  fi
  echo "sentinel-cron: run finished at $(date -u +%FT%TZ), sleeping ${INTERVAL_SECONDS}s"
  sleep "$INTERVAL_SECONDS"
done
