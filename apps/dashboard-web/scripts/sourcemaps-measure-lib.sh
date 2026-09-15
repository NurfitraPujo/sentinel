# sentinel-sourcemaps §13 — helpers shared by sourcemaps-m1.sh, sourcemaps-m2.sh and
# sourcemaps-m3.sh (sourced, never run). Reads the processor's Prometheus text at
# $PROCESSOR_URL/metrics (OTel's exporter adds otel_scope_* labels to every series; the helpers
# match on the metric name and one label substring and ignore the rest) and the seed's ids.
#
#   DATABASE_URL    postgres://sentinel:changeme@localhost:5432/sentinel
#   DASHBOARD_URL   http://localhost:3000
#   INGESTOR_URL    http://localhost:8080
#   PROCESSOR_URL   http://localhost:8081       (/metrics, /health)
#   PROCESSOR_CONTAINER  sentinel-processor     (for M2's RSS: /proc/<pid>/status of the container's
#                                                init process, read on the host)

DATABASE_URL="${DATABASE_URL:-postgres://sentinel:changeme@localhost:5432/sentinel}"
DASHBOARD_URL="${DASHBOARD_URL:-http://localhost:3000}"
INGESTOR_URL="${INGESTOR_URL:-http://localhost:8080}"
PROCESSOR_URL="${PROCESSOR_URL:-http://localhost:8081}"
PROCESSOR_CONTAINER="${PROCESSOR_CONTAINER:-sentinel-processor}"
export DATABASE_URL DASHBOARD_URL INGESTOR_URL

SEED="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/sourcemaps-seed.sh"
SEED_PROJECT_ID='00000000-0000-4000-8000-0000000000d2'
SEED_INGEST_KEY='sk_seed_sourcemaps_0123456789abcdef0123456789abcdef'

log() { printf '%s %s\n' "$(date -u +%H:%M:%S)" "$*" >&2; }

# calc <expression> — floating-point arithmetic without bc (awk is everywhere bash is).
calc() { awk "BEGIN { printf \"%.6g\", ($1) }"; }

# metrics_snapshot <file> — the processor's /metrics, saved.
metrics_snapshot() { curl -sS "$PROCESSOR_URL/metrics" > "$1"; }

# metric <file> <name> [label-substring] — the value of the one series of <name> whose label set
# contains the substring (or the sum over all its series when omitted). Empty → 0.
metric() {
  local file="$1" name="$2" sub="${3:-}"
  awk -v n="$name" -v s="$sub" '
    $1 ~ "^" n "(\\{|$)" { if (s == "" || index($1, s) > 0) v += $NF }
    END { printf "%s", (v == "" ? 0 : v) }' "$file"
}

# histogram_table <before> <after> <name> — the per-bucket DELTA of a cumulative histogram between
# two snapshots, as a markdown table with the count, the sum and the mean, so a run reports what
# it added and not the process's lifetime.
histogram_table() {
  local before="$1" after="$2" name="$3"
  local count sum
  count=$(calc "$(metric "$after" "${name}_count") - $(metric "$before" "${name}_count")")
  sum=$(calc "$(metric "$after" "${name}_sum") - $(metric "$before" "${name}_sum")")
  echo "| le (s) | cumulative count (Δ) |"
  echo "| --- | --- |"
  awk -v n="${name}_bucket" '
    FNR == NR { if ($1 ~ "^" n "\\{") { match($1, /le="[^"]*"/); le = substr($1, RSTART + 4, RLENGTH - 5); b[le] = $NF } ; next }
    $1 ~ "^" n "\\{" { match($1, /le="[^"]*"/); le = substr($1, RSTART + 4, RLENGTH - 5); order[++k] = le; a[le] = $NF }
    END { for (i = 1; i <= k; i++) { le = order[i]; printf "| %s | %d |\n", le, a[le] - b[le] } }' "$before" "$after"
  if [ "$(calc "$count > 0")" = 1 ]; then
    printf '| **count** | %s |\n| **sum (s)** | %s |\n| **mean (ms)** | %s |\n' "$count" "$sum" "$(calc "1000 * $sum / $count")"
  else
    printf '| **count** | 0 |\n'
  fi
}

# fingerprint_set <file> — the seed project's distinct issues.fingerprint, sorted, one per line.
fingerprint_set() {
  psql "$DATABASE_URL" -At -c "SELECT DISTINCT fingerprint FROM issues WHERE project_id = '$SEED_PROJECT_ID' ORDER BY 1" > "$1"
}

# wait_metric <name> <label-substring> <target> [timeout s] — poll /metrics until the series reaches
# at least <target>.
wait_metric() {
  local name="$1" sub="$2" target="$3" timeout="${4:-120}" t=0 v
  while :; do
    v=$(curl -sS "$PROCESSOR_URL/metrics" | awk -v n="$name" -v s="$sub" '$1 ~ "^" n "(\\{|$)" && index($1, s) > 0 { v += $NF } END { printf "%s", (v == "" ? 0 : v) }')
    if [ "$(calc "${v:-0} >= $target")" = 1 ]; then return 0; fi
    if [ "$t" -ge "$timeout" ]; then log "wait_metric $name $sub: $v < $target after ${timeout}s"; return 1; fi
    sleep 1; t=$((t + 1))
  done
}

# wait_occurrences <target> [timeout s] — poll the seed project's occurrence count.
wait_occurrences() {
  local target="$1" timeout="${2:-300}" t=0 n
  while :; do
    n=$(psql "$DATABASE_URL" -At -c "SELECT count(*) FROM error_occurrences eo JOIN issues i ON i.id = eo.issue_id WHERE i.project_id = '$SEED_PROJECT_ID'")
    if [ "$n" -ge "$target" ]; then echo "$n"; return 0; fi
    if [ "$t" -ge "$timeout" ]; then log "wait_occurrences: $n < $target after ${timeout}s"; echo "$n"; return 1; fi
    sleep 1; t=$((t + 1))
  done
}

# processor_rss_kib — VmRSS of the processor container's init process, from the host's /proc
# (rootless podman: the process belongs to this user). Falls back to `docker stats` (cgroup
# usage, which also counts page cache) when the pid is not readable.
processor_rss_kib() {
  local pid
  pid=$(docker inspect --format '{{.State.Pid}}' "$PROCESSOR_CONTAINER" 2>/dev/null || true)
  if [ -n "$pid" ] && [ -r "/proc/$pid/status" ]; then
    awk '/^VmRSS:/ { print $2 }' "/proc/$pid/status"
  else
    docker stats --no-stream --format '{{.MemUsage}}' "$PROCESSOR_CONTAINER" | awk '{ v = $1; u = substr(v, length(v) - 1); n = v + 0;
      if (u == "GB" || u == "GiB") n *= 1048576; else if (u == "MB" || u == "MiB") n *= 1024; else if (u == "kB" || u == "KiB") n *= 1; else n /= 1024; printf "%d", n }'
  fi
}
