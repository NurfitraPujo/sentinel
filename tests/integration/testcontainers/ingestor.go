package testcontainers

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/testcontainers/testcontainers-go"
)

// IngestorContainer wraps a testcontainers.Container for the ingestor service.
//
// It is always backed by a real, freshly-started container bound to a private
// port chosen for this run (see freePort below). There is no fallback to a
// well-known host port: doing that used to let this type silently hand back
// {HostIP: "localhost", HostPort: "8080"} with a NIL error whenever the
// container failed to start, which on a machine where docker-compose also
// owns 8080 meant the test suite unknowingly drove the COMPOSE ingestor —
// pointed at the shared dev database — while the test had seeded its
// project into the testcontainer database. That produced "Expected status
// 202, got 401" failures that looked like an auth bug but were actually a
// misrouted request. See docs/memory or CLAUDE.md for the full writeup.
type IngestorContainer struct {
	testcontainers.Container
	HostIP   string
	HostPort string
}

// freePort asks the OS for a free TCP port by binding 127.0.0.1:0, reading
// back the assigned port, and closing the listener immediately. There is a
// small, inherent TOCTOU race: another process could claim the same port
// between our Close() and the container's bind(). That window is tiny
// compared to hardcoding 8080, which the compose stack owns unconditionally
// on any developer machine that has `docker compose up` running — so this is
// strictly better despite not being perfectly race-free.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("failed to find a free port: %w", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// choosePort returns TEST_INGESTOR_PORT if set (an explicit override, e.g.
// for a developer who wants a stable port to attach a debugger to), and
// otherwise asks the OS for one via freePort.
func choosePort() (int, error) {
	if v := os.Getenv("TEST_INGESTOR_PORT"); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil {
			return 0, fmt.Errorf("TEST_INGESTOR_PORT=%q is not a valid port number: %w", v, err)
		}
		return p, nil
	}
	return freePort()
}

// waitForHealth polls url until it answers with a 2xx/3xx-ish response (any
// response at all is enough — we only need proof OUR process is listening,
// not that it reports healthy) or the timeout expires. On timeout it returns
// an error; the caller is expected to attach container logs for diagnosis.
func waitForHealth(ctx context.Context, url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 2 * time.Second}
	var lastErr error
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err == nil {
			resp, err := client.Do(req)
			if err == nil {
				resp.Body.Close()
				return nil
			}
			lastErr = err
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
	return fmt.Errorf("timed out after %s waiting for %s to answer: %w", timeout, url, lastErr)
}

// fetchLogs returns the container's stdout/stderr, or a placeholder string
// noting why it couldn't be fetched. Used to make provisioning failures
// actionable instead of a bare error.
func fetchLogs(ctx context.Context, container testcontainers.Container) string {
	logsReader, err := container.Logs(ctx)
	if err != nil {
		return fmt.Sprintf("(failed to fetch container logs: %v)", err)
	}
	defer logsReader.Close()
	logsBytes, err := io.ReadAll(logsReader)
	if err != nil {
		return fmt.Sprintf("(failed to read container logs: %v)", err)
	}
	return string(logsBytes)
}

