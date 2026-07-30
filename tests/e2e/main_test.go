// Package e2e is the P7 proof harness: it drives every row of the use-case matrix in
// docs/plans/E2E_RECOVERY_PLAN.md against the REAL compose stack — real ingestor over HTTP, real NATS
// hop, real processor, real Postgres, real dashboard — with no mocks and no in-process shortcuts.
//
// Why this package exists at all: this repository has repeatedly shipped features whose package tests
// passed while the code was never reachable from main() (BUGS.md B3), and whose two sides of a wire
// contract drifted with no test spanning the boundary (B5). Component tests cannot catch either class.
// Only a test that speaks to the deployed binaries can.
//
// # Running it
//
//	docker compose up -d --build --force-recreate && ./scripts/wait-healthy.sh
//	SENTINEL_E2E=1 go test ./tests/e2e/... -count=1 -timeout 15m
//
// # SENTINEL_E2E is the difference between "skipped" and "failed"
//
// Unset, a missing stack makes every test skip — so `go test ./...` on a laptop with nothing running
// stays usable. Set to 1 (as CI does), a missing stack is a hard failure and no test is permitted to
// skip for any reason. That asymmetry is deliberate: P0-4 exists because this suite's ancestors
// silently skipped themselves into irrelevance, reporting "ok" for years while asserting nothing.
//
// # These tests share one physical database
//
// The services under test are the compose services; they are configured to one Postgres and cannot be
// repointed per-test. Isolation is therefore by DATA, not by database: every test seeds its own
// organization and project through newFixture and scopes every assertion to those IDs. This is the
// opposite of tests/integration/db_migrations_test.go, which needs its own cloned database because it
// runs DDL. Do not add DDL — or any unscoped DELETE/UPDATE — to this package.
package e2e

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
)

// stackConfig holds the endpoints of the running stack. Every field has a compose default so the
// common case needs no environment at all.
type stackConfig struct {
	IngestorURL     string
	ProcessorHealth string
	DashboardURL    string
	DatabaseURL     string
	NATSURL         string
	RedisAddr       string
}

