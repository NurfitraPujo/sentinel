package testcontainers

import (
	"context"
	"fmt"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/nats"
	"github.com/testcontainers/testcontainers-go/wait"
)

type NATSContainer struct {
	*nats.NATSContainer
	HostIP   string
	HostPort string
}

func StartNATS(ctx context.Context) (*NATSContainer, error) {
	provider := ConfigureProvider()

	container, err := nats.Run(ctx,
		"nats:2.10-alpine",
		testcontainers.WithWaitStrategy(wait.ForListeningPort("4222").WithStartupTimeout(30*time.Second)),
		testcontainers.CustomizeRequest(
			testcontainers.GenericContainerRequest{
				ProviderType: provider,
				ContainerRequest: testcontainers.ContainerRequest{
					Cmd: []string{"-js"},
				},
			},
		),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to start nats: %w", err)
	}

	hostIP, err := container.Host(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get host: %w", err)
	}

	hostPort, err := container.MappedPort(ctx, "4222/tcp")
	if err != nil {
		return nil, fmt.Errorf("failed to get mapped port: %w", err)
	}

	return &NATSContainer{
		NATSContainer: container,
		HostIP:        hostIP,
		HostPort:      hostPort.Port(),
	}, nil
}

func (c *NATSContainer) URL() string {
	return fmt.Sprintf("nats://%s:%s", c.HostIP, c.HostPort)
}
