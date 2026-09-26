package sentinel

import (
	"context"
	"testing"
)

// TestContextMetadataReachesWireOnBothPaths is P1.3 (c): the context's
// metadata-routed keys, and additionalContext / ctxTags at the call site,
// must reach the wire identically on the Client path and the NewEvent +
// ApplyContext + Push path.
func TestContextMetadataReachesWireOnBothPaths(t *testing.T) {
	build := func() (ctxBuild func() (Config, *Event)) {
		return func() (Config, *Event) {
			ctx := WithTag(context.Background(), "request_id", "r1")
			ctx = WithTag(ctx, "fingerprint", "fp-ctx")
			ctx = WithTag(ctx, "error_class", "CtxClass")
			ctx = WithTag(ctx, "release_version", "9.9.9")
			ctx = WithContextMap(ctx, map[string]interface{}{
				"order_ref":   "o-1",
				"k":           "ctx",
				"db_password": "x",
			})

			cfg := Config{ProjectKey: "pk_test"}
			callSiteMD := map[string]interface{}{"k": "call"}
			merged := make(map[string]interface{}, len(callSiteMD))
			for k, v := range MetadataFromContext(ctx) {
				merged[k] = v
			}
			for k, v := range callSiteMD {
				merged[k] = v
			}
			ev := NewEvent(cfg, nil, merged)
			ApplyContext(ctx, ev)
			return cfg, ev
		}
	}

	assertEvent := func(t *testing.T, ev *Event) {
		t.Helper()
		if ev.Metadata["request_id"] != "r1" {
			t.Errorf("metadata.request_id = %v, want r1", ev.Metadata["request_id"])
		}
		if ev.Metadata["release_version"] != "9.9.9" {
			t.Errorf("metadata.release_version = %v, want 9.9.9", ev.Metadata["release_version"])
		}
		if ev.Metadata["order_ref"] != "o-1" {
			t.Errorf("metadata.order_ref = %v, want o-1", ev.Metadata["order_ref"])
		}
		if ev.Metadata["k"] != "call" {
			t.Errorf("metadata.k = %v, want call (the call site wins)", ev.Metadata["k"])
		}
		if ev.Metadata["db_password"] != "[FILTERED]" {
			t.Errorf("metadata.db_password = %v, want [FILTERED]", ev.Metadata["db_password"])
		}
		if _, ok := ev.Tags["fingerprint"]; ok {
			t.Error("tags must not carry fingerprint")
		}
		if _, ok := ev.Tags["error_class"]; ok {
			t.Error("tags must not carry error_class")
		}
		if _, ok := ev.Tags["release_version"]; ok {
			t.Error("tags must not carry release_version")
		}
		if ev.Fingerprint != "fp-ctx" {
			t.Errorf("Fingerprint = %q, want fp-ctx", ev.Fingerprint)
		}
		if ev.ErrorClass != "CtxClass" {
			t.Errorf("ErrorClass = %q, want CtxClass", ev.ErrorClass)
		}
	}

	t.Run("NewEvent+ApplyContext path", func(t *testing.T) {
		_, ev := build()()
		assertEvent(t, ev)
	})

	t.Run("WithFingerprint wins over the metadata key", func(t *testing.T) {
		ctx := WithTag(context.Background(), "fingerprint", "fp-ctx")
		ctx = WithFingerprint(ctx, "fp-typed")
		cfg := Config{ProjectKey: "pk_test"}
		ev := NewEvent(cfg, nil, MetadataFromContext(ctx))
		ApplyContext(ctx, ev)
		if ev.Fingerprint != "fp-typed" {
			t.Errorf("Fingerprint = %q, want fp-typed", ev.Fingerprint)
		}
	})
}
