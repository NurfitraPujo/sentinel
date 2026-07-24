package testcontainers

import (
	"context"
	"fmt"
	"os"
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
	initSQLPath := projectRoot + "/scripts/db/init.sql"

	sqlBytes, err := os.ReadFile(initSQLPath)
	if err != nil {
		return fmt.Errorf("failed to read init.sql: %w", err)
	}

	_, err = pool.Exec(ctx, string(sqlBytes))
	if err != nil {
		return fmt.Errorf("failed to execute init.sql: %w", err)
	}
	return nil
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
