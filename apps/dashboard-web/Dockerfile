# Stage 1: Build
FROM node:20-alpine AS builder

WORKDIR /app

# Copy package files from project root (context is project root)
COPY apps/dashboard-web/package.json apps/dashboard-web/package-lock.json ./
RUN npm install -g pnpm && pnpm install

# Copy source
COPY apps/dashboard-web/ ./

# Build
RUN pnpm build

# Stage 2: Production
FROM node:20-alpine

RUN apk add --no-cache ca-certificates tzdata

# Create non-root user
RUN addgroup -g 1001 appgroup && adduser -u 1001 -G appgroup -s /bin/sh -D appuser

WORKDIR /app

# Copy built assets from builder
COPY --from=builder /app/build ./build
COPY --from=builder /app/node_modules ./node_modules
COPY --from=builder /app/package.json ./

USER appuser

EXPOSE 3000

ENV PORT=3000
ENV POSTGRES_HOST=localhost
ENV POSTGRES_PORT=5432
ENV POSTGRES_USER=sentinel
ENV POSTGRES_PASSWORD=changeme
ENV POSTGRES_DB=sentinel
ENV DATABASE_URL=postgres://sentinel:changeme@localhost:5432/sentinel

ENTRYPOINT ["node", "build"]
