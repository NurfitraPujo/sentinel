package sentinel

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

func TestBoundContextsDecodedEvictsNonWellKnownFirst(t *testing.T) {
	big := strings.Repeat("x", 40000)
	contexts := map[string]map[string]interface{}{
		"custom1": {"v": big},
		"custom2": {"v": big},
		"os":      {"name": "golden-os"},
	}
	out, removed := boundContextsDecoded(contexts)
	if _, ok := out["os"]; !ok {
		t.Error("well-known context must survive eviction before non-well-known ones")
	}
	if len(removed) == 0 {
		t.Error("expected at least one context evicted over the size bound")
	}
}

func TestBoundContextsDecodedDropsInvalidNames(t *testing.T) {
	contexts := map[string]map[string]interface{}{
		"":                      {"v": 1},
		strings.Repeat("n", 65): {"v": 1},
		"ok":                    {"v": 1},
	}
	out, _ := boundContextsDecoded(contexts)
	if _, ok := out["ok"]; !ok {
		t.Error("valid context name must survive")
	}
	if len(out) != 1 {
		t.Errorf("len(out) = %d, want 1", len(out))
	}
}

func TestBoundMetadataDecodedShrinksOverBound(t *testing.T) {
	metadata := map[string]interface{}{
		"tenant_id": "keep-me", // exempt, removed last
		"big1":      strings.Repeat("a", 40000),
		"big2":      strings.Repeat("b", 40000),
	}
	marker := &sdkMarker{}
	out := boundMetadataDecoded(metadata, marker)
	if _, ok := out["tenant_id"]; !ok {
		t.Error("exempt key must be removed only after every other key")
	}
	if !marker.metadata {
		t.Error("marker.metadata must be set when the shrink ran")
	}
	if _, ok := out[sdkMetadataKeyTruncated]; !ok {
		t.Error("the truncated marker must be written")
	}
}

func TestBoundMetadataDecodedRemovesServerOwnedMarkers(t *testing.T) {
	metadata := map[string]interface{}{
		sdkMetadataKeyTruncated:     "caller value",
		sdkMetadataKeyTruncatedKeys: "caller value",
		"safe":                      "v",
	}
	marker := &sdkMarker{}
	out := boundMetadataDecoded(metadata, marker)
	if _, ok := out[sdkMetadataKeyTruncated]; ok {
		t.Error("a caller value under the truncated key must not survive untouched when nothing was cut")
	}
	if out["safe"] != "v" {
		t.Error("an unrelated key must survive")
	}
}

