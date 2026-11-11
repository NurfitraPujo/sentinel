# Blueprint: Sentinel Error Service

**Branch**: `001-sentinel-error-service` | **Date**: 2026-05-10
**Mode**: doc-only
**Total Tasks**: 28 | **Files**: 28 new, 0 modified, 0 deleted

## Key Decisions

- Using NATS JetStream for async messaging between ingestor and processor to handle backpressure during high-volume spikes → T-INF-001, T-PRC-001
- Protobuf-based contracts in `packages/proto` for language-agnostic error event schema → T-PKG-001
- PostgreSQL for all persistent storage (issues, occurrences, search index) with pgx driver → T-INF-003, T-PRC-006, T-PRC-007
- Go 1.22+ for ingestor and processor workers, SvelteKit for dashboard → T-ING-001, T-DSH-001
- Centralized PII/secret masking in processor-go before storage → T-PRC-005
- Google Workspace OIDC for dashboard authentication with configurable email domain → T-DSH-001
- Email and Telegram for alert notifications with retry queue (exponential backoff: 1s, 5s, 30s, max 3 attempts) → T-PRC-009, T-PRC-010
- Use `pgx/v5` for database driver (batch operations, prepared statements, better typing) → T-PKG-002
- Use `proto-gen-validate` for payload validation rules in proto definitions (max 100 frames, 64KB metadata, 10000 char message) → T-PKG-001, T-VLD-001
- Use `JetStream.PullSubscribe()` with `Fetch()` for NATS consumer (proper JetStream API) → T-PKG-003
- Use SvelteKit server actions with strict authorization enforcement in load functions → T-DSH-001, T-SEC-003
- Use Drizzle ORM for type-safe database queries with PostgreSQL → T-DSH-002, T-DSH-003
- Rate limiting: 5000 req/min per API key with HTTP 429 + Retry-After header → T-SEC-004
- Graceful degradation: bounded in-memory queue (10000 events) when PostgreSQL unavailable → T-PRC-011
- Data retention: 30-day cleanup cron job at 02:00 UTC daily → T-DSH-006

## Implementation Order

```
T-INF-001 (Docker Compose)
T-INF-002 (NATS JetStream config)
T-INF-003 (PostgreSQL schema)
T-PKG-001 (Protobuf contract with validation)
T-PKG-002 (Shared Go DB util)
T-PKG-003 (Shared Go NATS wrappers)
T-ING-001 (Ingestor HTTP endpoint)
T-ING-002 (NATS publisher)
T-SEC-001 (API Key auth)
T-SEC-004 (Rate limiting 5000 req/min)
T-VLD-001 (Payload validation)
T-PRC-001 (NATS consumer)
T-PRC-002 (Event deserialization)
T-PRC-003 (Fingerprinting with custom override)
T-PRC-004 (Normalization)
T-PRC-005 (PII masking)
T-PRC-006 (De-duplication)
T-PRC-007 (Search indexing)
T-PRC-008 (Alert dispatcher)
T-PRC-009 (Email worker with retry)
T-PRC-010 (Telegram worker with retry)
T-PRC-011 (Graceful degradation)
T-DSH-001 (SvelteKit + OIDC)
T-SEC-003 (RBAC)
T-DSH-002 (Issue List)
T-DSH-003 (Issue Detail)
T-DSH-004 (Advanced search)
T-DSH-005 (Full-text search)
T-DSH-006 (Data retention cleanup)
T-TST-001 (Integration test)
T-TST-002 (Unit tests)
T-TST-003 (Load test 1k+/sec)
T-SEC-002 (NATS NKEYs + TLS)
```

---

## Phase 1: Shared Foundation & Infrastructure

### T-INF-001: Setup Docker Compose for PostgreSQL 15 and NATS JetStream

**File**: `docker-compose.yml` (new)

**Requirements**: FR-008

```yaml
version: '3.8'

services:
  postgres:
    image: postgres:15-alpine
    container_name: sentinel-postgres
    environment:
      POSTGRES_USER: sentinel
      POSTGRES_PASSWORD: changeme
      POSTGRES_DB: sentinel
    ports:
      - "5432:5432"
    volumes:
      - postgres_data:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U sentinel"]
      interval: 5s
      timeout: 5s
      retries: 5

  nats:
    image: nats:2.10-alpine
    container_name: sentinel-nats
    ports:
      - "4222:4222"
      - "4223:4222"
      - "8222:8222"
    volumes:
      - nats_data:/data
    command: [
      "-js",
      "-c", "/etc/nats/nats-server.conf"
    ]
    healthcheck:
      test: ["CMD", "nats", "server-check"]
      interval: 5s
      timeout: 5s
      retries: 10

  postgres-init:
    image: postgres:15-alpine
    depends_on:
      postgres:
        condition: service_healthy
    entrypoint: ["/bin/bash", "-c"]
    command: |
      PGPASSWORD=changeme psql -h postgres -U sentinel -d sentinel -c "SELECT 1" > /dev/null 2>&1 && echo "PostgreSQL ready" || exit 1
    restart: "no"

volumes:
  postgres_data:
  nats_data:
```

**Verification**: `docker compose up -d` starts both services without error.

---

### T-INF-002: Configure NATS JetStream stream and consumer for `error_events`

**File**: `scripts/nats-init.sh` (new)

**Requirements**: FR-008

```bash
#!/bin/bash
set -e

NATS_URL="${NATS_URL:-nats://localhost:4222}"
STREAM_NAME="ERROR_EVENTS"
SUBJECT="error_events"
CONSUMER_NAME="processor-consumer"

echo "Waiting for NATS to be ready..."
until nats server check --server "$NATS_URL" 2>/dev/null; do
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
  --discard=new

echo "Creating consumer $CONSUMER_NAME..."
nats consumer add "$STREAM_NAME" \
  --server "$NATS_URL" \
  --consumer="$CONSUMER_NAME" \
  --deliver=all \
  --ack=none

echo "NATS JetStream initialization complete."
```

**Verification**: `nats stream list` shows ERROR_EVENTS stream; `nats consumer list ERROR_EVENTS` shows processor-consumer.

---

### T-INF-003: Initialize PostgreSQL database with tables

**File**: `scripts/db/init.sql` (new)

**Requirements**: FR-001, FR-002, FR-003, FR-004

```sql
-- Enable UUID extension
CREATE EXTENSION IF NOT EXISTS "uuid-ossp";

-- Projects table
CREATE TABLE IF NOT EXISTS projects (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    name VARCHAR(255) NOT NULL,
    api_key VARCHAR(64) NOT NULL UNIQUE,
    api_key_hash VARCHAR(128) NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX idx_projects_api_key ON projects(api_key);
CREATE INDEX idx_projects_api_key_hash ON projects(api_key_hash);

-- Issues table
CREATE TABLE IF NOT EXISTS issues (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    fingerprint VARCHAR(64) NOT NULL,
    message TEXT NOT NULL,
    error_class VARCHAR(255) NOT NULL,
    status VARCHAR(20) NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'resolved', 'ignored')),
    first_seen TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    last_seen TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    count BIGINT NOT NULL DEFAULT 1
);

CREATE INDEX idx_issues_project_id ON issues(project_id);
CREATE INDEX idx_issues_fingerprint ON issues(fingerprint);
CREATE INDEX idx_issues_status ON issues(status);
CREATE INDEX idx_issues_last_seen ON issues(last_seen DESC);

-- Error occurrences table
CREATE TABLE IF NOT EXISTS error_occurrences (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    issue_id UUID NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    environment VARCHAR(50) NOT NULL,
    platform VARCHAR(50) NOT NULL,
    stacktrace JSONB NOT NULL DEFAULT '[]',
    metadata JSONB NOT NULL DEFAULT '{}',
    trace_id VARCHAR(64),
    span_id VARCHAR(64),
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX idx_error_occurrences_issue_id ON error_occurrences(issue_id);
CREATE INDEX idx_error_occurrences_created_at ON error_occurrences(created_at DESC);
CREATE INDEX idx_error_occurrences_trace_id ON error_occurrences(trace_id);

-- Error search index table
CREATE TABLE IF NOT EXISTS error_search_index (
    occurrence_id UUID PRIMARY KEY REFERENCES error_occurrences(id) ON DELETE CASCADE,
    user_id VARCHAR(255),
    tenant_id VARCHAR(255),
    trace_id VARCHAR(64),
    span_id VARCHAR(64),
    request_id VARCHAR(255)
);

CREATE INDEX idx_error_search_user_id ON error_search_index(user_id);
CREATE INDEX idx_error_search_tenant_id ON error_search_index(tenant_id);
CREATE INDEX idx_error_search_trace_id ON error_search_index(trace_id);
CREATE INDEX idx_error_search_request_id ON error_search_index(request_id);

-- Full-text search index on issues
CREATE INDEX idx_issues_message_fts ON issues USING gin(to_tsvector('english', message));

-- Alert configurations table
CREATE TABLE IF NOT EXISTS alert_configs (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    channel VARCHAR(20) NOT NULL CHECK (channel IN ('email', 'telegram')),
    channel_config JSONB NOT NULL DEFAULT '{}',
    frequency_threshold INT NOT NULL DEFAULT 50,
    frequency_window_seconds INT NOT NULL DEFAULT 60,
    enabled BOOLEAN NOT NULL DEFAULT true,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX idx_alert_configs_project_id ON alert_configs(project_id);
```

**Verification**: `psql` connection to sentinel database shows all tables; `\d` lists all tables with correct columns.

---

### T-PKG-001: Define ErrorEvent Protobuf contract

**File**: `packages/proto/error_event.proto` (new)

**Requirements**: FR-007, FR-008

```protobuf
syntax = "proto3";

package sentinel.v1;

import "google/protobuf/timestamp.proto";
import "google/protobuf/struct.proto";
import "buf/validate/validate.proto";

option go_package = "github.com/NurfitraPujo/sentinel/gen/sentinel/v1;sentinelv1";

message ErrorEvent {
  option (buf.validate.message) = {
    cel: [
      {
        id: "error_event.project_key"
        message: "project_key is required and must not exceed 64 characters"
        expression: "this.project_key.size() > 0 && this.project_key.size() <= 64"
      },
      {
        id: "error_event.platform"
        message: "platform is required and must be lowercase alphanumeric"
        expression: "this.platform.matches('^[a-z0-9]+$')"
      },
      {
        id: "error_event.environment"
        message: "environment is required and must be lowercase alphanumeric"
        expression: "this.environment.matches('^[a-z0-9]+$')"
      },
      {
        id: "error_event.message"
        message: "message must not exceed 10000 characters"
        expression: "this.message.size() <= 10000"
      }
    ]
  };

  string project_key = 1 [(buf.validate.field).required = true];
  string platform = 2 [(buf.validate.field).required = true];
  string environment = 3 [(buf.validate.field).required = true];
  string message = 4 [(buf.validate.field).max_length = 10000];
  string error_class = 5 [(buf.validate.field).required = true];
  string trace_id = 6;
  string span_id = 7;
  repeated StackFrame stacktrace = 8 [(buf.validate.field).max_items = 100];
  google.protobuf.Struct metadata = 9;
  google.protobuf.Timestamp timestamp = 10;
  uint32 trace_flags = 11;
}

message StackFrame {
    option (buf.validate.message) = {
        cel: [
          {
            id: "stack_frame.file_required"
            message: "file is required for app frames"
            expression: "!this.in_app || this.file.size() > 0"
          }
        ]
    };

    string file = 1;
    int32 line = 2;
    string function = 3;
    bool in_app = 4;
}
```

**Verification**: `buf generate` produces Go code with validation methods in `gen/sentinel/v1/`.

---

### T-PKG-002: Implement shared Go database connection and migration utility

**File**: `packages/shared-go/database/database.go` (new)

**Requirements**: FR-001

```go
package database

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Config struct {
	Host            string
	Port            int
	User            string
	Password        string
	Database        string
	MaxConns        int
	MinConns        int
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration
}

func NewConnection(ctx context.Context, cfg Config) (*pgxpool.Pool, error) {
	connString := fmt.Sprintf(
		"postgres://%s:%s@%s:%d/%s?sslmode=disable",
		cfg.User, cfg.Password, cfg.Host, cfg.Port, cfg.Database,
	)

	poolConfig, err := pgxpool.ParseConfig(connString)
	if err != nil {
		return nil, fmt.Errorf("failed to parse connection string: %w", err)
	}

	poolConfig.MaxConns = int32(cfg.MaxConns)
	poolConfig.MinConns = int32(cfg.MinConns)
	poolConfig.MaxConnLifetime = cfg.MaxConnLifetime
	poolConfig.MaxConnIdleTime = cfg.MaxConnIdleTime

	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create connection pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	return pool, nil
}

func RunMigrations(ctx context.Context, pool *pgxpool.Pool, migrationSQL string) error {
	_, err := pool.Exec(ctx, migrationSQL)
	if err != nil {
		return fmt.Errorf("failed to run migrations: %w", err)
	}
	return nil
}
```

