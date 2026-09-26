// This file holds spec §10.2 P1.11's near-limit body-cap tests: several
// build and marshal single- and multi-megabyte-scale event/batch bodies (up
// to ~5,242,880 bytes) to exercise the exact ingestor caps byte-for-byte. It
// used to carry a `//go:build !race` constraint after two attempts at this
// brief were OOM-killed running it under -race (21.4 GB and 17.9 GB RSS -
// -race's shadow memory and extra bookkeeping multiply an already
// megabyte-scale working set past what the host could give it); that made
// every proof here run in no gate, since sdk-go's only CI and Taskfile unit
// gates run `go test -race`. The OOM cause was a helper's unbounded probe
// ceiling, fixed since, so the constraint is gone and this file now runs
// under -race like the rest of the package (measured 242 MB RSS / 228s).

package sentinel

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// measureBeforeShed returns the one-element array size and publish cost
// spec §3.5 step 4 would measure for ev, before any shedding runs - so a
// case can size its construction against the real thresholds instead of
// guessing (spec §10.2 P1.11).
func measureBeforeShed(t *testing.T, ev *Event, cfg Config) (arraySize, cost int) {
	t.Helper()
	b, mirror, raw, err := boundAndMeasure(ev, cfg)
	if err != nil {
		t.Fatalf("boundAndMeasure: %v", err)
	}
	return len(raw) + 2, publishCost(b, mirror)
}

// heavyHeaders returns n headers of a rune repeated width times.
func heavyHeaders(n int, r rune, width int) map[string]string {
	headers := make(map[string]string, n)
	for i := 0; i < n; i++ {
		headers[headerName(i)] = strings.Repeat(string(r), width)
	}
	return headers
}

// exactBodyEventViaHeaders builds a golden-fixed event whose bound-but-unshed
// one-element raw JSON body is exactly targetBytes, padded with '<' request
// headers (escaped to 6 JSON bytes each, spec §3.5 T39/T40) rather than a
// single field's raw length: '<' headers inflate array size (this
// function's only concern) far more than publish cost, so padding this way
// keeps publish cost well under maxPublishBytes and the event un-shed and
// un-dropped by prepareEvent's step 4 - unlike padding with plain frame text
// (near enough 1:1 in both array size and publish cost to push a
// megabyte-scale target over maxPublishBytes and get the whole event
// dropped instead of joined) or Metadata (shrunk at sdkMaxMetadataSize
// before this size is ever reached).
//
// Full '<' headers (each sdkMaxRequestHeaderValueLen runes, escaping to
// sdkMaxRequestHeaderValueLen*6 bytes) get exactBodyEventViaHeaders within
// one header's worth of targetBytes; one final header of plain 'a' bytes (no
// escaping, 1 JSON byte per rune) then closes the exact remaining gap by
// binary search, capped by the same value-length bound so it can only be
// found, never overshoot into a runaway allocation.
func exactBodyEventViaHeaders(t *testing.T, cfg Config, targetBytes, goldenN int) *Event {
	t.Helper()

	build := func(fullHeaders int, fill string) *Event {
		headers := heavyHeaders(fullHeaders, '<', sdkMaxRequestHeaderValueLen)
		headers["X-Zz-Fill"] = fill
		ev := NewEvent(Config{ProjectKey: "pk_test", ServerName: "golden-host"}, nil, nil)
		ev.Request = &RequestInfo{Method: "GET", URL: "http://api.example.test/items/42", Headers: headers}
		fixEventForGolden(ev, goldenN)
		return ev
	}
	rawLen := func(fullHeaders int, fill string) int {
		_, _, raw, err := boundAndMeasure(build(fullHeaders, fill), cfg)
		if err != nil {
			t.Fatalf("boundAndMeasure: %v", err)
		}
		return len(raw)
	}

	// Grow fullHeaders (each contributing a fixed, measured jump - one
	// sdkMaxRequestHeaderValueLen '<' header, escaped 6 bytes/rune) until the
	// present-but-empty X-Zz-Fill header's own baseline would exceed target;
	// that fixes fullHeaders so the remaining gap fits inside what the one
	// fill header's value (capped at sdkMaxRequestHeaderValueLen runes) can
	// still cover.
	fullHeaders := 0
	for rawLen(fullHeaders+1, "") <= targetBytes {
		fullHeaders++
		if fullHeaders > sdkMaxRequestHeaders {
			t.Fatalf("exactBodyEventViaHeaders: target %d unreachable within %d headers", targetBytes, sdkMaxRequestHeaders)
		}
	}
	base := rawLen(fullHeaders, "")
	remainder := targetBytes - base
	if remainder < 0 || remainder > sdkMaxRequestHeaderValueLen*6 {
		t.Fatalf("exactBodyEventViaHeaders: remainder %d out of the one fill header's [0, %d] range (base %d, target %d)", remainder, sdkMaxRequestHeaderValueLen*6, base, targetBytes)
	}
	// Fill = k '<' (6 escaped bytes each) then m plain 'a' (1 byte each),
	// 6k+m = remainder exactly, k+m capped at sdkMaxRequestHeaderValueLen.
	k := remainder / 6
	m := remainder - 6*k
	if k+m > sdkMaxRequestHeaderValueLen {
		t.Fatalf("exactBodyEventViaHeaders: fill length %d over the %d-rune header cap", k+m, sdkMaxRequestHeaderValueLen)
	}
	fill := strings.Repeat("<", k) + strings.Repeat("a", m)

	ev := build(fullHeaders, fill)
	if got := rawLen(fullHeaders, fill); got != targetBytes {
		t.Fatalf("exactBodyEventViaHeaders: body = %d bytes, want exactly %d", got, targetBytes)
	}
	return ev
}

// TestLoneEventOverOneMiBSentWhole is P1.11: an event whose request has 64
// headers of 4,096 '<' - over 1,048,576 bytes marshalled (each '<' escapes
// to <, 6 bytes) but under maxBatchBodyBytes, and with a publish cost
// (escaping-off: '<' costs 1 byte) well under maxPublishBytes - pushed alone:
// nothing shed (spec §10.2 P1.11, T38).
func TestLoneEventOverOneMiBSentWhole(t *testing.T) {
	rs := newRecordingServer(t)
	defer rs.Close()
	cfg := fixedTestConfig(rs.URL)
	tr := newTestTransport(t, cfg)

	ev := NewEvent(Config{ProjectKey: "pk_test", ServerName: "golden-host"}, nil, nil)
	ev.Request = &RequestInfo{Method: "GET", URL: "http://api.example.test/items/42", Headers: heavyHeaders(64, '<', 4096)}
	fixEventForGolden(ev, 40)

	arraySize, cost := measureBeforeShed(t, ev, cfg)
	if arraySize <= 1_048_576 {
		t.Fatalf("test setup: one-element array must be over 1 MiB, got %d", arraySize)
	}
	if arraySize > maxBatchBodyBytes {
		t.Fatalf("test setup: one-element array must be under %d, got %d", maxBatchBodyBytes, arraySize)
	}
	if cost > maxPublishBytes {
		t.Fatalf("test setup: publish cost must be <= %d, got %d", maxPublishBytes, cost)
	}

	got := pushOneAndUnmarshal(t, tr, rs, ev)
	if len(got.Request.Headers) != 64 {
		t.Errorf("expected all 64 headers intact, got %d", len(got.Request.Headers))
	}
	if tr.droppedCount != 0 {
		t.Errorf("droppedCount = %d, want 0", tr.droppedCount)
	}
}

