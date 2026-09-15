#!/usr/bin/env bash
# sentinel-sourcemaps §13 (decisions 35, 39) — the ONE seed behind M1–M3, the S8/S9/S12 probes and
# fe's D2 shot. bash + curl + psql + node, because the seed is UPLOADS through POST /api/sourcemaps
# and EVENTS through POST /ingest/batch, not rows: every sourcemaps row and object is written by
# the route, every occurrence by the processor, exactly as production would.
#
# What it builds, against a running compose stack (docker compose up -d --build --force-recreate
# && ./scripts/wait-healthy.sh — the processor must carry the S3 env and SOURCEMAP_NEGATIVE_TTL=5s,
# both in docker-compose.yml as of TASK-01):
#
#   * one organization `sourcemaps-seed`, one project `sourcemaps-seed` (the events' project_key),
#     one project-scoped `ingest` key whose plaintext is printed (SENTINEL_INGEST_KEY for the §4.8
#     curl), one engineer user for the browser rows (sign in the way releases-snooze-seed.sql's
#     header describes: mint a verification_token row and load the callback);
#   * release R1: the 8 maps of build A, then the 8 maps of build B (new minified_paths — the
#     `_b2` suffix — so they `create` rows, never `replaced`), plus the cap-size map the committed
#     generator emits into a temp dir at seed time (decision 39: ~49.5 MiB, never committed);
#   * 1,000 build-A events under R1, the same 1,000 under build B's chunk names, both again under
#     R0 (no maps), and one partial R1 event (a no-column frame, a frame on an unmapped chunk) —
#     all derived from the committed builds by tests/fixtures/sourcemaps/generate-events.mjs, whose
#     header states how N (the README's number) is fixed.
#
# Phases, so a measurement can snapshot between them (sourcemaps-m1.sh does):
#
#   setup      delete + recreate the org/project/key/user rows
#   generate   run the two generators into $SEED_DIR (default: a fresh mktemp -d, printed)
#   upload-a   POST build A's 8 maps + the cap-size map under R1 (one request, ≤ 110M)
#   events-a   POST the 1,000 build-A events (20 batches of 50)
#   upload-b   POST build B's 8 maps under R1
#   events-b   POST the 1,000 build-B events
#   events-r0a / events-r0b   the same two sets under R0
#   partial    POST the one partially symbolicated event
#   all        every phase above, in that order (the default)
#
#   usage: apps/dashboard-web/scripts/sourcemaps-seed.sh [phase ...]
#
#   DATABASE_URL   postgres://sentinel:changeme@localhost:5432/sentinel   (psql)
#   DASHBOARD_URL  http://localhost:3000                                  (POST /api/sourcemaps)
#   INGESTOR_URL   http://localhost:8080                                  (POST /ingest/batch)
#   SEED_DIR       where the generated map and events go; reused across invocations when set
#
# Idempotent: `setup` deletes its own organization's rows first (never anything else's), a
# re-run's uploads are all `unchanged` (the generators are deterministic), and a re-run's events
# add occurrences without adding issues (no event_id is set, so each posting is a fresh one).
# Never point it at a database you are not prepared to re-seed.
set -euo pipefail

DATABASE_URL="${DATABASE_URL:-postgres://sentinel:changeme@localhost:5432/sentinel}"
DASHBOARD_URL="${DASHBOARD_URL:-http://localhost:3000}"
INGESTOR_URL="${INGESTOR_URL:-http://localhost:8080}"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
FIXTURES="$ROOT/tests/fixtures/sourcemaps"

# Fixed ids and the key. The secret is a dev-only constant so the §4.8 curl and the measurement
# scripts can reuse it; the row stores sha256(secret), the shape the authenticators look up.
ORG_ID='00000000-0000-4000-8000-0000000000d1'
PROJECT_ID='00000000-0000-4000-8000-0000000000d2'
PROJECT_KEY='sourcemaps-seed'
USER_ID='sourcemaps-seed-engineer'
USER_EMAIL='sourcemaps-engineer@example.invalid'
INGEST_KEY='sk_seed_sourcemaps_0123456789abcdef0123456789abcdef'
RELEASE_R1='R1'
RELEASE_R0='R0'

