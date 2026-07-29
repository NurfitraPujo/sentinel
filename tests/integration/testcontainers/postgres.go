package testcontainers

import (
	"context"
	"fmt"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	DefaultDatabaseName = "sentinel"
	DefaultUsername     = "sentinel"
	DefaultPassword     = "changeme"
)

type PostgreSQLContainer struct {
	*postgres.PostgresContainer
	HostIP   string
	HostPort string
}

func StartPostgreSQL(ctx context.Context) (*PostgreSQLContainer, error) {
	provider := ConfigureProvider()

	container, err := postgres.Run(ctx,
		"postgres:15-alpine",
		postgres.WithDatabase(DefaultDatabaseName),
		postgres.WithUsername(DefaultUsername),
		postgres.WithPassword(DefaultPassword),
		// The official postgres:15-alpine image logs "database system is ready to accept
		// connections" TWICE on a fresh container: once for the short-lived initdb bootstrap
		// instance, and once for the real server that stays up. WithOccurrence(1) (the
		// previous configuration here) matched the FIRST occurrence and returned control while
		// the bootstrap instance was mid-shutdown/mid-restart, so a connection made immediately
		// after "container is ready" could hit "FATAL: the database system is starting up"
		// (SQLSTATE 57P03) — reproduced repeatedly running the suite with
		// FORCE_TESTCONTAINERS=1 (every test that provisions its own Postgres via tc.Setup hits
		// this independently). procgo_alerting_degradation_test.go's procgoSetupPostgres already
		// worked around this locally with a retry loop; fixing the wait strategy here closes it
		// at the source for every caller instead of requiring each one to remember the retry.
		testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").WithOccurrence(2)),
		testcontainers.CustomizeRequest(
			testcontainers.GenericContainerRequest{
				ProviderType: provider,
			},
		),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to start postgres: %w", err)
	}

	hostIP, err := container.Host(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get host: %w", err)
	}

	hostPort, err := container.MappedPort(ctx, "5432/tcp")
	if err != nil {
		return nil, fmt.Errorf("failed to get mapped port: %w", err)
	}

	return &PostgreSQLContainer{
		PostgresContainer: container,
		HostIP:            hostIP,
		HostPort:          hostPort.Port(),
	}, nil
}

func (c *PostgreSQLContainer) DSN() string {
	return fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		c.HostIP, c.HostPort, DefaultUsername, DefaultPassword, DefaultDatabaseName)
}
