#!/bin/sh
# wait-healthy.sh — block until every service in docker-compose.yml is ready.
#
# This is the single readiness gate shared by CI and by humans running the
# stack locally:
#
#   docker compose up -d && ./scripts/wait-healthy.sh
#
# A service counts as ready when:
#   - it declares a healthcheck (postgres, redis, nats) and that healthcheck
#     reports "healthy", or
#   - it is a one-shot job (nats-init, migrate — both `restart: "no"`) and has
#     exited with status 0, or
#   - it has no healthcheck (ingestor, processor, dashboard do not define one
#     today) and is simply "running".
#
# Any container that exits non-zero (outside the one-shot list), or any
# container still unhealthy/starting/missing when the timeout elapses,
# causes this script to print `docker compose ps` and exit non-zero.
#
# Requires: `docker compose` (or a compatible shim, e.g. podman-compose) and
# `jq`. Tolerant of both the Docker Compose v2 JSON shape (top-level
# `.Service` / `.Health` fields) and the podman-compose JSON shape (service
# name only in `.Labels["com.docker.compose.service"]`, `.Health` always
# null with health folded into the human-readable `.Status` string instead).

set -eu

TIMEOUT_SECONDS="${WAIT_HEALTHY_TIMEOUT:-180}"
POLL_INTERVAL_SECONDS="${WAIT_HEALTHY_INTERVAL:-2}"

# Services with a `healthcheck:` block in docker-compose.yml — these must
# reach "healthy", not just "running". Kept in sync by hand with the compose
# file; there are only three and they change rarely.
HEALTHCHECKED_SERVICES="postgres redis nats"

# One-shot services expected to run once and exit 0, rather than stay up.
ONESHOT_SERVICES="nats-init migrate minio-init"

# Services whose absence or failure must NOT fail the gate.
#
# jaeger is optional infrastructure by design: the OTel exporters degrade to a single warning when the
# collector is unreachable and every service runs fully without it (OBSERVABILITY_PLAN.md D-b). But this
# script enumerates `docker compose config --services`, so simply ADDING the service to the compose file
# silently made the whole stack gate depend on it — a failed image pull (~200MB, often not cached) or a
# squatted host port would classify as `failed` and exit 1 for everything, or spin to the timeout when the
# container is merely `created`. That is the degradation mandate defeated at the gate rather than in code,
# and it would take CI and every developer down with it.
#
# Listed services are still waited for and still reported; they just cannot fail the run.
OPTIONAL_SERVICES="jaeger"

log() {
  printf '%s\n' "$*" >&2
}

if ! command -v jq >/dev/null 2>&1; then
  log "wait-healthy: jq is required but not found on PATH"
  exit 1
fi

# `docker compose ps` only lists running containers by default. Docker Compose v2
# (what GitHub Actions provides) needs `--all` to also list the exited one-shot
# `nats-init`/`migrate` containers this script waits on — without it they are
# indistinguishable from "not created yet" and the ONESHOT_SERVICES fallback below
# spins until TIMEOUT_SECONDS elapses, even on a fully healthy stack.
#
# podman-compose (1.6.0, used for local dev) is the opposite: it lists exited
# containers by default and its `ps` does not implement `--all` at all — passing it
# is a hard error ("unrecognized arguments: --all"), not a no-op. So the flag can't
# just always be added; probe for support once, up front, and reuse the result.
PS_ALL_FLAG=""
if docker compose ps --format json --all >/dev/null 2>&1; then
  PS_ALL_FLAG="--all"
fi

in_list() {
  needle="$1"
  haystack="$2"
  for item in $haystack; do
    if [ "$item" = "$needle" ]; then
      return 0
    fi
  done
  return 1
}

services="$(docker compose config --services 2>/dev/null)"
if [ -z "$services" ]; then
  log "wait-healthy: could not list services via 'docker compose config --services'"
  exit 1
fi

log "wait-healthy: waiting up to ${TIMEOUT_SECONDS}s for: $(printf '%s' "$services" | tr '\n' ' ')"

