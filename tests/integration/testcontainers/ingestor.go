package testcontainers

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/testcontainers/testcontainers-go"
)

// IngestorContainer wraps a testcontainers.Container for the ingestor service.
// If the container fails to start properly, it falls back to localhost:8080
// assuming docker-compose or external services are running.
type IngestorContainer struct {
	testcontainers.Container
	HostIP   string
	HostPort string
}

// StartIngestor starts the ingestor-go container with the given
// PostgreSQL and NATS connection details. Uses a pre-built image.
// Falls back to localhost:8080 if container fails (for running with docker-compose).
func StartIngestor(ctx context.Context, pgHost, pgPort, pgUser, pgPassword, pgDB, natsURL string) (*IngestorContainer, error) {
	provider := ConfigureProvider()

	// Use the actual postgres host IP instead of localhost to ensure container can reach it
	// When running in podman, we need to use the podman machine's IP or hostname
	fmt.Printf("Starting ingestor with pgHost=%s, pgPort=%s, natsURL=%s\n", pgHost, pgPort, natsURL)

	req := testcontainers.GenericContainerRequest{
		ProviderType: provider,
		ContainerRequest: testcontainers.ContainerRequest{
			Image: "localhost/sentinel_ingestor:latest",
			Env: map[string]string{
				"POSTGRES_HOST":     pgHost,
				"POSTGRES_PORT":     pgPort,
				"POSTGRES_USER":     pgUser,
				"POSTGRES_PASSWORD": pgPassword,
				"POSTGRES_DB":       pgDB,
				"NATS_URL":          natsURL,
			},
			ExposedPorts: []string{"8080/tcp"},
			// Use host network mode so container can reach host ports
			NetworkMode: "host",
			AutoRemove:  false,
		},
	}

	// Start container without waiting
	fmt.Println("Starting ingestor container (no wait strategy)...")
	container, err := testcontainers.GenericContainer(ctx, req)
	if err != nil {
		fmt.Printf("Error: Failed to create container: %v\n", err)
		fmt.Printf("To run full integration tests, ensure 'docker compose up -d' is running\n")
		return &IngestorContainer{
			HostIP:   "localhost",
			HostPort: "8080",
		}, nil
	}

	// Manually start the container - GenericContainer doesn't auto-start
	fmt.Println("Manually starting container...")
	err = container.Start(ctx)
	if err != nil {
		fmt.Printf("Error: Failed to start container: %v\n", err)
		return &IngestorContainer{
			HostIP:   "localhost",
			HostPort: "8080",
		}, nil
	}
	fmt.Println("Container start called successfully")

	fmt.Printf("Container created successfully, container type: %T\n", container)

	// Give the container time to start and initialize
	fmt.Println("Waiting 10 seconds for ingestor to start...")
	time.Sleep(10 * time.Second)

	// Check if container is actually running by trying to fetch logs
	logsReader, logsErr := container.Logs(ctx)
	if logsErr != nil {
		fmt.Printf("Error fetching logs: %v\n", logsErr)
	} else {
		defer logsReader.Close()
		logsBytes, readErr := io.ReadAll(logsReader)
		if readErr == nil {
			fmt.Printf("Ingestor container logs:\n%s\n", string(logsBytes))
		}
	}

	// Try to determine if container is truly running
	// Check via container network inspect or just assume it's running if we got this far
	state, stateErr := container.State(ctx)
	fmt.Printf("Container state: %+v, err=%v\n", state, stateErr)

	// Container may be running but testcontainers.State() may not reflect that correctly
	// Let's try to access the health endpoint to verify
	if stateErr == nil && state.Running {
		// Already running according to state - good
	} else if stateErr != nil || !state.Running {
		// State says not running but container might actually be running
		// This can happen with host network mode on podman
		// Let's verify by checking if we can access the health endpoint
		fmt.Println("Container state reports not running, verifying with health check...")

		// Try to do a simple connectivity check - if using host network mode and port 8080 is bound, container is running
		client := &http.Client{Timeout: 2 * time.Second}
		resp, err := client.Get("http://localhost:8080/health")
		if err == nil {
			resp.Body.Close()
			fmt.Println("Health check passed - container is actually running")
		} else {
			fmt.Printf("Health check failed: %v\n", err)
			if container != nil {
				container.Terminate(ctx)
			}
			fmt.Printf("Warning: Ingestor container not running, using fallback localhost:8080\n")
			return &IngestorContainer{
				HostIP:   "localhost",
				HostPort: "8080",
			}, nil
		}
	}

	hostIP := "localhost" // For host network mode, always use localhost
	hostPort := "8080"    // For host network mode, always use the exposed port

	return &IngestorContainer{
		Container: container,
		HostIP:    hostIP,
		HostPort:  hostPort,
	}, nil
}

// URL returns the HTTP URL for the ingestor service
func (c *IngestorContainer) URL() string {
	return fmt.Sprintf("http://%s:%s", c.HostIP, c.HostPort)
}
