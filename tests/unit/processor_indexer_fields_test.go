package unit

import (
	"testing"

	"github.com/NurfitraPujo/sentinel/apps/processor-go/indexer"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// ExtractSearchFields tests
// ---------------------------------------------------------------------------

func TestExtractSearchFields_EmptyMetadataReturnsAllEmptyEntry(t *testing.T) {
	entry := indexer.ExtractSearchFields(map[string]interface{}{})

	require.NotNil(t, entry)
	assert.Equal(t, "", entry.UserID)
	assert.Equal(t, "", entry.TenantID)
	assert.Equal(t, "", entry.TraceID)
	assert.Equal(t, "", entry.SpanID)
	assert.Equal(t, "", entry.RequestID)
}

func TestExtractSearchFields_NilMetadataReturnsAllEmptyEntry(t *testing.T) {
	entry := indexer.ExtractSearchFields(nil)

	require.NotNil(t, entry)
	assert.Equal(t, "", entry.UserID)
	assert.Equal(t, "", entry.TenantID)
	assert.Equal(t, "", entry.TraceID)
	assert.Equal(t, "", entry.SpanID)
	assert.Equal(t, "", entry.RequestID)
}

// ---------------------------------------------------------------------------
// UserID alias tests
// ---------------------------------------------------------------------------

func TestExtractSearchFields_UserID(t *testing.T) {
	t.Run("snake_case user_id is picked up", func(t *testing.T) {
		entry := indexer.ExtractSearchFields(map[string]interface{}{
			"user_id": "u-snake",
		})
		assert.Equal(t, "u-snake", entry.UserID)
	})

	t.Run("camelCase userId is picked up when user_id is absent", func(t *testing.T) {
		entry := indexer.ExtractSearchFields(map[string]interface{}{
			"userId": "u-camel",
		})
		assert.Equal(t, "u-camel", entry.UserID)
	})

	t.Run("plain user is picked up when neither user_id nor userId is present", func(t *testing.T) {
		entry := indexer.ExtractSearchFields(map[string]interface{}{
			"user": "u-plain",
		})
		assert.Equal(t, "u-plain", entry.UserID)
	})

	t.Run("user_id wins over userId when both are present", func(t *testing.T) {
		entry := indexer.ExtractSearchFields(map[string]interface{}{
			"user_id": "u-snake",
			"userId":  "u-camel",
		})
		assert.Equal(t, "u-snake", entry.UserID)
	})

	t.Run("user_id wins over user when both are present", func(t *testing.T) {
		entry := indexer.ExtractSearchFields(map[string]interface{}{
			"user_id": "u-snake",
			"user":    "u-plain",
		})
		assert.Equal(t, "u-snake", entry.UserID)
	})

	t.Run("userId wins over user when both are present and user_id is absent", func(t *testing.T) {
		entry := indexer.ExtractSearchFields(map[string]interface{}{
			"userId": "u-camel",
			"user":   "u-plain",
		})
		assert.Equal(t, "u-camel", entry.UserID)
	})

	t.Run("no user-related key leaves UserID empty", func(t *testing.T) {
		entry := indexer.ExtractSearchFields(map[string]interface{}{
			"something_else": "value",
		})
		assert.Equal(t, "", entry.UserID)
	})

	t.Run("non-string user_id is ignored", func(t *testing.T) {
		entry := indexer.ExtractSearchFields(map[string]interface{}{
			"user_id": 12345,
		})
		assert.Equal(t, "", entry.UserID)
	})

	t.Run("non-string userId is ignored", func(t *testing.T) {
		entry := indexer.ExtractSearchFields(map[string]interface{}{
			"userId": true,
		})
		assert.Equal(t, "", entry.UserID)
	})

	t.Run("non-string user is ignored", func(t *testing.T) {
		entry := indexer.ExtractSearchFields(map[string]interface{}{
			"user": []string{"a", "b"},
		})
		assert.Equal(t, "", entry.UserID)
	})

	t.Run("non-string user_id falls through to userId", func(t *testing.T) {
		entry := indexer.ExtractSearchFields(map[string]interface{}{
			"user_id": 42,
			"userId":  "u-camel",
		})
		assert.Equal(t, "u-camel", entry.UserID)
	})

	t.Run("non-string user_id and userId fall through to user", func(t *testing.T) {
		entry := indexer.ExtractSearchFields(map[string]interface{}{
			"user_id": 42,
			"userId":  nil,
			"user":    "u-plain",
		})
		assert.Equal(t, "u-plain", entry.UserID)
	})
}

// ---------------------------------------------------------------------------
// TenantID alias tests
// ---------------------------------------------------------------------------

func TestExtractSearchFields_TenantID(t *testing.T) {
	t.Run("snake_case tenant_id is picked up", func(t *testing.T) {
		entry := indexer.ExtractSearchFields(map[string]interface{}{
			"tenant_id": "t-snake",
		})
		assert.Equal(t, "t-snake", entry.TenantID)
	})

	t.Run("camelCase tenantId is picked up when tenant_id is absent", func(t *testing.T) {
		entry := indexer.ExtractSearchFields(map[string]interface{}{
			"tenantId": "t-camel",
		})
		assert.Equal(t, "t-camel", entry.TenantID)
	})

	t.Run("organization_id is picked up when neither tenant_id nor tenantId is present", func(t *testing.T) {
		entry := indexer.ExtractSearchFields(map[string]interface{}{
			"organization_id": "org-1",
		})
		assert.Equal(t, "org-1", entry.TenantID)
	})

	t.Run("tenant_id wins over tenantId when both are present", func(t *testing.T) {
		entry := indexer.ExtractSearchFields(map[string]interface{}{
			"tenant_id": "t-snake",
			"tenantId":  "t-camel",
		})
		assert.Equal(t, "t-snake", entry.TenantID)
	})

	t.Run("tenant_id wins over organization_id when both are present", func(t *testing.T) {
		entry := indexer.ExtractSearchFields(map[string]interface{}{
			"tenant_id":       "t-snake",
			"organization_id": "org-1",
		})
		assert.Equal(t, "t-snake", entry.TenantID)
	})

	t.Run("tenantId wins over organization_id when both are present and tenant_id is absent", func(t *testing.T) {
		entry := indexer.ExtractSearchFields(map[string]interface{}{
			"tenantId":        "t-camel",
			"organization_id": "org-1",
		})
		assert.Equal(t, "t-camel", entry.TenantID)
	})

	t.Run("no tenant-related key leaves TenantID empty", func(t *testing.T) {
		entry := indexer.ExtractSearchFields(map[string]interface{}{
			"something_else": "value",
		})
		assert.Equal(t, "", entry.TenantID)
	})

	t.Run("non-string tenant_id is ignored", func(t *testing.T) {
		entry := indexer.ExtractSearchFields(map[string]interface{}{
			"tenant_id": 123,
		})
		assert.Equal(t, "", entry.TenantID)
	})

	t.Run("non-string tenantId is ignored", func(t *testing.T) {
		entry := indexer.ExtractSearchFields(map[string]interface{}{
			"tenantId": 1.5,
		})
		assert.Equal(t, "", entry.TenantID)
	})

	t.Run("non-string organization_id is ignored", func(t *testing.T) {
		entry := indexer.ExtractSearchFields(map[string]interface{}{
			"organization_id": false,
		})
		assert.Equal(t, "", entry.TenantID)
	})

	t.Run("non-string tenant_id and tenantId fall through to organization_id", func(t *testing.T) {
		entry := indexer.ExtractSearchFields(map[string]interface{}{
			"tenant_id":       42,
			"tenantId":        nil,
			"organization_id": "org-1",
		})
		assert.Equal(t, "org-1", entry.TenantID)
	})
}

// ---------------------------------------------------------------------------
// TraceID alias tests
// ---------------------------------------------------------------------------

func TestExtractSearchFields_TraceID(t *testing.T) {
	t.Run("snake_case trace_id is picked up", func(t *testing.T) {
		entry := indexer.ExtractSearchFields(map[string]interface{}{
			"trace_id": "tr-snake",
		})
		assert.Equal(t, "tr-snake", entry.TraceID)
	})

	t.Run("camelCase traceId is picked up when trace_id is absent", func(t *testing.T) {
		entry := indexer.ExtractSearchFields(map[string]interface{}{
			"traceId": "tr-camel",
		})
		assert.Equal(t, "tr-camel", entry.TraceID)
	})

	t.Run("kebab-case trace-id is picked up when neither trace_id nor traceId is present", func(t *testing.T) {
		entry := indexer.ExtractSearchFields(map[string]interface{}{
			"trace-id": "tr-kebab",
		})
		assert.Equal(t, "tr-kebab", entry.TraceID)
	})

	t.Run("trace_id wins over traceId when both are present", func(t *testing.T) {
		entry := indexer.ExtractSearchFields(map[string]interface{}{
			"trace_id": "tr-snake",
			"traceId":  "tr-camel",
		})
		assert.Equal(t, "tr-snake", entry.TraceID)
	})

	t.Run("trace_id wins over trace-id when both are present", func(t *testing.T) {
		entry := indexer.ExtractSearchFields(map[string]interface{}{
			"trace_id":  "tr-snake",
			"trace-id":  "tr-kebab",
		})
		assert.Equal(t, "tr-snake", entry.TraceID)
	})

	t.Run("traceId wins over trace-id when both are present and trace_id is absent", func(t *testing.T) {
		entry := indexer.ExtractSearchFields(map[string]interface{}{
			"traceId":  "tr-camel",
			"trace-id": "tr-kebab",
		})
		assert.Equal(t, "tr-camel", entry.TraceID)
	})

	t.Run("no trace-related key leaves TraceID empty", func(t *testing.T) {
		entry := indexer.ExtractSearchFields(map[string]interface{}{
			"something_else": "value",
		})
		assert.Equal(t, "", entry.TraceID)
	})

	t.Run("non-string trace_id is ignored", func(t *testing.T) {
		entry := indexer.ExtractSearchFields(map[string]interface{}{
			"trace_id": 99,
		})
		assert.Equal(t, "", entry.TraceID)
	})

	t.Run("non-string traceId is ignored", func(t *testing.T) {
		entry := indexer.ExtractSearchFields(map[string]interface{}{
			"traceId": []byte("bytes"),
		})
		assert.Equal(t, "", entry.TraceID)
	})

	t.Run("non-string trace-id is ignored", func(t *testing.T) {
		entry := indexer.ExtractSearchFields(map[string]interface{}{
			"trace-id": map[string]string{"k": "v"},
		})
		assert.Equal(t, "", entry.TraceID)
	})

	t.Run("non-string trace_id and traceId fall through to trace-id", func(t *testing.T) {
		entry := indexer.ExtractSearchFields(map[string]interface{}{
			"trace_id": 42,
			"traceId":  nil,
			"trace-id": "tr-kebab",
		})
		assert.Equal(t, "tr-kebab", entry.TraceID)
	})
}

// ---------------------------------------------------------------------------
// SpanID alias tests
// ---------------------------------------------------------------------------

func TestExtractSearchFields_SpanID(t *testing.T) {
	t.Run("snake_case span_id is picked up", func(t *testing.T) {
		entry := indexer.ExtractSearchFields(map[string]interface{}{
			"span_id": "sp-snake",
		})
		assert.Equal(t, "sp-snake", entry.SpanID)
	})

	t.Run("camelCase spanId is picked up when span_id is absent", func(t *testing.T) {
		entry := indexer.ExtractSearchFields(map[string]interface{}{
			"spanId": "sp-camel",
		})
		assert.Equal(t, "sp-camel", entry.SpanID)
	})

	t.Run("kebab-case span-id is picked up when neither span_id nor spanId is present", func(t *testing.T) {
		entry := indexer.ExtractSearchFields(map[string]interface{}{
			"span-id": "sp-kebab",
		})
		assert.Equal(t, "sp-kebab", entry.SpanID)
	})

	t.Run("span_id wins over spanId when both are present", func(t *testing.T) {
		entry := indexer.ExtractSearchFields(map[string]interface{}{
			"span_id": "sp-snake",
			"spanId":  "sp-camel",
		})
		assert.Equal(t, "sp-snake", entry.SpanID)
	})

	t.Run("span_id wins over span-id when both are present", func(t *testing.T) {
		entry := indexer.ExtractSearchFields(map[string]interface{}{
			"span_id": "sp-snake",
			"span-id": "sp-kebab",
		})
		assert.Equal(t, "sp-snake", entry.SpanID)
	})

	t.Run("spanId wins over span-id when both are present and span_id is absent", func(t *testing.T) {
		entry := indexer.ExtractSearchFields(map[string]interface{}{
			"spanId":  "sp-camel",
			"span-id": "sp-kebab",
		})
		assert.Equal(t, "sp-camel", entry.SpanID)
	})

	t.Run("no span-related key leaves SpanID empty", func(t *testing.T) {
		entry := indexer.ExtractSearchFields(map[string]interface{}{
			"something_else": "value",
		})
		assert.Equal(t, "", entry.SpanID)
	})

	t.Run("non-string span_id is ignored", func(t *testing.T) {
		entry := indexer.ExtractSearchFields(map[string]interface{}{
			"span_id": 7,
		})
		assert.Equal(t, "", entry.SpanID)
	})

	t.Run("non-string spanId is ignored", func(t *testing.T) {
		entry := indexer.ExtractSearchFields(map[string]interface{}{
			"spanId": true,
		})
		assert.Equal(t, "", entry.SpanID)
	})

	t.Run("non-string span-id is ignored", func(t *testing.T) {
		entry := indexer.ExtractSearchFields(map[string]interface{}{
			"span-id": 3.14,
		})
		assert.Equal(t, "", entry.SpanID)
	})

	t.Run("non-string span_id and spanId fall through to span-id", func(t *testing.T) {
		entry := indexer.ExtractSearchFields(map[string]interface{}{
			"span_id": 0,
			"spanId":  nil,
			"span-id": "sp-kebab",
		})
		assert.Equal(t, "sp-kebab", entry.SpanID)
	})
}

// ---------------------------------------------------------------------------
// RequestID alias tests
// ---------------------------------------------------------------------------

func TestExtractSearchFields_RequestID(t *testing.T) {
	t.Run("snake_case request_id is picked up", func(t *testing.T) {
		entry := indexer.ExtractSearchFields(map[string]interface{}{
			"request_id": "req-snake",
		})
		assert.Equal(t, "req-snake", entry.RequestID)
	})

	t.Run("camelCase requestId is picked up when request_id is absent", func(t *testing.T) {
		entry := indexer.ExtractSearchFields(map[string]interface{}{
			"requestId": "req-camel",
		})
		assert.Equal(t, "req-camel", entry.RequestID)
	})

	t.Run("kebab-case request-id is picked up when neither request_id nor requestId is present", func(t *testing.T) {
		entry := indexer.ExtractSearchFields(map[string]interface{}{
			"request-id": "req-kebab",
		})
		assert.Equal(t, "req-kebab", entry.RequestID)
	})

	t.Run("request_id wins over requestId when both are present", func(t *testing.T) {
		entry := indexer.ExtractSearchFields(map[string]interface{}{
			"request_id": "req-snake",
			"requestId":  "req-camel",
		})
		assert.Equal(t, "req-snake", entry.RequestID)
	})

	t.Run("request_id wins over request-id when both are present", func(t *testing.T) {
		entry := indexer.ExtractSearchFields(map[string]interface{}{
			"request_id": "req-snake",
			"request-id": "req-kebab",
		})
		assert.Equal(t, "req-snake", entry.RequestID)
	})

	t.Run("requestId wins over request-id when both are present and request_id is absent", func(t *testing.T) {
		entry := indexer.ExtractSearchFields(map[string]interface{}{
			"requestId":  "req-camel",
			"request-id": "req-kebab",
		})
		assert.Equal(t, "req-camel", entry.RequestID)
	})

	t.Run("no request-related key leaves RequestID empty", func(t *testing.T) {
		entry := indexer.ExtractSearchFields(map[string]interface{}{
			"something_else": "value",
		})
		assert.Equal(t, "", entry.RequestID)
	})

	t.Run("non-string request_id is ignored", func(t *testing.T) {
		entry := indexer.ExtractSearchFields(map[string]interface{}{
			"request_id": 1,
		})
		assert.Equal(t, "", entry.RequestID)
	})

	t.Run("non-string requestId is ignored", func(t *testing.T) {
		entry := indexer.ExtractSearchFields(map[string]interface{}{
			"requestId": false,
		})
		assert.Equal(t, "", entry.RequestID)
	})

	t.Run("non-string request-id is ignored", func(t *testing.T) {
		entry := indexer.ExtractSearchFields(map[string]interface{}{
			"request-id": []int{1, 2, 3},
		})
		assert.Equal(t, "", entry.RequestID)
	})

	t.Run("non-string request_id and requestId fall through to request-id", func(t *testing.T) {
		entry := indexer.ExtractSearchFields(map[string]interface{}{
			"request_id": 0,
			"requestId":  nil,
			"request-id": "req-kebab",
		})
		assert.Equal(t, "req-kebab", entry.RequestID)
	})
}

// ---------------------------------------------------------------------------
// Combined scenario
// ---------------------------------------------------------------------------

func TestExtractSearchFields_AllFieldsPopulated(t *testing.T) {
	entry := indexer.ExtractSearchFields(map[string]interface{}{
		"user_id":   "u-1",
		"tenant_id": "t-1",
		"trace_id":  "tr-1",
		"span_id":   "sp-1",
		"request_id": "req-1",
	})

	require.NotNil(t, entry)
	assert.Equal(t, "u-1", entry.UserID)
	assert.Equal(t, "t-1", entry.TenantID)
	assert.Equal(t, "tr-1", entry.TraceID)
	assert.Equal(t, "sp-1", entry.SpanID)
	assert.Equal(t, "req-1", entry.RequestID)
}

func TestExtractSearchFields_AllFieldsFromMixedAliases(t *testing.T) {
	// mix of camelCase and kebab-case aliases — verifies each field falls through
	// independently from the others.
	entry := indexer.ExtractSearchFields(map[string]interface{}{
		"user":          "u-plain",
		"organization_id": "org-1",
		"traceId":       "tr-camel",
		"span-id":       "sp-kebab",
		"requestId":     "req-camel",
	})

	require.NotNil(t, entry)
	assert.Equal(t, "u-plain", entry.UserID)
	assert.Equal(t, "org-1", entry.TenantID)
	assert.Equal(t, "tr-camel", entry.TraceID)
	assert.Equal(t, "sp-kebab", entry.SpanID)
	assert.Equal(t, "req-camel", entry.RequestID)
}

func TestExtractSearchFields_NonStringValuesForAllKeysAreIgnored(t *testing.T) {
	entry := indexer.ExtractSearchFields(map[string]interface{}{
		"user_id":   42,
		"tenant_id": 42,
		"trace_id":  42,
		"span_id":   42,
		"request_id": 42,
	})

	require.NotNil(t, entry)
	assert.Equal(t, "", entry.UserID)
	assert.Equal(t, "", entry.TenantID)
	assert.Equal(t, "", entry.TraceID)
	assert.Equal(t, "", entry.SpanID)
	assert.Equal(t, "", entry.RequestID)
}