**File**: `packages/shared-go/database/migrations.go` (new)

```go
package database

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"
)

func LoadMigrations(migrationsDir string) (string, error) {
	entries, err := os.ReadDir(migrationsDir)
	if err != nil {
		return "", fmt.Errorf("failed to read migrations directory: %w", err)
	}

	var files []string
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".sql" {
			files = append(files, entry.Name())
		}
	}
	sort.Strings(files)

	var totalSQL string
	for _, file := range files {
		content, err := os.ReadFile(filepath.Join(migrationsDir, file))
		if err != nil {
			return "", fmt.Errorf("failed to read migration %s: %w", file, err)
		}
		totalSQL += string(content) + "\n"
	}

	return totalSQL, nil
}

func RunMigrationsWithPool(ctx context.Context, pool *pgxpool.Pool, migrationsDir string) error {
	migrationSQL, err := LoadMigrations(migrationsDir)
	if err != nil {
		return err
	}
	return RunMigrations(ctx, pool, migrationSQL)
}
```

**Verification**: `go build ./packages/shared-go/...` compiles successfully.

---

### T-PKG-003: Implement shared Go NATS publisher and subscriber wrappers

**File**: `packages/shared-go/nats/nats.go` (new)

**Requirements**: FR-008

```go
package nats

import (
	"context"
	"fmt"
	"time"

	"github.com/NurfitraPujo/sentinel/gen/sentinel/v1"
	"github.com/NurfitraPujo/sentinel/packages/shared-go/bufutil"
	"github.com/bufbuild/connect-go"
	"github.com/nats-io/nats.go"
)

type PublisherConfig struct {
	URL       string
	Subject   string
	Timeout   time.Duration
}

type Publisher struct {
	conn   *nats.Conn
	js     nats.JetStreamContext
	subject string
	timeout time.Duration
}

func NewPublisher(ctx context.Context, cfg PublisherConfig) (*Publisher, error) {
	conn, err := nats.Connect(cfg.URL, nats.UseOldRequestStyle())
	if err != nil {
		return nil, fmt.Errorf("failed to connect to NATS: %w", err)
	}

	js, err := conn.JetStream()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to get JetStream context: %w", err)
	}

	return &Publisher{
		conn:    conn,
		js:      js,
		subject: cfg.Subject,
		timeout: cfg.Timeout,
	}, nil
}

func (p *Publisher) Publish(ctx context.Context, event *sentinelv1.ErrorEvent) error {
	msg, err := bufutil.Marshal(event)
	if err != nil {
		return fmt.Errorf("failed to marshal event: %w", err)
	}

	_, err = p.js.PublishAsync(p.subject, msg)
	if err != nil {
		return fmt.Errorf("failed to publish event: %w", err)
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.js.PublishAsyncComplete():
		return nil
	}
}

func (p *Publisher) Close() error {
	p.conn.Close()
	return nil
}

type SubscriberConfig struct {
	URL       string
	Stream    string
	Consumer  string
	BatchSize int
	BatchWait time.Duration
}

type Subscriber struct {
	conn    *nats.Conn
	js      nats.JetStreamContext
	batchSize int
	batchWait time.Duration
}

func NewSubscriber(ctx context.Context, cfg SubscriberConfig) (*Subscriber, error) {
	conn, err := nats.Connect(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to NATS: %w", err)
	}

	js, err := conn.JetStream()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to get JetStream context: %w", err)
	}

	return &Subscriber{
		conn:       conn,
		js:         js,
		batchSize:  cfg.BatchSize,
		batchWait:  cfg.BatchWait,
	}, nil
}

func (s *Subscriber) Consume(ctx context.Context, handler func([]*sentinelv1.ErrorEvent) error) error {
	sub, err := s.js.PullSubscribe("error_events", "processor-consumer",
		nats.BindStream("ERROR_EVENTS"),
		nats.AckExplicit,
	)
	if err != nil {
		return fmt.Errorf("failed to create pull subscription: %w", err)
	}

	for {
		msgs, err := sub.Fetch(s.batchSize, nats.Context(ctx), nats.MaxWait(s.batchWait))
		if err != nil {
			if err == nats.ErrTimeout || err == context.DeadlineExceeded {
				continue
			}
			return fmt.Errorf("failed to fetch messages: %w", err)
		}

		var events []*sentinelv1.ErrorEvent
		for _, msg := range msgs {
			event, err := bufutil.Unmarshal[sentinelv1.ErrorEvent](msg.Data)
			if err != nil {
				msg.Nak()
				continue
			}
			events = append(events, event)
			msg.Ack()
		}

		if len(events) > 0 {
			if err := handler(events); err != nil {
				return fmt.Errorf("handler failed: %w", err)
			}
		}
	}
}

func (s *Subscriber) Close() error {
	s.conn.Close()
	return nil
}
```

**File**: `packages/shared-go/bufutil/bufutil.go` (new)

```go
package bufutil

import (
	"google.golang.org/protobuf/proto"
)

func Marshal(msg proto.Message) ([]byte, error) {
	return proto.Marshal(msg)
}

func Unmarshal[T proto.Message](data []byte) (T, error) {
	var msg T
	if err := proto.Unmarshal(data, msg); err != nil {
		return msg, err
	}
	return msg, nil
}
```

**Verification**: `go build ./packages/shared-go/...` compiles successfully.

---

## Phase 2: Ingestion Pipeline (ingestor-go)

### T-ING-001: Implement HTTP POST endpoint `/ingest` for receiving JSON error payloads

**File**: `apps/ingestor-go/main.go` (new)

**Requirements**: FR-001, FR-007

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/NurfitraPujo/sentinel/gen/sentinel/v1"
	"github.com/NurfitraPujo/sentinel/packages/shared-go/database"
	"github.com/NurfitraPujo/sentinel/packages/shared-go/nats"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type Ingestor struct {
	db        *pgxpool.Pool
	publisher *nats.Publisher
	apiKeys   map[string]string
}

type ErrorPayload struct {
	ProjectKey  string            `json:"project_key"`
	Platform    string            `json:"platform"`
	Environment string            `json:"environment"`
	Message     string            `json:"message"`
	ErrorClass  string            `json:"error_class"`
	TraceID     string            `json:"trace_id"`
	SpanID      string            `json:"span_id"`
	Stacktrace  []StackFrameInput `json:"stacktrace"`
	Metadata    map[string]any    `json:"metadata"`
	Timestamp   int64             `json:"timestamp"`
	TraceFlags  uint32            `json:"trace_flags"`
}

type StackFrameInput struct {
	File     string `json:"file"`
	Line     int32  `json:"line"`
	Function string `json:"function"`
	InApp    bool   `json:"in_app"`
}

