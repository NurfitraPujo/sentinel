#!/bin/sh
# Entrypoint for the scheduled dlq-drainer compose service (see
# scripts/Dockerfile.dlq-drainer and docker-compose.dlq-drainer.yml.fragment).
#
# This wraps the tools/dlq binary in a sleep-loop so it runs periodically without needing an external
# cron/scheduler in the image. Two independent safety gates exist so that merely bringing the stack up
# (`docker compose up -d`) can never start deleting or replaying DLQ messages on its own:
#
#   1. DLQ_DRAINER_ENABLED must be "true", or this script sleeps forever and never invokes the binary
#      at all.
#   2. Even when enabled, DLQ_DRAINER_EXECUTE must be "true" for the binary to be invoked with -execute.
#      Left unset/false, every scheduled run is a dry run: it reports what it would replay/skip and
#      touches nothing.
#
# All knobs are environment variables so the interval and behavior can be tuned per-deployment without
# rebuilding the image; see docker-compose.dlq-drainer.yml.fragment for the defaults this repo ships.
set -eu

ENABLED="${DLQ_DRAINER_ENABLED:-false}"
INTERVAL_SECONDS="${DLQ_DRAINER_INTERVAL_SECONDS:-3600}"
EXECUTE="${DLQ_DRAINER_EXECUTE:-false}"
LIMIT="${DLQ_DRAINER_LIMIT:-50}"
MAX_REPLAYS="${DLQ_DRAINER_MAX_REPLAYS:-3}"
STATE_FILE="${DLQ_DRAINER_STATE_FILE:-/var/lib/dlq-drainer/state.json}"
TIMEOUT="${DLQ_DRAINER_TIMEOUT:-60s}"

if [ "$ENABLED" != "true" ]; then
  echo "dlq-drainer: DLQ_DRAINER_ENABLED=$ENABLED (not \"true\") — sleeping forever without touching the DLQ."
  echo "dlq-drainer: set DLQ_DRAINER_ENABLED=true to activate the schedule."
  exec sleep infinity
fi

mkdir -p "$(dirname "$STATE_FILE")"

set -- -drain -limit="$LIMIT" -max-replays="$MAX_REPLAYS" -state-file="$STATE_FILE" -timeout="$TIMEOUT"
if [ "$EXECUTE" = "true" ]; then
  set -- "$@" -execute
  echo "dlq-drainer: DLQ_DRAINER_EXECUTE=true — scheduled runs WILL replay and delete transient-class DLQ messages (capped at max-replays=$MAX_REPLAYS, limit=$LIMIT per run)."
else
  echo "dlq-drainer: DLQ_DRAINER_EXECUTE=$EXECUTE (not \"true\") — scheduled runs are DRY RUNS: reporting only, nothing is published or deleted."
fi

echo "dlq-drainer: starting schedule. interval=${INTERVAL_SECONDS}s args: dlq $*"

while true; do
  echo "dlq-drainer: run starting at $(date -u +%FT%TZ)"
  if ! /usr/local/bin/dlq "$@"; then
    echo "dlq-drainer: run exited non-zero — will try again next interval, not stopping the schedule."
  fi
  echo "dlq-drainer: run finished at $(date -u +%FT%TZ), sleeping ${INTERVAL_SECONDS}s"
  sleep "$INTERVAL_SECONDS"
done