var (
	cfg stackConfig

	// pool is the shared connection pool used for seeding and for assertions. Tests must never use it
	// to run DDL or an unscoped mutation — see the package comment.
	pool *pgxpool.Pool

	// e2eRequired mirrors SENTINEL_E2E=1: skips become failures and missing infrastructure is fatal.
	e2eRequired bool

	// infraErr, when non-nil, is why the stack was unusable. With e2eRequired it is fatal in TestMain;
	// without it, each test skips and reports this as the reason.
	infraErr error
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func TestMain(m *testing.M) {
	e2eRequired = os.Getenv("SENTINEL_E2E") == "1"

	cfg = stackConfig{
		IngestorURL:     strings.TrimRight(env("INGESTOR_URL", "http://localhost:8080"), "/"),
		ProcessorHealth: strings.TrimRight(env("PROCESSOR_HEALTH_URL", "http://localhost:8081"), "/"),
		DashboardURL:    strings.TrimRight(env("DASHBOARD_URL", "http://localhost:3000"), "/"),
		DatabaseURL:     env("E2E_DATABASE_URL", "postgres://sentinel:changeme@localhost:5432/sentinel?sslmode=disable"),
		NATSURL:         env("NATS_URL", "nats://localhost:4222"),
		RedisAddr:       env("REDIS_ADDR", "localhost:6379"),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	if err := dialStack(ctx); err != nil {
		infraErr = err
		if e2eRequired {
			fmt.Fprintf(os.Stderr, `
SENTINEL_E2E=1 but the stack is not usable: %v

This suite asserts against the deployed binaries; there is nothing meaningful to run without them.
Bring the stack up first:

    docker compose up -d --build --force-recreate
    ./scripts/wait-healthy.sh

`, err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "e2e: stack unavailable (%v) — every test will skip. Set SENTINEL_E2E=1 to make this fatal.\n", err)
	}

	code := m.Run()
	if pool != nil {
		pool.Close()
	}
	os.Exit(code)
}

// dialStack verifies every dependency the matrix needs and opens the shared pool. It checks all of
// them before returning so a single run reports every missing piece rather than one at a time.
func dialStack(ctx context.Context) error {
	var problems []string

	var err error
	pool, err = pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		problems = append(problems, fmt.Sprintf("postgres at %s: %v", redactDSN(cfg.DatabaseURL), err))
	} else if err := pool.Ping(ctx); err != nil {
		problems = append(problems, fmt.Sprintf("postgres ping at %s: %v", redactDSN(cfg.DatabaseURL), err))
	} else if err := checkSchema(ctx); err != nil {
		problems = append(problems, err.Error())
	}

	if err := waitHTTP(ctx, cfg.IngestorURL+"/health", 60*time.Second); err != nil {
		problems = append(problems, fmt.Sprintf("ingestor: %v", err))
	}
	if err := waitHTTP(ctx, cfg.ProcessorHealth+"/health", 60*time.Second); err != nil {
		problems = append(problems, fmt.Sprintf("processor health endpoint: %v", err))
	}
	if err := waitHTTP(ctx, cfg.DashboardURL+"/", 90*time.Second); err != nil {
		problems = append(problems, fmt.Sprintf("dashboard: %v", err))
	}

	// Connecting is not enough: 4222 is THE default NATS port, so any other stack on the machine that
	// happens to own it will accept the connection and answer happily. A test that then publishes
	// directly to a stream would be talking to a foreign cluster and would fail — or worse, pass — for
	// reasons nothing in this repo explains. docker-compose.yml names this server `sentinel-nats`
	// (`--server_name`), so check we reached ours and say what to do if not.
	if nc, err := nats.Connect(cfg.NATSURL, nats.Timeout(5*time.Second)); err != nil {
		problems = append(problems, fmt.Sprintf("nats at %s: %v", cfg.NATSURL, err))
	} else {
		if name := nc.ConnectedServerName(); name != "sentinel-nats" {
			problems = append(problems, fmt.Sprintf(
				"nats at %s is server %q, not \"sentinel-nats\" — something else owns that port. "+
					"Start the stack with NATS_HOST_PORT set and point NATS_URL at it "+
					"(e.g. NATS_HOST_PORT=14222 docker compose up -d, then NATS_URL=nats://localhost:14222)",
				cfg.NATSURL, name))
		}
		nc.Close()
	}

	if c, err := net.DialTimeout("tcp", cfg.RedisAddr, 5*time.Second); err != nil {
		problems = append(problems, fmt.Sprintf("redis at %s: %v", cfg.RedisAddr, err))
	} else {
		c.Close()
	}

	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// checkSchema fails fast when the database is reachable but not migrated. Without this the whole
// suite fails one obscure query at a time; the actual cause — an un-run `migrate` service, or a
// database corrupted by a migration test that isolated itself by ledger table instead of by database
// (see tests/integration/db_migrations_test.go) — is far more legible said once, up front.
func checkSchema(ctx context.Context) error {
	required := []string{
		"organizations", "projects", "project_api_keys",
		"issues", "error_occurrences", "error_search_index",
		"issue_activity", "issue_relations", "alert_configs",
		"user", "session",
	}
	var missing []string
	for _, table := range required {
		var exists bool
		err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables
			  WHERE table_schema = 'public' AND table_name = $1)`, table).Scan(&exists)
		if err != nil {
			return fmt.Errorf("probing for table %q: %w", table, err)
		}
		if !exists {
			missing = append(missing, table)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("database is reachable but missing tables %v — run the migrate service, and check "+
			"whether an integration test corrupted the schema", missing)
	}
	return nil
}

// waitHTTP polls until the endpoint answers with any non-5xx status. A 401/404/3xx counts as up: this
// is a liveness probe, not an assertion, and some of these roots legitimately redirect or reject —
// the dashboard root 303s to /auth/signin, which is a perfectly alive dashboard.
//
// Redirects are deliberately NOT followed. Following them turns a healthy 303 into whatever the
// redirect target does, which is both slower and a different question than "is this process serving".
func waitHTTP(ctx context.Context, url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{
		Timeout:       5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	var last error
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode < 500 {
				return nil
			}
			last = fmt.Errorf("%s returned %d", url, resp.StatusCode)
		} else {
			last = err
		}
		time.Sleep(500 * time.Millisecond)
	}
	if last == nil {
		last = fmt.Errorf("timed out")
	}
	return fmt.Errorf("%s not ready after %s: %w", url, timeout, last)
}

// redactDSN strips the password so a connection failure can be logged safely.
func redactDSN(dsn string) string {
	at := strings.LastIndex(dsn, "@")
	scheme := strings.Index(dsn, "://")
	if at < 0 || scheme < 0 || at < scheme {
		return dsn
	}
	return dsn[:scheme+3] + "***" + dsn[at:]
}

// requireStack is the first line of every test in this package. Under SENTINEL_E2E=1 it never skips,
// because a skip there is a lie about coverage.
func requireStack(t *testing.T) {
	t.Helper()
	if infraErr == nil {
		return
	}
	if e2eRequired {
		t.Fatalf("stack unavailable under SENTINEL_E2E=1: %v", infraErr)
	}
	t.Skipf("stack unavailable: %v", infraErr)
}

// skipNotPermitted refuses a skip under SENTINEL_E2E=1. Use it where a test would otherwise bail on a
// condition it cannot control, so the suite reports the gap instead of hiding it.
func skipNotPermitted(t *testing.T, format string, args ...any) {
	t.Helper()
	if e2eRequired {
		t.Fatalf("skip is a failure under SENTINEL_E2E=1: "+format, args...)
	}
	t.Skipf(format, args...)
}
