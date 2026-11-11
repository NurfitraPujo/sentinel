# Stage 1: Build
FROM docker.io/library/golang:1.25-alpine AS builder

RUN apk add --no-cache git

WORKDIR /app

# K8s Cloud runners fetch modules over flaky international egress:
# use the China proxy mirror so `go mod download` doesn't stall.
ENV GOPROXY=https://goproxy.cn,direct

# Copy module manifests first so the dependency layer stays cached
# across rebuilds (same pattern as cdc-pipeline Dockerfile).
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the source and build from project root.
COPY . .

# Build from project root
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-w -s" -o /ingestor ./apps/ingestor-go/

# Stage 2: Final
FROM docker.io/library/alpine:3.19

RUN apk add --no-cache ca-certificates tzdata

# Create non-root user
RUN addgroup -g 1001 appgroup && adduser -u 1001 -G appgroup -s /bin/sh -D appuser

WORKDIR /app

# Copy binary from builder
COPY --from=builder /ingestor .

USER appuser

EXPOSE 8080

ENV POSTGRES_HOST=localhost
ENV POSTGRES_PORT=5432
ENV POSTGRES_USER=sentinel
ENV POSTGRES_DB=sentinel
ENV NATS_URL=nats://localhost:4222

ENTRYPOINT ["/app/ingestor"]