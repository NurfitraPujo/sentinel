#!/bin/sh
# Keeps the byte-identical copies of the CLI's embedded files in sync. go:embed cannot reach outside
# tools/sentinel, so each file has ONE canonical source and generated copies:
#
#   docs/agents/SENTINEL_AGENT_GUIDE.md          (canonical) -> tools/sentinel/assets/GUIDE.md
#   tools/sentinel/assets/SKILL.md               (canonical) -> .agents/skills/sentinel-agent/SKILL.md
#
# (.claude/skills/sentinel-agent/SKILL.md is a shim pointing at the .agents copy; it needs no sync.)
#
#   scripts/sync-cli-embeds.sh          regenerate the copies
#   scripts/sync-cli-embeds.sh --check  exit 1 if any copy has drifted (CI)
set -eu
cd "$(dirname "$0")/.."

PAIRS="docs/agents/SENTINEL_AGENT_GUIDE.md:tools/sentinel/assets/GUIDE.md
tools/sentinel/assets/SKILL.md:.agents/skills/sentinel-agent/SKILL.md"

mode=sync
[ "${1:-}" = "--check" ] && mode=check
rc=0
for pair in $PAIRS; do
  src=${pair%%:*}
  dst=${pair##*:}
  if [ "$mode" = check ]; then
    if ! cmp -s "$src" "$dst"; then
      echo "DRIFT: $dst differs from canonical $src — run scripts/sync-cli-embeds.sh and commit" >&2
      rc=1
    fi
  else
    cp "$src" "$dst"
    echo "synced $dst <- $src"
  fi
done
exit $rc