// TestLoneEventOverPublishLimitShedsHeaders is P1.11's counterpart to
// TestLoneEventOverOneMiBSentWhole: the same shape with 64 headers of 4,096
// U+1F600 (4 bytes in JSON and in the publish-cost estimate alike, so the
// headers alone push the cost over maxPublishBytes while the one-element
// array stays under maxBatchBodyBytes) - the headers are shed, method and
// url survive, and the pushed *Event is untouched (spec §10.2 P1.11, T39).
func TestLoneEventOverPublishLimitShedsHeaders(t *testing.T) {
	rs := newRecordingServer(t)
	defer rs.Close()
	cfg := fixedTestConfig(rs.URL)
	tr := newTestTransport(t, cfg)

	ev := NewEvent(Config{ProjectKey: "pk_test", ServerName: "golden-host"}, nil, nil)
	ev.Request = &RequestInfo{Method: "GET", URL: "http://api.example.test/items/42", Headers: heavyHeaders(64, '\U0001F600', 4096)}
	fixEventForGolden(ev, 41)

	arraySize, cost := measureBeforeShed(t, ev, cfg)
	if cost <= maxPublishBytes {
		t.Fatalf("test setup: publish cost must be over %d, got %d", maxPublishBytes, cost)
	}
	if arraySize > maxBatchBodyBytes {
		t.Fatalf("test setup: one-element array must be under %d, got %d", maxBatchBodyBytes, arraySize)
	}

	got := pushOneAndUnmarshal(t, tr, rs, ev)
	if len(got.Request.Headers) != 0 {
		t.Errorf("expected headers shed, got %d", len(got.Request.Headers))
	}
	if got.Request.Method != "GET" || got.Request.URL != "http://api.example.test/items/42" {
		t.Errorf("expected method/url kept, got %+v", got.Request)
	}
	if tr.droppedCount != 0 {
		t.Errorf("droppedCount = %d, want 0", tr.droppedCount)
	}
	if len(ev.Request.Headers) != 64 {
		t.Errorf("expected the pushed *Event to still hold its 64 headers (shed never writes it), got %d", len(ev.Request.Headers))
	}
}

// TestFourHeaderHeavyEventsCutNotShed is P1.11: four TestLoneEventOverOneMiBSentWhole-shaped
// events (~1.57 MB one-element array each) batched together join to over
// maxBatchBodyBytes - cut into a body of 3 and a body of 1, never shed (spec
// §10.2 P1.11, T38, the sdkgo-rv1 fix).
func TestFourHeaderHeavyEventsCutNotShed(t *testing.T) {
	rs := newRecordingServer(t)
	defer rs.Close()
	cfg := fixedTestConfig(rs.URL)
	cfg.BatchSize = 4
	tr := newTestTransport(t, cfg)

	for i := 0; i < 4; i++ {
		ev := NewEvent(Config{ProjectKey: "pk_test", ServerName: "golden-host"}, nil, nil)
		ev.Request = &RequestInfo{Method: "GET", URL: "http://api.example.test/items/42", Headers: heavyHeaders(64, '<', 4096)}
		fixEventForGolden(ev, 42+i)
		tr.Push(ev)
	}
	if !tr.Flush(5 * time.Second) {
		t.Fatal("flush timed out")
	}

	if len(rs.bodies) != 2 {
		t.Fatalf("expected 2 bodies (a cut of 3 then 1), got %d", len(rs.bodies))
	}
	if len(rs.bodies[0]) != 3 {
		t.Errorf("first body: %d events, want 3", len(rs.bodies[0]))
	}
	if len(rs.bodies[1]) != 1 {
		t.Errorf("second body: %d events, want 1", len(rs.bodies[1]))
	}
	for bi, body := range rs.bodies {
		for ei, raw := range body {
			var got Event
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("unmarshal body %d event %d: %v", bi, ei, err)
			}
			if len(got.Request.Headers) != 64 {
				t.Errorf("body %d event %d: expected 64 headers intact (cut, not shed), got %d", bi, ei, len(got.Request.Headers))
			}
		}
	}
	if tr.droppedCount != 0 {
		t.Errorf("droppedCount = %d, want 0", tr.droppedCount)
	}
}

// TestOversizedEventShedsHeaders is P1.11: one event whose one-element array
// is over maxBatchBodyBytes WITH its 64 heavy headers but at most without
// them, with a small request.data, one breadcrumb, small metadata and a
// padded frame besides - the headers are shed and everything else survives
// (spec §10.2 P1.11).
func TestOversizedEventShedsHeaders(t *testing.T) {
	rs := newRecordingServer(t)
	defer rs.Close()
	cfg := fixedTestConfig(rs.URL)
	tr := newTestTransport(t, cfg)

	build := func(padLen int, withHeaders bool) *Event {
		ev := NewEvent(Config{ProjectKey: "pk_test", ServerName: "golden-host"}, nil, map[string]interface{}{"k": "v"})
		frames := make([]Frame, 100)
		for i := range frames {
			frames[i] = Frame{File: "x.go", Function: "f", Line: 1, InApp: true}
		}
		frames[0].Function = strings.Repeat("<", padLen)
		ev.Stacktrace = frames
		ev.Request = &RequestInfo{Method: "GET", URL: "http://api.example.test/items/42", Data: map[string]interface{}{"q": "x"}}
		if withHeaders {
			ev.Request.Headers = heavyHeaders(64, '<', 4096)
		}
		ev.Breadcrumbs = []Breadcrumb{{Message: "m", Category: "test"}}
		fixEventForGolden(ev, 50)
		ev.Stacktrace = frames // fixEventForGolden overwrites Stacktrace; restore
		return ev
	}

	padLen := solveCountForTarget(maxBatchBodyBytes+1, func(n int) int {
		arraySize, _ := measureBeforeShed(t, build(n, true), cfg)
		return arraySize
	})
	ev := build(padLen, true)
	arraySize, cost := measureBeforeShed(t, ev, cfg)
	if arraySize <= maxBatchBodyBytes {
		t.Fatalf("test setup: with headers, one-element array must be over %d, got %d", maxBatchBodyBytes, arraySize)
	}
	if cost > maxPublishBytes {
		t.Fatalf("test setup: publish cost must be <= %d, got %d", maxPublishBytes, cost)
	}
	withoutHeaders := build(padLen, false)
	arraySizeNoHeaders, costNoHeaders := measureBeforeShed(t, withoutHeaders, cfg)
	if arraySizeNoHeaders > maxBatchBodyBytes {
		t.Fatalf("test setup: without headers, one-element array must be <= %d, got %d", maxBatchBodyBytes, arraySizeNoHeaders)
	}
	if costNoHeaders > maxPublishBytes {
		t.Fatalf("test setup: publish cost without headers must be <= %d, got %d", maxPublishBytes, costNoHeaders)
	}

	got := pushOneAndUnmarshal(t, tr, rs, ev)
	if len(got.Request.Headers) != 0 {
		t.Errorf("expected headers shed, got %d", len(got.Request.Headers))
	}
	if got.Request.Method != "GET" || got.Request.URL != "http://api.example.test/items/42" {
		t.Errorf("expected method/url kept, got %+v", got.Request)
	}
	if got.Request.Data == nil {
		t.Error("expected request.data kept")
	}
	if len(got.Breadcrumbs) != 1 {
		t.Errorf("expected the breadcrumb kept, got %d", len(got.Breadcrumbs))
	}
	if got.Metadata["k"] != "v" {
		t.Errorf("expected metadata kept, got %v", got.Metadata)
	}
	if len(got.Stacktrace) != 100 {
		t.Errorf("expected all 100 frames kept, got %d", len(got.Stacktrace))
	}
	if tr.droppedCount != 0 {
		t.Errorf("droppedCount = %d, want 0", tr.droppedCount)
	}
	if len(ev.Request.Headers) != 64 {
		t.Errorf("expected the pushed *Event to still hold its 64 headers, got %d", len(ev.Request.Headers))
	}
}