func (i *Ingestor) handleIngest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var payload ErrorPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
		return
	}

	event := &sentinelv1.ErrorEvent{
		ProjectKey:  payload.ProjectKey,
		Platform:    payload.Platform,
		Environment: payload.Environment,
		Message:     payload.Message,
		ErrorClass:  payload.ErrorClass,
		TraceId:     payload.TraceID,
		SpanId:      payload.SpanID,
		Stacktrace:  make([]*sentinelv1.StackFrame, len(payload.Stacktrace)),
		Metadata:     nil,
		Timestamp:   timestamppb.FromUnix(payload.Timestamp),
		TraceFlags:  payload.TraceFlags,
	}

	for idx, frame := range payload.Stacktrace {
		event.Stacktrace[idx] = &sentinelv1.StackFrame{
			File:     frame.File,
			Line:     frame.Line,
			Function: frame.Function,
			InApp:    frame.InApp,
		}
	}

	if payload.Metadata != nil {
		event.Metadata, err = structpb.NewStruct(payload.Metadata)
		if err != nil {
			http.Error(w, "Invalid metadata format", http.StatusBadRequest)
			return
		}
	}

	if err := i.publisher.Publish(ctx, event); err != nil {
		log.Printf("Failed to publish event: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"status": "accepted"})
}

func main() {
	ctx := context.Background()

	dbCfg := database.Config{
		Host:            getEnv("DB_HOST", "localhost"),
		Port:            5432,
		User:            getEnv("DB_USER", "sentinel"),
		Password:        getEnv("DB_PASSWORD", "changeme"),
		Database:        getEnv("DB_NAME", "sentinel"),
		MaxOpenConns:    25,
		MaxIdleConns:    5,
		ConnMaxLifetime: 5 * time.Minute,
	}

	db, err := database.NewConnection(ctx, dbCfg)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer db.Close()

	publisherCfg := nats.PublisherConfig{
		URL:     getEnv("NATS_URL", "nats://localhost:4222"),
		Subject: "error_events",
		Timeout: 5 * time.Second,
	}

	publisher, err := nats.NewPublisher(ctx, publisherCfg)
	if err != nil {
		log.Fatalf("Failed to create NATS publisher: %v", err)
	}
	defer publisher.Close()

	apiKeys, err := loadAPIKeys(ctx, db)
	if err != nil {
		log.Fatalf("Failed to load API keys: %v", err)
	}

	ingestor := &Ingestor{
		db:        db,
		publisher: publisher,
		apiKeys:   apiKeys,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/ingest", ingestor.handleIngest)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "healthy"})
	})

	server := &http.Server{
		Addr:         ":8080",
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		log.Println("Starting ingestor on :8080")
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server failed: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	server.Shutdown(shutdownCtx)
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func loadAPIKeys(ctx context.Context, db *pgxpool.Pool) (map[string]string, error) {
	rows, err := db.Query(ctx, "SELECT api_key FROM projects")
	if err != nil {
		return nil, fmt.Errorf("failed to load API keys: %w", err)
	}
	defer rows.Close()

	keys := make(map[string]string)
	for rows.Next() {
		var apiKey string
		if err := rows.Scan(&apiKey); err != nil {
			continue
		}
		keys[apiKey] = apiKey
	}
	return keys, nil
}
```

**Verification**: `POST /ingest` with valid payload returns 202 Accepted.

---

### T-ING-002: Implement publisher to NATS JetStream `error_events` subject

**File**: `apps/ingestor-go/publisher.go` (new)

**Requirements**: FR-008

```go
package main

import (
	"context"
	"fmt"

	"github.com/NurfitraPujo/sentinel/gen/sentinel/v1"
	"github.com/NurfitraPujo/sentinel/packages/shared-go/nats"
)

type EventPublisher struct {
	publisher *nats.Publisher
}

func NewEventPublisher(publisher *nats.Publisher) *EventPublisher {
	return &EventPublisher{publisher: publisher}
}

func (ep *EventPublisher) PublishErrorEvent(ctx context.Context, event *sentinelv1.ErrorEvent) error {
	if err := ep.publisher.Publish(ctx, event); err != nil {
		return fmt.Errorf("failed to publish error event: %w", err)
	}
	return nil
}
```

**Verification**: Integration test confirms events appear in NATS JetStream.

---

### T-SEC-001: Implement API Key authentication

**File**: `apps/ingestor-go/auth.go` (new)

**Requirements**: FR-001

```go
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type APIKeyAuth struct {
	db *pgxpool.Pool
}

func NewAPIKeyAuth(db *pgxpool.Pool) *APIKeyAuth {
	return &APIKeyAuth{db: db}
}

func (a *APIKeyAuth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiKey := r.Header.Get("X-API-Key")
		if apiKey == "" {
			parts := strings.SplitN(r.Header.Get("Authorization"), " ", 2)
			if len(parts) == 2 && parts[0] == "Bearer" {
				apiKey = parts[1]
			}
		}

		if apiKey == "" {
			http.Error(w, "Missing API key", http.StatusUnauthorized)
			return
		}

		hash := sha256.Sum256([]byte(apiKey))
		hashStr := hex.EncodeToString(hash[:])

		var projectKey string
		err := a.db.QueryRowContext(r.Context(),
			"SELECT api_key FROM projects WHERE api_key_hash = $1",
			hashStr,
		).Scan(&projectKey)

		if err == pgx.ErrNoRows {
			http.Error(w, "Invalid API key", http.StatusUnauthorized)
			return
		}
		if err != nil {
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		ctx := context.WithValue(r.Context(), "project_key", projectKey)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (a *APIKeyAuth) ValidateAPIKey(ctx context.Context, apiKey string) (string, error) {
	hash := sha256.Sum256([]byte(apiKey))
	hashStr := hex.EncodeToString(hash[:])

	var projectKey string
	err := a.db.QueryRowContext(ctx,
		"SELECT api_key FROM projects WHERE api_key_hash = $1",
		hashStr,
	).Scan(&projectKey)

	if err == pgx.ErrNoRows {
		return "", fmt.Errorf("invalid API key")
	}
	if err != nil {
		return "", fmt.Errorf("database error: %w", err)
	}

	return projectKey, nil
}
```

**Verification**: Requests without valid API key return 401; requests with valid key pass through.

---

### T-SEC-004: Implement rate limiting for ingestion endpoint

**File**: `apps/ingestor-go/ratelimit.go` (new)

**Requirements**: FR-001

```go
package main

import (
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type RateLimiter struct {
	requests map[string][]time.Time
	mu       sync.RWMutex
	limit    int
	window   time.Duration
}

func NewRateLimiter(limit int, window time.Duration) *RateLimiter {
	rl := &RateLimiter{
		requests: make(map[string][]time.Time),
		limit:    limit,
		window:   window,
	}
	go rl.cleanup()
	return rl
}

func (rl *RateLimiter) cleanup() {
	ticker := time.NewTicker(time.Minute)
	for range ticker.C {
		rl.mu.Lock()
		now := time.Now()
		for key, times := range rl.requests {
			var valid []time.Time
			for _, t := range times {
				if now.Sub(t) <= rl.window {
					valid = append(valid, t)
				}
			}
			if len(valid) == 0 {
				delete(rl.requests, key)
			} else {
				rl.requests[key] = valid
			}
		}
		rl.mu.Unlock()
	}
}

func (rl *RateLimiter) Allow(apiKey string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	times := rl.requests[apiKey]

	var valid []time.Time
	for _, t := range times {
		if now.Sub(t) <= rl.window {
			valid = append(valid, t)
		}
	}

	if len(valid) >= rl.limit {
		rl.requests[apiKey] = valid
		return false
	}

	valid = append(valid, now)
	rl.requests[apiKey] = valid
	return true
}

type RateLimitMiddleware struct {
	limiter *RateLimiter
}

func NewRateLimitMiddleware(limiter *RateLimiter) *RateLimitMiddleware {
	return &RateLimitMiddleware{limiter: limiter}
}

func (m *RateLimitMiddleware) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiKey := r.Header.Get("X-API-Key")
		if apiKey == "" {
			parts := r.Header.Get("Authorization")
			if len(parts) > 0 {
				apiKey = parts
			}
		}

		if apiKey != "" && !m.limiter.Allow(apiKey) {
			w.Header().Set("Retry-After", "60")
			http.Error(w, "Rate limit exceeded", http.StatusTooManyRequests)
			return
		}

		next.ServeHTTP(w, r)
	})
}
```

**Verification**: Exceeding 5000 req/min returns HTTP 429 with Retry-After header.

---

### T-VLD-001: Validate payloads using proto-gen-validate

**File**: `apps/ingestor-go/validator.go` (new)

**Requirements**: FR-001

```go
package main

import (
	"fmt"
	"io"
	"net/http"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	sentinelv1 "github.com/NurfitraPujo/sentinel/gen/sentinel/v1"
)

const (
	maxStacktraceFrames = 100
	maxMetadataSize     = 64 * 1024
	maxMessageLength    = 10000
)

func validateErrorPayload(data []byte) (*sentinelv1.ErrorEvent, error) {
	var event sentinelv1.ErrorEvent
	if err := protojson.Unmarshal(data, &event); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}

	if event.ProjectKey == "" {
		return nil, fmt.Errorf("project_key is required")
	}
	if len(event.ProjectKey) > 64 {
		return nil, fmt.Errorf("project_key must not exceed 64 characters")
	}
	if event.Platform == "" {
		return nil, fmt.Errorf("platform is required")
	}
	if event.Environment == "" {
		return nil, fmt.Errorf("environment is required")
	}
	if len(event.Message) > maxMessageLength {
		return nil, fmt.Errorf("message must not exceed %d characters", maxMessageLength)
	}
	if event.ErrorClass == "" {
		return nil, fmt.Errorf("error_class is required")
	}
	if len(event.Stacktrace) > maxStacktraceFrames {
		return nil, fmt.Errorf("stacktrace must not exceed %d frames", maxStacktraceFrames)
	}

	if event.Metadata != nil {
		metadataBytes, _ := protojson.Marshal(event.Metadata)
		if len(metadataBytes) > maxMetadataSize {
			return nil, fmt.Errorf("metadata must not exceed %d bytes", maxMetadataSize)
		}
	}

	return &event, nil
}

func validationMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ingest" || r.Method != http.MethodPost {
			next.ServeHTTP(w, r)
			return
		}

		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1024*1024))
		if err != nil {
			http.Error(w, "Request too large", http.StatusRequestEntityTooLarge)
			return
		}

		_, err = validateErrorPayload(body)
		if err != nil {
			http.Error(w, fmt.Sprintf("Validation failed: %v", err), http.StatusBadRequest)
			return
		}

		r = r.WithContext(WithValidatedPayload(r.Context(), body))
		next.ServeHTTP(w, r)
	})
}

type contextKey string

const validatedPayloadKey contextKey = "validated_payload"

func WithValidatedPayload(ctx context.Context, payload []byte) context.Context {
	return ctx
}
```

**Verification**: Oversized payloads (stacktrace >100 frames, metadata >64KB, message >10000 chars) return HTTP 400.

---

### T-PRC-011: Implement graceful degradation when PostgreSQL unavailable

**File**: `apps/processor-go/graceful.go` (new)

**Requirements**: FR-008

```go
package main

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/NurfitraPujo/sentinel/gen/sentinel/v1"
)

const (
	maxBufferSize = 10000
	flushInterval = 5 * time.Second
)

type GracefulHandler struct {
	db          *pgxpool.Pool
	handler     *EventHandler
	buffer      []*sentinelv1.ErrorEvent
	mu          sync.Mutex
	isDBUp      bool
	flushTicker *time.Ticker
	stopCh      chan struct{}
}

func NewGracefulHandler(db *pgxpool.Pool, handler *EventHandler) *GracefulHandler {
	gh := &GracefulHandler{
		db:       db,
		handler:  handler,
		buffer:   make([]*sentinelv1.ErrorEvent, 0, maxBufferSize),
		isDBUp:   true,
		stopCh:   make(chan struct{}),
	}
	go gh.checkDBConnection()
	go gh.periodicFlush()
	return gh
}

func (gh *GracefulHandler) checkDBConnection() {
	ticker := time.NewTicker(10 * time.Second)
	for {
		select {
		case <-ticker.C:
			if err := gh.db.Ping(context.Background()); err != nil {
				if gh.isDBUp {
					log.Printf("WARNING: PostgreSQL unavailable: %v. Buffering events.", err)
					gh.isDBUp = false
				}
			} else {
				if !gh.isDBUp {
					log.Println("INFO: PostgreSQL connection restored. Flushing buffered events.")
					gh.flush()
					gh.isDBUp = true
				}
			}
		case <-gh.stopCh:
			ticker.Stop()
			return
		}
	}
}

func (gh *GracefulHandler) periodicFlush() {
	flushTicker := time.NewTicker(flushInterval)
	for {
		select {
		case <-flushTicker.C:
			gh.mu.Lock()
			if len(gh.buffer) > 0 && gh.isDBUp {
				gh.flush()
			}
			gh.mu.Unlock()
		case <-gh.stopCh:
			flushTicker.Stop()
			return
		}
	}
}

func (gh *GracefulHandler) HandleBatch(ctx context.Context, events []*sentinelv1.ErrorEvent) error {
	gh.mu.Lock()
	defer gh.mu.Unlock()

	if !gh.isDBUp {
		for _, event := range events {
			if len(gh.buffer) < maxBufferSize {
				gh.buffer = append(gh.buffer, event)
			} else {
				log.Printf("WARNING: Buffer full, dropping event for %s", event.ProjectKey)
			}
		}
		log.Printf("INFO: Buffered %d events (buffer size: %d/%d)", len(events), len(gh.buffer), maxBufferSize)
		return nil
	}

	return gh.handler.HandleBatch(ctx, events)
}

func (gh *GracefulHandler) flush() error {
	if len(gh.buffer) == 0 {
		return nil
	}

	ctx := context.Background()
	for _, event := range gh.buffer {
		if err := gh.handler.processEvent(ctx, event); err != nil {
			log.Printf("ERROR: Failed to flush event: %v", err)
		}
	}

	log.Printf("INFO: Flushed %d buffered events", len(gh.buffer))
	gh.buffer = make([]*sentinelv1.ErrorEvent, 0, maxBufferSize)
	return nil
}

func (gh *GracefulHandler) Stop() {
	close(gh.stopCh)
	gh.mu.Lock()
	defer gh.mu.Unlock()
	if len(gh.buffer) > 0 {
		gh.flush()
	}
}
```

**Verification**: When PostgreSQL is down, events are buffered (max 10000). On recovery, buffered events are flushed.

---

## Phase 3: Error Processing (processor-go)

### T-PRC-001: Implement NATS JetStream consumer for `error_events`

**File**: `apps/processor-go/main.go` (new)

**Requirements**: FR-008

```go
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/NurfitraPujo/sentinel/packages/shared-go/database"
	"github.com/NurfitraPujo/sentinel/packages/shared-go/nats"
)

type Processor struct {
	db         *pgxpool.Pool
	subscriber *nats.Subscriber
	handler    *EventHandler
}

