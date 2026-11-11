package sentinel

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"
)

// TestContextHelpers is rewritten for v0.4.0's routing (T20): user_id moves
// to the typed user, tenant_id to tags, and request_id (metadata-routed)
// stays in metadata.
func TestContextHelpers(t *testing.T) {
	ctx := context.Background()
	ctx = WithUser(ctx, "usr_100")
	ctx = WithTenant(ctx, "org_acme")
	ctx = WithTag(ctx, "request_id", "req_999")

	u, ok := UserFromContext(ctx)
	if !ok || u.ID != "usr_100" {
		t.Errorf("expected typed user id usr_100, got %+v (ok=%v)", u, ok)
	}
	tags := TagsFromContext(ctx)
	if tags["tenant_id"] != "org_acme" {
		t.Errorf("expected tags.tenant_id org_acme, got %v", tags["tenant_id"])
	}
	if _, ok := tags["user_id"]; ok {
		t.Error("user_id must not be in tags")
	}
	md := MetadataFromContext(ctx)
	if md["request_id"] != "req_999" {
		t.Errorf("expected metadata.request_id req_999, got %v", md["request_id"])
	}
	if _, ok := tags["request_id"]; ok {
		t.Error("request_id must not be in tags")
	}
}

// TestWithTagRouting is P1.3 (b): every rule of §4.2's routing table.
func TestWithTagRouting(t *testing.T) {
	t.Run("metadata-routed keys stay in metadata", func(t *testing.T) {
		for _, k := range []string{
			"fingerprint", "error_class", "release_version",
			"request_id", "requestId", "request-id",
			"trace_id", "traceId", "trace-id", "span_id", "spanId", "span-id",
			"userId", "user", "tenantId", "organization_id", "runtime",
			"server_truncated", "server_truncated_keys", "truncated", "truncated_keys",
		} {
			ctx := WithTag(context.Background(), k, "v")
			md := MetadataFromContext(ctx)
			if md[k] != "v" {
				t.Errorf("key %q: expected metadata[%q]=v, got %v", k, k, md[k])
			}
			if _, ok := TagsFromContext(ctx)[k]; ok {
				t.Errorf("key %q: must not be in tags", k)
			}
		}
	})

	t.Run("user_id routes to the typed user", func(t *testing.T) {
		ctx := WithTag(context.Background(), "user_id", "u-1")
		u, ok := UserFromContext(ctx)
		if !ok || u.ID != "u-1" {
			t.Errorf("expected typed user id u-1, got %+v", u)
		}
	})
	t.Run("tenant_id routes to tags", func(t *testing.T) {
		ctx := WithTag(context.Background(), "tenant_id", "t-1")
		if TagsFromContext(ctx)["tenant_id"] != "t-1" {
			t.Error("expected tags.tenant_id")
		}
	})
	t.Run("a non-scalar value routes to metadata", func(t *testing.T) {
		ctx := WithTag(context.Background(), "list", []string{"a"})
		if _, ok := MetadataFromContext(ctx)["list"]; !ok {
			t.Error("expected a non-scalar value in metadata")
		}
	})
	t.Run("nil or a nil pointer removes the key from both stores", func(t *testing.T) {
		ctx := WithTag(context.Background(), "plan", "pro")
		ctx = WithTag(ctx, "plan", nil)
		if _, ok := TagsFromContext(ctx)["plan"]; ok {
			t.Error("plan must not be in tags after a nil write")
		}
		if _, ok := MetadataFromContext(ctx)["plan"]; ok {
			t.Error("plan must not be in metadata after a nil write")
		}
	})

	t.Run("scalar kinds and string form", func(t *testing.T) {
		type Region string
		ctx := WithTag(context.Background(), "region", Region("eu"))
		if TagsFromContext(ctx)["region"] != "eu" {
			t.Errorf("named string type must be a tag, got %v", TagsFromContext(ctx)["region"])
		}

		type Shard int
		ctx = WithTag(context.Background(), "shard", Shard(3))
		if TagsFromContext(ctx)["shard"] != "3" {
			t.Errorf("named int type must be a tag, got %v", TagsFromContext(ctx)["shard"])
		}

		ctx = WithTag(context.Background(), "d", 2*time.Second)
		if TagsFromContext(ctx)["d"] != "2s" {
			t.Errorf("time.Duration (a Stringer) must be a tag as its String(), got %v", TagsFromContext(ctx)["d"])
		}

		type notStringer struct{ X int }
		ctx = WithTag(context.Background(), "s", notStringer{X: 1})
		if _, ok := MetadataFromContext(ctx)["s"]; !ok {
			t.Error("a non-Stringer struct must route to metadata")
		}
	})

	t.Run("invalid tags route to metadata (rule 4)", func(t *testing.T) {
		key32 := strings.Repeat("k", 32)
		ctx := WithTag(context.Background(), key32, "v")
		if _, ok := TagsFromContext(ctx)[key32]; !ok {
			t.Error("a 32-character key must be a tag")
		}
		key33 := strings.Repeat("k", 33)
		ctx = WithTag(context.Background(), key33, "v")
		if _, ok := MetadataFromContext(ctx)[key33]; !ok {
			t.Error("a 33-character key must route to metadata")
		}

		ctx = WithTag(context.Background(), "k", "")
		if _, ok := MetadataFromContext(ctx)["k"]; !ok {
			t.Error("an empty value must route to metadata")
		}

		val200 := strings.Repeat("é", 200)
		ctx = WithTag(context.Background(), "k", val200)
		if _, ok := TagsFromContext(ctx)["k"]; !ok {
			t.Error("a 200-code-point value must be a tag")
		}
		val201 := strings.Repeat("é", 201)
		ctx = WithTag(context.Background(), "k", val201)
		if _, ok := MetadataFromContext(ctx)["k"]; !ok {
			t.Error("a 201-code-point value must route to metadata")
		}

		ctx = WithTag(context.Background(), "k", "a\nb")
		if _, ok := MetadataFromContext(ctx)["k"]; !ok {
			t.Error("a multi-line value must route to metadata")
		}
	})

	t.Run("WithTag tenant_id empty routes to tags, not metadata (rule 1, not rule 4)", func(t *testing.T) {
		ctx := WithTag(context.Background(), "tenant_id", "")
		if _, ok := TagsFromContext(ctx)["tenant_id"]; !ok {
			t.Error("expected tenant_id in tags even when empty (rule 1 wins over rule 4)")
		}
		if _, ok := MetadataFromContext(ctx)["tenant_id"]; ok {
			t.Error("tenant_id must not be in metadata")
		}
	})

	t.Run("one key, one store: last write wins across stores", func(t *testing.T) {
		base := WithTag(context.Background(), "plan", "pro")
		if TagsFromContext(base)["plan"] != "pro" {
			t.Fatal("setup: expected plan in tags")
		}

		ctx := WithTag(base, "plan", "")
		if _, ok := TagsFromContext(ctx)["plan"]; ok {
			t.Error("plan must leave tags once it becomes invalid")
		}
		if MetadataFromContext(ctx)["plan"] != "" {
			t.Error("plan must land in metadata with its new value")
		}

		ctx = WithTag(base, "plan", strings.Repeat("x", 201))
		if _, ok := TagsFromContext(ctx)["plan"]; ok {
			t.Error("plan must leave tags")
		}

		ctx = WithTag(base, "plan", []string{"a"})
		if _, ok := TagsFromContext(ctx)["plan"]; ok {
			t.Error("plan must leave tags for a non-scalar")
		}

		reverse := WithTag(context.Background(), "plan", strings.Repeat("x", 201))
		reverse = WithTag(reverse, "plan", "pro")
		if TagsFromContext(reverse)["plan"] != "pro" {
			t.Error("plan must move to tags")
		}
		if _, ok := MetadataFromContext(reverse)["plan"]; ok {
			t.Error("plan must leave metadata")
		}

		viaMap := WithContextMap(context.Background(), map[string]interface{}{"plan": "x"})
		viaMap = WithTag(viaMap, "plan", "pro")
		if TagsFromContext(viaMap)["plan"] != "pro" {
			t.Error("WithTag after WithContextMap must move the key to tags")
		}
		if _, ok := MetadataFromContext(viaMap)["plan"]; ok {
			t.Error("plan must leave metadata")
		}

		viaTag := WithTag(context.Background(), "plan", "pro")
		viaTag = WithContextMap(viaTag, map[string]interface{}{"plan": "x"})
		if _, ok := TagsFromContext(viaTag)["plan"]; ok {
			t.Error("WithContextMap after WithTag must move the key to metadata")
		}
		if MetadataFromContext(viaTag)["plan"] != "x" {
			t.Error("plan must be in metadata")
		}

		tenantFirst := WithTenant(context.Background(), "t1")
		tenantFirst = WithContextMap(tenantFirst, map[string]interface{}{"tenant_id": "t2"})
		if MetadataFromContext(tenantFirst)["tenant_id"] != "t2" {
			t.Error("expected metadata.tenant_id t2")
		}
		if _, ok := TagsFromContext(tenantFirst)["tenant_id"]; ok {
			t.Error("tags must lack tenant_id")
		}

		mapFirst := WithContextMap(context.Background(), map[string]interface{}{"user_id": "m"})
		mapFirst = WithTag(mapFirst, "user_id", "u")
		if u, _ := UserFromContext(mapFirst); u.ID != "u" {
			t.Errorf("expected typed user u, got %+v", u)
		}
		if _, ok := MetadataFromContext(mapFirst)["user_id"]; ok {
			t.Error("metadata must lack user_id")
		}
	})

	t.Run("a nil pointer Stringer is also a nil write", func(t *testing.T) {
		var s *strings.Builder
		base := WithTag(context.Background(), "plan", "pro")
		ctx := WithTag(base, "plan", s)
		if _, ok := TagsFromContext(ctx)["plan"]; ok {
			t.Error("a nil pointer must remove the key, not become the tag <nil>")
		}
	})

	t.Run("WithUserInfo then a nil user_id write clears only ID", func(t *testing.T) {
		ctx := WithUserInfo(context.Background(), UserInfo{ID: "u", Email: "e@example.test"})
		ctx = WithTag(ctx, "user_id", nil)
		u, ok := UserFromContext(ctx)
		if !ok {
			t.Fatal("expected the user to remain (Email survives)")
		}
		if u.ID != "" || u.Email != "e@example.test" {
			t.Errorf("expected ID cleared, Email kept, got %+v", u)
		}

		ctx2 := WithUser(context.Background(), "u")
		ctx2 = WithTag(ctx2, "user_id", nil)
		if _, ok := UserFromContext(ctx2); ok {
			t.Error("a user left with no field must be removed")
		}
	})

	t.Run("parent context is never modified", func(t *testing.T) {
		ctx1 := WithTag(context.Background(), "plan", "pro")
		_ = WithTag(ctx1, "plan", "enterprise")
		if TagsFromContext(ctx1)["plan"] != "pro" {
			t.Error("ctx1 must still read its own first value")
		}
	})
}

