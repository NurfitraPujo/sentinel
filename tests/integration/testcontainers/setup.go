package testcontainers

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
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

type RedisConfig struct {
	Addr     string
	Password string
	DB       int
}

// ResourceFlag represents the bitmask of resources a test can request.
type ResourceFlag uint

const (
	PostgresResource ResourceFlag = 1 << iota
	NATSResource
	RedisResource
	IngestorResource
	ProcessorResource

	// Common combinations
	AllResources = PostgresResource | NATSResource | RedisResource | IngestorResource | ProcessorResource
)

// Environment holds the running testcontainers / provisioned test infrastructure
// requested by a test suite or individual test.
type Environment struct {
	PGConfig    PostgreSQLConfig
	NATSConfig  NATSConfig
	RedisConfig RedisConfig
	IngestorURL string

	PostgresContainer  *PostgreSQLContainer
	NATSContainer      *NATSContainer
	RedisContainer     *RedisContainer
	IngestorContainer  *IngestorContainer
	ProcessorContainer *ProcessorContainer

	PGPool   *pgxpool.Pool
	NATSConn *nats.Conn
}

// SetupOption configures the test environment setup.
type SetupOption func(*setupConfig)

type setupConfig struct {
	resources ResourceFlag
	migrated  bool
	timeout   time.Duration
}

// WithResources specifies which resources to provision for the test environment.
func WithResources(resources ResourceFlag) SetupOption {
	return func(cfg *setupConfig) {
		cfg.resources = resources
	}
}

// WithMigrations specifies whether Postgres migrations should be applied during setup.
func WithMigrations(migrated bool) SetupOption {
	return func(cfg *setupConfig) {
		cfg.migrated = migrated
	}
}

// WithTimeout sets a custom startup timeout for provisioning containers.
func WithTimeout(timeout time.Duration) SetupOption {
	return func(cfg *setupConfig) {
		cfg.timeout = timeout
	}
}

// Setup provisions requested testcontainers or connects to existing docker-compose
// services based on environment variables (TESTCONTAINER_PROVIDER / TESTCONTAINERS_PROVIDER,
// FORCE_TESTCONTAINERS, etc.).
// It registers a t.Cleanup function to automatically terminate containers and close pools.
func Setup(t *testing.T, opts ...SetupOption) *Environment {
	t.Helper()

	cfg := &setupConfig{
		resources: PostgresResource | NATSResource | RedisResource,
		migrated:  true,
		timeout:   3 * time.Minute,
	}
	for _, opt := range opts {
		opt(cfg)
	}

	// Auto-detect & configure Podman vs Docker provider based on env vars
	ConfigureProvider()

	ctx, cancel := context.WithTimeout(context.Background(), cfg.timeout)
	defer cancel()

	env := &Environment{}

	// If Ingestor/Processor are requested, ensure underlying PG and NATS are enabled
	if cfg.resources&IngestorResource != 0 || cfg.resources&ProcessorResource != 0 {
		cfg.resources |= PostgresResource | NATSResource
	}

	// Check if environment is already provisioned (e.g. by TestMain in docker-compose mode)
	// when FORCE_TESTCONTAINERS is not set.
	forceTC := os.Getenv("FORCE_TESTCONTAINERS") != ""
	existingPGHost := ""
	existingNATSURL := ""
	existingRedisAddr := ""
	if !forceTC {
		existingPGHost = os.Getenv("POSTGRES_HOST")
		existingNATSURL = os.Getenv("NATS_URL")
		existingRedisAddr = os.Getenv("REDIS_ADDR")
	}

	// 1. Provision Redis if requested
	if cfg.resources&RedisResource != 0 {
		if existingRedisAddr != "" {
			env.RedisConfig = RedisConfig{Addr: existingRedisAddr}
		} else {
			redisContainer, err := StartRedis(ctx)
			if err != nil {
				t.Fatalf("Setup: failed to start Redis: %v", err)
			}
			env.RedisContainer = redisContainer
			env.RedisConfig = RedisConfig{
				Addr:     redisContainer.Addr(),
				Password: "",
				DB:       0,
			}
			os.Setenv("REDIS_ADDR", env.RedisConfig.Addr)
			t.Cleanup(func() {
				cleanupCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
				defer c()
				_ = redisContainer.Terminate(cleanupCtx)
			})
		}
	}

	// 2. Provision Postgres if requested
	if cfg.resources&PostgresResource != 0 {
		if existingPGHost != "" {
			env.PGConfig = PostgreSQLConfig{
				Host:     existingPGHost,
				Port:     os.Getenv("POSTGRES_PORT"),
				User:     os.Getenv("POSTGRES_USER"),
				Password: os.Getenv("POSTGRES_PASSWORD"),
				DB:       os.Getenv("POSTGRES_DB"),
			}
		} else {
			pgContainer, err := StartPostgreSQL(ctx)
			if err != nil {
				t.Fatalf("Setup: failed to start PostgreSQL: %v", err)
			}
			env.PostgresContainer = pgContainer
			env.PGConfig = PostgreSQLConfig{
				Host:     pgContainer.HostIP,
				Port:     pgContainer.HostPort,
				User:     DefaultUsername,
				Password: DefaultPassword,
				DB:       DefaultDatabaseName,
			}

			os.Setenv("POSTGRES_HOST", env.PGConfig.Host)
			os.Setenv("POSTGRES_PORT", env.PGConfig.Port)
			os.Setenv("POSTGRES_USER", env.PGConfig.User)
			os.Setenv("POSTGRES_PASSWORD", env.PGConfig.Password)
			os.Setenv("POSTGRES_DB", env.PGConfig.DB)

			t.Cleanup(func() {
				cleanupCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
				defer c()
				_ = pgContainer.Terminate(cleanupCtx)
			})

			// Run DB migrations if requested
			if cfg.migrated {
				if err := runInitSQLMigrations(ctx, env.PGConfig); err != nil {
					t.Fatalf("Setup: failed to run database migrations: %v", err)
				}
			}
		}

		// Connect a helper pgxpool
		dsn := fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable",
			env.PGConfig.User, env.PGConfig.Password, env.PGConfig.Host, env.PGConfig.Port, env.PGConfig.DB)
		pool, err := pgxpool.New(ctx, dsn)
		if err == nil {
			env.PGPool = pool
			t.Cleanup(func() { pool.Close() })
		}
	}

	// 3. Provision NATS if requested
	if cfg.resources&NATSResource != 0 {
		if existingNATSURL != "" {
			env.NATSConfig = NATSConfig{URL: existingNATSURL}
		} else {
			natsContainer, err := StartNATS(ctx)
			if err != nil {
				t.Fatalf("Setup: failed to start NATS: %v", err)
			}
			env.NATSContainer = natsContainer
			env.NATSConfig = NATSConfig{
				URL: natsContainer.URL(),
			}
			os.Setenv("NATS_URL", env.NATSConfig.URL)

			t.Cleanup(func() {
				cleanupCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
				defer c()
				_ = natsContainer.Terminate(cleanupCtx)
			})

			// Connect NATS client helper and init JetStream
			nc, err := nats.Connect(env.NATSConfig.URL)
			if err == nil {
				env.NATSConn = nc
				t.Cleanup(func() { nc.Close() })

				_ = setupJetStreamStream(nc)
			}
		}
	}

	// 4. Provision Ingestor if requested
	if cfg.resources&IngestorResource != 0 {
		ingestorContainer, err := StartIngestor(ctx,
			env.PGConfig.Host, env.PGConfig.Port,
			env.PGConfig.User, env.PGConfig.Password, env.PGConfig.DB,
			env.NATSConfig.URL,
		)
		if err != nil {
			t.Fatalf("Setup: failed to start Ingestor: %v", err)
		}
		env.IngestorContainer = ingestorContainer
		env.IngestorURL = ingestorContainer.URL()
		os.Setenv("INGESTOR_URL", env.IngestorURL)

		if ingestorContainer.Container != nil {
			t.Cleanup(func() {
				cleanupCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
				defer c()
				_ = ingestorContainer.Terminate(cleanupCtx)
			})
		}
	}

	// 5. Provision Processor if requested
	if cfg.resources&ProcessorResource != 0 {
		processorContainer, err := StartProcessor(ctx,
			env.PGConfig.Host, env.PGConfig.Port,
			env.PGConfig.User, env.PGConfig.Password, env.PGConfig.DB,
			env.NATSConfig.URL,
		)
		if err != nil {
			t.Logf("Setup: processor container not started: %v (continuing)", err)
		} else if processorContainer != nil && processorContainer.Container != nil {
			env.ProcessorContainer = processorContainer
			t.Cleanup(func() {
				cleanupCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
				defer c()
				_ = processorContainer.Terminate(cleanupCtx)
			})
		}
	}

	return env
}

