package integration

import (
	"context"
	"fmt"
	"net/http"
	"os"
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

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Check if docker-compose services are available (for hybrid approach)
	ingestorAvailable := checkService("http://localhost:8080/health")

	if ingestorAvailable {
		fmt.Println("Using docker-compose services at localhost")
		// Use docker-compose services - assume standard ports and database is initialized
		os.Setenv("INGESTOR_URL", "http://localhost:8080")
		os.Setenv("POSTGRES_HOST", "localhost")
		os.Setenv("POSTGRES_PORT", "5432")
		os.Setenv("POSTGRES_USER", "sentinel")
		os.Setenv("POSTGRES_PASSWORD", "changeme")
		os.Setenv("POSTGRES_DB", "sentinel")
		os.Setenv("NATS_URL", "nats://localhost:4222")

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

	// Try to start ingestor
	ingestorContainer, err := tc.StartIngestor(ctx,
		testConfig.Host, testConfig.Port,
		testConfig.User, testConfig.Password, testConfig.DB,
		natsConfig.URL,
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

	// Tail ingestor logs
	ingestorContainer.FollowOutput(&logConsumer{prefix: "INGESTOR"})
	ingestorContainer.StartLogProducer(ctx)

	// Start processor
	processorContainer, err := tc.StartProcessor(ctx,
		testConfig.Host, testConfig.Port,
		testConfig.User, testConfig.Password, testConfig.DB,
		natsConfig.URL,
	)
	if err != nil {
		fmt.Printf("Failed to start processor: %v\n", err)
		os.Exit(1)
	}
	defer processorContainer.Terminate(ctx)

	// Tail processor logs
	processorContainer.FollowOutput(&logConsumer{prefix: "PROCESSOR"})
	processorContainer.StartLogProducer(ctx)

	os.Setenv("POSTGRES_HOST", testConfig.Host)
	os.Setenv("POSTGRES_PORT", testConfig.Port)
	os.Setenv("POSTGRES_USER", testConfig.User)
	os.Setenv("POSTGRES_PASSWORD", testConfig.Password)
	os.Setenv("POSTGRES_DB", testConfig.DB)
	os.Setenv("NATS_URL", natsConfig.URL)
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

	// Find project root by looking for go.mod marker
	projectRoot := findProjectRoot()
	initSQLPath := projectRoot + "/scripts/db/init.sql"

	sqlBytes, err := os.ReadFile(initSQLPath)
	if err != nil {
		return fmt.Errorf("failed to read init.sql: %w", err)
	}

	_, err = pool.Exec(ctx, string(sqlBytes))
	if err != nil {
		return fmt.Errorf("failed to execute migrations: %w", err)
	}

	return nil
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
	return testConfig, natsConfig
}