# Normalizes one compose-ps JSON object to a single TSV line:
#   service<TAB>state<TAB>healthy(0/1)<TAB>exitcode
JQ_NORMALIZE='
def svc:
  if (.Service // "") != "" then .Service
  elif (.Labels | type) == "object" then (.Labels["com.docker.compose.service"] // "")
  elif (.Labels | type) == "string" then
    ((.Labels | split(",") | map(select(startswith("com.docker.compose.service="))) | .[0]) // "" | sub("^com.docker.compose.service="; ""))
  else "" end;
def is_healthy:
  ((.Health // "") | ascii_downcase) as $h
  | (.Status // "" | ascii_downcase) as $st
  | ($h == "healthy") or ($st | test("\\(healthy\\)"));
(if type == "array" then .[] else . end)
| [ svc, ((.State // .state // "") | ascii_downcase), (if is_healthy then "1" else "0" end), ((.ExitCode // .exit_code // -1) | tostring) ]
| @tsv
'

elapsed=0

while true; do
  raw="$(docker compose ps --format json $PS_ALL_FLAG 2>/dev/null || true)"
  status_tsv="$(printf '%s' "$raw" | jq -r "$JQ_NORMALIZE" 2>/dev/null || true)"

  all_ready=1
  failed=""
  pending=""
  optional_down=""

  for svc in $services; do
    line="$(printf '%s\n' "$status_tsv" | awk -F'\t' -v s="$svc" '$1 == s {print; exit}')"

    if [ -z "$line" ]; then
      if in_list "$svc" "$ONESHOT_SERVICES"; then
        # Under normal operation PS_ALL_FLAG (above) already makes exited
        # one-shot containers visible on every supported compose provider.
        # This branch is a defensive fallback for the brief window before
        # the container is created at all; treat "missing" as "not yet
        # run" and keep waiting rather than failing immediately.
        all_ready=0
        pending="$pending $svc(no-container)"
        continue
      fi
      if in_list "$svc" "$OPTIONAL_SERVICES"; then
        # Never created — e.g. the image could not be pulled. Optional means optional.
        optional_down="$optional_down $svc(no-container)"
        continue
      fi
      all_ready=0
      pending="$pending $svc(no-container)"
      continue
    fi

    state="$(printf '%s' "$line" | cut -f2)"
    healthy="$(printf '%s' "$line" | cut -f3)"
    exitcode="$(printf '%s' "$line" | cut -f4)"

    if in_list "$svc" "$ONESHOT_SERVICES"; then
      case "$state" in
        exited)
          if [ "$exitcode" != "0" ]; then
            failed="$failed $svc(exit=$exitcode)"
            all_ready=0
          fi
          ;;
        *)
          all_ready=0
          pending="$pending $svc($state)"
          ;;
      esac
      continue
    fi

    case "$state" in
      running)
        if in_list "$svc" "$HEALTHCHECKED_SERVICES" && [ "$healthy" != "1" ]; then
          all_ready=0
          pending="$pending $svc(starting)"
        fi
        ;;
      exited|dead|restarting)
        if in_list "$svc" "$OPTIONAL_SERVICES"; then
          optional_down="$optional_down $svc($state)"
        else
          failed="$failed $svc($state)"
          all_ready=0
        fi
        ;;
      *)
        if in_list "$svc" "$OPTIONAL_SERVICES"; then
          optional_down="$optional_down $svc($state)"
        else
          all_ready=0
          pending="$pending $svc($state)"
        fi
        ;;
    esac
  done

  if [ -n "$failed" ]; then
    log "wait-healthy: service(s) failed:$failed"
    docker compose ps $PS_ALL_FLAG >&2 2>/dev/null || true
    exit 1
  fi

  if [ "$all_ready" -eq 1 ]; then
    if [ -n "$optional_down" ]; then
      # Visible, not silent: an optional service being down is a real degradation (no traces will be
      # collected), it just is not a reason to fail the gate.
      log "wait-healthy: optional service(s) NOT up:$optional_down — continuing (see OPTIONAL_SERVICES)"
    fi
    log "wait-healthy: all services ready after ${elapsed}s"
    exit 0
  fi

  if [ "$elapsed" -ge "$TIMEOUT_SECONDS" ]; then
    log "wait-healthy: timed out after ${TIMEOUT_SECONDS}s, still pending:$pending"
    docker compose ps $PS_ALL_FLAG >&2 2>/dev/null || true
    exit 1
  fi

  sleep "$POLL_INTERVAL_SECONDS"
  elapsed=$((elapsed + POLL_INTERVAL_SECONDS))
done
