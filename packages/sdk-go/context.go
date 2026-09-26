package sentinel

import (
	"context"
	"fmt"
	"reflect"

	"go.opentelemetry.io/otel/trace"
)

type contextKey string

const (
	tagsContextKey        contextKey = "sentinel_tags"
	metadataContextKey    contextKey = "sentinel_metadata"
	userContextKey        contextKey = "sentinel_user"
	levelContextKey       contextKey = "sentinel_level"
	fingerprintContextKey contextKey = "sentinel_fingerprint"
	errorClassContextKey  contextKey = "sentinel_error_class"
)

// metadataRoutedKeys is the complete, audited set of metadata keys any SDK
// or server code reads by name (spec §4.2, T11, T27): every key here stays
// in metadata rather than routing to tags, however WithTag routes by
// default. If a reader below gains a key, or a new reader appears, this
// table must change in the same PR (B5).
var metadataRoutedKeys = map[string]bool{
	// fingerprint: SDK event.go: NewEvent (ctxTags["fingerprint"]) -> Event.Fingerprint
	"fingerprint": true,
	// error_class: SDK event.go: NewEvent (ctxTags["error_class"]) -> overrides Event.ErrorClass
	"error_class": true,
	// release_version: processor event/event.go: (*ErrorEvent).Prepare -> the release fallback
	"release_version": true,
	// request_id and aliases: processor indexer/indexer.go: ExtractSearchFields -> error_search_index.request_id
	"request_id": true, "requestId": true, "request-id": true,
	// trace_id/span_id and aliases: processor indexer/indexer.go: ExtractSearchFields (metadata first, typed fallback)
	"trace_id": true, "traceId": true, "trace-id": true,
	"span_id": true, "spanId": true, "span-id": true,
	// userId/user: processor indexer/indexer.go: UserIDFromMetadata -> the affected user fallback
	"userId": true, "user": true,
	// tenantId/organization_id: processor indexer/indexer.go: TenantIDFromMetadata -> the affected tenant fallback
	"tenantId": true, "organization_id": true,
	// runtime: ingestor enrich/enrich.go: IsClientPlatform, isTunnelOnly -> client/server classification
	"runtime": true,
	// server_truncated/server_truncated_keys: ingestor validation/bounds.go: writeMarkers -> server-owned
	"server_truncated": true, "server_truncated_keys": true,
	// truncated/truncated_keys: ingestor validation/bounds.go: metadataExemptKeys (shrinkMetadata); from
	// v0.4.0 also written by this SDK's own transport (spec §3.4, "The SDK's marker")
	"truncated": true, "truncated_keys": true,
}

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

// isScalarValue reports whether value's dynamic kind is one WithTag treats
// as a tag candidate (spec §4.2): a string, bool, any integer or float kind,
// or a value implementing fmt.Stringer. Uintptr, complex numbers, and every
// pointer, slice, map, struct, func and chan that is not a Stringer are not.
func isScalarValue(value interface{}) bool {
	if _, ok := value.(fmt.Stringer); ok {
		return true
	}
	switch reflect.ValueOf(value).Kind() {
	case reflect.String, reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return true
	default:
		return false
	}
}

// isNilValue reports whether value is untyped nil, or a nil pointer (which
// fmt.Sprint would render "<nil>" - spec §4.2: "A nil value, and a nil
// pointer ... remove the key from both stores").
func isNilValue(value interface{}) bool {
	if value == nil {
		return true
	}
	rv := reflect.ValueOf(value)
	return rv.Kind() == reflect.Ptr && rv.IsNil()
}

// strValue is spec §4.2's str(value): fmt.Sprint for every scalar - it calls
// String() on a Stringer (and Error() on a value that is also an error,
// fmt's own precedence), prints a named string's underlying string, and a
// number's decimal form.
func strValue(value interface{}) string {
	return fmt.Sprint(value)
}

func WithUser(ctx context.Context, userID string) context.Context {
	ctx = removeMetadataKey(ctx, "user_id")
	u, _ := userFromContext(ctx)
	u.ID = userID
	return context.WithValue(ctx, userContextKey, u)
}

// WithUserInfo replaces the context's typed user with u wholesale.
func WithUserInfo(ctx context.Context, u UserInfo) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = removeMetadataKey(ctx, "user_id")
	return context.WithValue(ctx, userContextKey, u)
}

// UserFromContext returns the context's typed user, false if none.
func UserFromContext(ctx context.Context) (UserInfo, bool) {
	if ctx == nil {
		return UserInfo{}, false
	}
	u, ok := ctx.Value(userContextKey).(UserInfo)
	return u, ok
}

func userFromContext(ctx context.Context) (UserInfo, bool) {
	return UserFromContext(ctx)
}

func WithTenant(ctx context.Context, tenantID string) context.Context {
	return writeTag(ctx, "tenant_id", tenantID)
}