// TestNumberDenseCrumbsShed is P1.11 (T40): metadata {a: 30,000 zeros} stays
// under sdkMaxMetadataSize (60,007 bytes of JSON), so it is kept whole, while
// 32 crumbs of data {a: 2,030 zeros} (§3.4 keeps 31) inflate far more in the
// publish-cost estimate than in JSON - a number costs ~11 bytes as a Struct
// Value against ~2 as JSON (T40) - pushing the event over maxPublishBytes on
// crumbs alone; shedding them (not metadata) brings it back under (spec
// §10.2 P1.11).
func TestNumberDenseCrumbsShed(t *testing.T) {
	rs := newRecordingServer(t)
	defer rs.Close()
	cfg := fixedTestConfig(rs.URL)
	tr := newTestTransport(t, cfg)

	ev := NewEvent(Config{ProjectKey: "pk_test", ServerName: "golden-host"}, nil, map[string]interface{}{"a": zerosList(30000)})
	ev.Breadcrumbs = make([]Breadcrumb, 32)
	for i := range ev.Breadcrumbs {
		ev.Breadcrumbs[i] = Breadcrumb{Message: "m", Category: "test", Data: map[string]interface{}{"a": zerosList(2030)}}
	}
	fixEventForGolden(ev, 70)

	arraySize, cost := measureBeforeShed(t, ev, cfg)
	if cost <= maxPublishBytes {
		t.Fatalf("test setup: publish cost must be over %d with the crumbs, got %d", maxPublishBytes, cost)
	}
	if arraySize > maxBatchBodyBytes {
		t.Fatalf("test setup: one-element array must be under %d, got %d", maxBatchBodyBytes, arraySize)
	}
	if arraySize >= 200_000 {
		t.Fatalf("test setup: expected the escaping-off JSON size under 200,000, got %d (round 7's measure sent it whole)", arraySize)
	}

	got := pushOneAndUnmarshal(t, tr, rs, ev)
	if len(got.Breadcrumbs) != 0 {
		t.Errorf("expected breadcrumbs shed, got %d", len(got.Breadcrumbs))
	}
	a, _ := got.Metadata["a"].([]interface{})
	if len(a) != 30000 {
		t.Errorf("expected metadata.a kept whole (30,000 elements), got %d", len(a))
	}
	if tr.droppedCount != 0 {
		t.Errorf("droppedCount = %d, want 0", tr.droppedCount)
	}
}

// TestMetadataAndContextsShrunkNotShed is P1.11 (spec review round 8): a
// caller value the SDK's own client-side shrink (boundMetadataDecoded /
// boundContextsDecoded, spec §3.4) removes before step 4 ever runs, so
// nothing is shed at the transport level - only the truncated/truncated_keys
// marker records it, and everything else the event carried survives intact.
func TestMetadataAndContextsShrunkNotShed(t *testing.T) {
	t.Run("metadata: debug_dump removed, request_id/headers/crumb kept", func(t *testing.T) {
		rs := newRecordingServer(t)
		defer rs.Close()
		cfg := fixedTestConfig(rs.URL)
		tr := newTestTransport(t, cfg)

		ev := NewEvent(Config{ProjectKey: "pk_test", ServerName: "golden-host"}, nil, map[string]interface{}{
			"request_id": "req-1",
			"debug_dump": strings.Repeat("x", 1_500_000),
		})
		ev.Request = &RequestInfo{Method: "GET", URL: "http://api.example.test/items/42", Headers: heavyHeaders(64, 'a', 16)}
		ev.Breadcrumbs = []Breadcrumb{{Message: "m", Category: "test"}}
		fixEventForGolden(ev, 80)

		arraySize, cost := measureBeforeShed(t, ev, cfg)
		if arraySize > maxBatchBodyBytes || cost > maxPublishBytes {
			t.Fatalf("test setup: expected nothing over either limit, got arraySize=%d cost=%d", arraySize, cost)
		}
		if cost >= 5000 {
			t.Fatalf("test setup: expected publish cost under 5,000 once debug_dump is shrunk away, got %d", cost)
		}

		got := pushOneAndUnmarshal(t, tr, rs, ev)
		if _, ok := got.Metadata["debug_dump"]; ok {
			t.Error("expected metadata.debug_dump removed")
		}
		if got.Metadata["request_id"] != "req-1" {
			t.Errorf("expected metadata.request_id kept, got %v", got.Metadata["request_id"])
		}
		if len(got.Request.Headers) != 64 {
			t.Errorf("expected all 64 headers kept, got %d", len(got.Request.Headers))
		}
		if len(got.Breadcrumbs) != 1 {
			t.Errorf("expected the breadcrumb kept, got %d", len(got.Breadcrumbs))
		}
		truncated, _ := got.Metadata["truncated"].([]interface{})
		if !sliceContainsStr(truncated, "metadata") {
			t.Errorf("truncated = %v, want it to contain %q", truncated, "metadata")
		}
		keys, _ := got.Metadata["truncated_keys"].([]interface{})
		if !sliceContainsStr(keys, "debug_dump") {
			t.Errorf("truncated_keys = %v, want it to contain %q", keys, "debug_dump")
		}
		if tr.droppedCount != 0 {
			t.Errorf("droppedCount = %d, want 0", tr.droppedCount)
		}
	})

	t.Run("contexts: a well-known context removed for being the largest", func(t *testing.T) {
		rs := newRecordingServer(t)
		defer rs.Close()
		cfg := fixedTestConfig(rs.URL)
		tr := newTestTransport(t, cfg)

		ev := NewEvent(Config{ProjectKey: "pk_test", ServerName: "golden-host"}, nil, nil)
		ev.Contexts["app"] = map[string]interface{}{"state": strings.Repeat("x", 1_200_000)}
		fixEventForGolden(ev, 81)

		arraySize, cost := measureBeforeShed(t, ev, cfg)
		if arraySize > maxBatchBodyBytes || cost > maxPublishBytes {
			t.Fatalf("test setup: expected nothing over either limit, got arraySize=%d cost=%d", arraySize, cost)
		}

		got := pushOneAndUnmarshal(t, tr, rs, ev)
		if _, ok := got.Contexts["app"]; ok {
			t.Error("expected contexts.app removed (nothing else is left to remove)")
		}
		keys, _ := got.Metadata["truncated_keys"].([]interface{})
		if !sliceContainsStr(keys, "contexts.app") {
			t.Errorf("truncated_keys = %v, want it to contain %q", keys, "contexts.app")
		}
		if tr.droppedCount != 0 {
			t.Errorf("droppedCount = %d, want 0", tr.droppedCount)
		}
	})
}

