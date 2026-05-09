# Quickstart: Sentinel Error Service

## Setup Prerequisites
- Docker & Docker Compose
- Go 1.22
- Node.js 20
- NATS JetStream server

## Local Development
1. Start infrastructure: `docker-compose up -d nats postgres`
2. Run Ingestor: `cd apps/ingestor-go && go run main.go`
3. Run Processor: `cd apps/processor-go && go run main.go`
4. Run Dashboard: `cd apps/dashboard-web && npm run dev`

## Ingesting First Error
```bash
curl -X POST http://localhost:8080/ingest \
  -H "Content-Type: application/json" \
  -H "X-API-Key: test-key" \
  -d '{
    "message": "database connection refused",
    "error_class": "pq.Error",
    "platform": "golang",
    "environment": "development",
    "stacktrace": [{"file": "main.go", "line": 10, "function": "main"}]
  }'
```
