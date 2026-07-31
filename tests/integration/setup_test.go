package integration

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	tc "github.com/NurfitraPujo/sentinel/tests/integration/testcontainers"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/testcontainers/testcontainers-go"
)

var (
	testConfig PostgreSQLConfig
	natsConfig NATSConfig
	redisCfg   RedisConfig
)

type PostgreSQLConfig struct {
	Host     string
	Port     string
	User     string
	Password string
	DB       string
}

type NATSConfig struct {
	URL string
}

// RedisConfig carries the address and credentials required to connect a
// redis client to the testcontainer started in TestMain.
type RedisConfig struct {
	Addr     string
	Password string
	DB       int
}

// checkService attempts an HTTP request to determine if a service is available
func checkService(url string) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode < 500
}

// setEnvDefault sets key only when it is unset or empty, so an explicit value from the environment
// always wins over the compose default.
func setEnvDefault(key, value string) {
	if os.Getenv(key) == "" {
		os.Setenv(key, value)
	}
}

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Check if docker-compose services are available (for hybrid approach).
	// FORCE_TESTCONTAINERS=1 bypasses the docker-compose probe so the suite
	// always provisions an isolated Postgres/NATS/Ingestor/Processor stack
	// even when something else happens to be listening on port 8080.
	ingestorAvailable := os.Getenv("FORCE_TESTCONTAINERS") == "" && checkService("http://localhost:8080/health")

	// A Redis container is started in both modes so that rate-limiter
	// integration tests have a real Redis to exercise against.
	redisContainer, err := tc.StartRedis(ctx)
	if err != nil {
		fmt.Printf("Failed to start Redis: %v\n", err)
		os.Exit(1)
	}
	defer redisContainer.Terminate(ctx)

	redisCfg = RedisConfig{
		Addr:     redisContainer.Addr(),
		Password: "",
		DB:       0,
	}
	os.Setenv("REDIS_ADDR", redisCfg.Addr)
	fmt.Printf("Redis started at %s\n", redisCfg.Addr)

	if ingestorAvailable {
		fmt.Println("Using docker-compose services at localhost")
		// Compose defaults, but NEVER overwrite a value the caller already set.
		//
		// These used to be unconditional os.Setenv calls, which silently discarded an explicit
		// NATS_URL. On a machine where another project already owns the default NATS port 4222,
		// sentinel's nats is published elsewhere (NATS_HOST_PORT), and this suite would connect to the
		// FOREIGN server no matter what the operator passed — then fail with errors that look like
		// sentinel bugs. It cost real time to trace: eight tests failing with
		// "nats: insufficient storage resources available", which turned out to be the other project's
		// server refusing new streams. Compose defaults are a fallback, not an override.
		setEnvDefault("INGESTOR_URL", "http://localhost:8080")
		setEnvDefault("POSTGRES_HOST", "localhost")
		setEnvDefault("POSTGRES_PORT", "5432")
		setEnvDefault("POSTGRES_USER", "sentinel")
		setEnvDefault("POSTGRES_PASSWORD", "changeme")
		setEnvDefault("POSTGRES_DB", "sentinel")
		setEnvDefault("NATS_URL", "nats://localhost:4222")

		// Populate natsConfig too. Several tests dial gonats.Connect(natsConfig.URL) directly, and in
		// this branch natsConfig was left at its zero value — so they passed an EMPTY url, which the
		// client silently resolves to its own default (nats://127.0.0.1:4222). On a machine where
		// something else owns 4222 that means dialing a foreign server while every environment variable
		// says otherwise. Only the testcontainers branch below ever set this.
		natsConfig = NATSConfig{URL: os.Getenv("NATS_URL")}

		os.Exit(m.Run())
		return
	}

	// Testcontainers-only approach (fully isolated)
	fmt.Println("Starting isolated testcontainers infrastructure...")

	// Start PostgreSQL
	pgContainer, err := tc.StartPostgreSQL(ctx)
	if err != nil {
		fmt.Printf("Failed to start PostgreSQL: %v\n", err)
		os.Exit(1)
	}
	defer pgContainer.Terminate(ctx)

	testConfig = PostgreSQLConfig{
		Host:     pgContainer.HostIP,
		Port:     pgContainer.HostPort,
		User:     "sentinel",
		Password: "changeme",
		DB:       "sentinel",
	}

	fmt.Printf("PostgreSQL started at %s:%s\n", testConfig.Host, testConfig.Port)

	// Give PostgreSQL a moment to be fully ready
	time.Sleep(2 * time.Second)

	// Run migrations
	if err := runMigrations(ctx, testConfig); err != nil {
		fmt.Printf("Failed to run migrations: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("Database migrations applied")

	// Start NATS
	natsContainer, err := tc.StartNATS(ctx)
	if err != nil {
		fmt.Printf("Failed to start NATS: %v\n", err)
		os.Exit(1)
	}
	defer natsContainer.Terminate(ctx)

	natsConfig = NATSConfig{
		URL: natsContainer.URL(),
	}

	fmt.Printf("NATS started at %s\n", natsConfig.URL)

	// Initialize JetStream
	if err := initJetStream(natsConfig.URL); err != nil {
		fmt.Printf("Failed to initialize JetStream: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("NATS JetStream initialized")

	// Try to start ingestor. redisCfg.Addr must be the isolated redis
	// testcontainer started above, not the compose redis — see StartIngestor's
	// doc comment for why an empty/wrong value here silently reintroduces the
	// same "talked to the wrong backing service" class of bug the private
	// port fixed for Postgres/NATS.
	ingestorContainer, err := tc.StartIngestor(ctx,
		testConfig.Host, testConfig.Port,
		testConfig.User, testConfig.Password, testConfig.DB,
		natsConfig.URL, redisCfg.Addr,
	)
	if err != nil {
		fmt.Printf("Failed to start ingestor: %v\n", err)
		os.Exit(1)
	}

	// Only terminate if we have an actual container
	if ingestorContainer.Container != nil {
		defer ingestorContainer.Terminate(ctx)
	}

	ingestorURL := ingestorContainer.URL()
	fmt.Printf("Ingestor started at %s\n", ingestorURL)

	// Tail ingestor logs only when the container was actually created.
	if ingestorContainer.Container != nil {
		ingestorContainer.FollowOutput(&logConsumer{prefix: "INGESTOR"})
		ingestorContainer.StartLogProducer(ctx)
	}

	// Start processor. If the pre-built image is unavailable we tolerate it
	// here: tests that need the processor (TestIngestAndProcess,
	// TestSearchIndexing) don't skip unconditionally — they call requireInfra
	// when they subsequently hit a dependent infra failure (e.g. no ingester
	// reachable), which skips locally but hard-fails when SENTINEL_E2E=1 (see
	// setup_helper_test.go, P0-4). Tests that only exercise library code
	// (database, nats, store, alerts, middleware, service) continue to run
	// against the testcontainer Postgres/NATS stack regardless of processor
	// availability.
	processorContainer, err := tc.StartProcessor(ctx,
		testConfig.Host, testConfig.Port,
		testConfig.User, testConfig.Password, testConfig.DB,
		natsConfig.URL,
	)
	if err != nil {
		fmt.Printf("Processor container not started: %v (continuing)\n", err)
		processorContainer = nil
	}
	if processorContainer != nil && processorContainer.Container != nil {
		defer processorContainer.Terminate(ctx)
		processorContainer.FollowOutput(&logConsumer{prefix: "PROCESSOR"})
		processorContainer.StartLogProducer(ctx)
	}

	os.Setenv("POSTGRES_HOST", testConfig.Host)
	os.Setenv("POSTGRES_PORT", testConfig.Port)
	os.Setenv("POSTGRES_USER", testConfig.User)
	os.Setenv("POSTGRES_PASSWORD", testConfig.Password)
	os.Setenv("POSTGRES_DB", testConfig.DB)
	os.Setenv("NATS_URL", natsConfig.URL)
	os.Setenv("REDIS_ADDR", redisCfg.Addr)
	os.Setenv("INGESTOR_URL", ingestorURL)

	os.Exit(m.Run())
}

type logConsumer struct {
	prefix string
}

func (c *logConsumer) Accept(l testcontainers.Log) {
	fmt.Printf("[%s] %s", c.prefix, string(l.Content))
}

func initJetStream(url string) error {
	nc, err := nats.Connect(url)
	if err != nil {
		return err
	}
	defer nc.Close()

	js, err := nc.JetStream()
	if err != nil {
		return err
	}

	_, err = js.AddStream(&nats.StreamConfig{
		Name:     "ERROR_EVENTS",
		Subjects: []string{"error_events"},
	})
	if err != nil {
		return err
	}

	_, err = js.AddConsumer("ERROR_EVENTS", &nats.ConsumerConfig{
		Durable:       "processor-consumer",
		DeliverPolicy: nats.DeliverAllPolicy,
		AckPolicy:     nats.AckExplicitPolicy,
	})
	return err
}

func runMigrations(ctx context.Context, cfg PostgreSQLConfig) error {
	dsn := fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable",
		cfg.User, cfg.Password, cfg.Host, cfg.Port, cfg.DB)

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("failed to create pool: %w", err)
	}
	defer pool.Close()

	// Apply the REAL migrations, not scripts/db/init.sql. init.sql is a third hand-maintained schema
	// frozen at the 1716508800 baseline — no organizations, no project_api_keys, and the pre-S12
	// status CHECK — so bootstrapping from it silently gave every test a schema that predates
	// features 005 and 008. This mirrors testcontainers/setup.go's runPostgresMigrations; both had
	// to change, which is itself the argument for there being one migration entrypoint.
	projectRoot := findProjectRoot()
	migDir := projectRoot + "/packages/db-migrations/migrations"

	entries, err := os.ReadDir(migDir)
	if err != nil {
		return fmt.Errorf("failed to read migrations dir %s: %w", migDir, err)
	}

	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)

	for _, name := range files {
		raw, rErr := os.ReadFile(filepath.Join(migDir, name))
		if rErr != nil {
			return fmt.Errorf("failed to read migration %s: %w", name, rErr)
		}
		up := extractGooseUpSection(string(raw))
		if strings.TrimSpace(up) == "" {
			continue
		}
		if _, eErr := pool.Exec(ctx, up); eErr != nil {
			return fmt.Errorf("failed to apply migration %s: %w", name, eErr)
		}
	}

	return nil
}

