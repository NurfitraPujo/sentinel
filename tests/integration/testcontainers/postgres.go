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
		testcontainers.WithWaitStrategy(wait.ForLog("database system is ready")),
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
