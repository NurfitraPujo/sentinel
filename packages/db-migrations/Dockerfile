# Migration runner image for Sentinel.
#
# Build CONTEXT is this directory (packages/db-migrations), NOT the repo root — the migrate module
# is a self-contained Go module with no replace directives, so it needs nothing else from the tree.
#
#   docker build -t sentinel/db-migrate:<tag> packages/db-migrations
#
# IMPORTANT: the migrate CLI reads the .sql files from DISK at runtime (packages/db-migrations/
# migrations, resolved relative to the working directory) — the migrations are NOT embedded in the
# binary. This image therefore ships both the binary AND that directory, laid out so the binary's
# default path resolution (WORKDIR /app -> ./packages/db-migrations/migrations) finds them.
#
# Usage (entrypoint is `migrate`; all three targets share one physical database and are idempotent):
#   docker run --rm -e DB_URL_PROCESSOR=postgres://... sentinel/db-migrate up -target=processor
#   docker run --rm -e DB_URL_DASHBOARD=postgres://... sentinel/db-migrate status -target=dashboard
# The DSN comes from DB_URL_<TARGET> (e.g. DB_URL_INGESTOR); the CLI redacts the password in logs.

# ---- Stage 1: build the static binary ----
FROM docker.io/library/golang:1.25-alpine AS builder

WORKDIR /src

# Leverage layer caching: resolve modules before copying the rest.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o /migrate ./cmd/migrate

# ---- Stage 2: minimal runtime ----
FROM docker.io/library/alpine:3.19

RUN apk add --no-cache ca-certificates

# Non-root, matching the app images' 1001:1001 convention.
RUN addgroup -g 1001 appgroup && adduser -u 1001 -G appgroup -s /bin/sh -D appuser

WORKDIR /app

COPY --from=builder /migrate /usr/local/bin/migrate
# Ship the .sql files at the path the binary resolves by default from WORKDIR /app.
COPY --from=builder /src/migrations /app/packages/db-migrations/migrations

USER appuser

# `migrate` is on PATH; pass the subcommand + flags as args, e.g. ["up", "-target=processor"].
ENTRYPOINT ["migrate"]