// StartIngestor starts the ingestor-go container with the given PostgreSQL,
// NATS and Redis connection details, using a pre-built image and host
// network mode (so it can reach the postgres/NATS/redis testcontainers via
// their host-mapped ports on localhost).
//
// It binds the ingestor to a private port chosen for this run (see
// choosePort) rather than the well-known 8080, since a developer machine
// running `docker compose up` already owns 8080 — a hardcoded HostPort there
// means the container fails to bind, and any fallback that shrugged that off
// and returned localhost:8080 anyway would silently hand the test suite the
// COMPOSE ingestor instead of this one. There is no such fallback: every
// error path below returns (nil, error) so a genuine provisioning failure is
// loud, per setup_test.go's os.Exit(1) on error.
//
// redisAddr must point at the isolated redis testcontainer, not left empty.
// apps/ingestor-go/main.go defaults REDIS_ADDR to "localhost:6379" when
// unset, and under NetworkMode "host" that default resolves to whatever is
// bound to the HOST's port 6379 — the docker-compose redis on a developer
// machine that has the stack up. That is the same class of bug the port
// fix above addresses: caching API-key lookups (apps/ingestor-go/auth/apikey.go,
// defaultCacheTTL) in a Redis shared with other runs let a stale, previous
// test's project id leak into this run within the TTL window, producing
// "project not found" failures that have nothing to do with auth.
func StartIngestor(ctx context.Context, pgHost, pgPort, pgUser, pgPassword, pgDB, natsURL, redisAddr string) (*IngestorContainer, error) {
	provider := ConfigureProvider()

	if redisAddr == "" {
		return nil, fmt.Errorf("StartIngestor: redisAddr must not be empty — it must point at the isolated redis testcontainer, or the ingestor falls back to localhost:6379 under host networking, which is the docker-compose redis on a machine that has the stack up")
	}

	port, err := choosePort()
	if err != nil {
		return nil, fmt.Errorf("could not choose a port for the ingestor container: %w", err)
	}
	portStr := strconv.Itoa(port)

	fmt.Printf("Starting ingestor with pgHost=%s, pgPort=%s, natsURL=%s, redisAddr=%s, port=%s\n", pgHost, pgPort, natsURL, redisAddr, portStr)

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
				// Must be the isolated redis testcontainer's address — see the
				// redisAddr doc above for why this cannot be left to default.
				"REDIS_ADDR": redisAddr,
				// PORT exists precisely so a second, standalone ingestor can run
				// alongside the compose one without a port clash — see
				// apps/ingestor-go/main.go's getEnv("PORT", "8080") handling.
				"PORT": portStr,
			},
			ExposedPorts: []string{portStr + "/tcp"},
			// Use host network mode so container can reach host ports.
			NetworkMode: "host",
			AutoRemove:  false,
		},
	}

	fmt.Println("Starting ingestor container (no wait strategy)...")
	container, err := testcontainers.GenericContainer(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to create ingestor container (port=%s): %w. Ensure 'docker compose up -d --build' has produced the localhost/sentinel_ingestor:latest image", portStr, err)
	}

	fmt.Println("Manually starting container...")
	if err := container.Start(ctx); err != nil {
		return nil, fmt.Errorf("failed to start ingestor container (port=%s): %w\ncontainer logs:\n%s", portStr, err, fetchLogs(ctx, container))
	}
	fmt.Println("Container start called successfully")

	hostIP := "localhost" // For host network mode, always use localhost.
	healthURL := fmt.Sprintf("http://%s:%s/health", hostIP, portStr)

	// Poll OUR container's health endpoint on OUR private port until it
	// answers or the timeout expires. Because the port was chosen for this
	// run, a response here proves it came from the container we just
	// started, not from some other process that happens to be listening
	// on a well-known port (the trap this whole file used to fall into on
	// 8080, and the same class of trap tests/e2e/main_test.go's NATS
	// preflight guards against for "sentinel-nats" on 4222).
	if err := waitForHealth(ctx, healthURL, 30*time.Second); err != nil {
		state, stateErr := container.State(ctx)
		logs := fetchLogs(ctx, container)
		container.Terminate(ctx)
		return nil, fmt.Errorf(
			"ingestor container on port %s never became healthy: %w (container state=%+v, stateErr=%v)\ncontainer logs:\n%s",
			portStr, err, state, stateErr, logs,
		)
	}
	fmt.Printf("Ingestor container health check passed on %s\n", healthURL)

	return &IngestorContainer{
		Container: container,
		HostIP:    hostIP,
		HostPort:  portStr,
	}, nil
}

// URL returns the HTTP URL for the ingestor service.
func (c *IngestorContainer) URL() string {
	return fmt.Sprintf("http://%s:%s", c.HostIP, c.HostPort)
}