// WithTag routes key/value by the rules of spec §4.2: user_id/tenant_id to
// the typed user / tags, a metadata-routed key to metadata, a non-scalar
// value to metadata, an invalid tag candidate to metadata, and everything
// else to tags. Whichever store wins, the key is removed from the other. A
// nil value (or nil pointer) removes the key from both.
func WithTag(ctx context.Context, key string, value interface{}) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if isNilValue(value) {
		ctx = removeTagKey(ctx, key)
		ctx = removeMetadataKey(ctx, key)
		if key == "user_id" {
			if u, ok := UserFromContext(ctx); ok {
				u.ID = ""
				if u == (UserInfo{}) {
					ctx = context.WithValue(ctx, userContextKey, nil)
				} else {
					ctx = context.WithValue(ctx, userContextKey, u)
				}
			}
		}
		return ctx
	}

	if key == "user_id" && isScalarValue(value) {
		return WithUser(ctx, strValue(value))
	}
	if key == "tenant_id" && isScalarValue(value) {
		return WithTenant(ctx, strValue(value))
	}
	if metadataRoutedKeys[key] {
		return writeMetadata(ctx, key, value)
	}
	if !isScalarValue(value) {
		return writeMetadata(ctx, key, value)
	}
	str := strValue(value)
	if !validTag(key, str) {
		return writeMetadata(ctx, key, value)
	}
	return writeTag(ctx, key, str)
}

// writeTag stores tags[key] = value, copy-on-write, and removes key from
// metadata (one key, one store).
func writeTag(ctx context.Context, key, value string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	tags := getTagsStore(ctx)
	newTags := make(map[string]string, len(tags)+1)
	for k, v := range tags {
		newTags[k] = v
	}
	newTags[key] = value
	ctx = context.WithValue(ctx, tagsContextKey, newTags)
	return removeMetadataKey(ctx, key)
}

// writeMetadata stores metadata[key] = value, copy-on-write, and removes
// key from tags (one key, one store).
func writeMetadata(ctx context.Context, key string, value interface{}) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	md := getMetadataStore(ctx)
	newMD := make(map[string]interface{}, len(md)+1)
	for k, v := range md {
		newMD[k] = v
	}
	newMD[key] = value
	ctx = context.WithValue(ctx, metadataContextKey, newMD)
	return removeTagKey(ctx, key)
}

func removeTagKey(ctx context.Context, key string) context.Context {
	tags := getTagsStore(ctx)
	if _, ok := tags[key]; !ok {
		return ctx
	}
	newTags := make(map[string]string, len(tags))
	for k, v := range tags {
		if k != key {
			newTags[k] = v
		}
	}
	return context.WithValue(ctx, tagsContextKey, newTags)
}

func removeMetadataKey(ctx context.Context, key string) context.Context {
	md := getMetadataStore(ctx)
	if _, ok := md[key]; !ok {
		return ctx
	}
	newMD := make(map[string]interface{}, len(md))
	for k, v := range md {
		if k != key {
			newMD[k] = v
		}
	}
	return context.WithValue(ctx, metadataContextKey, newMD)
}

func getTagsStore(ctx context.Context) map[string]string {
	if ctx == nil {
		return nil
	}
	if tags, ok := ctx.Value(tagsContextKey).(map[string]string); ok {
		return tags
	}
	return nil
}

func getMetadataStore(ctx context.Context) map[string]interface{} {
	if ctx == nil {
		return nil
	}
	if md, ok := ctx.Value(metadataContextKey).(map[string]interface{}); ok {
		return md
	}
	return nil
}

// TagsFromContext returns a copy of the context's tags, nil if none.
func TagsFromContext(ctx context.Context) map[string]string {
	tags := getTagsStore(ctx)
	if tags == nil {
		return nil
	}
	out := make(map[string]string, len(tags))
	for k, v := range tags {
		out[k] = v
	}
	return out
}

// WithContextMap stores additionalTags in the context's metadata store
// (unchanged: "extra"), except that every key it writes is removed from the
// context's tags (one key, one store). A user_id key here stays in
// metadata and does not touch the typed user.
func WithContextMap(ctx context.Context, additionalTags map[string]interface{}) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	md := getMetadataStore(ctx)
	newMD := make(map[string]interface{}, len(md)+len(additionalTags))
	for k, v := range md {
		newMD[k] = v
	}
	for k, v := range additionalTags {
		newMD[k] = v
	}
	ctx = context.WithValue(ctx, metadataContextKey, newMD)
	for k := range additionalTags {
		ctx = removeTagKey(ctx, k)
	}
	return ctx
}

// MetadataFromContext returns a shallow copy of the context's metadata map,
// unscrubbed, nil if none. ApplyContext scrubs what it merges.
func MetadataFromContext(ctx context.Context) map[string]interface{} {
	md := getMetadataStore(ctx)
	if md == nil {
		return nil
	}
	out := make(map[string]interface{}, len(md))
	for k, v := range md {
		out[k] = v
	}
	return out
}

// WithLevel stores NormalizeLevel(level); an unknown level leaves ctx
// unchanged.
func WithLevel(ctx context.Context, level string) context.Context {
	n := NormalizeLevel(level)
	if n == "" {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, levelContextKey, n)
}