if [ -z "${SEED_DIR:-}" ]; then
  SEED_DIR="$(mktemp -d -t sourcemaps-seed.XXXXXX)"
  echo "SEED_DIR=$SEED_DIR (export it to reuse the generated files)" >&2
fi
CAPSIZE_MAP="$SEED_DIR/capsize.map"
EVENTS_DIR="$SEED_DIR/events"

log() { printf '%s %s\n' "$(date -u +%H:%M:%S)" "$*" >&2; }

phase_setup() {
  log "setup: recreating organization $ORG_ID / project $PROJECT_ID / key / user"
  psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -q <<SQL
-- Idempotency: remove what a previous run created, by this seed's own ids only, in FK order
-- (the shape tests/e2e/harness_test.go: cleanup uses). sourcemaps, releases, buckets and
-- memberships cascade from the project; issues do not (1716508800_init.sql).
DELETE FROM error_search_index WHERE occurrence_id IN (
  SELECT eo.id FROM error_occurrences eo JOIN issues i ON i.id = eo.issue_id WHERE i.project_id = '$PROJECT_ID');
DELETE FROM error_occurrences WHERE issue_id IN (SELECT id FROM issues WHERE project_id = '$PROJECT_ID');
DELETE FROM issue_activity WHERE issue_id IN (SELECT id FROM issues WHERE project_id = '$PROJECT_ID');
DELETE FROM issue_relations WHERE source_issue_id IN (SELECT id FROM issues WHERE project_id = '$PROJECT_ID')
   OR target_issue_id IN (SELECT id FROM issues WHERE project_id = '$PROJECT_ID');
DELETE FROM issues WHERE project_id = '$PROJECT_ID';
DELETE FROM alert_configs WHERE project_id = '$PROJECT_ID';
DELETE FROM project_api_keys WHERE organization_id = '$ORG_ID';
DELETE FROM projects WHERE organization_id = '$ORG_ID';
DELETE FROM organization_invitations WHERE organization_id = '$ORG_ID';
DELETE FROM organization_members WHERE organization_id = '$ORG_ID';
DELETE FROM organizations WHERE id = '$ORG_ID';
DELETE FROM verification_token WHERE identifier = '$USER_EMAIL';
DELETE FROM "user" WHERE id = '$USER_ID';

INSERT INTO organizations (id, name, slug) VALUES ('$ORG_ID', 'sourcemaps-seed', 'sourcemaps-seed');
INSERT INTO projects (id, name, organization_id, api_key, api_key_hash)
VALUES ('$PROJECT_ID', '$PROJECT_KEY', '$ORG_ID', 'pk_sourcemaps_seed', 'hash_sourcemaps_seed');
-- A project-scoped ingest key: the credential fixes the tenant (B7); the events name the project
-- by its NAME in project_key, which the ingestor checks against the key's project.
INSERT INTO project_api_keys
  (project_id, organization_id, key_hash, key_prefix, name, scope, status, rate_limit_rpm, created_by)
VALUES ('$PROJECT_ID', '$ORG_ID', encode(digest('$INGEST_KEY'::bytea, 'sha256'), 'hex'),
        substr('$INGEST_KEY', 1, 12), 'sourcemaps-seed', 'ingest', 'active', 100000, 'sourcemaps-seed');
INSERT INTO "user" (id, name, email) VALUES ('$USER_ID', 'Sourcemaps Engineer', '$USER_EMAIL');
INSERT INTO organization_members (organization_id, user_id, role) VALUES ('$ORG_ID', '$USER_ID', 'engineer');
INSERT INTO project_members (project_id, user_id, role) VALUES ('$PROJECT_ID', '$USER_ID', 'developer');
SQL
  log "setup: done — SENTINEL_INGEST_KEY=$INGEST_KEY, project_key=$PROJECT_KEY, org slug sourcemaps-seed"
}

phase_generate() {
  mkdir -p "$SEED_DIR"
  log "generate: cap-size map → $CAPSIZE_MAP"
  node "$FIXTURES/generate-capsize-map.mjs" "$CAPSIZE_MAP"
  log "generate: events → $EVENTS_DIR"
  node "$FIXTURES/generate-events.mjs" "$EVENTS_DIR" "$PROJECT_KEY"
  sha256sum "$CAPSIZE_MAP" >&2
}