func TestFingerprintAndErrorClassContext(t *testing.T) {
	ctx := context.Background()
	if got := ExtractFingerprint(ctx); got != "" {
		t.Fatalf("expected empty fingerprint, got %q", got)
	}
	if got := ExtractErrorClass(ctx); got != "" {
		t.Fatalf("expected empty error class, got %q", got)
	}
	if got := ExtractFingerprint(nil); got != "" {
		t.Fatalf("expected empty fingerprint for nil ctx, got %q", got)
	}
	if got := ExtractErrorClass(nil); got != "" {
		t.Fatalf("expected empty error class for nil ctx, got %q", got)
	}

	ctx = WithFingerprint(ctx, "custom-fingerprint")
	ctx = WithErrorClass(ctx, "CustomErrorClass")

	if got := ExtractFingerprint(ctx); got != "custom-fingerprint" {
		t.Errorf("expected custom-fingerprint, got %q", got)
	}
	if got := ExtractErrorClass(ctx); got != "CustomErrorClass" {
		t.Errorf("expected CustomErrorClass, got %q", got)
	}
}

// The server accepts, maps and stores trace_flags end to end
// (ingestor validation -> mapping -> processor), but ExtractTraceIDs read only the
// ids off the SpanContext and dropped the flags that sit on the same object — so
// every SDK-originated event arrived with its sampled bit lost.
func TestExtractTraceContextCarriesSampledFlag(t *testing.T) {
	traceID, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	spanID, _ := trace.SpanIDFromHex("00f067aa0ba902b7")

	sampled := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), sampled)

	gotTrace, gotSpan, gotFlags := ExtractTraceContext(ctx)
	if gotTrace != traceID.String() || gotSpan != spanID.String() {
		t.Fatalf("ids not carried: trace=%q span=%q", gotTrace, gotSpan)
	}
	if gotFlags != uint32(trace.FlagsSampled) {
		t.Fatalf("expected sampled flag %d, got %d", trace.FlagsSampled, gotFlags)
	}

	// An unsampled span must report 0 rather than being indistinguishable from
	// sampled — that distinction is the whole point of sending the field.
	unsampled := trace.NewSpanContext(trace.SpanContextConfig{TraceID: traceID, SpanID: spanID})
	_, _, gotFlags = ExtractTraceContext(trace.ContextWithSpanContext(context.Background(), unsampled))
	if gotFlags != 0 {
		t.Fatalf("expected 0 flags for an unsampled span, got %d", gotFlags)
	}

	// No span context at all: no ids, no flags, and no panic.
	if tr, sp, fl := ExtractTraceContext(context.Background()); tr != "" || sp != "" || fl != 0 {
		t.Fatalf("expected empty for a context with no span, got %q %q %d", tr, sp, fl)
	}
	if tr, sp, fl := ExtractTraceContext(nil); tr != "" || sp != "" || fl != 0 {
		t.Fatalf("expected empty for a nil context, got %q %q %d", tr, sp, fl)
	}
}

// ExtractTraceIDs is exported and callers outside this repo may depend on its
// two-value signature; it must keep behaving as it did.
func TestExtractTraceIDsUnchanged(t *testing.T) {
	traceID, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	spanID, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled,
	}))

	gotTrace, gotSpan := ExtractTraceIDs(ctx)
	if gotTrace != traceID.String() || gotSpan != spanID.String() {
		t.Fatalf("ExtractTraceIDs changed: trace=%q span=%q", gotTrace, gotSpan)
	}
}