func levelFromContext(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	l, ok := ctx.Value(levelContextKey).(string)
	return l, ok
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

// ApplyContext copies onto ev what ctx carries for Sentinel. Call it right after
// NewEvent and before your own assignments, so that what you set afterwards wins.
// Client.CaptureErrorContext calls exactly this.
//
// It acts only when ctx carries a value, so it never erases anything. It assigns
// fields of the *Event it is given, but never writes into a map, slice or pointer
// that event holds: every row that adds to a map or a struct builds a new one and
// assigns it (spec §4.3).
func ApplyContext(ctx context.Context, ev *Event) {
	if ctx == nil || ev == nil {
		return
	}

	// Metadata row: ScrubPII of the context's map merged onto ev.Metadata for
	// keys it does not already carry, and the same fingerprint/error_class
	// effect NewEvent gives those keys in ctxTags - but only when no option
	// set the field (spec §3.2, §4.3).
	if rawMD := getMetadataStore(ctx); len(rawMD) > 0 {
		scrubbed := ScrubPII(rawMD)
		added := false
		newMD := ev.Metadata
		for k, v := range scrubbed {
			if _, exists := ev.Metadata[k]; exists {
				continue
			}
			if !added {
				newMD = make(map[string]interface{}, len(ev.Metadata)+1)
				for ek, ev2 := range ev.Metadata {
					newMD[ek] = ev2
				}
				added = true
			}
			newMD[k] = v
			if k == "fingerprint" {
				if s, ok := v.(string); ok && s != "" && !ev.fingerprintWasSet() {
					ev.Fingerprint = s
				}
			}
			if k == "error_class" {
				if s, ok := v.(string); ok && s != "" && !ev.errorClassWasSet() {
					ev.ErrorClass = s
				}
			}
		}
		if added {
			ev.Metadata = newMD
		}
	}

	// Level: the context always wins (NewEvent always sets "error", so an
	// existing value cannot be told from the default).
	if l, ok := levelFromContext(ctx); ok {
		ev.Level = l
	}

	// Typed user: merged field by field, the event's existing field wins.
	if u, ok := UserFromContext(ctx); ok {
		merged := UserInfo{}
		if ev.User != nil {
			merged = *ev.User
		}
		if merged.ID == "" {
			merged.ID = u.ID
		}
		if merged.Username == "" {
			merged.Username = u.Username
		}
		if merged.Email == "" {
			merged.Email = u.Email
		}
		if merged.IPAddress == "" {
			merged.IPAddress = u.IPAddress
		}
		ev.User = &merged
	}

	// Tags: each context tag ev.Tags does not already carry.
	if tags := getTagsStore(ctx); len(tags) > 0 {
		added := false
		newTags := ev.Tags
		for k, v := range tags {
			if _, exists := ev.Tags[k]; exists {
				continue
			}
			if !added {
				newTags = make(map[string]string, len(ev.Tags)+1)
				for ek, ev2 := range ev.Tags {
					newTags[ek] = ev2
				}
				added = true
			}
			newTags[k] = v
		}
		if added {
			ev.Tags = newTags
		}
	}

	// Breadcrumb buffer (spec §5.3, lands with C5).
	applyContextBreadcrumbs(ctx, ev)

	// Request scope and transaction (spec §6.2-§6.4, lands with C7).
	applyContextRequestScope(ctx, ev)

	// Trace context: only fills empty ids.
	if ev.TraceID == "" {
		if traceID, spanID, flags := ExtractTraceContext(ctx); traceID != "" {
			ev.TraceID = traceID
			ev.SpanID = spanID
			ev.TraceFlags = flags
		}
	}

	// WithFingerprint / WithErrorClass: the context wins over everything else.
	if fp := ExtractFingerprint(ctx); fp != "" {
		ev.Fingerprint = fp
	}
	if ec := ExtractErrorClass(ctx); ec != "" {
		ev.ErrorClass = ec
	}
}

// applyContextBreadcrumbs attaches the context's breadcrumb buffer when the
// event does not already have breadcrumbs of its own (spec §5.3).
func applyContextBreadcrumbs(ctx context.Context, ev *Event) {
	if len(ev.Breadcrumbs) > 0 {
		return
	}
	if crumbs := BreadcrumbsFromContext(ctx); len(crumbs) > 0 {
		ev.Breadcrumbs = crumbs
	}
}

// applyContextRequestScope fills ev.Request, ev.Transaction and, on opt-in,
// ev.User.IPAddress from the middleware's request scope (spec §6.2-§6.4). It
// builds the scope's lazy snapshot on first use (scope.ensureBuilt) and gives
// this event its own deep copy, never the scope's shared *RequestInfo.
func applyContextRequestScope(ctx context.Context, ev *Event) {
	scope := requestScopeFromContext(ctx)
	if scope == nil {
		return
	}
	scope.ensureBuilt()

	if ev.Request == nil && scope.snapshot != nil {
		ev.Request = deepCopyRequestInfo(scope.snapshot)
	}
	if ev.Transaction == "" {
		ev.Transaction = scope.transaction
	}
	if scope.clientIP != "" {
		if ev.User == nil {
			ev.User = &UserInfo{}
		}
		if ev.User.IPAddress == "" {
			merged := *ev.User
			merged.IPAddress = scope.clientIP
			ev.User = &merged
		}
	}
}