# upload <release> <build a|b> [extra minified_path extra file]... — one POST /api/sourcemaps
# carrying every map of the build (paired by position, as the route requires), printing the
# response body and the status.
upload() {
  local release="$1" build="$2"; shift 2
  local -a parts=()
  while IFS=$'\t' read -r path map; do
    parts+=(-F "minified_path=$path" -F "file=@$FIXTURES/$map")
  done < <(node -e '
    const m = require(process.argv[1]);
    for (const f of m.maps[process.argv[2]]) process.stdout.write(`${f.minified_path}\t${f.map}\n`);
  ' "$EVENTS_DIR/manifest.json" "$build")
  while [ $# -ge 2 ]; do
    parts+=(-F "minified_path=$1" -F "file=@$2"); shift 2
  done
  local out status
  out="$(curl -sS -o - -w '\n%{http_code} %{time_total}s' -X POST "$DASHBOARD_URL/api/sourcemaps" \
    -H "X-API-Key: $INGEST_KEY" -F "release_version=$release" "${parts[@]}")"
  status="$(printf '%s' "$out" | tail -n1)"
  printf '%s\n' "$out" | head -n -1
  log "upload $build → $release: HTTP ${status% *} in ${status#* }"
  case "$status" in 200*|201*) ;; *) echo "upload failed" >&2; return 1 ;; esac
}

phase_upload_a() {
  [ -s "$CAPSIZE_MAP" ] || { echo "no $CAPSIZE_MAP — run the generate phase first" >&2; return 1; }
  local capsize_path
  capsize_path="$(node -e 'process.stdout.write(require(process.argv[1]).capsize.minified_path)' "$EVENTS_DIR/manifest.json")"
  upload "$RELEASE_R1" a "$capsize_path" "$CAPSIZE_MAP"
}

phase_upload_b() { upload "$RELEASE_R1" b; }

# events <dir> — POST every batch file under $EVENTS_DIR/<dir> to /ingest/batch.
events() {
  local dir="$1" ingested=0 failed=0 f body
  for f in "$EVENTS_DIR/$dir"/*.json; do
    body="$(curl -sS -X POST "$INGESTOR_URL/ingest/batch" -H "X-API-Key: $INGEST_KEY" \
      -H 'Content-Type: application/json' --data-binary "@$f")"
    local i; i="$(printf '%s' "$body" | grep -o '"ingested":[0-9]*' | head -n1 | tr -dc 0-9)"
    local x; x="$(printf '%s' "$body" | grep -o '"failed":[0-9]*' | head -n1 | tr -dc 0-9)"
    if [ -z "$i" ]; then echo "batch $f: unexpected response: $body" >&2; return 1; fi
    ingested=$((ingested + i)); failed=$((failed + ${x:-0}))
  done
  log "events $dir: ingested=$ingested failed=$failed"
  [ "$failed" -eq 0 ]
}

phase_events_a()   { events events-a; }
phase_events_b()   { events events-b; }
phase_events_r0a() { events events-r0a; }
phase_events_r0b() { events events-r0b; }

phase_partial() {
  local body
  body="$(curl -sS -X POST "$INGESTOR_URL/ingest" -H "X-API-Key: $INGEST_KEY" \
    -H 'Content-Type: application/json' --data-binary "@$EVENTS_DIR/partial.json")"
  log "partial: $body"
  printf '%s' "$body" | grep -q '"accepted"'
}

run_phase() {
  case "$1" in
    setup) phase_setup ;;
    generate) phase_generate ;;
    upload-a) phase_upload_a ;;
    events-a) phase_events_a ;;
    upload-b) phase_upload_b ;;
    events-b) phase_events_b ;;
    events-r0a) phase_events_r0a ;;
    events-r0b) phase_events_r0b ;;
    partial) phase_partial ;;
    all) for p in setup generate upload-a events-a upload-b events-b events-r0a events-r0b partial; do run_phase "$p"; done ;;
    *) echo "unknown phase: $1" >&2; exit 2 ;;
  esac
}

if [ $# -eq 0 ]; then set -- all; fi
for phase in "$@"; do run_phase "$phase"; done
