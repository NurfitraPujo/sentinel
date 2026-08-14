#!/bin/sh
# triage-loop.sh -- a minimal, defensive triage loop for the sentinel CLI.
#
# Requires: sentinel CLI on PATH (tools/sentinel-cli), jq.
# Requires env: SENTINEL_URL, SENTINEL_AGENT_KEY (see docs/agents/SENTINEL_AGENT_GUIDE.md §11).
#
# What it does, per new issue.created-ish event seen on the org activity feed:
#   1. claim it (skip if someone else already has it -- exit code 5 = conflict)
#   2. fetch full detail
#   3. post a triage comment (summarizing what we can tell from the payload)
#   4. if we can't say anything useful, ask a blocking question and release the claim
#   5. otherwise mark it resolved and release the claim
#
# This is a building block, not a finished bot: the "can we say anything useful" step below is a
# stand-in for real analysis -- replace decide_and_act() with your own logic.

set -eu

: "${SENTINEL_URL:?set SENTINEL_URL}"
: "${SENTINEL_AGENT_KEY:?set SENTINEL_AGENT_KEY}"

log() { printf '[triage-loop] %s\n' "$*" >&2; }

decide_and_act() {
	issue_id="$1"
	detail_json="$2"

	issue_type=$(printf '%s' "$detail_json" | jq -r '.issue.issueType')

	if [ "$issue_type" = "system_error" ]; then
		stacktrace=$(printf '%s' "$detail_json" | jq -r '.latestOccurrence.stacktrace // empty')
		if [ -z "$stacktrace" ]; then
			# Nothing to go on -- ask, don't guess.
			sentinel question "$issue_id" \
				--body "No stacktrace attached to the latest occurrence -- can you attach one, or confirm this reproduces?" \
				--waiting-on team
			sentinel release "$issue_id"
			return 0
		fi
		sentinel comment "$issue_id" \
			--body "Triage: system_error with a stacktrace present. Needs human review of the trace before resolving."
		sentinel progress "$issue_id" --body "Picked up by triage-bot; stacktrace captured, awaiting review."
		return 0
	fi

	# user_report
	body_md=$(printf '%s' "$detail_json" | jq -r '.report.bodyMd // empty')
	if [ -z "$body_md" ]; then
		sentinel question "$issue_id" \
			--body "This report has no description body -- can the reporter add repro steps?" \
			--waiting-on reporter
		sentinel release "$issue_id"
		return 0
	fi
	sentinel comment "$issue_id" --body "Triage: user report received and read. Investigating."
	return 0
}

log "starting; following events for issue.created-like activity (report_created, status_changed)"

sentinel events --follow --type report_created,status_changed 2>/dev/null | while IFS= read -r line; do
	[ -z "$line" ] && continue

	issue_id=$(printf '%s' "$line" | jq -r '.issue.id // empty')
	if [ -z "$issue_id" ]; then
		log "skipping malformed event line: $line"
		continue
	fi

	log "saw activity on issue $issue_id"

	if ! sentinel claim "$issue_id"; then
		rc=$?
		if [ "$rc" -eq 5 ]; then
			log "issue $issue_id already claimed by someone else, skipping"
		else
			log "claim failed for $issue_id (exit $rc), skipping"
		fi
		continue
	fi

	detail_json=$(sentinel issues get "$issue_id") || {
		log "could not fetch detail for $issue_id after claiming; releasing"
		sentinel release "$issue_id" || true
		continue
	}

	if decide_and_act "$issue_id" "$detail_json"; then
		log "handled $issue_id"
	else
		log "decide_and_act failed for $issue_id; releasing claim so it isn't stuck"
		sentinel release "$issue_id" || true
	fi
done
