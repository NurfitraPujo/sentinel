#!/bin/sh
set -e

NATS_URL="${NATS_URL:-nats://localhost:4222}"

# --- ERROR_EVENTS -----------------------------------------------------------
#
# P0 incident: this stream (and ERROR_EVENTS_DLQ) were running with retention=Limits but MaxAge=0,
# MaxMsgs=-1, MaxBytes=-1 - i.e. no limit at all - so nothing was ever removed. Combined with
# discard=new, once JetStream storage filled the server started rejecting new publishes, which stops
# ingestion at the front door. A sibling stream on this host already hit exactly that failure today
# ("nats: insufficient storage resources available"). See docs/plans/E2E_RECOVERY_PLAN.md.
STREAM_NAME="ERROR_EVENTS"
SUBJECT="error_events"
CONSUMER_NAME="processor-consumer"
# Discard=new (unchanged): correct for an ingest stream - rejecting a publish surfaces backpressure to
# the ingestor, whereas DiscardOld would silently drop events nobody has processed yet. With MaxAge
# bounding growth, a DiscardNew rejection becomes a real emergency signal (storage is actually full)
# instead of an inevitability (storage was never bounded at all).
#
# Sizing (re-derived 2026-09-14 for core's events - prd core-sentinel-integration, decision 24):
# The previous bound (MaxAge=72h, MaxBytes=4GiB) was derived from 18,654 events over ~5h averaging
# ~290 bytes each, measured during the P0 incident above. Core's events are not that shape: every one
# carries request metadata (http_route/status, request/tenant/user ids, query, headers, a stack trace -
# contract §4.7's example) and measures ~5 KiB, 17x the size the 4GiB bound assumed.
#   event size      5 KiB          contract §4.7 example (request metadata + stack trace)
#   design rate     20 events/s    decision 24 caps each core process at 20 events/s; this is that
#                                  bucket held open for the ENTIRE window (or ~24 processes at ~1/s
#                                  each, a day-long storm) - a ceiling, not normal load. The ingestor's
#                                  own per-key limit (5,000 req/min x 10 events/batch = 833 events/s)
#                                  sits far above it, so the byte bound below is the operative ceiling.
#   store capacity  50Gi           the shared-cluster JetStream file store: infra
#                                  k8s/hcs/argocd/cluster/nats/helm-values.{staging,production}.yaml
#                                  config.jetstream.fileStore.pvc.size: 50Gi / fileStore.maxSize: 50Gi.
#                                  (production's merge.max_file_store 107374182400 = 100 GiB exceeds
#                                  its own 50Gi PVC, so the disk is the ceiling that actually applies.)
#   20 events/s x 5 KiB x 24h = 20 x 5120 x 86400 = 8,847,360,000 B = 8.24 GiB  <=  10GiB  <=  50Gi
# MaxAge=24h: the replay window. 72h at the design rate is 24.7 GiB - half of a store this stream
# shares with every other tenant's streams (core's own included) - so the window shrinks to a day.
# The processor consumes continuously, so a day covers a re-process after a bad deploy; the evidence
# that needs to survive longer is in ERROR_EVENTS_DLQ, which keeps its own 30d window below.
# MaxBytes=10GiB: the design-rate day (8.24 GiB) with ~20% headroom - it fires only above ~24 events/s
# sustained for a full day. Sentinel's worst case on the shared store is then 10GiB + 1GiB (DLQ) +
# 2 x 64MiB (control plane) = 11.1 GiB, ~22% of one server's 50Gi, leaving the rest to the cluster's
# other tenants. Re-derive against the infra values above before raising it.
#
# Both limits are overridable per environment (ERROR_EVENTS_MAX_AGE / ERROR_EVENTS_MAX_BYTES, set from
# the chart's init.nats.errorEvents.{maxAge,maxBytes} in deploy/helm/sentinel) so a re-size on the
# shared cluster is a values change and an init-job re-run, never an image rebuild. The values must be
# what `nats stream edit --max-age/--max-bytes` accepts (e.g. 24h, 10GiB).
STREAM_DISCARD="new"
STREAM_MAX_AGE="${ERROR_EVENTS_MAX_AGE:-24h}"
STREAM_MAX_BYTES="${ERROR_EVENTS_MAX_BYTES:-10GiB}"

# --- ERROR_EVENTS_DLQ --------------------------------------------------------
#
# Created lazily by packages/shared-go/nats (Subscriber.ensureDLQStream) the first time a message is
# dead-lettered, with no limits set - so if nats-init.sh does not create it first with the limits below,
# it inherits the same unbounded-growth bug as ERROR_EVENTS the moment anything fails. Managing it here
# means ensureDLQStream's "already exists" check (StreamInfo) short-circuits and its own creation path
# never runs.
DLQ_STREAM_NAME="ERROR_EVENTS_DLQ"
DLQ_SUBJECT="error_events.dlq"
# MaxAge=30d: dead letters are evidence for an investigation, not transient state - 30 days is the
# window ops needs to notice, triage, and replay/action them (see tools/dlq).
# Discard=old (unchanged): essential. If a full DLQ ever rejected new parks (DiscardNew), the subscriber
# could not park a poison message and would Nak it forever instead - recreating the S13 livelock the DLQ
# exists to prevent. DiscardOld means the DLQ can always accept a new dead letter, at the cost of aging
# out the oldest evidence once genuinely full; MaxAge/MaxBytes together already cover 30 days of full
# history under normal load, and a full DLQ is itself an alarm condition someone should be paging on.
# MaxBytes=1GiB: dead letters are expected to be rare relative to ERROR_EVENTS (2 messages observed
# against 18,654 successfully processed at the time of writing), but each one carries the original
# payload plus DLQ headers. 1GiB is sized smaller than the primary stream's bound while still covering a
# sustained failure that dead-letters a meaningful fraction of traffic for the full 30-day window.
DLQ_DISCARD="old"
DLQ_MAX_AGE="30d"
DLQ_MAX_BYTES="1GiB"