// TestServerCutsNothing is P1.11 (spec review round 10, T40): three events
// carrying exactly what the SDK's own client-side bounds resolve - a name
// collision, U+0000 at depth, oversized metadata - each pushed alone and
// checked against the same real bound the SDK mirrors (the recording
// server's per-element decode and publish-cost check), standing in locally
// for P1.8's root half, which runs the ingestor's own ValidatePayload on the
// committed goldens.
func TestServerCutsNothing(t *testing.T) {
	t.Run("1: collision resolved, oversized metadata removed, headers kept", func(t *testing.T) {
		rs := newRecordingServer(t)
		defer rs.Close()
		cfg := fixedTestConfig(rs.URL)
		tr := newTestTransport(t, cfg)

		ev := NewEvent(Config{ProjectKey: "pk_test", ServerName: "golden-host"}, nil, map[string]interface{}{
			"m": zerosList(32748),
		})
		ev.Contexts["a"] = map[string]interface{}{"v": "x"}
		ev.Contexts["a\x00"] = map[string]interface{}{"v": "y"}
		ev.Request = &RequestInfo{Method: "GET", Headers: heavyHeaders(32, '\U0001F600', 4096)}
		frames := make([]Frame, 100)
		for i := range frames {
			frames[i] = Frame{File: "x.go", Function: strings.Repeat("f", 3000), Line: 1, InApp: true}
		}
		ev.Stacktrace = frames
		fixEventForGolden(ev, 90)
		ev.Stacktrace = frames // fixEventForGolden overwrites Stacktrace; restore

		arraySize, cost := measureBeforeShed(t, ev, cfg)
		if arraySize > maxBatchBodyBytes || cost > maxPublishBytes {
			t.Fatalf("test setup: expected nothing over either limit, got arraySize=%d cost=%d", arraySize, cost)
		}

		got := pushOneAndUnmarshal(t, tr, rs, ev)
		a, ok := got.Contexts["a"]
		if !ok || a["v"] != "x" {
			t.Errorf("expected contexts.a = {v: x}, got %v (present=%v)", a, ok)
		}
		if _, ok := got.Contexts["a\x00"]; ok {
			t.Error("expected the colliding context name absent")
		}
		if _, ok := got.Metadata["m"]; ok {
			t.Error("expected metadata.m removed")
		}
		if len(got.Request.Headers) != 32 {
			t.Errorf("expected all 32 headers kept, got %d", len(got.Request.Headers))
		}
		if tr.droppedCount != 0 {
			t.Errorf("droppedCount = %d, want 0", tr.droppedCount)
		}
	})

	t.Run("2: duplicate empty context names recorded once, metadata kept at the margin", func(t *testing.T) {
		rs := newRecordingServer(t)
		defer rs.Close()
		cfg := fixedTestConfig(rs.URL)
		tr := newTestTransport(t, cfg)

		ev := NewEvent(Config{ProjectKey: "pk_test", ServerName: "golden-host"}, nil, map[string]interface{}{})
		ev.Contexts[""] = map[string]interface{}{"v": 1}
		ev.Contexts["\x00"] = map[string]interface{}{"v": 2}
		fixEventForGolden(ev, 91)

		// Size metadata.m so metadata, WITH the SDK's marker recording
		// "contexts." once, measures exactly at sdkMaxMetadataSize - a bug
		// that records the collision twice would push it over and remove m too.
		padCount := solveCountForTarget(sdkMaxMetadataSize, func(n int) int {
			ev.Metadata["m"] = strings.Repeat("x", n)
			marker := &sdkMarker{}
			marker.metadata = true
			marker.addKey("contexts.")
			return jsonSize(marker.applyMarker(ev.Metadata))
		})
		ev.Metadata["m"] = strings.Repeat("x", padCount)

		arraySize, cost := measureBeforeShed(t, ev, cfg)
		if arraySize > maxBatchBodyBytes || cost > maxPublishBytes {
			t.Fatalf("test setup: expected nothing over either limit, got arraySize=%d cost=%d", arraySize, cost)
		}

		got := pushOneAndUnmarshal(t, tr, rs, ev)
		if _, ok := got.Contexts[""]; ok {
			t.Error("expected the empty context name absent")
		}
		if _, ok := got.Contexts["\x00"]; ok {
			t.Error("expected the U+0000 context name absent")
		}
		if _, ok := got.Metadata["m"]; !ok {
			t.Error("expected metadata.m kept (recording the collision once must not push metadata over its bound)")
		}
		keys, _ := got.Metadata["truncated_keys"].([]interface{})
		if len(keys) != 1 || keys[0] != "contexts." {
			t.Errorf("truncated_keys = %v, want [contexts.]", keys)
		}
		if tr.droppedCount != 0 {
			t.Errorf("droppedCount = %d, want 0", tr.droppedCount)
		}
	})

	t.Run("3: request.data stripped of U+0000, oversized metadata removed", func(t *testing.T) {
		rs := newRecordingServer(t)
		defer rs.Close()
		cfg := fixedTestConfig(rs.URL)
		tr := newTestTransport(t, cfg)

		ev := NewEvent(Config{ProjectKey: "pk_test", ServerName: "golden-host"}, nil, map[string]interface{}{
			"m": zerosList(32764),
		})
		ev.Request = &RequestInfo{Method: "GET", Data: map[string]interface{}{"q": "a\x00"}}
		frames := make([]Frame, 100)
		for i := range frames {
			frames[i] = Frame{File: "x.go", Function: strings.Repeat("f", 6500), Line: 1, InApp: true}
		}
		ev.Stacktrace = frames
		fixEventForGolden(ev, 92)
		ev.Stacktrace = frames // fixEventForGolden overwrites Stacktrace; restore

		arraySize, cost := measureBeforeShed(t, ev, cfg)
		if arraySize > maxBatchBodyBytes || cost > maxPublishBytes {
			t.Fatalf("test setup: expected nothing over either limit, got arraySize=%d cost=%d", arraySize, cost)
		}

		got := pushOneAndUnmarshal(t, tr, rs, ev)
		if got.Request.Data == nil {
			t.Fatal("expected request.data kept")
		}
		data, _ := got.Request.Data.(map[string]interface{})
		if data["q"] != "a" {
			t.Errorf("request.data.q = %v, want \"a\" (U+0000 stripped)", data["q"])
		}
		if _, ok := got.Metadata["m"]; ok {
			t.Error("expected metadata.m removed")
		}
		if tr.droppedCount != 0 {
			t.Errorf("droppedCount = %d, want 0", tr.droppedCount)
		}
	})

	t.Run("4: contexts trimmed to the real 61,440-byte bound, not the SDK's 65,536-byte margin", func(t *testing.T) {
		rs := newRecordingServer(t)
		defer rs.Close()
		cfg := fixedTestConfig(rs.URL)
		tr := newTestTransport(t, cfg)

		ev := NewEvent(Config{ProjectKey: "pk_test", ServerName: "golden-host"}, nil, nil)
		// Size the non-well-known context "a" so contexts, as a whole,
		// land at 62,000 bytes: over sdkMaxContextsSize (61,440, the
		// server's 65,536-byte bound less the SDK's 4,096-byte margin) but
		// under the server's own 65,536-byte bound - the exact window in
		// which a shrink that measured against the server's raw bound,
		// instead of the SDK's margin, would wrongly leave "a" in place
		// (spec §3.5, "The margin").
		pad := solveCountForTarget(62000, func(n int) int {
			ev.Contexts["a"] = map[string]interface{}{"v": strings.Repeat("x", n)}
			return jsonSize(ev.Contexts)
		})
		ev.Contexts["a"] = map[string]interface{}{"v": strings.Repeat("x", pad)}
		fixEventForGolden(ev, 93)

		arraySize, cost := measureBeforeShed(t, ev, cfg)
		if arraySize > maxBatchBodyBytes || cost > maxPublishBytes {
			t.Fatalf("test setup: expected nothing over either limit, got arraySize=%d cost=%d", arraySize, cost)
		}

		got := pushOneAndUnmarshal(t, tr, rs, ev)
		if _, ok := got.Contexts["a"]; ok {
			t.Error("expected contexts.a removed (over the real 61,440-byte contexts bound)")
		}
		if tr.droppedCount != 0 {
			t.Errorf("droppedCount = %d, want 0", tr.droppedCount)
		}
	})
}

// padToExactSize finds a '<' count and a top-up ascii-'a' count so that
// build(clen, alen) is exactly target: '<' moves the size in steps of 6
// bytes (its escaped form), so once solveCountForTarget finds the minimal
// '<' count that reaches or passes target, backing off by one '<' (6 bytes)
// and topping up with 0-5 ascii 'a' (1 byte each) lands exactly on it (spec
// §10.2 P1.11, sdkgo-rc11).
func padToExactSize(t *testing.T, target int, build func(clen, alen int) int) (clen, alen int) {
	t.Helper()
	clen = solveCountForTarget(target, func(n int) int { return build(n, 0) })
	if build(clen, 0) == target {
		return clen, 0
	}
	clen--
	base := build(clen, 0)
	alen = target - base
	if alen < 0 || alen > 6 {
		t.Fatalf("padToExactSize: could not hit %d exactly (base=%d, alen=%d)", target, base, alen)
	}
	return clen, alen
}

// TestLoneEventWithHeadersExactlyAtCapSentWhole is P1.11's boundary case: an
// event whose one-element array is exactly maxBatchBodyBytes must be sent
// whole, not shed - proving the check is "<=", not "<" (spec §10.2 P1.11,
// sdkgo-rc11's exact-boundary note).
func TestLoneEventWithHeadersExactlyAtCapSentWhole(t *testing.T) {
	rs := newRecordingServer(t)
	defer rs.Close()
	cfg := fixedTestConfig(rs.URL)
	tr := newTestTransport(t, cfg)

	build := func(clen, alen int) *Event {
		ev := NewEvent(Config{ProjectKey: "pk_test", ServerName: "golden-host"}, nil, nil)
		ev.Request = &RequestInfo{Method: "GET", URL: "http://api.example.test/items/42", Headers: heavyHeaders(25, '<', 4000)}
		frames := make([]Frame, 100)
		for i := range frames {
			frames[i] = Frame{File: "x.go", Function: "f", Line: 1, InApp: true}
		}
		frames[0].Function = strings.Repeat("<", clen) + strings.Repeat("a", alen)
		ev.Stacktrace = frames
		fixEventForGolden(ev, 95)
		ev.Stacktrace = frames // fixEventForGolden overwrites Stacktrace; restore
		return ev
	}

	clen, alen := padToExactSize(t, maxBatchBodyBytes, func(c, a int) int {
		sz, _ := measureBeforeShed(t, build(c, a), cfg)
		return sz
	})
	ev := build(clen, alen)
	arraySize, cost := measureBeforeShed(t, ev, cfg)
	if arraySize != maxBatchBodyBytes {
		t.Fatalf("test setup: want exactly %d, got %d", maxBatchBodyBytes, arraySize)
	}
	if cost > maxPublishBytes {
		t.Fatalf("test setup: publish cost must be <= %d, got %d", maxPublishBytes, cost)
	}

	got := pushOneAndUnmarshal(t, tr, rs, ev)
	if len(got.Request.Headers) != 25 {
		t.Errorf("expected all 25 headers intact at the exact %d-byte boundary, got %d", maxBatchBodyBytes, len(got.Request.Headers))
	}
	if tr.droppedCount != 0 {
		t.Errorf("droppedCount = %d, want 0", tr.droppedCount)
	}
}

