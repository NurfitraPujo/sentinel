#!/usr/bin/env bash
#
# Every path in externalSecrets.secrets must exist in SSM.
#
# WHY THIS EXISTS (report-an-issue/BUG-77). External Secrets v2.10.0 processes an
# ExternalSecret's spec.data as ALL OR NOTHING: one unresolvable entry fails the whole
# reconcile and leaves the previous Secret in place. On 2026-10-01 GITHUB_CLIENT_SECRET was
# added to values.aws.staging.yml without its SSM parameter being created, and for two days
# that single missing path silently froze EVERY key in sentinel-sentinel-secrets --
# refreshTime stuck at 2026-09-26 while the status still read Ready=True SecretSynced.
#
# It surfaced only when an unrelated Job needed a key added later and could not start:
#   CreateContainerConfigError: couldn't find key MIGRATE_POSTGRES_PASSWORD
#
# So a values-only edit is a deploy-time landmine that detonates in a different component at
# an arbitrary later time. The chart already refuses a path that is not absolute and a
# migrate.postgres.passwordSecretKey absent from externalSecrets.secrets, but it cannot know
# whether the parameter exists. This can.
#
# IT CHECKS EXISTENCE WITHOUT READING VALUES. `describe-parameters` with Option=Equals is a
# list-level call that returns only the name, so the credential this needs is
# ssm:DescribeParameters -- NOT GetParameter. Nothing that runs this can read a secret.
#
# Usage:  scripts/verify-externalsecret-paths.sh [values-file ...]
#         (defaults to every AWS values file in the chart)

set -uo pipefail

CHART_DIR="${CHART_DIR:-deploy/helm/sentinel}"

if [ "$#" -gt 0 ]; then
  FILES=("$@")
else
  FILES=("$CHART_DIR/values.aws.staging.yml" "$CHART_DIR/values.aws.production.yml")
fi

# A CREDENTIAL FAULT MUST NOT READ AS "EVERY PARAMETER IS MISSING". Probe once up front: if
# we cannot list SSM at all, say so and exit 2, distinct from the exit 1 that means a path is
# genuinely absent. A gate that cries wolf is a gate someone disables.
if ! probe=$(aws ssm describe-parameters --max-results 1 --query 'Parameters[].Name' --output text 2>&1); then
  echo "FATAL: cannot list SSM parameters, so this gate cannot judge anything." >&2
  echo "       aws said: ${probe}" >&2
  echo "       This needs ssm:DescribeParameters. It does NOT need read access to any value." >&2
  exit 2
fi

missing=0
checked=0

for f in "${FILES[@]}"; do
  if [ ! -f "$f" ]; then
    echo "FATAL: no such values file: $f" >&2
    exit 2
  fi

  # Read the mapping with the YAML parser rather than grep: these are nested keys and a
  # grep for `KEY: /path` also matches comments and unrelated blocks.
  #
  # NOT `mapfile`: it is bash 4+ and absent from the bash 3.2 on a macOS operator's shell.
  # The first draft of this script used it, silently read nothing, and printed
  # "OK: all 0 mapped path(s) exist" with exit 0 -- a gate that could not fail.
  pairs_file="$(mktemp)"
  if ! "$(dirname "$0")/externalsecret-paths.py" "$f" > "$pairs_file"; then
    echo "FATAL: could not read externalSecrets.secrets out of $f" >&2
    rm -f "$pairs_file"
    exit 2
  fi

  n=$(wc -l < "$pairs_file" | tr -d ' ')
  if [ "$n" -eq 0 ]; then
    echo "$f: no externalSecrets.secrets (not the AWS path) -- skipped"
    rm -f "$pairs_file"
    continue
  fi

  echo "$f: $n mapped path(s)"
  while IFS=$'\t' read -r key path; do
    [ -n "${path:-}" ] || continue
    checked=$((checked + 1))

    found=$(aws ssm describe-parameters \
              --parameter-filters "Key=Name,Option=Equals,Values=${path}" \
              --query 'Parameters[].Name' --output text 2>/dev/null)

    if [ "$found" = "$path" ]; then
      printf '  ok       %-28s %s\n' "$key" "$path"
    else
      printf '  MISSING  %-28s %s\n' "$key" "$path"
      missing=$((missing + 1))
    fi
  done < "$pairs_file"
  rm -f "$pairs_file"
done

echo

# A GATE THAT CHECKED NOTHING MUST NOT REPORT SUCCESS. Having examined zero paths looks
# identical to having examined them all and found them fine, which is how a broken checker
# survives. Every AWS values file in this chart maps secrets, so zero is always a fault in
# the gate -- a renamed file, an unparsed mapping, a shell builtin that is not there.
if [ "$checked" -eq 0 ]; then
  echo "FATAL: examined 0 paths, so this run proves nothing. The gate itself is broken." >&2
  exit 2
fi

if [ "$missing" -gt 0 ]; then
  cat >&2 <<MSG
FAILED: $missing of $checked mapped path(s) do not exist in SSM.

Create the parameter BEFORE merging the values change. Until it exists, External Secrets
fails the whole ExternalSecret and every other key in that Secret stops updating -- while
the ExternalSecret's status goes on reporting Ready=True SecretSynced.
MSG
  exit 1
fi

echo "OK: all $checked mapped path(s) exist in SSM."
