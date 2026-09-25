#!/usr/bin/env bash
# test-integration-gate.sh — runs the Go integration suite AND the dashboard's real-Postgres tag-facets
# proofs (scripts/test-dashboard-pg.sh) INDEPENDENTLY of one another: both always run, regardless of
# whether the other failed, both exit codes are captured and printed, and this script exits non-zero if
# EITHER failed.
#
# sentinel-issue-tag-facets tfr-gate-dashboard-proofs-never-run: `task test-integration` used to run these
# as two plain cmds, so a failing `go test` line (e.g. the two pre-existing failures,
# TestOncallWeekdayCheckReplayOverIsoRow and TestIngestAndProcess) aborted the task before
# ./scripts/test-dashboard-pg.sh — the only gate step that runs the retention and search real-Postgres
# proofs (contract decision 5) — ever executed. That gap is how a real regression in the dashboard proofs
# reached review green. Do not add `set -e`/`errexit` to this script: it must survive either step failing
# and still run the other.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

go test ./tests/integration/... -v -count=1 -timeout 15m
go_status=$?

./scripts/test-dashboard-pg.sh
dashboard_status=$?

echo "test-integration-gate: go test ./tests/integration/... exit ${go_status}"
echo "test-integration-gate: ./scripts/test-dashboard-pg.sh exit ${dashboard_status}"

# sentinel-issue-tag-facets tfr-gate-dashboard-proofs-never-run (reopened): a non-zero exit here, from
# either a real test failure or the script crashing outright (a bad exec, the container dying, etc.), is
# not by itself readable by the plane's NO_WORSE comparison — waycore/baseline.py and wayexec/failures.py
# find findings by matching Go-test-shaped `--- FAIL: <name>` lines, and this script's own prose ("exit
# 1") is not one. Without this line a broken dashboard half is invisible to the comparison against a base
# revision that never ran it, so print the marker on any non-zero exit, not only on a parsed test failure.
if [[ "$dashboard_status" -ne 0 ]]; then
	echo "--- FAIL: DashboardTagFacetsPgProofs (0.00s)"
fi

if [[ "$go_status" -ne 0 || "$dashboard_status" -ne 0 ]]; then
	exit 1
fi
exit 0
