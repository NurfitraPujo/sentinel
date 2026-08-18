#!/bin/sh
# Entrypoint for the sentinel-worker container (see docs/plans/AGENT_WORKER_PLAN.md §6).
#
# Mirrors tools/dlq/entrypoint.sh's dlq-drainer pattern: startup order is load-bearing. This
# script checks WORKER_ENABLED FIRST, before reading or validating any other configuration — a
# gated-off container can never crash-loop, because nothing past the gate check runs at all.
#
#   WORKER_ENABLED must be "true", or this script sleeps forever and never execs the worker binary.
#
# Once enabled, the binary itself owns the WORKER_EXECUTE distinction (dry-run vs. live mutation,
# plan §5) — that gate lives in-process (main.go's Config), not in this shell wrapper, because
# dry-run still needs the full poll/dispatch/journal pipeline running, just without sending
# mutating calls.
set -eu

ENABLED="${WORKER_ENABLED:-false}"

if [ "$ENABLED" != "true" ]; then
  echo "sentinel-worker: WORKER_ENABLED=$ENABLED (not \"true\") — sleeping forever without starting the worker."
  echo "sentinel-worker: set WORKER_ENABLED=true to activate."
  exec sleep infinity
fi

echo "sentinel-worker: WORKER_ENABLED=true — starting. WORKER_EXECUTE=${WORKER_EXECUTE:-false} (false = dry-run: decisions are journaled, nothing mutating is sent)."

exec /app/worker