// extractGooseUpSection returns only the `-- +goose Up` portion of a migration file, dropping the
// Down section and goose's StatementBegin/StatementEnd parser directives.
func extractGooseUpSection(content string) string {
	var out []string
	inUp := false
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "-- +goose Up"):
			inUp = true
			continue
		case strings.HasPrefix(trimmed, "-- +goose Down"):
			inUp = false
			continue
		case strings.HasPrefix(trimmed, "-- +goose StatementBegin"),
			strings.HasPrefix(trimmed, "-- +goose StatementEnd"):
			continue
		}
		if inUp {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

func findProjectRoot() string {
	// Start from working directory and walk up looking for go.mod
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		if _, err := os.Stat(dir + "/go.mod"); err == nil {
			return dir
		}
		parent := dir + "/.."
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

func GetTestConfig() (PostgreSQLConfig, NATSConfig) {
	if testConfig.Host == "" {
		host := os.Getenv("POSTGRES_HOST")
		if host != "" {
			return PostgreSQLConfig{
					Host:     host,
					Port:     os.Getenv("POSTGRES_PORT"),
					User:     os.Getenv("POSTGRES_USER"),
					Password: os.Getenv("POSTGRES_PASSWORD"),
					DB:       os.Getenv("POSTGRES_DB"),
				}, NATSConfig{
					URL: os.Getenv("NATS_URL"),
				}
		}
	}
	return testConfig, natsConfig
}

// GetRedisConfig returns the address and credentials of the Redis
// testcontainer started in TestMain or via tc.Setup.
func GetRedisConfig() RedisConfig {
	if redisCfg.Addr == "" {
		addr := os.Getenv("REDIS_ADDR")
		if addr != "" {
			return RedisConfig{Addr: addr}
		}
	}
	return redisCfg
}
