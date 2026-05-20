package testcontainers

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/testcontainers/testcontainers-go"
)

// ProcessorContainer wraps a testcontainers.Container for the processor service.
type ProcessorContainer struct {
	testcontainers.Container
}

// StartProcessor starts the processor-go container with the given
// PostgreSQL and NATS connection details. Uses a pre-built image.
func StartProcessor(ctx context.Context, pgHost, pgPort, pgUser, pgPassword, pgDB, natsURL string) (*ProcessorContainer, error) {
	provider := ConfigureProvider()

	fmt.Printf("Starting processor with pgHost=%s, pgPort=%s, natsURL=%s\n", pgHost, pgPort, natsURL)

	req := testcontainers.GenericContainerRequest{
		ProviderType: provider,
		ContainerRequest: testcontainers.ContainerRequest{
			Image: "localhost/sentinel_processor:latest",
			Env: map[string]string{
				"POSTGRES_HOST":     pgHost,
				"POSTGRES_PORT":     pgPort,
				"POSTGRES_USER":     pgUser,
				"POSTGRES_PASSWORD": pgPassword,
				"POSTGRES_DB":       pgDB,
				"NATS_URL":          natsURL,
			},
			// Use host network mode so container can reach host ports
			NetworkMode: "host",
			AutoRemove:  false,
		},
	}

	fmt.Println("Starting processor container...")
	container, err := testcontainers.GenericContainer(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to create processor container: %w", err)
	}

	err = container.Start(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to start processor container: %w", err)
	}

	fmt.Println("Processor container started")

	// Give it some time to initialize
	time.Sleep(5 * time.Second)

	// Fetch logs for debugging
	logsReader, logsErr := container.Logs(ctx)
	if logsErr == nil {
		defer logsReader.Close()
		logsBytes, readErr := io.ReadAll(logsReader)
		if readErr == nil {
			fmt.Printf("Processor container logs:\n%s\n", string(logsBytes))
		}
	}

	return &ProcessorContainer{
		Container: container,
	}, nil
}