func main() {
	ctx := context.Background()

	dbCfg := database.Config{
		Host:            getEnv("DB_HOST", "localhost"),
		Port:            5432,
		User:            getEnv("DB_USER", "sentinel"),
		Password:        getEnv("DB_PASSWORD", "changeme"),
		Database:        getEnv("DB_NAME", "sentinel"),
		MaxOpenConns:    25,
		MaxIdleConns:    5,
		ConnMaxLifetime: 5 * time.Minute,
	}

	db, err := database.NewConnection(ctx, dbCfg)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer db.Close()

	subscriberCfg := nats.SubscriberConfig{
		URL:       getEnv("NATS_URL", "nats://localhost:4222"),
		Stream:    "ERROR_EVENTS",
		Consumer:  "processor-consumer",
		BatchSize: 10,
		BatchWait: 1 * time.Second,
	}

	subscriber, err := nats.NewSubscriber(ctx, subscriberCfg)
	if err != nil {
		log.Fatalf("Failed to create NATS subscriber: %v", err)
	}
	defer subscriber.Close()

	handler := NewEventHandler(db)

	processor := &Processor{
		db:         db,
		subscriber: subscriber,
		handler:    handler,
	}

	go func() {
		log.Println("Starting processor...")
		if err := subscriber.Consume(ctx, handler.HandleBatch); err != nil {
			log.Fatalf("Consumer error: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("Shutting down processor...")
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
```

**Verification**: Processor starts and consumes messages from NATS JetStream.

---

### T-PRC-002: Implement de-serialization and basic validation of consumed events

**File**: `apps/processor-go/handler.go` (new)

**Requirements**: FR-008, FR-002

```go
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/NurfitraPujo/sentinel/gen/sentinel/v1"
)

type EventHandler struct {
	db           *pgxpool.Pool
	fingerprinter *Fingerprinter
	normalizer   *Normalizer
	masker       *Masker
	deduplicator *Deduplicator
	indexer      *Indexer
	alerter      *AlertDispatcher
}

func NewEventHandler(db *pgxpool.Pool) *EventHandler {
	return &EventHandler{
		db:           db,
		fingerprinter: NewFingerprinter(),
		normalizer:   NewNormalizer(),
		masker:       NewMasker(),
		deduplicator: NewDeduplicator(db),
		indexer:      NewIndexer(db),
		alerter:      NewAlertDispatcher(db),
	}
}

func (h *EventHandler) HandleBatch(ctx context.Context, events []*sentinelv1.ErrorEvent) error {
	for _, event := range events {
		if err := h.processEvent(ctx, event); err != nil {
			log.Printf("Failed to process event: %v", err)
			continue
		}
	}
	return nil
}

func (h *EventHandler) processEvent(ctx context.Context, event *sentinelv1.ErrorEvent) error {
	if event.ProjectKey == "" || event.ErrorClass == "" {
		return fmt.Errorf("invalid event: missing required fields")
	}

	normalizedMessage := h.normalizer.Normalize(event.Message)
	maskedMessage := h.masker.Mask(normalizedMessage)
	maskedClass := h.masker.Mask(event.ErrorClass)

	var stacktrace []*sentinelv1.StackFrame
	for _, frame := range event.Stacktrace {
		normalizedFrame := &sentinelv1.StackFrame{
			File:     h.normalizer.Normalize(frame.File),
			Line:     frame.Line,
			Function: h.normalizer.Normalize(frame.Function),
			InApp:    frame.InApp,
		}
		maskedFrame := &sentinelv1.StackFrame{
			File:     h.masker.Mask(normalizedFrame.File),
			Line:     maskedFrame.Line,
			Function: h.masker.Mask(normalizedFrame.Function),
			InApp:    maskedFrame.InApp,
		}
		stacktrace = append(stacktrace, maskedFrame)
	}

	fingerprint := h.fingerprinter.Fingerprint(maskedClass, stacktrace)

	issueID, occurrenceID, err := h.deduplicator.Upsert(ctx, &DeduplicationInput{
		ProjectKey:   event.ProjectKey,
		Fingerprint:  fingerprint,
		Message:      maskedMessage,
		ErrorClass:   maskedClass,
		Environment:  event.Environment,
		Platform:     event.Platform,
		Stacktrace:   stacktrace,
		Metadata:     event.Metadata,
		TraceID:      event.TraceId,
		SpanID:       event.SpanId,
	})
	if err != nil {
		return fmt.Errorf("deduplication failed: %w", err)
	}

	if err := h.indexer.IndexOccurrence(ctx, occurrenceID, event.Metadata); err != nil {
		log.Printf("Failed to index occurrence: %v", err)
	}

	if err := h.alerter.CheckAndDispatch(ctx, issueID); err != nil {
		log.Printf("Failed to dispatch alert: %v", err)
	}

	return nil
}
```

**Verification**: Events are processed and stored in database.

---

### T-PRC-003: Implement fingerprinting logic

**File**: `apps/processor-go/fingerprint.go` (new)

**Requirements**: FR-002, FR-009, FR-010

```go
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/NurfitraPujo/sentinel/gen/sentinel/v1"
)

type Fingerprinter struct {
	customPatterns map[string]interface{}
}

func NewFingerprinter() *Fingerprinter {
	return &Fingerprinter{
		customPatterns: make(map[string]interface{}),
	}
}

func (f *Fingerprinter) Fingerprint(errorClass string, stacktrace []*sentinelv1.StackFrame) string {
	appFrames := f.extractAppFrames(stacktrace, 3)
	framesStr := strings.Join(appFrames, "|")
	fingerprintInput := fmt.Sprintf("%s|%s", errorClass, framesStr)
	hash := sha256.Sum256([]byte(fingerprintInput))
	return hex.EncodeToString(hash[:])[:16]
}

func (f *Fingerprinter) FingerprintWithCustom(errorClass string, stacktrace []*sentinelv1.StackFrame, customFingerprint string) string {
	if customFingerprint != "" {
		hash := sha256.Sum256([]byte(customFingerprint))
		return hex.EncodeToString(hash[:])[:16]
	}
	return f.Fingerprint(errorClass, stacktrace)
}

func (f *Fingerprinter) extractAppFrames(stacktrace []*sentinelv1.StackFrame, maxFrames int) []string {
	var appFrames []string
	for _, frame := range stacktrace {
		if frame.InApp {
			appFrames = append(appFrames, fmt.Sprintf("%s:%s", frame.File, frame.Function))
			if len(appFrames) >= maxFrames {
				break
			}
		}
	}
	if len(appFrames) == 0 {
		for _, frame := range stacktrace {
			if len(appFrames) >= maxFrames {
				break
			}
			appFrames = append(appFrames, fmt.Sprintf("%s:%s", frame.File, frame.Function))
		}
	}
	return appFrames
}
```

**Verification**: Identical errors produce same fingerprint; different errors produce different fingerprints. Custom fingerprint override takes precedence when provided.

---

### T-PRC-004: Implement normalization/scrubbing

**File**: `apps/processor-go/normalizer.go` (new)

**Requirements**: FR-010

```go
package main

import (
	"regexp"
	"strings"
)

type Normalizer struct {
	patterns []*regexp.Regexp
}

func NewNormalizer() *Normalizer {
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}`),
		regexp.MustCompile(`\b\d{10,}\b`),
		regexp.MustCompile(`0x[a-fA-F0-9]+`),
		regexp.MustCompile(`["']email["']\s*:\s*["'][^"']+["']`),
		regexp.MustCompile(`\b\w+\.\w+@\w+\.\w+\b`),
	}

	return &Normalizer{patterns: patterns}
}

func (n *Normalizer) Normalize(input string) string {
	result := input

	result = regexp.MustCompile(`\s+`).ReplaceAllString(result, " ")
	result = strings.TrimSpace(result)

	for _, pattern := range n.patterns {
		result = pattern.ReplaceAllString(result, "<normalized>")
	}

	return result
}

func (n *Normalizer) NormalizeStackFrame(file string) string {
	normalized := file
	normalized = regexp.MustCompile(`v4.2.1| v4.1.0| v3.0.0`).ReplaceAllString(normalized, "")
	normalized = regexp.MustCompile(`/Users/\w+/`).ReplaceAllString(normalized, "/Users/<user>/")
	normalized = regexp.MustCompile(`/home/\w+/`).ReplaceAllString(normalized, "/home/<user>/")
	return normalized
}
```

**Verification**: Dynamic values (UUIDs, IDs, paths) are replaced with placeholders.

---

### T-PRC-005: Implement centralized PII and secret masking

**File**: `apps/processor-go/masker.go` (new)

**Requirements**: FR-011

```go
package main

import (
	"regexp"
	"strings"
)

type Masker struct {
	piiPatterns []*regexp.Regexp
	secretPatterns []*regexp.Regexp
}

func NewMasker() *Masker {
	piiPatterns := []*regexp.Regexp{
		regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`),
		regexp.MustCompile(`\b[A-Z]{1,2}\d{6,8}\b`),
		regexp.MustCompile(`"name"\s*:\s*"[^"]+"`),
		regexp.MustCompile(`"address"\s*:\s*"[^"]+"`),
	}

	secretPatterns := []*regexp.Regexp{
		regexp.MustCompile(`(?i)(api[_-]?key|secret[_-]?key|access[_-]?token)\s*[:=]\s*["'][^"']+["']`),
		regexp.MustCompile(`(?i)password\s*[:=]\s*["'][^"']+["']`),
		regexp.MustCompile(`(?i)bearer\s+[a-zA-Z0-9\-._~+/]+`),
		regexp.MustCompile(`(?i)token\s*[:=]\s*["'][^"']+["']`),
	}

	return &Masker{
		piiPatterns:    piiPatterns,
		secretPatterns: secretPatterns,
	}
}

func (m *Masker) Mask(input string) string {
	result := input

	for _, pattern := range m.secretPatterns {
		result = pattern.ReplaceAllStringFunc(result, func(match string) string {
			return regexp.MustCompile(`(["'])([^"']+)(["'])$`).ReplaceAllString(match, "$1<MASKED>$3")
		})
	}

	for _, pattern := range m.piiPatterns {
		result = pattern.ReplaceAllString(result, "<PII_MASKED>")
	}

	return result
}

func (m *Masker) MaskMetadata(metadata map[string]interface{}) map[string]interface{} {
	masked := make(map[string]interface{})
	for key, value := range metadata {
		if m.isSensitiveKey(key) {
			masked[key] = "<MASKED>"
		} else if str, ok := value.(string); ok {
			masked[key] = m.Mask(str)
		} else {
			masked[key] = value
		}
	}
	return masked
}

func (m *Masker) isSensitiveKey(key string) bool {
	lowerKey := strings.ToLower(key)
	sensitiveKeys := []string{"password", "token", "secret", "api_key", "apikey", "credential", "auth"}
	for _, sensitive := range sensitiveKeys {
		if strings.Contains(lowerKey, sensitive) {
			return true
		}
	}
	return false
}
```

**Verification**: PII and secret patterns are replaced with placeholder values.

---

### T-PRC-006: Implement de-duplication logic

**File**: `apps/processor-go/deduplicate.go` (new)

**Requirements**: FR-002

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/NurfitraPujo/sentinel/gen/sentinel/v1"
	"github.com/google/uuid"
)

type DeduplicationInput struct {
	ProjectKey   string
	Fingerprint  string
	Message      string
	ErrorClass   string
	Environment  string
	Platform     string
	Stacktrace   []*sentinelv1.StackFrame
	Metadata     interface{}
	TraceID      string
	SpanID       string
}

type Deduplicator struct {
	db *pgxpool.Pool
}

func NewDeduplicator(db *pgxpool.Pool) *Deduplicator {
	return &Deduplicator{db: db}
}

func (d *Deduplicator) Upsert(ctx context.Context, input *DeduplicationInput) (issueID, occurrenceID uuid.UUID, err error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	var existingIssueID uuid.UUID
	var existingCount int64
	err = tx.QueryRowContext(ctx, `
		SELECT id, count FROM issues
		WHERE project_id = (SELECT id FROM projects WHERE api_key = $1 LIMIT 1)
		AND fingerprint = $2
		LIMIT 1
	`, input.ProjectKey, input.Fingerprint).Scan(&existingIssueID, &existingCount)

	var issueUUID uuid.UUID
	if err == pgx.ErrNoRows {
		issueUUID = uuid.New()
		_, err = tx.ExecContext(ctx, `
			INSERT INTO issues (id, project_id, fingerprint, message, error_class, status, first_seen, last_seen, count)
			VALUES ($1, (SELECT id FROM projects WHERE api_key = $2 LIMIT 1), $3, $4, $5, 'open', NOW(), NOW(), 1)
		`, issueUUID, input.ProjectKey, input.Fingerprint, input.Message, input.ErrorClass)
		if err != nil {
			return uuid.Nil, uuid.Nil, fmt.Errorf("failed to insert issue: %w", err)
		}
	} else if err != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("failed to query issue: %w", err)
	} else {
		issueUUID = existingIssueID
		_, err = tx.ExecContext(ctx, `
			UPDATE issues SET last_seen = NOW(), count = count + 1 WHERE id = $1
		`, issueUUID)
		if err != nil {
			return uuid.Nil, uuid.Nil, fmt.Errorf("failed to update issue: %w", err)
		}
	}

	occurrenceUUID := uuid.New()
	stacktraceJSON, _ := json.Marshal(input.Stacktrace)
	metadataJSON, _ := json.Marshal(input.Metadata)

	_, err = tx.ExecContext(ctx, `
		INSERT INTO error_occurrences (id, issue_id, environment, platform, stacktrace, metadata, trace_id, span_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`, occurrenceUUID, issueUUID, input.Environment, input.Platform, stacktraceJSON, metadataJSON, input.TraceID, input.SpanID, time.Now())
	if err != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("failed to insert occurrence: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("failed to commit transaction: %w", err)
	}

	return issueUUID, occurrenceUUID, nil
}
```

**Verification**: Duplicate errors increment count; new errors create new issues.

---

### T-PRC-007: Implement specialized indexing

**File**: `apps/processor-go/indexer.go` (new)

**Requirements**: FR-004

```go
package main

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

type Indexer struct {
	db *pgxpool.Pool
}

func NewIndexer(db *pgxpool.Pool) *Indexer {
	return &Indexer{db: db}
}

func (i *Indexer) IndexOccurrence(ctx context.Context, occurrenceID uuid.UUID, metadata interface{}) error {
	var userID, tenantID, traceID, spanID, requestID *string

	if metadataMap, ok := metadata.(map[string]interface{}); ok {
		if v, ok := metadataMap["user_id"].(string); ok {
			userID = &v
		}
		if v, ok := metadataMap["tenant_id"].(string); ok {
			tenantID = &v
		}
		if v, ok := metadataMap["trace_id"].(string); ok {
			traceID = &v
		}
		if v, ok := metadataMap["span_id"].(string); ok {
			spanID = &v
		}
		if v, ok := metadataMap["request_id"].(string); ok {
			requestID = &v
		}
	}

	_, err := i.db.ExecContext(ctx, `
		INSERT INTO error_search_index (occurrence_id, user_id, tenant_id, trace_id, span_id, request_id)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (occurrence_id) DO UPDATE SET
			user_id = COALESCE($2, error_search_index.user_id),
			tenant_id = COALESCE($3, error_search_index.tenant_id),
			trace_id = COALESCE($4, error_search_index.trace_id),
			span_id = COALESCE($5, error_search_index.span_id),
			request_id = COALESCE($6, error_search_index.request_id)
	`, occurrenceID, userID, tenantID, traceID, spanID, requestID)

	if err != nil {
		return fmt.Errorf("failed to index occurrence: %w", err)
	}

	return nil
}
```

**Verification**: Search index contains extracted metadata fields.

---

### T-PRC-008: Implement alerting dispatcher logic

**File**: `apps/processor-go/alerter.go` (new)

**Requirements**: FR-005

```go
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/google/uuid"
)

type AlertDispatcher struct {
	db          *pgxpool.Pool
	emailWorker *EmailWorker
	tgWorker    *TelegramWorker
}

func NewAlertDispatcher(db *pgxpool.Pool) *AlertDispatcher {
	return &AlertDispatcher{
		db:          db,
		emailWorker: NewEmailWorker(db),
		tgWorker:    NewTelegramWorker(db),
	}
}

type AlertConfig struct {
	ID                    uuid.UUID
	ProjectID             uuid.UUID
	Channel               string
	ChannelConfig         map[string]interface{}
	FrequencyThreshold    int
	FrequencyWindowSeconds int
	Enabled               bool
}

func (a *AlertDispatcher) CheckAndDispatch(ctx context.Context, issueID uuid.UUID) error {
	var projectID uuid.UUID
	var count int64
	var fingerprint string
	var message string
	var errorClass string

	err := a.db.QueryRowContext(ctx, `
		SELECT project_id, count, fingerprint, message, error_class FROM issues WHERE id = $1
	`, issueID).Scan(&projectID, &count, &fingerprint, &message, &errorClass)
	if err != nil {
		return fmt.Errorf("failed to query issue: %w", err)
	}

	alertConfigs, err := a.getAlertConfigs(ctx, projectID)
	if err != nil {
		return fmt.Errorf("failed to get alert configs: %w", err)
	}

	for _, config := range alertConfigs {
		if !config.Enabled {
			continue
		}

		shouldAlert := false

		if count == 1 {
			shouldAlert = true
		} else {
			withinWindow, err := a.checkFrequencyThreshold(ctx, issueID, config.FrequencyThreshold, config.FrequencyWindowSeconds)
			if err != nil {
				continue
			}
			shouldAlert = withinWindow
		}

		if shouldAlert {
			switch config.Channel {
			case "email":
				a.emailWorker.Send(ctx, issueID, config, errorClass, message, int(count))
			case "telegram":
				a.tgWorker.Send(ctx, issueID, config, errorClass, message, int(count))
			}
		}
	}

	return nil
}

func (a *AlertDispatcher) getAlertConfigs(ctx context.Context, projectID uuid.UUID) ([]AlertConfig, error) {
	rows, err := a.db.QueryContext(ctx, `
		SELECT id, project_id, channel, channel_config, frequency_threshold, frequency_window_seconds, enabled
		FROM alert_configs
		WHERE project_id = $1
	`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var configs []AlertConfig
	for rows.Next() {
		var cfg AlertConfig
		var channelConfigJSON []byte
		err := rows.Scan(&cfg.ID, &cfg.ProjectID, &cfg.Channel, &channelConfigJSON, &cfg.FrequencyThreshold, &cfg.FrequencyWindowSeconds, &cfg.Enabled)
		if err != nil {
			continue
		}
		configs = append(configs, cfg)
	}

	return configs, nil
}

func (a *AlertDispatcher) checkFrequencyThreshold(ctx context.Context, issueID uuid.UUID, threshold int, windowSeconds int) (bool, error) {
	windowStart := time.Now().Add(-time.Duration(windowSeconds) * time.Second)

	var recentCount int64
	err := a.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM error_occurrences
		WHERE issue_id = $1 AND created_at >= $2
	`, issueID, windowStart).Scan(&recentCount)
	if err != nil {
		return false, err
	}

	return recentCount >= int64(threshold), nil
}
```

**Verification**: Alerts are dispatched when thresholds are met.

---

### T-PRC-009: Implement Email notification worker

**File**: `apps/processor-go/email_worker.go` (new)

**Requirements**: FR-005

```go
package main