func runInitSQLMigrations(ctx context.Context, cfg PostgreSQLConfig) error {
	dsn := fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable",
		cfg.User, cfg.Password, cfg.Host, cfg.Port, cfg.DB)

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("failed to create pool: %w", err)
	}
	defer pool.Close()

	projectRoot := findProjectRoot()

	// Apply the REAL migration files, in order, rather than scripts/db/init.sql.
	//
	// init.sql is a third, hand-maintained copy of the schema frozen at the 1716508800 baseline: it
	// has no organizations, no organization_members, no project_api_keys, and still carries the
	// pre-S12 `CHECK (status IN ('open','resolved','ignored'))` on issues. Bootstrapping test
	// containers from it meant integration tests ran against a schema that had never seen features
	// 005 or 008 — so every test touching organizations or API-key auth failed with
	// `relation "organizations" does not exist` or an unexplained 401, and the S12 constraint fix
	// was invisible to them. See ARCHITECTURE.md A1's 2026-07-29 note, which recommends exactly this.
	//
	// goose directives are stripped rather than interpreted: this is a one-shot bootstrap of a
	// disposable container, so only the Up sections are needed and no version ledger is required.
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
	sort.Strings(files) // filenames are timestamp-prefixed, so lexical order is apply order

	for _, name := range files {
		raw, err := os.ReadFile(filepath.Join(migDir, name))
		if err != nil {
			return fmt.Errorf("failed to read migration %s: %w", name, err)
		}
		up := extractGooseUp(string(raw))
		if strings.TrimSpace(up) == "" {
			continue
		}
		if _, err := pool.Exec(ctx, up); err != nil {
			return fmt.Errorf("failed to apply migration %s: %w", name, err)
		}
	}
	return nil
}

// extractGooseUp returns only the `-- +goose Up` portion of a goose migration, stopping at
// `-- +goose Down`, and drops the StatementBegin/StatementEnd markers (which are goose parser
// directives, not SQL).
func extractGooseUp(content string) string {
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

func setupJetStreamStream(nc *nats.Conn) error {
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
