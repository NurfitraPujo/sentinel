package unit

import (
	"strconv"
	"strings"
	"testing"
	"time"

	sharedredis "github.com/NurfitraPujo/sentinel/packages/shared-go/redis"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetWindowKey(t *testing.T) {
	got := sharedredis.GetWindowKey("ratelimit", "abc", time.Second)
	parts := strings.Split(got, ":")

	require.Len(t, parts, 3)
	assert.Equal(t, "ratelimit", parts[0])
	assert.Equal(t, "abc", parts[1])
	_, err := strconv.ParseInt(parts[2], 10, 64)
	assert.NoError(t, err)
}

func TestGetWindowKey_CallsWithinWindowShareBucket(t *testing.T) {
	first := sharedredis.GetWindowKey("ratelimit", "abc", time.Hour)
	time.Sleep(1 * time.Millisecond)
	second := sharedredis.GetWindowKey("ratelimit", "abc", time.Hour)

	assert.Equal(t, first, second)
}

func TestGetWindowKey_DifferentPrefixesProduceDifferentKeys(t *testing.T) {
	first := sharedredis.GetWindowKey("ratelimit", "abc", time.Second)
	second := sharedredis.GetWindowKey("quota", "abc", time.Second)

	assert.NotEqual(t, first, second)
	assert.Equal(t, "ratelimit", strings.Split(first, ":")[0])
	assert.Equal(t, "quota", strings.Split(second, ":")[0])
}

func TestGetWindowKey_DifferentKeysProduceDifferentKeys(t *testing.T) {
	first := sharedredis.GetWindowKey("ratelimit", "abc", time.Second)
	second := sharedredis.GetWindowKey("ratelimit", "xyz", time.Second)

	assert.NotEqual(t, first, second)
	assert.Equal(t, "abc", strings.Split(first, ":")[1])
	assert.Equal(t, "xyz", strings.Split(second, ":")[1])
}

func TestGetWindowKey_DifferentWindowsProduceDifferentBuckets(t *testing.T) {
	first := sharedredis.GetWindowKey("ratelimit", "abc", time.Nanosecond)
	second := sharedredis.GetWindowKey("ratelimit", "abc", time.Hour)

	firstParts := strings.Split(first, ":")
	secondParts := strings.Split(second, ":")
	require.Len(t, firstParts, 3)
	require.Len(t, secondParts, 3)
	assert.NotEqual(t, firstParts[2], secondParts[2])
}
