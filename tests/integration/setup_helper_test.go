package integration

import (
	"testing"
	"time"

	tc "github.com/NurfitraPujo/sentinel/tests/integration/testcontainers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