import (
	"context"
	"fmt"
	"log"
	"net/smtp"
	"sync"
	"time"

	"github.com/google/uuid"
)

type RetryConfig struct {
	maxRetries    int
	backoffDelays []time.Duration
}

var defaultRetryConfig = RetryConfig{
	maxRetries:    3,
	backoffDelays: []time.Duration{1 * time.Second, 5 * time.Second, 30 * time.Second},
}

type EmailWorker struct {
	db           *pgxpool.Pool
	retryConfig  RetryConfig
	queue        chan *AlertJob
	stopCh       chan struct{}
	wg           sync.WaitGroup
}

type AlertJob struct {
	IssueID     uuid.UUID
	Channel     string
	Config      AlertConfig
	ErrorClass  string
	Message     string
	Count       int
	RetryCount  int
}

func NewEmailWorker(db *pgxpool.Pool) *EmailWorker {
	w := &EmailWorker{
		db:          db,
		retryConfig: defaultRetryConfig,
		queue:       make(chan *AlertJob, 1000),
		stopCh:      make(chan struct{}),
	}
	w.startWorkers(3)
	return w
}

func (w *EmailWorker) startWorkers(n int) {
	for i := 0; i < n; i++ {
		w.wg.Add(1)
		go w.processQueue()
	}
}

func (w *EmailWorker) processQueue() {
	defer w.wg.Done()
	for {
		select {
		case job := <-w.queue:
			w.sendWithRetry(job)
		case <-w.stopCh:
			return
		}
	}
}

func (w *EmailWorker) Send(ctx context.Context, issueID uuid.UUID, config AlertConfig, errorClass, message string, count int) {
	job := &AlertJob{
		IssueID:    issueID,
		Channel:    "email",
		Config:     config,
		ErrorClass: errorClass,
		Message:    message,
		Count:      count,
	}
	select {
	case w.queue <- job:
	default:
		log.Printf("WARNING: Email queue full, dropping alert for issue %s", issueID)
	}
}

func (w *EmailWorker) sendWithRetry(job *AlertJob) {
	delay := time.Duration(0)
	if job.RetryCount > 0 && job.RetryCount <= len(w.retryConfig.backoffDelays) {
		delay = w.retryConfig.backoffDelays[job.RetryCount-1]
		time.Sleep(delay)
	}

	err := w.sendEmail(job)
	if err != nil {
		log.Printf("ERROR: Failed to send email (attempt %d/%d): %v", job.RetryCount+1, w.retryConfig.maxRetries, err)
		if job.RetryCount < w.retryConfig.maxRetries {
			job.RetryCount++
			w.queue <- job
		} else {
			log.Printf("ERROR: Email alert failed after %d attempts for issue %s", w.retryConfig.maxRetries, job.IssueID)
		}
	} else {
		log.Printf("INFO: Email sent for issue %s", job.IssueID)
	}
}

func (w *EmailWorker) sendEmail(job *AlertJob) error {
	emailConfig, ok := job.Config.ChannelConfig.(map[string]interface{})
	if !ok {
		return fmt.Errorf("invalid email config")
	}

	to, ok := emailConfig["to"].(string)
	if !ok {
		return fmt.Errorf("missing 'to' address in email config")
	}

	smtpHost, _ := emailConfig["smtp_host"].(string)
	if smtpHost == "" {
		smtpHost = "localhost"
	}

	smtpPort, _ := emailConfig["smtp_port"].(string)
	if smtpPort == "" {
		smtpPort = "587"
	}

	from, _ := emailConfig["from"].(string)
	if from == "" {
		from = "sentinel@localhost"
	}

	subject := fmt.Sprintf("[Sentinel Alert] New error: %s (count: %d)", job.ErrorClass, job.Count)
	body := fmt.Sprintf(`
Error Class: %s
Message: %s
Count: %d
Issue ID: %s

View in dashboard: http://sentinel.example.com/issues/%s
	`, job.ErrorClass, job.Message, job.Count, job.IssueID, job.IssueID)

	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\n\r\n%s",
		from, to, subject, body)

	addr := fmt.Sprintf("%s:%s", smtpHost, smtpPort)
	auth := smtp.PlainAuth("", emailConfig["username"].(string), emailConfig["password"].(string), smtpHost)

	err := smtp.SendMail(addr, auth, from, []string{to}, []byte(msg))
	if err != nil {
		return err
	}

	return nil
}

func (w *EmailWorker) Stop() {
	close(w.stopCh)
	w.wg.Wait()
}
```

**Verification**: Email is sent when alert is triggered. Failed emails are retried with exponential backoff (1s, 5s, 30s) up to 3 attempts.

---

### T-PRC-010: Implement Telegram notification worker

**File**: `apps/processor-go/telegram_worker.go` (new)

**Requirements**: FR-005

```go
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/google/uuid"
)

type TelegramWorker struct {
	db    *pgxpool.Pool
	client *http.Client
}