// TestLoneEventBackslashHeadersSentWhole is P1.11's rc14 case 1: an event
// with 64 headers of 4,096 backslashes must be sent whole, not shed, because
// its publish cost (once the escape-overcount is fixed) fits under
// 1,000,000 bytes even though the naively-escaped count would not (spec §3,
// the sdkgo-rc14 note).
func TestLoneEventBackslashHeadersSentWhole(t *testing.T) {
	rs := newRecordingServer(t)
	defer rs.Close()

	tr := newTestTransport(t, fixedTestConfig(rs.URL))

	headers := make(map[string]string, 64)
	for i := 0; i < 64; i++ {
		headers[headerName(i)] = strings.Repeat(`\`, 4096)
	}
	// 30 number-dense crumbs pad publishCost close enough to maxPublishBytes
	// that the rc14 escape-overcount alone (64*4096 = 262,144 bytes, from
	// treating each backslash's "\\" as 2 bytes instead of the 1 the
	// escaping-off/protobuf estimate actually costs it) is what stands
	// between "sent whole" and "headers shed": remove the excess
	// subtraction in publishCostPart1 and this crosses maxPublishBytes,
	// which is what makes this test discriminate rather than merely assert
	// a wide-margin pass (bodycap_test.go, the rv2 note).
	crumbs := make([]Breadcrumb, 30)
	for i := range crumbs {
		data := make(map[string]interface{}, 1)
		nums := make([]interface{}, 2000)
		for j := range nums {
			nums[j] = 0
		}
		data["rows"] = nums
		crumbs[i] = Breadcrumb{Message: "m", Data: data}
	}

	ev := NewEvent(Config{ProjectKey: "pk_test", ServerName: "golden-host"}, nil, nil)
	ev.Request = &RequestInfo{Method: "GET", URL: "http://api.example.test/items/42", Headers: headers}
	ev.Breadcrumbs = crumbs
	fixEventForGolden(ev, 1)

	b := boundEvent(ev, 50)
	cost := publishCost(b, &decodeMirror{})
	if cost > maxPublishBytes {
		t.Fatalf("publishCost = %d, want <= %d (the bounded event, before send)", cost, maxPublishBytes)
	}

	tr.Push(ev)
	if !tr.Flush(5 * time.Second) {
		t.Fatal("flush timed out")
	}
	if got := len(rs.bodies); got != 1 {
		t.Fatalf("expected 1 body, got %d", got)
	}
	if got := len(rs.bodies[0]); got != 1 {
		t.Fatalf("expected a one-element array, got %d", got)
	}
	var got Event
	if err := json.Unmarshal(rs.bodies[0][0], &got); err != nil {
		t.Fatalf("unmarshal received event: %v", err)
	}
	if len(got.Request.Headers) != 64 {
		t.Errorf("expected all 64 headers intact, got %d", len(got.Request.Headers))
	}
	if tr.droppedCount != 0 {
		t.Errorf("droppedCount = %d, want 0", tr.droppedCount)
	}
}

// TestNewEventPathControlCharHeadersSentWhole is P1.11's rc14 case 2: 64
// headers of 4,096 U+0001 (each 6 bytes marshalled, so the array is over 1
// MiB but the publish cost, escaping-aware, fits under 1,000,000).
func TestNewEventPathControlCharHeadersSentWhole(t *testing.T) {
	rs := newRecordingServer(t)
	defer rs.Close()

	tr := newTestTransport(t, fixedTestConfig(rs.URL))

	headers := make(map[string]string, 64)
	for i := 0; i < 64; i++ {
		headers[headerName(i)] = strings.Repeat("\x01", 4096)
	}
	ev := NewEvent(Config{ProjectKey: "pk_test", ServerName: "golden-host"}, nil, nil)
	ev.Request = &RequestInfo{Method: "GET", Headers: headers}
	fixEventForGolden(ev, 2)

	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(raw)+2 <= 1_048_576 {
		t.Fatalf("test setup: one-element array must be over 1 MiB, got %d", len(raw)+2)
	}
	if len(raw)+2 > maxBatchBodyBytes {
		t.Fatalf("test setup: one-element array must be under 5 MiB, got %d", len(raw)+2)
	}
	b := boundEvent(ev, 50)
	if cost := publishCost(b, &decodeMirror{}); cost > maxPublishBytes {
		t.Fatalf("publishCost = %d, want <= %d", cost, maxPublishBytes)
	}

	tr.Push(ev)
	if !tr.Flush(5 * time.Second) {
		t.Fatal("flush timed out")
	}
	if got := len(rs.bodies); got != 1 || len(rs.bodies[0]) != 1 {
		t.Fatalf("expected one body of one event, got %d bodies", len(rs.bodies))
	}
	var got Event
	if err := json.Unmarshal(rs.bodies[0][0], &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Request.Headers) != 64 {
		t.Errorf("expected all 64 headers intact, got %d", len(got.Request.Headers))
	}
	if tr.droppedCount != 0 {
		t.Errorf("droppedCount = %d, want 0", tr.droppedCount)
	}
}

// shedOrderBuild builds a TestShedOrder case: 100 frames (one padded with
// padLen '<' characters), and headers/data/breadcrumbs present or absent per
// the flags - the same shape at every step, so removing one field is the
// only thing that changes between measurements.
func shedOrderBuild(padLen int, headers, data, crumbs bool) *Event {
	ev := NewEvent(Config{ProjectKey: "pk_test", ServerName: "golden-host"}, nil, nil)
	frames := make([]Frame, 100)
	for i := range frames {
		frames[i] = Frame{File: "x.go", Function: "f", Line: 1, InApp: true}
	}
	frames[0].Function = strings.Repeat("<", padLen)
	ev.Stacktrace = frames
	ev.Request = &RequestInfo{Method: "GET", URL: "http://api.example.test/items/42"}
	if headers {
		// 25 headers of 4,000 '<': small enough that removing them still
		// leaves room, under the publish-cost budget, for data/crumbs/pad to
		// still be over the array cap in cases (b) and (c).
		ev.Request.Headers = heavyHeaders(25, '<', 4000)
	}
	if data {
		// ~2,700 '<' characters lands request.data's own JSON near its
		// 16,384-byte cap (6 bytes an escaped '<') without exceeding it -
		// over, it would be dropped by boundRequestDataDecoded before ever
		// reaching step 4, proving nothing about shedding.
		ev.Request.Data = map[string]interface{}{"d": strings.Repeat("<", 2700)}
	}
	if crumbs {
		// 5 crumbs of 8,192 '<' (sdkMaxBreadcrumbMessageLen, so none is
		// truncated) push well past the 128 KiB list bound; boundBreadcrumbListSize
		// keeps only as many as fit, so this always lands near the cap
		// regardless of how many are pushed above it.
		cr := make([]Breadcrumb, 5)
		for i := range cr {
			cr[i] = Breadcrumb{Message: strings.Repeat("<", 8192), Category: "test"}
		}
		ev.Breadcrumbs = cr
	}
	fixEventForGolden(ev, 60)
	ev.Stacktrace = frames // fixEventForGolden overwrites Stacktrace with a 1-frame golden list; restore
	return ev
}

// TestShedOrder proves the full last-resort shed order of spec §3.5 step 4:
// (a) request.headers/env, (b) request.data, (c) breadcrumbs, (d) drop with
// OnError - each case padded so exactly one more step is needed than the
// last (spec §10.2 P1.11).
func TestShedOrder(t *testing.T) {
	const limit = maxBatchBodyBytes

	t.Run("a: headers shed, nothing else needed", func(t *testing.T) {
		rs := newRecordingServer(t)
		defer rs.Close()
		cfg := fixedTestConfig(rs.URL)
		tr := newTestTransport(t, cfg)

		pad := solveCountForTarget(limit+1, func(n int) int {
			sz, _ := measureBeforeShed(t, shedOrderBuild(n, true, false, false), cfg)
			return sz
		})
		ev := shedOrderBuild(pad, true, false, false)
		sz, cost := measureBeforeShed(t, ev, cfg)
		if sz <= limit {
			t.Fatalf("setup: want over %d with headers, got %d", limit, sz)
		}
		if cost > maxPublishBytes {
			t.Fatalf("setup: publish cost over %d, got %d", maxPublishBytes, cost)
		}
		if szNo, _ := measureBeforeShed(t, shedOrderBuild(pad, false, false, false), cfg); szNo > limit {
			t.Fatalf("setup: want at most %d without headers, got %d", limit, szNo)
		}

		got := pushOneAndUnmarshal(t, tr, rs, ev)
		if len(got.Request.Headers) != 0 {
			t.Errorf("expected headers shed, got %d", len(got.Request.Headers))
		}
		if tr.droppedCount != 0 {
			t.Errorf("droppedCount = %d, want 0", tr.droppedCount)
		}
	})

	t.Run("b: headers and data shed, breadcrumbs kept", func(t *testing.T) {
		rs := newRecordingServer(t)
		defer rs.Close()
		cfg := fixedTestConfig(rs.URL)
		tr := newTestTransport(t, cfg)

		pad := solveCountForTarget(limit+1, func(n int) int {
			sz, _ := measureBeforeShed(t, shedOrderBuild(n, false, true, true), cfg)
			return sz
		})
		ev := shedOrderBuild(pad, true, true, true)
		if _, cost := measureBeforeShed(t, ev, cfg); cost > maxPublishBytes {
			t.Fatalf("setup: publish cost over %d, got %d", maxPublishBytes, cost)
		}
		szNoHeaders, _ := measureBeforeShed(t, shedOrderBuild(pad, false, true, true), cfg)
		if szNoHeaders <= limit {
			t.Fatalf("setup: want still over %d without headers (data kept), got %d", limit, szNoHeaders)
		}
		szNoHeadersNoData, _ := measureBeforeShed(t, shedOrderBuild(pad, false, false, true), cfg)
		if szNoHeadersNoData > limit {
			t.Fatalf("setup: want at most %d without headers and data, got %d", limit, szNoHeadersNoData)
		}

		got := pushOneAndUnmarshal(t, tr, rs, ev)
		if len(got.Request.Headers) != 0 {
			t.Errorf("expected headers shed, got %d", len(got.Request.Headers))
		}
		if got.Request.Data != nil {
			t.Errorf("expected request.data shed, got %v", got.Request.Data)
		}
		if len(got.Breadcrumbs) == 0 {
			t.Error("expected breadcrumbs kept")
		}
		if tr.droppedCount != 0 {
			t.Errorf("droppedCount = %d, want 0", tr.droppedCount)
		}
	})

	t.Run("c: headers, data and breadcrumbs shed", func(t *testing.T) {
		rs := newRecordingServer(t)
		defer rs.Close()
		cfg := fixedTestConfig(rs.URL)
		tr := newTestTransport(t, cfg)

		pad := solveCountForTarget(limit+1, func(n int) int {
			sz, _ := measureBeforeShed(t, shedOrderBuild(n, false, false, true), cfg)
			return sz
		})
		ev := shedOrderBuild(pad, true, true, true)
		if _, cost := measureBeforeShed(t, ev, cfg); cost > maxPublishBytes {
			t.Fatalf("setup: publish cost over %d, got %d", maxPublishBytes, cost)
		}
		szNoHeadersNoData, _ := measureBeforeShed(t, shedOrderBuild(pad, false, false, true), cfg)
		if szNoHeadersNoData <= limit {
			t.Fatalf("setup: want still over %d with only breadcrumbs left, got %d", limit, szNoHeadersNoData)
		}
		szNothingLeft, _ := measureBeforeShed(t, shedOrderBuild(pad, false, false, false), cfg)
		if szNothingLeft > limit {
			t.Fatalf("setup: want at most %d with nothing left to shed, got %d", limit, szNothingLeft)
		}

		got := pushOneAndUnmarshal(t, tr, rs, ev)
		if len(got.Request.Headers) != 0 {
			t.Errorf("expected headers shed, got %d", len(got.Request.Headers))
		}
		if got.Request.Data != nil {
			t.Errorf("expected request.data shed, got %v", got.Request.Data)
		}
		if len(got.Breadcrumbs) != 0 {
			t.Errorf("expected breadcrumbs shed, got %d", len(got.Breadcrumbs))
		}
		if tr.droppedCount != 0 {
			t.Errorf("droppedCount = %d, want 0", tr.droppedCount)
		}
	})

	t.Run("d: drop when even breadcrumbs are not enough", func(t *testing.T) {
		var onErrorCalls int
		rs := newRecordingServer(t)
		defer rs.Close()
		cfg := fixedTestConfig(rs.URL)
		cfg.OnError = func(error) { onErrorCalls++ }
		tr := newTestTransport(t, cfg)

		// An event whose frame Function text alone is enormous: §3.5 says
		// only frame file/function can hold an event over either limit once
		// (a)-(c) are shed.
		frames := make([]Frame, 100)
		for i := range frames {
			frames[i] = Frame{Function: strings.Repeat("f", 65000), File: "x.go", Line: 1, InApp: true}
		}
		ev := NewEvent(Config{ProjectKey: "pk_test"}, nil, nil)
		ev.Stacktrace = frames
		fixEventForGolden(ev, 3)
		ev.Stacktrace = frames // fixEventForGolden overwrites Stacktrace with a 1-frame golden list; restore

		tr.Push(ev)
		if !tr.Flush(5 * time.Second) {
			t.Fatal("flush timed out")
		}
		if tr.droppedCount != 1 {
			t.Errorf("droppedCount = %d, want 1", tr.droppedCount)
		}
		if onErrorCalls != 1 {
			t.Errorf("OnError calls = %d, want 1", onErrorCalls)
		}
	})

	// The publish family: the same a-d order, but driven by publish cost
	// (maxPublishBytes) instead of array size (maxBatchBodyBytes). U+1F600
	// headers, number-dense request.data and crumbs all inflate publish
	// cost far more than JSON size (spec §3.5, T40), and a 100-frame,
	// 11,000-byte-per-frame stacktrace is the un-sheddable base load that
	// (d) still cannot fit under even with everything else shed (spec §10.2
	// P1.11, sdkgo-rv2b).
	t.Run("publish a: headers shed, nothing else needed", func(t *testing.T) {
		rs := newRecordingServer(t)
		defer rs.Close()
		cfg := fixedTestConfig(rs.URL)
		tr := newTestTransport(t, cfg)

		pad := solveCountForTarget(maxPublishBytes+1, func(n int) int {
			_, cost := measureBeforeShed(t, shedOrderPublishBuild(n, true, false, false), cfg)
			return cost
		})
		ev := shedOrderPublishBuild(pad, true, false, false)
		sz, cost := measureBeforeShed(t, ev, cfg)
		if cost <= maxPublishBytes {
			t.Fatalf("setup: want publish cost over %d with headers, got %d", maxPublishBytes, cost)
		}
		if sz > maxBatchBodyBytes {
			t.Fatalf("setup: want array size at most %d, got %d", maxBatchBodyBytes, sz)
		}
		if szNo, costNo := measureBeforeShed(t, shedOrderPublishBuild(pad, false, false, false), cfg); costNo > maxPublishBytes || szNo > maxBatchBodyBytes {
			t.Fatalf("setup: want at most %d/%d without headers, got sz=%d cost=%d", maxBatchBodyBytes, maxPublishBytes, szNo, costNo)
		}

		got := pushOneAndUnmarshal(t, tr, rs, ev)
		if len(got.Request.Headers) != 0 {
			t.Errorf("expected headers shed, got %d", len(got.Request.Headers))
		}
		if len(got.Stacktrace) != 100 {
			t.Errorf("expected all 100 frames kept, got %d", len(got.Stacktrace))
		}
		if tr.droppedCount != 0 {
			t.Errorf("droppedCount = %d, want 0", tr.droppedCount)
		}
	})

	t.Run("publish b: headers and data shed, breadcrumbs kept", func(t *testing.T) {
		rs := newRecordingServer(t)
		defer rs.Close()
		cfg := fixedTestConfig(rs.URL)
		tr := newTestTransport(t, cfg)

		pad := solveCountForTarget(maxPublishBytes+1, func(n int) int {
			_, cost := measureBeforeShed(t, shedOrderPublishBuild(n, false, true, true), cfg)
			return cost
		})
		ev := shedOrderPublishBuild(pad, true, true, true)
		if _, cost := measureBeforeShed(t, ev, cfg); cost <= maxPublishBytes {
			t.Fatalf("setup: want publish cost over %d with headers, got %d", maxPublishBytes, cost)
		}
		_, costNoHeaders := measureBeforeShed(t, shedOrderPublishBuild(pad, false, true, true), cfg)
		if costNoHeaders <= maxPublishBytes {
			t.Fatalf("setup: want still over %d without headers (data/crumbs kept), got %d", maxPublishBytes, costNoHeaders)
		}
		szNoHD, costNoHD := measureBeforeShed(t, shedOrderPublishBuild(pad, false, false, true), cfg)
		if costNoHD > maxPublishBytes || szNoHD > maxBatchBodyBytes {
			t.Fatalf("setup: want at most %d/%d without headers and data, got sz=%d cost=%d", maxBatchBodyBytes, maxPublishBytes, szNoHD, costNoHD)
		}

		got := pushOneAndUnmarshal(t, tr, rs, ev)
		if len(got.Request.Headers) != 0 {
			t.Errorf("expected headers shed, got %d", len(got.Request.Headers))
		}
		if got.Request.Data != nil {
			t.Errorf("expected request.data shed, got %v", got.Request.Data)
		}
		if len(got.Breadcrumbs) == 0 {
			t.Error("expected breadcrumbs kept")
		}
		if tr.droppedCount != 0 {
			t.Errorf("droppedCount = %d, want 0", tr.droppedCount)
		}
	})

	t.Run("publish c: headers, data and breadcrumbs shed", func(t *testing.T) {
		rs := newRecordingServer(t)
		defer rs.Close()
		cfg := fixedTestConfig(rs.URL)
		tr := newTestTransport(t, cfg)

		pad := solveCountForTarget(maxPublishBytes+1, func(n int) int {
			_, cost := measureBeforeShed(t, shedOrderPublishBuild(n, false, false, true), cfg)
			return cost
		})
		ev := shedOrderPublishBuild(pad, true, true, true)
		if _, cost := measureBeforeShed(t, ev, cfg); cost <= maxPublishBytes {
			t.Fatalf("setup: want publish cost over %d with headers, got %d", maxPublishBytes, cost)
		}
		_, costNoHD := measureBeforeShed(t, shedOrderPublishBuild(pad, false, false, true), cfg)
		if costNoHD <= maxPublishBytes {
			t.Fatalf("setup: want still over %d with only breadcrumbs left, got %d", maxPublishBytes, costNoHD)
		}
		szNothingLeft, costNothingLeft := measureBeforeShed(t, shedOrderPublishBuild(pad, false, false, false), cfg)
		if costNothingLeft > maxPublishBytes || szNothingLeft > maxBatchBodyBytes {
			t.Fatalf("setup: want at most %d/%d with nothing left to shed, got sz=%d cost=%d", maxBatchBodyBytes, maxPublishBytes, szNothingLeft, costNothingLeft)
		}

		got := pushOneAndUnmarshal(t, tr, rs, ev)
		if len(got.Request.Headers) != 0 {
			t.Errorf("expected headers shed, got %d", len(got.Request.Headers))
		}
		if got.Request.Data != nil {
			t.Errorf("expected request.data shed, got %v", got.Request.Data)
		}
		if len(got.Breadcrumbs) != 0 {
			t.Errorf("expected breadcrumbs shed, got %d", len(got.Breadcrumbs))
		}
		if tr.droppedCount != 0 {
			t.Errorf("droppedCount = %d, want 0", tr.droppedCount)
		}
	})

	t.Run("publish d: drop when even breadcrumbs are not enough", func(t *testing.T) {
		var onErrorCalls int
		rs := newRecordingServer(t)
		defer rs.Close()
		cfg := fixedTestConfig(rs.URL)
		cfg.OnError = func(error) { onErrorCalls++ }
		tr := newTestTransport(t, cfg)

		// The stacktrace alone - number-dense enough in aggregate, but here
		// just long text, mirrors the array-size family's (d): frame
		// function text is the only field spec §3.5 step 4 never sheds, so
		// enough of it alone forces a drop.
		frames := make([]Frame, 100)
		for i := range frames {
			frames[i] = Frame{Function: strings.Repeat("f", 65000), File: "x.go", Line: 1, InApp: true}
		}
		ev := NewEvent(Config{ProjectKey: "pk_test"}, nil, nil)
		ev.Stacktrace = frames
		fixEventForGolden(ev, 63)
		ev.Stacktrace = frames // fixEventForGolden overwrites Stacktrace with a 1-frame golden list; restore

		if _, cost := measureBeforeShed(t, ev, cfg); cost <= maxPublishBytes {
			t.Fatalf("setup: want publish cost over %d even bare, got %d", maxPublishBytes, cost)
		}

		tr.Push(ev)
		if !tr.Flush(5 * time.Second) {
			t.Fatal("flush timed out")
		}
		if tr.droppedCount != 1 {
			t.Errorf("droppedCount = %d, want 1", tr.droppedCount)
		}
		if onErrorCalls != 1 {
			t.Errorf("OnError calls = %d, want 1", onErrorCalls)
		}
	})

	// The exact publish-cost boundary spec §3.5 step 4 names: a cost of
	// exactly maxPublishBytes (1,000,000) must be sent whole (the check is
	// "<=", not "<"), and one byte over must shed headers - the mutation
	// this discriminates is transport.go's fits() comparing with "<" instead
	// of "<=" (spec §10.2 P1.11, sdkgo-rv2b).
	t.Run("publish boundary: cost 1,000,000 sent whole, 1,000,001 sheds headers", func(t *testing.T) {
		// padLen sizes a stacktrace frame's Function text, not the request
		// header value: header values are truncated at
		// sdkMaxRequestHeaderValueLen (4,096) before cost is measured, so a
		// header-padded build plateaus below the 1,000,000/1,000,001 targets
		// and solveCountForTarget's doubling search never converges - it did
		// not converge for two prior attempts, each OOM-killed trying to
		// strings.Repeat a multi-gigabyte pad. A stacktrace frame's Function
		// field has no such cap (only frame count is capped, at 100), so it
		// gives the fine, unbounded per-byte control the exact boundary
		// needs. The single "pad" header stays present at a fixed cheap
		// value so the kept/shed assertions below still exercise headers.
		build := func(padLen int) *Event {
			ev := NewEvent(Config{ProjectKey: "pk_test", ServerName: "golden-host"}, nil, nil)
			frames := []Frame{{File: "x.go", Function: strings.Repeat("f", padLen), Line: 1, InApp: true}}
			ev.Stacktrace = frames
			ev.Request = &RequestInfo{Method: "GET", URL: "http://api.example.test/items/42", Headers: map[string]string{"pad": "x"}}
			fixEventForGolden(ev, 64)
			ev.Stacktrace = frames // fixEventForGolden overwrites Stacktrace with a 1-frame golden list; restore
			return ev
		}
		exact := func(t *testing.T, cfg Config, target int) int {
			t.Helper()
			n := solveCountForTarget(target, func(n int) int {
				_, cost := measureBeforeShed(t, build(n), cfg)
				return cost
			})
			for i := 0; i < 10; i++ {
				_, cost := measureBeforeShed(t, build(n), cfg)
				if cost == target {
					return n
				}
				if cost < target {
					n++
				} else {
					n--
				}
			}
			t.Fatalf("could not size padLen for an exact publish cost of %d", target)
			return 0
		}

		const limit = maxPublishBytes

		t.Run("1,000,000: sent whole", func(t *testing.T) {
			rs := newRecordingServer(t)
			defer rs.Close()
			cfg := fixedTestConfig(rs.URL)
			tr := newTestTransport(t, cfg)

			n := exact(t, cfg, limit)
			ev := build(n)
			sz, cost := measureBeforeShed(t, ev, cfg)
			if cost != limit {
				t.Fatalf("setup: want publish cost exactly %d, got %d", limit, cost)
			}
			if sz > maxBatchBodyBytes {
				t.Fatalf("setup: want array size at most %d, got %d", maxBatchBodyBytes, sz)
			}

			got := pushOneAndUnmarshal(t, tr, rs, ev)
			if len(got.Request.Headers) != 1 {
				t.Errorf("expected the header kept (cost exactly at the limit is not over it), got %d", len(got.Request.Headers))
			}
			if tr.droppedCount != 0 {
				t.Errorf("droppedCount = %d, want 0", tr.droppedCount)
			}
		})

		t.Run("1,000,001: headers shed", func(t *testing.T) {
			rs := newRecordingServer(t)
			defer rs.Close()
			cfg := fixedTestConfig(rs.URL)
			tr := newTestTransport(t, cfg)

			n := exact(t, cfg, limit+1)
			ev := build(n)
			sz, cost := measureBeforeShed(t, ev, cfg)
			if cost != limit+1 {
				t.Fatalf("setup: want publish cost exactly %d, got %d", limit+1, cost)
			}
			if sz > maxBatchBodyBytes {
				t.Fatalf("setup: want array size at most %d, got %d", maxBatchBodyBytes, sz)
			}

			got := pushOneAndUnmarshal(t, tr, rs, ev)
			if len(got.Request.Headers) != 0 {
				t.Errorf("expected the header shed (cost one byte over the limit), got %d", len(got.Request.Headers))
			}
			if tr.droppedCount != 0 {
				t.Errorf("droppedCount = %d, want 0", tr.droppedCount)
			}
		})
	})
}

// shedOrderPublishBuild builds a TestShedOrder publish-cost case: a 100-frame
// stacktrace (frame text is the field spec §3.5 step 4 never sheds, spec
// §10.2 P1.11), and headers/data/breadcrumbs present or absent per the
// flags. U+1F600 headers and number-dense request.data/crumbs inflate
// publish cost far more than array size (spec §3.5, T40), so it is the
// publish-cost limit, not the array-size limit, that forces every shed here
// - the counterpart to shedOrderBuild's array-size family.
//
// The un-padded base frame text is kept small (2,000 bytes/frame, 200,000
// total) rather than large enough to be un-sheddable on its own: unlike
// case (d) below, which builds its own much larger (65,000 bytes/frame)
// stacktrace specifically so nothing shed still leaves it over the limit,
// cases (a)-(c) need the base to leave headroom under maxPublishBytes once
// the one field each case tests is removed - solveCountForTarget then pads
// frames[0] with however much more frame text (an unbounded field, unlike
// a request header value's 4,096-byte cap) it takes to push the specific
// combination under test just over the limit.
func shedOrderPublishBuild(padLen int, headers, data, crumbs bool) *Event {
	ev := NewEvent(Config{ProjectKey: "pk_test", ServerName: "golden-host"}, nil, nil)
	const baseFrameBytes = 2000
	frames := make([]Frame, 100)
	for i := range frames {
		frames[i] = Frame{File: "x.go", Function: strings.Repeat("f", baseFrameBytes), Line: 1, InApp: true}
	}
	frames[0].Function = strings.Repeat("f", baseFrameBytes+padLen)
	ev.Stacktrace = frames
	ev.Request = &RequestInfo{Method: "GET", URL: "http://api.example.test/items/42"}
	if headers {
		// 64 headers of 1,024 U+1F600: each header entry adds a flat 6-byte
		// publish-cost allowance (the rc14 map-entry fix) on top of its
		// encoded bytes, which array size never charges - so headers inflate
		// publish cost disproportionately to array size (spec §3.5 T40).
		ev.Request.Headers = heavyHeaders(64, '\U0001F600', 1024)
	}
	if data {
		// 1,500 number-dense elements: ~3 bytes/element as JSON (well under
		// the 16,384-byte request.data cap, so boundRequestDataDecoded never
		// drops it whole) but 9 bytes/element as the protobuf Value estimate
		// (spec §3.5 T40) - costly in publish cost, cheap in array size.
		ev.Request.Data = map[string]interface{}{"d": zerosList(1500)}
	}
	if crumbs {
		// Same number-dense reasoning as request.data, kept well under
		// sdkMaxBreadcrumbMessageLen and the 128 KiB list bound so none of
		// the 5 crumbs is truncated or dropped before step 4 runs.
		cr := make([]Breadcrumb, 5)
		for i := range cr {
			cr[i] = Breadcrumb{Message: "m", Category: "test", Data: map[string]interface{}{"d": zerosList(400)}}
		}
		ev.Breadcrumbs = cr
	}
	fixEventForGolden(ev, 61)
	ev.Stacktrace = frames // fixEventForGolden overwrites Stacktrace with a 1-frame golden list; restore
	return ev
}

// TestBatchCutByCount proves sendBatch cuts a joined body at 500 events
// (maxBatchEvents), posting a second body for the remainder, even though no
// single body is anywhere near the 5 MiB byte cap (spec §3.5 step 4, §8
// guarantee 3).
func TestBatchCutByCount(t *testing.T) {
	rs := newRecordingServer(t)
	defer rs.Close()

	tr := newTestTransport(t, fixedTestConfig(rs.URL))

	const n = maxBatchEvents + 1
	events := make([]*Event, n)
	for i := range events {
		ev := NewEvent(Config{ProjectKey: "pk_test", ServerName: "golden-host"}, nil, nil)
		fixEventForGolden(ev, 100+i)
		events[i] = ev
	}

	tr.sendBatch(events)

	if got := len(rs.bodies); got != 2 {
		t.Fatalf("expected 2 bodies, got %d", got)
	}
	if got := len(rs.bodies[0]); got != maxBatchEvents {
		t.Errorf("first body: %d events, want %d", got, maxBatchEvents)
	}
	if got := len(rs.bodies[1]); got != 1 {
		t.Errorf("second body: %d events, want 1", got)
	}
	if tr.droppedCount != 0 {
		t.Errorf("droppedCount = %d, want 0", tr.droppedCount)
	}
}

// TestBatchCutBySize proves sendBatch cuts a joined body at 5,242,880 bytes
// (maxBatchBodyBytes) well before it reaches 500 events, when each event is
// individually far under either per-event limit but the join is not (spec
// §3.5 step 4, §8 guarantee 3). The 5 events are sized to exactly 1,048,576
// bytes each - 5,242,880 (maxBatchBodyBytes) on the nose before the joined
// array's own brackets and commas are counted, over it once they are - so
// the cut is exercised at the exact boundary the byte-counting math has to
// get right, not a wide margin any size-counting bug would also pass (spec
// §10.2 P1.11, sdkgo-rv2b).
func TestBatchCutBySize(t *testing.T) {
	rs := newRecordingServer(t)
	defer rs.Close()

	cfg := fixedTestConfig(rs.URL)
	tr := newTestTransport(t, cfg)

	const n = 5
	const perEventBytes = 1_048_576
	events := make([]*Event, n)
	for i := range events {
		events[i] = exactBodyEventViaHeaders(t, cfg, perEventBytes, 200+i)
	}

	tr.sendBatch(events)

	if got := len(rs.bodies); got != 2 {
		t.Fatalf("expected 2 bodies from the exact-boundary cut, got %d", got)
	}
	if got := len(rs.bodies[0]); got != 4 {
		t.Errorf("first body: %d events, want 4", got)
	}
	if got := len(rs.bodies[1]); got != 1 {
		t.Errorf("second body: %d events, want 1", got)
	}
	total := 0
	for i, body := range rs.bodies {
		total += len(body)
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body %d: %v", i, err)
		}
		if len(raw) > maxBatchBodyBytes {
			t.Errorf("body %d re-marshalled size = %d, want <= %d", i, len(raw), maxBatchBodyBytes)
		}
		if len(body) > maxBatchEvents {
			t.Errorf("body %d has %d events, want <= %d", i, len(body), maxBatchEvents)
		}
	}
	if total != n {
		t.Errorf("total events across bodies = %d, want %d", total, n)
	}
	if tr.droppedCount != 0 {
		t.Errorf("droppedCount = %d, want 0", tr.droppedCount)
	}
}
