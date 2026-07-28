package integration

import (
	"os"
	"testing"
	"time"

	tc "github.com/NurfitraPujo/sentinel/tests/integration/testcontainers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// requireInfra fails the test when required infrastructure is unavailable and
// SENTINEL_E2E=1 (CI mode), and skips gracefully otherwise (local/laptop mode).
//
// This exists because a bare t.Skipf when infra is unreachable is correct for
// a laptop and catastrophic for CI: a green run proves nothing if every
// assertion was silently skipped. See docs/plans/E2E_RECOVERY_PLAN.md P0-4
// and constraint C5 (this is the direct reason S3 — 100% of /ingest returning
// 400 — survived to main undetected).
func requireInfra(t *testing.T, err error, what string) {
	if err == nil {
		return
	}
	if os.Getenv("SENTINEL_E2E") == "1" {
		t.Fatalf("%s unavailable and SENTINEL_E2E=1: %v", what, err)
	}
	t.Skipf("skipping: %s unavailable: %v", what, err)
}

func TestSetupHelperGranularResources(t *testing.T) {
	// Provision only Postgres and Redis for this specific test
	env := tc.Setup(t,
		tc.WithResources(tc.PostgresResource|tc.RedisResource),
		tc.WithMigrations(true),
		tc.WithTimeout(2*time.Minute),
	)

	require.NotEmpty(t, env.PGConfig.Host, "Postgres host should be configured")
	require.NotEmpty(t, env.RedisConfig.Addr, "Redis address should be configured")
	assert.Nil(t, env.NATSContainer, "NATS should not be provisioned when not requested")
	assert.Nil(t, env.IngestorContainer, "Ingestor should not be provisioned when not requested")

	// Verify database connection pool works
	require.NotNil(t, env.PGPool)
	err := env.PGPool.Ping(t.Context())
	assert.NoError(t, err, "Postgres pool ping should succeed")

	// Verify Redis address is set
	assert.NotEmpty(t, env.RedisConfig.Addr)
}
