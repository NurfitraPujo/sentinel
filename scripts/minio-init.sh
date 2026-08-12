#!/bin/sh
set -e

S3_ENDPOINT="${S3_ENDPOINT:-http://minio:9000}"
S3_ACCESS_KEY="${S3_ACCESS_KEY:-minioadmin}"
S3_SECRET_KEY="${S3_SECRET_KEY:-minioadmin}"
S3_BUCKET="${S3_BUCKET:-sentinel-attachments}"
ALIAS="sentinel-minio"

echo "Waiting for MinIO to be ready..."
until mc alias set "$ALIAS" "$S3_ENDPOINT" "$S3_ACCESS_KEY" "$S3_SECRET_KEY" >/dev/null 2>&1; do
  sleep 1
done

# `mc mb` against a bucket that already exists exits non-zero without `--ignore-existing`, which
# would fail this script under `set -e` on every replay after the first — mirrors nats-init.sh's
# create-or-update idempotency rationale for the same reason (safe to run on every `compose up`).
echo "Ensuring bucket $S3_BUCKET exists..."
mc mb --ignore-existing "$ALIAS/$S3_BUCKET"

echo "MinIO initialization complete."
