package testcontainers

import (
	"context"
	"fmt"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	// RedisImage is the default Redis image used for integration tests.
	RedisImage = "redis:7-alpine"
	// RedisPort is the port Redis listens on inside the container.
	RedisPort = "6379/tcp"
)

type RedisContainer struct {
	*tcredis.RedisContainer
	HostIP   string
	HostPort string
}

// StartRedis starts a Redis testcontainer and returns a wrapper that exposes
// the host/port for client construction. It mirrors the PostgreSQL helper
// pattern so that integration tests can use a real Redis service.
func StartRedis(ctx context.Context) (*RedisContainer, error) {
	provider := ConfigureProvider()

	container, err := tcredis.Run(ctx,
		RedisImage,
		testcontainers.WithWaitStrategy(
			wait.ForListeningPort(RedisPort).WithStartupTimeout(30*time.Second),
			wait.ForLog("* Ready to accept connections"),
		),
		testcontainers.CustomizeRequest(
			testcontainers.GenericContainerRequest{
				ProviderType: provider,
			},
		),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to start redis: %w", err)
	}

	hostIP, err := container.Host(ctx)
	if err != nil {
		_ = container.Terminate(ctx)
		return nil, fmt.Errorf("failed to get host: %w", err)
	}

	hostPort, err := container.MappedPort(ctx, RedisPort)
	if err != nil {
		_ = container.Terminate(ctx)
		return nil, fmt.Errorf("failed to get mapped port: %w", err)
	}

	return &RedisContainer{
		RedisContainer: container,
		HostIP:         hostIP,
		HostPort:       hostPort.Port(),
	}, nil
}

// Addr returns the host:port address suitable for a Redis client.
func (c *RedisContainer) Addr() string {
	return fmt.Sprintf("%s:%s", c.HostIP, c.HostPort)
}
