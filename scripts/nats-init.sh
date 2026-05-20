#!/bin/sh
set -e

NATS_URL="${NATS_URL:-nats://localhost:4222}"
STREAM_NAME="ERROR_EVENTS"
SUBJECT="error_events"
CONSUMER_NAME="processor-consumer"

echo "Waiting for NATS to be ready..."
until nats server check connection --server "$NATS_URL" 2>/dev/null; do
  sleep 1
done

echo "Creating stream $STREAM_NAME..."
nats stream add "$STREAM_NAME" \
  --server "$NATS_URL" \
  --subjects="$SUBJECT" \
  --retention=limits \
  --max-msgs=-1 \
  --max-bytes=-1 \
  --storage=file \
  --replicas=1 \
  --discard=new \
  --defaults

echo "Creating consumer $CONSUMER_NAME..."
nats consumer add "$STREAM_NAME" "$CONSUMER_NAME" \
  --server "$NATS_URL" \
  --pull \
  --deliver=all \
  --ack=explicit \
  --defaults

echo "NATS JetStream initialization complete."