# --- Control-plane streams ---------------------------------------------------
#
# ALERT_CONFIG and API_KEYS are small, low-frequency invalidation/change-notification streams (13 and 25
# messages observed respectively). They do not need a MaxAge - nothing here suggests they should ever
# expire messages consumers might still need - but leaving MaxBytes at -1 (unbounded) is the same class
# of bug as ERROR_EVENTS, and a bound costs nothing at this volume, so give them one too.
CONTROL_PLANE_MAX_BYTES="64MiB"

API_KEYS_STREAM_NAME="API_KEYS"
API_KEYS_SUBJECT="api_key.invalidated"
API_KEYS_CONSUMER_NAME="ingestor_apikey_invalidated"

ALERT_CONFIG_STREAM_NAME="ALERT_CONFIG"
ALERT_CONFIG_SUBJECT="alert_config.changed"
ALERT_CONFIG_CONSUMER_NAME="processor_alert_config_changed"

echo "Waiting for NATS to be ready..."
until nats server check connection --server "$NATS_URL" 2>/dev/null; do
  sleep 1
done

# stream_exists NAME - true (exit 0) if the stream is already present on the server.
stream_exists() {
  nats stream info "$1" --server "$NATS_URL" -j >/dev/null 2>&1
}

# create_or_update_stream NAME SUBJECT DISCARD MAX_AGE MAX_BYTES
#
# `nats stream add` against a stream that ALREADY EXISTS with a different config does not update it - it
# exits non-zero with "stream name already in use with a different configuration" (verified against nats
# CLI 0.4.0, the version pinned in Dockerfile.nats-init/natsio/nats-box:latest). That means limits added
# only to the `add` invocation, as this script used to do, would apply on first creation and then never
# again: every subsequent `compose up` against an already-running stack would hit that error under `set
# -e` and the limits would silently never reach ERROR_EVENTS or ERROR_EVENTS_DLQ, which is exactly the
# bug this function exists to close. `nats stream edit` (alias `update`) is the command that changes an
# existing stream's config in place, and is a genuine no-op (exit 0, "No difference in configuration")
# when the requested config already matches - so this function, and the whole script, is safe to run on
# every `compose up`.
create_or_update_stream() {
  name="$1"
  subject="$2"
  discard="$3"
  max_age="$4"
  max_bytes="$5"

  if stream_exists "$name"; then
    echo "Stream $name exists - applying limits (discard=$discard max-age=$max_age max-bytes=$max_bytes)..."
    nats stream edit "$name" \
      --server "$NATS_URL" \
      --discard="$discard" \
      --max-age="$max_age" \
      --max-bytes="$max_bytes" \
      -f
  else
    echo "Creating stream $name..."
    nats stream add "$name" \
      --server "$NATS_URL" \
      --subjects="$subject" \
      --retention=limits \
      --max-msgs=-1 \
      --max-bytes="$max_bytes" \
      --max-age="$max_age" \
      --storage=file \
      --replicas=1 \
      --discard="$discard" \
      --defaults
  fi
}

create_or_update_stream "$STREAM_NAME" "$SUBJECT" "$STREAM_DISCARD" "$STREAM_MAX_AGE" "$STREAM_MAX_BYTES"

echo "Creating consumer $CONSUMER_NAME..."
nats consumer add "$STREAM_NAME" "$CONSUMER_NAME" \
  --server "$NATS_URL" \
  --pull \
  --deliver=all \
  --ack=explicit \
  --defaults

create_or_update_stream "$DLQ_STREAM_NAME" "$DLQ_SUBJECT" "$DLQ_DISCARD" "$DLQ_MAX_AGE" "$DLQ_MAX_BYTES"

create_or_update_stream "$API_KEYS_STREAM_NAME" "$API_KEYS_SUBJECT" "new" "0s" "$CONTROL_PLANE_MAX_BYTES"

echo "Creating consumer $API_KEYS_CONSUMER_NAME..."
nats consumer add "$API_KEYS_STREAM_NAME" "$API_KEYS_CONSUMER_NAME" \
  --server "$NATS_URL" \
  --pull \
  --deliver=all \
  --ack=explicit \
  --defaults

create_or_update_stream "$ALERT_CONFIG_STREAM_NAME" "$ALERT_CONFIG_SUBJECT" "new" "0s" "$CONTROL_PLANE_MAX_BYTES"

echo "Creating consumer $ALERT_CONFIG_CONSUMER_NAME..."
nats consumer add "$ALERT_CONFIG_STREAM_NAME" "$ALERT_CONFIG_CONSUMER_NAME" \
  --server "$NATS_URL" \
  --pull \
  --deliver=all \
  --ack=explicit \
  --defaults

echo "NATS JetStream initialization complete."
