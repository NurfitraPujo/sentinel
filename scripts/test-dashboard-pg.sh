#!/usr/bin/env bash
# test-dashboard-pg.sh — run the dashboard's real-Postgres tag-facets suites against their own disposable,
# migrated Postgres (sentinel-issue-tag-facets spec §10.1, contract decision 5).
#
#   ./scripts/test-dashboard-pg.sh        # `task test-integration` runs it after the go test line
#
# The container publishes 5432 on a RANDOM loopback port, so this can never reach the shared dev database
# on 5432 (CLAUDE.md "Never point tests at the shared dev database"). The container is removed on any exit.
# TAG_FACETS_INTEGRATION_REQUIRED=1 makes both suites throw at module load when the database does not
# answer or issue_tag_values is missing, instead of skipping. tests/unit/tag_facets_gate_test.go asserts
# this file's suite paths, the flag, the random port and the migration target.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

IMAGE="${TAG_FACETS_PG_IMAGE:-postgres:15-alpine}"

# 1. A disposable Postgres on a random loopback port.
CID="$(docker run -d --rm \
	-e POSTGRES_USER=sentinel -e POSTGRES_PASSWORD=changeme -e POSTGRES_DB=sentinel \
	-p 127.0.0.1::5432 "$IMAGE")"
trap 'docker rm -f "$CID" >/dev/null 2>&1 || true' EXIT

PORT="$(docker port "$CID" 5432 | head -n 1 | sed 's/.*://')"
if [[ ! "$PORT" =~ ^[0-9]+$ ]]; then
	echo "test-dashboard-pg: could not read the published port of $CID (got '$PORT')" >&2
	exit 1
fi
DSN="postgres://sentinel:changeme@127.0.0.1:${PORT}/sentinel?sslmode=disable"
echo "test-dashboard-pg: postgres $CID on 127.0.0.1:$PORT"

# 2. Wait up to 60 s for it to accept connections. Over TCP (-h 127.0.0.1), not the socket: the image
#    runs a temporary socket-only server for initdb and then restarts, and a socket probe reports that
#    temporary server ready, so the first host connection is reset (measured 2026-09-25).
ready=0
for _ in $(seq 1 60); do
	if docker exec "$CID" pg_isready -h 127.0.0.1 -U sentinel -d sentinel >/dev/null 2>&1; then
		ready=1
		break
	fi
	sleep 1
done
if [[ "$ready" != 1 ]]; then
	echo "test-dashboard-pg: postgres not ready after 60 s" >&2
	docker logs "$CID" >&2 || true
	exit 1
fi

# 3. The real migration CLI, as CI's dashboard job runs it (GOWORK=off: the module on its own).
(cd packages/db-migrations && GOWORK=off DB_URL_DASHBOARD="$DSN" go run ./cmd/migrate up -target dashboard)

# 4. Dependencies, exactly as locked.
pnpm --dir apps/dashboard-web install --frozen-lockfile

# 5. Both suites, which cannot skip under the flag.
DATABASE_URL="$DSN" TAG_FACETS_INTEGRATION_REQUIRED=1 pnpm --dir apps/dashboard-web exec vitest run \
	src/lib/server/retention.tag-facets.integration.test.ts \
	src/lib/server/search.tag-facets.integration.test.ts