func NewTelegramWorker(db *pgxpool.Pool) *TelegramWorker {
	return &TelegramWorker{
		db: db,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

type TelegramMessage struct {
	ChatID string `json:"chat_id"`
	Text   string `json:"text"`
	ParseMode string `json:"parse_mode,omitempty`
}

func (w *TelegramWorker) Send(ctx context.Context, issueID uuid.UUID, config AlertConfig, errorClass, message string, count int) error {
	tgConfig, ok := config.ChannelConfig.(map[string]interface{})
	if !ok {
		return fmt.Errorf("invalid telegram config")
	}

	botToken, ok := tgConfig["bot_token"].(string)
	if !ok {
		return fmt.Errorf("missing 'bot_token' in telegram config")
	}

	chatID, ok := tgConfig["chat_id"].(string)
	if !ok {
		return fmt.Errorf("missing 'chat_id' in telegram config")
	}

	text := fmt.Sprintf("*Sentinel Alert*\n\n*Error:* `%s`\n*Message:* %s\n*Count:* %d\n*Issue:* `%s`",
		errorClass, message, count, issueID)

	tgMsg := TelegramMessage{
		ChatID:    chatID,
		Text:      text,
		ParseMode: "Markdown",
	}

	payload, err := json.Marshal(tgMsg)
	if err != nil {
		return fmt.Errorf("failed to marshal telegram message: %w", err)
	}

	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", botToken)
	resp, err := w.client.Post(url, "application/json", bytes.NewBuffer(payload))
	if err != nil {
		log.Printf("Failed to send telegram message: %v", err)
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram API returned status %d", resp.StatusCode)
	}

	log.Printf("Telegram message sent to chat %s for issue %s", chatID, issueID)
	return nil
}
```

**Verification**: Telegram message is sent when alert is triggered.

---

## Phase 4: Dashboard & Analytics (dashboard-web)

### T-DSH-001: Setup SvelteKit with Google Workspace OIDC authentication

**File**: `apps/dashboard-web/package.json` (new)

```json
{
  "name": "sentinel-dashboard",
  "version": "0.1.0",
  "private": true,
  "scripts": {
    "dev": "vite dev",
    "build": "vite build",
    "preview": "vite preview",
    "check": "svelte-kit sync && svelte-check --tsconfig ./tsconfig.json",
    "check:watch": "svelte-kit sync && svelte-check --tsconfig ./tsconfig.json --watch"
  },
  "devDependencies": {
    "@sveltejs/adapter-auto": "^3.0.0",
    "@sveltejs/kit": "^2.0.0",
    "@sveltejs/vite-plugin-svelte": "^3.0.0",
    "svelte": "^4.2.0",
    "svelte-check": "^3.6.0",
    "typescript": "^5.0.0",
    "vite": "^5.0.0"
  },
  "dependencies": {
    "@google-cloud/local-auth": "^2.1.0",
    "drizzle-orm": "^0.29.0",
    "google-auth-library": "^9.0.0",
    "postgres": "^3.4.0"
  },
  "type": "module"
}
```

**File**: `apps/dashboard-web/svelte.config.js` (new)

```js
import adapter from '@sveltejs/adapter-auto';
import { vitePreprocess } from '@sveltejs/vite-plugin-svelte';

/** @type {import('@sveltejs/kit').Config} */
const config = {
	preprocess: vitePreprocess(),
	kit: {
		adapter: adapter(),
		alias: {
			$lib: './src/lib',
			'$lib/*': './src/lib/*'
		}
	}
};

export default config;
```

**File**: `apps/dashboard-web/src/app.html` (new)

```html
<!doctype html>
<html lang="en">
	<head>
		<meta charset="utf-8" />
		<link rel="icon" href="%sveltekit.assets%/favicon.png" />
		<meta name="viewport" content="width=device-width, initial-scale=1" />
		%sveltekit.head%
	</head>
	<body data-sveltekit-preload-data="hover">
		<div style="display: contents">%sveltekit.body%</div>
	</body>
</html>
```

**File**: `apps/dashboard-web/src/routes/+layout.server.ts` (new)

```typescript
import type { LayoutServerLoad } from './$types';
import { redirect } from '@sveltejs/kit';
import { getUserFromSession } from '$lib/auth.server';

export const load: LayoutServerLoad = async ({ url, cookies }) => {
	const sessionCookie = cookies.get('session');

	if (!sessionCookie && !url.pathname.startsWith('/auth')) {
		throw redirect(302, '/auth/login');
	}

	if (sessionCookie) {
		const session = JSON.parse(sessionCookie);
		const user = await getUserFromSession(session.email);
		return {
			user,
			session
		};
	}

	return {
		user: null,
		session: null
	};
};
```

**File**: `apps/dashboard-web/src/routes/auth/login/+server.ts` (new)

```typescript
import { redirect } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { google } from 'google-auth-library';

const clientId = process.env.GOOGLE_CLIENT_ID || '';
const redirectUri = process.env.GOOGLE_REDIRECT_URI || 'http://localhost:5173/auth/callback';

export const GET: RequestHandler = async ({ cookies }) => {
	const oauth2Client = new google.auth.OAuth2(clientId, '', redirectUri);
	oauth2Client.redirectUri = redirectUri;

	const state = crypto.randomUUID();
	cookies.set('oauth_state', state, {
		path: '/',
		httpOnly: true,
		secure: process.env.NODE_ENV === 'production',
		sameSite: 'lax',
		maxAge: 60 * 10
	});

	const authUrl = oauth2Client.generateAuthUrl({
		access_type: 'offline',
		scope: ['openid', 'email', 'profile'],
		hd: 'example.com',
		state
	});

	throw redirect(302, authUrl);
};
```

**File**: `apps/dashboard-web/src/routes/auth/callback/+server.ts` (new)

```typescript
import type { RequestHandler } from './$types';
import { google } from 'google-auth-library';
import { redirect } from '@sveltejs/kit';
import { db } from '$lib/db';
import { users } from '$lib/schema';
import { eq } from 'drizzle-orm';

const clientId = process.env.GOOGLE_CLIENT_ID || '';
const clientSecret = process.env.GOOGLE_CLIENT_SECRET || '';
const redirectUri = process.env.GOOGLE_REDIRECT_URI || 'http://localhost:5173/auth/callback';

export const GET: RequestHandler = async ({ url, cookies }) => {
	const code = url.searchParams.get('code');
	const state = url.searchParams.get('state');
	const storedState = cookies.get('oauth_state');

	if (!code || !state || state !== storedState) {
		throw redirect(302, '/auth/login');
	}

	const oauth2Client = new google.auth.OAuth2(clientId, clientSecret, redirectUri);
	const { tokens } = await oauth2Client.getToken(code);

	oauth2Client.setCredentials(tokens);

	const oauth2 = google.oauth2({ version: 'v2', auth: oauth2Client });
	const { data: profile } = await oauth2.userinfo.get();

	if (!profile.hd || profile.hd !== 'example.com') {
		throw redirect(302, '/auth/unauthorized');
	}

	const [existingUser] = await db.select().from(users).where(eq(users.email, profile.email!)).limit(1);
	if (!existingUser) {
		await db.insert(users).values({
			email: profile.email!,
			name: profile.name
		});
	}

	cookies.delete('oauth_state', { path: '/' });
	cookies.set('session', JSON.stringify({
		email: profile.email,
		name: profile.name,
		picture: profile.picture,
		access_token: tokens.access_token
	}), {
		path: '/',
		httpOnly: true,
		secure: process.env.NODE_ENV === 'production',
		sameSite: 'lax',
		maxAge: 60 * 60 * 24 * 7
	});

	throw redirect(302, '/');
};
```

**Verification**: SvelteKit dev server starts; Google OIDC login flow works.

---

### T-SEC-003: Implement Project-level RBAC for dashboard access

**File**: `apps/dashboard-web/src/lib/rbac.server.ts` (new)

**Requirements**: FR-006

```typescript
import type { Role } from './rbac';

export interface User {
	email: string;
	name: string;
	projectAccess: ProjectAccess[];
}

export interface ProjectAccess {
	projectId: string;
	role: Role;
}

export function canAccessProject(user: User | null, projectId: string, requiredRole: Role): boolean {
	if (!user) return false;

	const access = user.projectAccess.find(a => a.projectId === projectId);
	if (!access) return false;

	const roleHierarchy: Record<Role, number> = {
		admin: 3,
		developer: 2,
		viewer: 1
	};

	return roleHierarchy[access.role] >= roleHierarchy[requiredRole];
}

export function canManageProject(user: User | null, projectId: string): boolean {
	return canAccessProject(user, projectId, 'admin');
}

export function canViewProject(user: User | null, projectId: string): boolean {
	return canAccessProject(user, projectId, 'viewer');
}

export function canEditProject(user: User | null, projectId: string): boolean {
	return canAccessProject(user, projectId, 'developer');
}

export function requireProjectAccess(user: User | null, projectId: string, requiredRole: Role): void {
	if (!canAccessProject(user, projectId, requiredRole)) {
		throw error(403, 'Access denied');
	}
}
```

**File**: `apps/dashboard-web/src/lib/auth.server.ts` (new)

```typescript
import { db } from './db';
import { projectAccess, users } from '$lib/schema';
import { eq } from 'drizzle-orm';
import type { User } from './rbac.server';

export async function getUserFromSession(email: string): Promise<User | null> {
	const [userRecord] = await db.select().from(users).where(eq(users.email, email)).limit(1);
	if (!userRecord) return null;

	const accessRecords = await db.select().from(projectAccess).where(eq(projectAccess.userId, userRecord.id));

	return {
		email: userRecord.email,
		name: userRecord.name,
		projectAccess: accessRecords.map(r => ({
			projectId: r.projectId,
			role: r.role
		}))
	};
}
```

**Verification**: Unauthorized users cannot access projects they don't have access to.

---

### T-DSH-002: Implement Issue List view

**File**: `apps/dashboard-web/src/lib/schema.ts` (new)

```typescript
import { pgTable, uuid, varchar, timestamp, bigint, text, jsonb, index } from 'drizzle-orm/pg-core';

export const projects = pgTable('projects', {
	id: uuid('id').primaryKey().defaultRandom(),
	name: varchar('name', { length: 255 }).notNull(),
	apiKey: varchar('api_key', { length: 64 }).notNull(),
	createdAt: timestamp('created_at').defaultNow()
});

export const issues = pgTable('issues', {
	id: uuid('id').primaryKey().defaultRandom(),
	projectId: uuid('project_id').notNull().references(() => projects.id),
	fingerprint: varchar('fingerprint', { length: 64 }).notNull(),
	message: text('message').notNull(),
	errorClass: varchar('error_class', { length: 255 }).notNull(),
	status: varchar('status', { length: 20 }).notNull().default('open'),
	firstSeen: timestamp('first_seen').defaultNow(),
	lastSeen: timestamp('last_seen').defaultNow(),
	count: bigint('count', { mode: 'number' }).notNull().default(1)
}, (table) => ({
	projectIdx: index('idx_issues_project_id').on(table.projectId),
	fingerprintIdx: index('idx_issues_fingerprint').on(table.fingerprint),
	statusIdx: index('idx_issues_status').on(table.status)
}));

export const errorOccurrences = pgTable('error_occurrences', {
	id: uuid('id').primaryKey().defaultRandom(),
	issueId: uuid('issue_id').notNull().references(() => issues.id),
	environment: varchar('environment', { length: 50 }).notNull(),
	platform: varchar('platform', { length: 50 }).notNull(),
	stacktrace: jsonb('stacktrace').notNull().default([]),
	metadata: jsonb('metadata').notNull().default({}),
	traceId: varchar('trace_id', { length: 64 }),
	spanId: varchar('span_id', { length: 64 }),
	createdAt: timestamp('created_at').defaultNow()
});

export const users = pgTable('users', {
	id: uuid('id').primaryKey().defaultRandom(),
	email: varchar('email', { length: 255 }).notNull().unique(),
	name: varchar('name', { length: 255 })
});

export const projectAccess = pgTable('project_access', {
	id: uuid('id').primaryKey().defaultRandom(),
	userId: uuid('user_id').notNull().references(() => users.id),
	projectId: uuid('project_id').notNull().references(() => projects.id),
	role: varchar('role', { length: 20 }).notNull()
});

export type Project = typeof projects.$inferSelect;
export type Issue = typeof issues.$inferSelect;
export type ErrorOccurrence = typeof errorOccurrences.$inferSelect;
```

**File**: `apps/dashboard-web/src/lib/db.ts` (new)

```typescript
import { drizzle } from 'drizzle-orm/postgres-js';
import postgres from 'postgres';
import * as schema from './schema';

const connectionString = process.env.DATABASE_URL || 'postgres://sentinel:changeme@localhost:5432/sentinel';

const client = postgres(connectionString);
export const db = drizzle(client, { schema });
```

**File**: `apps/dashboard-web/src/routes/+page.svelte` (new)

```svelte
<script lang="ts">
	import type { PageData } from './$types';

	export let data: PageData;

	let issues: any[] = [];
	let selectedProject = '';
	let selectedStatus = '';
	let isLoading = false;

	async function loadIssues() {
		isLoading = true;
		const response = await fetch(`/api/issues?project=${selectedProject}&status=${selectedStatus}`);
		if (response.ok) {
			issues = await response.json();
		}
		isLoading = false;
	}

	function formatDate(dateStr: string): string {
		return new Date(dateStr).toLocaleString();
	}

	$: if (selectedProject || selectedStatus) {
		loadIssues();
	}
</script>

<div class="container mx-auto p-4">
	<div class="flex gap-4 mb-6">
		<select bind:value={selectedProject} class="border p-2 rounded">
			<option value="">All Projects</option>
			{#each data.projects as project}
				<option value={project.id}>{project.name}</option>
			{/each}
		</select>

		<select bind:value={selectedStatus} class="border p-2 rounded">
			<option value="">All Statuses</option>
			<option value="open">Open</option>
			<option value="resolved">Resolved</option>
			<option value="ignored">Ignored</option>
		</select>
	</div>

	{#if isLoading}
		<p>Loading...</p>
	{:else}
		<table class="min-w-full border">
			<thead>
				<tr class="bg-gray-100">
					<th class="p-2 border">Error Class</th>
					<th class="p-2 border">Message</th>
					<th class="p-2 border">Status</th>
					<th class="p-2 border">Count</th>
					<th class="p-2 border">Last Seen</th>
				</tr>
			</thead>
			<tbody>
				{#each issues as issue}
					<tr class="hover:bg-gray-50">
						<td class="p-2 border">
							<a href="/issues/{issue.id}" class="text-blue-600 hover:underline">
								{issue.error_class}
							</a>
						</td>
						<td class="p-2 border truncate max-w-md">{issue.message}</td>
						<td class="p-2 border">
							<span class="px-2 py-1 rounded text-sm {issue.status === 'open' ? 'bg-red-100 text-red-800' : issue.status === 'resolved' ? 'bg-green-100 text-green-800' : 'bg-gray-100 text-gray-800'}">
								{issue.status}
							</span>
						</td>
						<td class="p-2 border text-right">{issue.count}</td>
						<td class="p-2 border">{formatDate(issue.last_seen)}</td>
					</tr>
				{/each}
			</tbody>
		</table>
	{/if}
</div>
```

**File**: `apps/dashboard-web/src/routes/+page.server.ts` (new)

```typescript
import type { PageServerLoad } from './$types';
import { db } from '$lib/db';
import { issues, projects, projectAccess } from '$lib/schema';
import { eq, and, inArray } from 'drizzle-orm';

export const load: PageServerLoad = async ({ parent, url }) => {
	const { user } = await parent();
	const selectedProject = url.searchParams.get('project') || '';
	const selectedStatus = url.searchParams.get('status') || '';

	if (!user) {
		return { projects: [], issues: [] };
	}

	const userAccess = await db.select().from(projectAccess).where(eq(projectAccess.userId, user.id));
	const projectIds = userAccess.map(a => a.projectId);

	const projectList = await db.select().from(projects).where(inArray(projects.id, projectIds));

	let issueList = selectedProject
		? await db.select().from(issues).where(eq(issues.projectId, selectedProject))
		: await db.select().from(issues).where(inArray(issues.projectId, projectIds));

	if (selectedStatus) {
		issueList = issueList.filter(i => i.status === selectedStatus);
	}

	return {
		projects: projectList,
		issues: issueList
	};
};
```

**Verification**: Issue list displays with filtering by project and status.

---

### T-DSH-003: Implement Issue Detail view

**File**: `apps/dashboard-web/src/routes/issues/[id]/+page.svelte` (new)

```svelte
<script lang="ts">
	import type { PageData } from './$types';

	export let data: PageData;

	let issue = data.issue;
	let occurrences = data.occurrences;

	function formatDate(dateStr: string): string {
		return new Date(dateStr).toLocaleString();
	}

	function formatStackFrame(frame: any): string {
		return `  at ${frame.function} (${frame.file}:${frame.line})`;
	}
</script>

<div class="container mx-auto p-4">
	<div class="mb-6">
		<h1 class="text-2xl font-bold">{issue.error_class}</h1>
		<p class="text-gray-600 mt-2">{issue.message}</p>
		<div class="flex gap-4 mt-4">
			<span class="px-3 py-1 rounded text-sm bg-red-100 text-red-800">
				{issue.status}
			</span>
			<span class="text-gray-500">{issue.count} occurrences</span>
			<span class="text-gray-500">First seen: {formatDate(issue.first_seen)}</span>
			<span class="text-gray-500">Last seen: {formatDate(issue.last_seen)}</span>
		</div>
	</div>

	<div class="mb-6">
		<h2 class="text-xl font-semibold mb-4">Occurrences</h2>
		<div class="space-y-4">
			{#each occurrences as occurrence}
				<div class="border rounded p-4">
					<div class="flex justify-between items-start mb-2">
						<div>
							<span class="font-medium">{occurrence.environment}</span>
							<span class="text-gray-500 ml-2">{occurrence.platform}</span>
						</div>
						<span class="text-gray-400 text-sm">{formatDate(occurrence.created_at)}</span>
					</div>

					{#if occurrence.trace_id}
						<p class="text-sm text-gray-500 mb-2">Trace ID: {occurrence.trace_id}</p>
					{/if}

					{#if occurrence.stacktrace && occurrence.stacktrace.length > 0}
						<pre class="bg-gray-100 p-3 rounded text-sm overflow-x-auto mt-2">{@html occurrence.stacktrace.map(f => formatStackFrame(f)).join('\n')}</pre>
					{/if}

					{#if occurrence.metadata && Object.keys(occurrence.metadata).length > 0}
						<div class="mt-2">
							<h4 class="text-sm font-medium text-gray-700">Metadata</h4>
							<div class="bg-gray-50 p-2 rounded text-sm">
								{#each Object.entries(occurrence.metadata) as [key, value]}
									<p><span class="text-gray-500">{key}:</span> {value}</p>
								{/each}
							</div>
						</div>
					{/if}
				</div>
			{/each}
		</div>
	</div>
</div>
```

**File**: `apps/dashboard-web/src/routes/issues/[id]/+page.server.ts` (new)

```typescript
import type { PageServerLoad } from './$types';
import { error } from '@sveltejs/kit';
import { db } from '$lib/db';
import { issues, errorOccurrences, projectAccess } from '$lib/schema';
import { eq, and } from 'drizzle-orm';
import { requireProjectAccess } from '$lib/rbac.server';

export const load: PageServerLoad = async ({ params, parent }) => {
	const issueId = params.id;
	const { user } = await parent();

	const [issue] = await db.select().from(issues).where(eq(issues.id, issueId)).limit(1);
	if (!issue) {
		throw error(404, 'Issue not found');
	}

	requireProjectAccess(user, issue.projectId, 'viewer');

	const occurrenceList = await db.select().from(errorOccurrences).where(eq(errorOccurrences.issueId, issueId));

	return {
		issue,
		occurrences: occurrenceList
	};
};
```

**Verification**: Issue detail displays full stack traces and metadata for each occurrence.

---

### T-DSH-004: Implement advanced search

**File**: `apps/dashboard-web/src/routes/search/+page.server.ts` (new)

**Requirements**: FR-004

```typescript
import type { PageServerLoad } from './$types';
import { db } from '$lib/db';
import { errorOccurrences, errorSearchIndex, issues, projectAccess } from '$lib/schema';
import { eq, and, like, inArray } from 'drizzle-orm';

export const load: PageServerLoad = async ({ parent, url }) => {
	const { user } = await parent();

	const query = url.searchParams.get('q') || '';
	const userId = url.searchParams.get('user_id');
	const traceId = url.searchParams.get('trace_id');
	const spanId = url.searchParams.get('span_id');
	const requestId = url.searchParams.get('request_id');

	if (!user) {
		return { results: [] };
	}

	const userAccess = await db.select().from(projectAccess).where(eq(projectAccess.userId, user.id));
	const projectIds = userAccess.map(a => a.projectId);

	let results = [];

	const searchConditions = [];
	if (userId) searchConditions.push(eq(errorSearchIndex.userId, userId));
	if (traceId) searchConditions.push(eq(errorSearchIndex.traceId, traceId));
	if (spanId) searchConditions.push(eq(errorSearchIndex.spanId, spanId));
	if (requestId) searchConditions.push(eq(errorSearchIndex.requestId, requestId));

	if (searchConditions.length > 0) {
		const searchRecords = await db
			.select()
			.from(errorSearchIndex)
			.where(and(...searchConditions));

		const occurrenceIds = searchRecords.map(r => r.occurrenceId);

		if (occurrenceIds.length > 0) {
			const occurrences = await db
				.select()
				.from(errorOccurrences)
				.where(inArray(errorOccurrences.id, occurrenceIds))
				.limit(100);

			const issueIds = [...new Set(occurrences.map(o => o.issueId))];
			const issueList = await db
				.select()
				.from(issues)
				.where(inArray(issues.projectId, projectIds))
				.limit(100);

			const issueMap = new Map(issueList.map(i => [i.id, i]));
			results = occurrences
				.filter(o => issueMap.has(o.issueId))
				.map(o => ({
					...o,
					issue: issueMap.get(o.issueId)
				}));
		}
	} else if (query) {
		const issueList = await db
			.select()
			.from(issues)
			.where(and(
				inArray(issues.projectId, projectIds),
				like(issues.message, `%${query}%`)
			))
			.limit(100);

		results = issueList;
	}

	return { results };
};
```

**Verification**: Search returns filtered results by user_id, trace_id, etc.

---

### T-DSH-005: Implement full-text search

**File**: `apps/dashboard-web/src/routes/search/+page.svelte` (new)

```svelte
<script lang="ts">
	import type { PageData } from './$types';
	import { goto } from '$app/navigation';

	export let data: PageData;

	let searchQuery = '';

	function formatDate(dateStr: string): string {
		return new Date(dateStr).toLocaleString();
	}

	function doSearch() {
		const params = new URLSearchParams();
		if (searchQuery) params.set('q', searchQuery);
		goto(`/search?${params}`);
	}
</script>

<div class="container mx-auto p-4">
	<div class="mb-6">
		<div class="flex gap-2">
			<input
				type="text"
				bind:value={searchQuery}
				placeholder="Search error messages and stack traces..."
				class="flex-1 border p-2 rounded"
				on:keydown={(e) => e.key === 'Enter' && doSearch()}
			/>
			<button
				on:click={doSearch}
				class="bg-blue-600 text-white px-4 py-2 rounded hover:bg-blue-700"
			>
				Search
			</button>
		</div>
	</div>

	{#if data.results.length > 0}
		<div class="space-y-4">
			{#each data.results as result}
				<div class="border rounded p-4">
					<div class="flex justify-between items-start">
						<div>
							<a href="/issues/{result.issue?.id}" class="text-blue-600 hover:underline font-medium">
								{result.issue?.error_class}
							</a>
							<p class="text-gray-600 mt-1">{result.issue?.message}</p>
						</div>
						<span class="text-gray-400 text-sm">{formatDate(result.created_at)}</span>
					</div>
				</div>
			{/each}
		</div>
	{:else if searchQuery}
		<p class="text-gray-500 text-center">No results found for "{searchQuery}"</p>
	{/if}
</div>
```

**Verification**: Full-text search returns relevant results with highlighted matches.

---

## Phase 5: Verification & Hardening

### T-TST-001: Implement integration test for end-to-end flow

**File**: `apps/processor-go/e2e_test.go` (new)

**Requirements**: FR-001, FR-002, FR-003, FR-004

```go
package processor_test

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/nats"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

func TestEndToEndFlow(t *testing.T) {
	ctx := context.Background()

	postgresContainer, err := postgres.Run(ctx,
		"postgres:15-alpine",
		testcontainers.WithWaitStrategy(log.WaitStrategy),
		postgres.WithDatabase("sentinel"),
		postgres.WithUsername("sentinel"),
		postgres.WithPassword("changeme"),
	)
	if err != nil {
		t.Fatalf("Failed to start postgres: %v", err)
	}
	defer postgresContainer.Terminate(ctx)

	_, natsPort, err := natspubsub.Start(ctx)
	if err != nil {
		t.Fatalf("Failed to start NATS: %v", err)
	}

	connStr, err := postgresContainer.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("Failed to get postgres connection string: %v", err)
	}

	db, err := pgxpool.New(ctx, connStr)
	if err != nil {
		t.Fatalf("Failed to connect to postgres: %v", err)
	}
	defer db.Close()

	var count int
	err = db.QueryRow(ctx, "SELECT COUNT(*) FROM issues WHERE error_class = $1", "TestError").Scan(&count)
	if err != nil {
		t.Fatalf("Failed to query issues: %v", err)
	}

	if count != 1 {
		t.Errorf("Expected 1 issue, got %d", count)
	}
}

func natspubsub(ctx context.Context) (nats.Container, int, error) {
	natsContainer, err := nats.Run(ctx, "nats:2.10-alpine")
	if err != nil {
		return nats.Container{}, 0, err
	}

	mappedPort, err := natsContainer.MappedPort(ctx, "4222")
	if err != nil {
		natsContainer.Terminate(ctx)
		return nats.Container{}, 0, err
	}

	return nats.Container{}, mappedPort.Int(), nil
}
```

**Verification**: `go test -v ./apps/processor-go/... -run TestEndToEndFlow` passes.

---

### T-TST-002: Implement unit tests for fingerprinting and masking

**File**: `apps/processor-go/fingerprint_test.go` (new)

```go
package main

import (
	"testing"

	"github.com/NurfitraPujo/sentinel/gen/sentinel/v1"
)

func TestFingerprinter_Fingerprint(t *testing.T) {
	fp := NewFingerprinter()

	stacktrace := []*sentinelv1.StackFrame{
		{File: "app/main.go", Line: 42, Function: "main", InApp: true},
		{File: "app/handler.go", Line: 100, Function: "handleRequest", InApp: true},
		{File: "vendor/foo.go", Line: 10, Function: "vendorFunc", InApp: false},
	}

	fp1 := fp.Fingerprint("TestError", stacktrace)
	fp2 := fp.Fingerprint("TestError", stacktrace)

	if fp1 != fp2 {
		t.Errorf("Identical errors should have same fingerprint: %s != %s", fp1, fp2)
	}

	differentStacktrace := []*sentinelv1.StackFrame{
		{File: "app/main.go", Line: 50, Function: "main", InApp: true},
		{File: "app/handler.go", Line: 100, Function: "handleRequest", InApp: true},
		{File: "vendor/foo.go", Line: 10, Function: "vendorFunc", InApp: false},
	}

	fp3 := fp.Fingerprint("TestError", differentStacktrace)
	if fp1 == fp3 {
		t.Errorf("Different errors should have different fingerprints: %s == %s", fp1, fp3)
	}
}

func TestMasker_Mask(t *testing.T) {
	masker := NewMasker()

	tests := []struct {
		name     string
		input    string
		expected string
		contains bool
	}{
		{
			name:     "masks API key",
			input:    `api_key = "secret123"`,
			contains: true,
		},
		{
			name:     "masks password",
			input:    `password = "mysecretpassword"`,
			contains: true,
		},
		{
			name:     "masks bearer token",
			input:    `Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9`,
			contains: true,
		},
		{
			name:     "masks SSN",
			input:    `SSN: 123-45-6789`,
			contains: true,
		},
		{
			name:     "preserves normal text",
			input:    `This is a normal error message`,
			contains: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := masker.Mask(tt.input)
			hasMasked := result != tt.input
			if hasMasked != tt.contains {
				t.Errorf("Mask(%q) = %q, contains masked value = %v, expected %v", tt.input, result, hasMasked, tt.contains)
			}
		})
	}
}

func TestNormalizer_Normalize(t *testing.T) {
	normalizer := NewNormalizer()

	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "replaces UUID",
			input:    "Error in user abc123-def456-789",
			expected: "Error in user <normalized>",
		},
		{
			name:     "replaces large numbers",
			input:    "ID: 12345678901",
			expected: "ID: <normalized>",
		},
		{
			name:     "replaces hex values",
			input:    "Pointer: 0x7fff5fbff8c",
			expected: "Pointer: <normalized>",
		},
		{
			name:     "normalizes whitespace",
			input:    "Error   with    multiple    spaces",
			expected: "Error with multiple spaces",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := normalizer.Normalize(tt.input)
			if result != tt.expected {
				t.Errorf("Normalize(%q) = %q, expected %q", tt.input, result, tt.expected)
			}
		})
	}
}
```

**Verification**: `go test -v ./apps/processor-go/... -run TestFingerprinter` passes.

---

### T-SEC-002: Implement NATS NKEYs authentication and enable TLS

**File**: `scripts/nats-server.conf` (new)

**Requirements**: FR-008

```conf
listen: localhost:4222

http_port: 8222

server_name: sentinel-nats

operator: /etc/nats/creds/sentinel-operator.jwt
system_account: sentinel_system

resolver {
  type: memory
  use_authentication: true
}

cluster {
  name: sentinel-cluster
  port: 6222
  routes: []
}

tls {
  cert_file: /etc/nats/tls/server.crt
  key_file: /etc/nats/tls/server.key
  ca_file: /etc/nats/tls/ca.crt
  verify: true
  verify_and_map: true
}

jetstream {
  store_dir: /data/jetstream
  max_memory_store: 1GB
  max_file_store: 10GB
}

accounts {
  sentinel_service {
    jetstream: true
    users: [
      {user: processor, password: changeme, permissions: {subscribe: {allow: ["error_events"]}, publish: {deny: ["_INBOX.>"]}}}
    ]
  }
  sentinel_system {
    users: [
      {user: admin, password: changeme, permissions: {publish: {allow: ["$SYS.>"]}}}
    ]
  }
}
```

**File**: `scripts/generate-nkeys.sh` (new)

```bash
#!/bin/bash
set -e

OUTPUT_DIR="${1:-./nkeys}"

mkdir -p "$OUTPUT_DIR"

nkeys generate -o "$OUTPUT_DIR/sentinel-service" --type user
nkeys generate -o "$OUTPUT_DIR/sentinel-processor" --type user
nkeys generate -o "$OUTPUT_DIR/sentinel-operator" --type operator

echo "NKEYs generated in $OUTPUT_DIR"
echo "Store the operator JWT securely - it's needed for authentication"
```

**File**: `scripts/generate-certs.sh` (new)

```bash
#!/bin/bash
set -e

OUTPUT_DIR="${1:-./certs}"
mkdir -p "$OUTPUT_DIR"

openssl req -new -x509 -days 365 -nodes \
  -out "$OUTPUT_DIR/server.crt" \
  -keyout "$OUTPUT_DIR/server.key" \
  -subj "/CN=localhost"

openssl req -new -x509 -days 365 -nodes \
  -out "$OUTPUT_DIR/ca.crt" \
  -keyout "$OUTPUT_DIR/ca.key" \
  -subj "/CN=Sentinel CA"

echo "TLS certificates generated in $OUTPUT_DIR"
```

**File**: `apps/processor-go/nats_tls.go` (new)

```go
package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"

	"github.com/nats-io/nats.go"
)

func newSecureNATSConn(url, certFile, keyFile, caFile string) (*nats.Conn, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load client certificate: %w", err)
	}

	caCert, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read CA certificate: %w", err)
	}

	caCertPool := x509.NewCertPool()
	if !caCertPool.AppendCertsFromPEM(caCert) {
		return nil, fmt.Errorf("failed to add CA certificate to pool")
	}

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:     caCertPool,
		MinVersion:  tls.VersionTLS12,
	}

	opts := []nats.Option{
		nats.Secure(tlsConfig),
		nats.ClientCert(certFile, keyFile),
		nats.RootCAs(caFile),
	}

	conn, err := nats.Connect(url, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to NATS with TLS: %w", err)
	}

	return conn, nil
}
```

**Verification**: NATS connections use TLS and NKEYs authentication.

---

### T-DSH-006: Implement cron job for data retention cleanup

**File**: `apps/dashboard-web/src/lib/retention.server.ts` (new)

**Requirements**: SC-006 (implicit via data retention assumption)

```typescript
import { db } from './db';
import { errorOccurrences, issues, errorSearchIndex } from './schema';
import { lt, and } from 'drizzle-orm';

const RETENTION_DAYS = 30;

export async function cleanupOldData(): Promise<{ deleted: number }> {
	const cutoffDate = new Date();
	cutoffDate.setDate(cutoffDate.getDate() - RETENTION_DAYS);

	const deletedOccurrences = await db
		.delete(errorOccurrences)
		.where(lt(errorOccurrences.createdAt, cutoffDate));

	const deletedSearchIndex = await db
		.delete(errorSearchIndex)
		.where(and(
			errorSearchIndex.occurrenceId.in(
				db.select({ id: errorOccurrences.id })
					.from(errorOccurrences)
					.where(lt(errorOccurrences.createdAt, cutoffDate))
			)
		));

	const resolvedToDelete = await db
		.select({ id: issues.id })
		.from(issues)
		.where(and(
			lt(issues.lastSeen, cutoffDate),
			issues.status.equals('resolved')
		));

	const deletedIssues = await db
		.delete(issues)
		.where(and(
			lt(issues.lastSeen, cutoffDate),
			issues.status.equals('resolved')
		));

	return {
		deleted: deletedOccurrences + deletedIssues
	};
}
```

**File**: `apps/dashboard-web/scripts/cleanup-cron.sh` (new)

```bash
#!/bin/bash
set -e

DATABASE_URL="${DATABASE_URL:-postgres://sentinel:changeme@localhost:5432/sentinel}"

echo "Running data retention cleanup at $(date)"
psql "$DATABASE_URL" -c "
DELETE FROM error_occurrences WHERE created_at < NOW() - INTERVAL '30 days';
DELETE FROM error_search_index WHERE occurrence_id NOT IN (SELECT id FROM error_occurrences);
DELETE FROM issues WHERE last_seen < NOW() - INTERVAL '30 days' AND status = 'resolved';
"
echo "Cleanup completed at $(date)"
```

**Verification**: Cron job removes data older than 30 days. Run via `0 2 * * *` cron schedule (02:00 UTC daily).

---

### T-TST-003: Implement load test for 1k+ events/second spike

**File**: `apps/ingestor-go/load_test.go` (new)

**Requirements**: SC-005

```go
package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

type LoadTestResult struct {
	TotalRequests   int64
	SuccessRequests int64
	FailedRequests  int64
	ErrorDropRate   float64
	Duration        time.Duration
}

func runLoadTest(url string, apiKey string, duration time.Duration, targetRPS int) (*LoadTestResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), duration+10*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	var total, success, failed int64

	ticker := time.NewTicker(time.Second)
	done := time.After(duration)

	payload := []byte(`{"project_key":"test-project","platform":"go","environment":"production","message":"Load test error","error_class":"LoadTestError","stacktrace":[],"metadata":{}}`)

	go func() {
		for {
			select {
			case <-ticker.C:
				current := atomic.LoadInt64(&total)
				s := atomic.LoadInt64(&success)
				f := atomic.LoadInt64(&failed)
				fmt.Printf("\rRPS: %d | Total: %d | Success: %d | Failed: %d | Drop Rate: %.2f%%",
					current/int64(time.Since(ctx).Seconds()), current, s, f, float64(f)*100/float64(current+1))
			case <-done:
				ticker.Stop()
				return
			}
		}
	}()

	workerCount := targetRPS / 10
	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ticker := time.NewTicker(time.Duration(1000000000/targetRPS*workerCount) * time.Nanosecond)
			defer ticker.Stop()

			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					atomic.AddInt64(&total, 1)
					req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(payload))
					req.Header.Set("Content-Type", "application/json")
					req.Header.Set("X-API-Key", apiKey)

					resp, err := http.DefaultClient.Do(req)
					if err != nil {
						atomic.AddInt64(&failed, 1)
						continue
					}
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()

					if resp.StatusCode >= 200 && resp.StatusCode < 300 {
						atomic.AddInt64(&success, 1)
					} else {
						atomic.AddInt64(&failed, 1)
					}
				}
			}
		}()
	}

	wg.Wait()

	t := time.Since(ctx)
	totalVal := atomic.LoadInt64(&total)
	successVal := atomic.LoadInt64(&success)
	failedVal := atomic.LoadInt64(&failed)

	return &LoadTestResult{
		TotalRequests:   totalVal,
		SuccessRequests: successVal,
		FailedRequests:  failedVal,
		ErrorDropRate:   float64(failedVal) * 100 / float64(totalVal+1),
		Duration:        t,
	}, nil
}
```

**Verification**: Load test achieves 1k+ events/second with < 1% error drop rate.

---

## Checklist

- [ ] T-INF-001: Setup Docker Compose for PostgreSQL 15 and NATS JetStream
- [ ] T-INF-002: Configure NATS JetStream stream and consumer for error_events
- [ ] T-INF-003: Initialize PostgreSQL database with tables
- [ ] T-PKG-001: Define ErrorEvent Protobuf contract with proto-gen-validate rules
- [ ] T-PKG-002: Implement shared Go database connection and migration utility
- [ ] T-PKG-003: Implement shared Go NATS publisher and subscriber wrappers
- [ ] T-ING-001: Implement HTTP POST endpoint /ingest for receiving JSON error payloads
- [ ] T-ING-002: Implement publisher to NATS JetStream error_events subject
- [ ] T-SEC-001: Implement API Key authentication for the /ingest endpoint
- [ ] T-SEC-004: Implement rate limiting (5000 req/min per API key)
- [ ] T-VLD-001: Validate payloads (max 100 frames, 64KB metadata, 10000 char message)
- [ ] T-PRC-001: Implement NATS JetStream consumer for error_events
- [ ] T-PRC-002: Implement de-serialization and basic validation of consumed events
- [ ] T-PRC-003: Implement fingerprinting logic with custom fingerprint override
- [ ] T-PRC-004: Implement normalization/scrubbing
- [ ] T-PRC-005: Implement centralized PII and secret masking
- [ ] T-PRC-006: Implement de-duplication logic
- [ ] T-PRC-007: Implement specialized indexing in error_search_index table
- [ ] T-PRC-008: Implement alerting dispatcher logic
- [ ] T-PRC-009: Implement Email notification worker with retry queue
- [ ] T-PRC-010: Implement Telegram notification worker with retry queue
- [ ] T-PRC-011: Implement graceful degradation (bounded buffer when PostgreSQL unavailable)
- [ ] T-DSH-001: Setup SvelteKit with Google Workspace OIDC authentication
- [ ] T-SEC-003: Implement Project-level RBAC (admin/developer/viewer)
- [ ] T-DSH-002: Implement Issue List view with prominent card layout
- [ ] T-DSH-003: Implement Issue Detail view with occurrence history and stack trace
- [ ] T-DSH-004: Implement advanced search using error_search_index table
- [ ] T-DSH-005: Implement full-text search across issue messages and stack traces
- [ ] T-DSH-006: Implement cron job for data retention cleanup (30-day retention)
- [ ] T-TST-001: Implement integration test for end-to-end flow
- [ ] T-TST-002: Implement unit tests for fingerprinting, normalization, masking
- [ ] T-TST-003: Implement load test for 1k+ events/second with <1% error drop
- [ ] T-SEC-002: Implement NATS NKEYs authentication and enable TLS for all worker connections
