# Stage 1: Build
FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git

WORKDIR /app

# Copy entire project for module resolution
COPY . .

# Build from project root
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-w -s" -o /ingestor ./apps/ingestor-go/

# Stage 2: Final
FROM alpine:3.19

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
ENV POSTGRES_PASSWORD=changeme
ENV POSTGRES_DB=sentinel
ENV NATS_URL=nats://localhost:4222

ENTRYPOINT ["/app/ingestor"]