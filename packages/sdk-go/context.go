package sentinel

import (
	"context"

	"go.opentelemetry.io/otel/trace"
)

type contextKey string

const (
	tagsContextKey        contextKey = "sentinel_tags"
	fingerprintContextKey contextKey = "sentinel_fingerprint"
	errorClassContextKey  contextKey = "sentinel_error_class"
)

// WithFingerprint binds a custom grouping key to ctx. When Sentinel captures an error
// with this context, the server uses this fingerprint directly rather than hashing stack frames.
// Must not exceed 64 characters.
func WithFingerprint(ctx context.Context, fingerprint string) context.Context {
	return context.WithValue(ctx, fingerprintContextKey, fingerprint)
}

// WithErrorClass overrides the error class on events captured using ctx.
func WithErrorClass(ctx context.Context, errorClass string) context.Context {
	return context.WithValue(ctx, errorClassContextKey, errorClass)
}

// ExtractFingerprint returns the custom fingerprint set on ctx, if any.
func ExtractFingerprint(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if fp, ok := ctx.Value(fingerprintContextKey).(string); ok {
		return fp
	}
	return ""
}

// ExtractErrorClass returns the custom error class set on ctx, if any.
func ExtractErrorClass(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if ec, ok := ctx.Value(errorClassContextKey).(string); ok {
		return ec
	}
	return ""
}

func WithUser(ctx context.Context, userID string) context.Context {
	return WithTag(ctx, "user_id", userID)
}

func WithTenant(ctx context.Context, tenantID string) context.Context {
	return WithTag(ctx, "tenant_id", tenantID)
}

func WithTag(ctx context.Context, key string, value interface{}) context.Context {
	tags := getTagsMap(ctx)
	newTags := make(map[string]interface{}, len(tags)+1)
	for k, v := range tags {
		newTags[k] = v
	}
	newTags[key] = value
	return context.WithValue(ctx, tagsContextKey, newTags)
}

func WithContextMap(ctx context.Context, additionalTags map[string]interface{}) context.Context {
	tags := getTagsMap(ctx)
	newTags := make(map[string]interface{}, len(tags)+len(additionalTags))
	for k, v := range tags {
		newTags[k] = v
	}
	for k, v := range additionalTags {
		newTags[k] = v
	}
	return context.WithValue(ctx, tagsContextKey, newTags)
}

func getTagsMap(ctx context.Context) map[string]interface{} {
	if ctx == nil {
		return nil
	}
	if tags, ok := ctx.Value(tagsContextKey).(map[string]interface{}); ok {
		return tags
	}
	return nil
}

func ExtractTraceIDs(ctx context.Context) (traceID string, spanID string) {
	traceID, spanID, _ = ExtractTraceContext(ctx)
	return traceID, spanID
}

// ExtractTraceContext returns the trace id, span id AND the W3C trace-flags byte from
// ctx's span context.
//
// ExtractTraceIDs above dropped the flags, which sit on the very same SpanContext, so
// every SDK-originated event reached the server with its sampled bit lost even though
// the ingestor accepts and stores trace_flags. ExtractTraceIDs is kept as-is because it
// is exported and callers outside this repo may depend on its signature.
func ExtractTraceContext(ctx context.Context) (traceID string, spanID string, traceFlags uint32) {
	if ctx == nil {
		return "", "", 0
	}
	spanCtx := trace.SpanContextFromContext(ctx)
	if spanCtx.IsValid() {
		return spanCtx.TraceID().String(), spanCtx.SpanID().String(), uint32(spanCtx.TraceFlags())
	}
	return "", "", 0
}