// TestBoundMetadata is spec §10.2 P1.11's rv11 fix: boundMetadataDecoded's
// fast path (bounds.go:714) used to measure metadata WITHOUT the SDK
// marker, then applyMarker added the marker afterward - so a marker large
// enough on its own (here, step 2's isolation of 50 unmarshallable-value
// keys, which records each removed key's name in the marker's
// truncated_keys list) could push what is actually sent past the server's
// 65,536-byte bound while the fast path's cheaper, marker-less measure
// still took the "nothing to shrink" branch. The server would then have to
// cut the SDK's own event - the one thing spec §3.5's margin exists to
// prevent (spec §10.2 P1.11, sdkgo-rv11).
func TestBoundMetadata(t *testing.T) {
	push := func(t *testing.T, ev *Event) Event {
		t.Helper()
		rs := newRecordingServer(t)
		defer rs.Close()
		cfg := fixedTestConfig(rs.URL)
		tr := newTestTransport(t, cfg)
		return pushOneAndUnmarshal(t, tr, rs, ev)
	}

	naNCase := func(t *testing.T, heavyKeys bool) {
		metadata := map[string]interface{}{"x": strings.Repeat("a", 60000)}
		for i := 0; i < 50; i++ {
			key := fmt.Sprintf("nan%d", i)
			if heavyKeys {
				key = strings.Repeat("<", 64) + fmt.Sprintf("-%d", i)
			}
			metadata[key] = math.NaN()
		}
		ev := NewEvent(Config{ProjectKey: "pk_test", ServerName: "golden-host"}, nil, nil)
		ev.Metadata = metadata

		// "x" is not asserted to survive: with '<'-heavy key names, step 2's
		// marker.truncatedKeysValue() (a comma-joined list of the 50 removed
		// names, each escaping to 6 JSON bytes per '<') is itself large
		// enough that boundMetadataDecoded's shrink loop must evict further
		// non-exempt keys, largest first, to stay under the bound - "x" is
		// the largest remaining candidate. Whether "x" survives or not, the
		// one property spec §10.2 P1.11 requires is what both assertions
		// below check.
		got := push(t, ev)
		for i := 0; i < 50; i++ {
			key := fmt.Sprintf("nan%d", i)
			if heavyKeys {
				key = strings.Repeat("<", 64) + fmt.Sprintf("-%d", i)
			}
			if _, ok := got.Metadata[key]; ok {
				t.Errorf("expected the unmarshallable key %q removed by step 2", key)
			}
		}
		if s := jsonSize(got.Metadata); s > serverMaxObjectSize {
			t.Errorf("sent metadata = %d bytes, want <= %d", s, serverMaxObjectSize)
		}
	}
	t.Run("50 NaN-valued keys removed at step 2, x: 60,000 'a', plain keys", func(t *testing.T) {
		naNCase(t, false)
	})
	t.Run("50 NaN-valued keys removed at step 2, x: 60,000 'a', '<'-heavy keys", func(t *testing.T) {
		naNCase(t, true)
	})

	t.Run("90 contexts with 64-rune '<' names, 58 removed", func(t *testing.T) {
		contexts := make(map[string]map[string]interface{}, 90)
		for i := 0; i < 90; i++ {
			name := fmt.Sprintf("%s-%d", strings.Repeat("<", 64), i)
			contexts[name] = map[string]interface{}{"v": strings.Repeat("a", 700)}
		}
		ev := NewEvent(Config{ProjectKey: "pk_test", ServerName: "golden-host"}, nil, nil)
		ev.Contexts = contexts

		got := push(t, ev)
		if s := jsonSize(got.Contexts); s > serverMaxObjectSize {
			t.Errorf("sent contexts = %d bytes, want <= %d", s, serverMaxObjectSize)
		}
		if len(got.Contexts) >= 90 {
			t.Errorf("expected some of the 90 contexts evicted over the size bound, got %d survive", len(got.Contexts))
		}
	})
}

func TestBoundRequestDataDecodedDropsOverBound(t *testing.T) {
	small := map[string]interface{}{"a": "b"}
	if got := boundRequestDataDecoded(small); got == nil {
		t.Error("small request.data must survive")
	}
	big := map[string]interface{}{"a": strings.Repeat("x", 20000)}
	if got := boundRequestDataDecoded(big); got != nil {
		t.Error("over-16KiB request.data must be dropped to nil, not truncated")
	}
}

func TestBoundBreadcrumbDataDecodedDropsOverBound(t *testing.T) {
	big := map[string]interface{}{"a": strings.Repeat("x", 5000)}
	if got := boundBreadcrumbDataDecoded(big); got != nil {
		t.Error("over-4KiB crumb data must be dropped to nil")
	}
}

func TestBoundBreadcrumbListSizeTrimsOldest(t *testing.T) {
	crumbs := make([]Breadcrumb, 300)
	data := make([]map[string]interface{}, 300)
	for i := range crumbs {
		crumbs[i] = Breadcrumb{Message: strings.Repeat("m", 8100)}
	}
	trimmed, _ := boundBreadcrumbListSize(crumbs, data)
	if len(trimmed) >= len(crumbs) {
		t.Errorf("expected some crumbs trimmed, got %d of %d", len(trimmed), len(crumbs))
	}
	// The kept crumbs must be the newest (oldest first in the slice).
	if len(trimmed) > 0 && trimmed[0].Message != crumbs[len(crumbs)-len(trimmed)].Message {
		t.Error("expected the oldest crumbs dropped, newest kept")
	}
}

func TestNulDeepStripsAtEveryDepth(t *testing.T) {
	v := map[string]interface{}{
		"a\x00b": []interface{}{
			map[string]interface{}{"c\x00d": "e\x00f"},
		},
	}
	out := nulDeep(v).(map[string]interface{})
	if _, ok := out["ab"]; !ok {
		t.Fatal("top-level key must be NUL-stripped")
	}
	inner := out["ab"].([]interface{})[0].(map[string]interface{})
	if inner["cd"] != "ef" {
		t.Errorf("nested key/value must be NUL-stripped, got %+v", inner)
	}
}
