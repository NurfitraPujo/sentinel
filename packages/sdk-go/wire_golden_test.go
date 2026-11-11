package sentinel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// goldenConfig is P1.7/P1.8's "fixed Config": server_name is golden-host,
// never os.Hostname or SENTINEL_SERVER_NAME (spec §10.2, "the golden rule").
func goldenConfig() Config {
	return Config{ProjectKey: "pk_test", ServerName: "golden-host", Environment: "production", MaxBreadcrumbs: 100}
}

// goldenScope assigns exactly what ApplyContext will add from a middleware
// scope once C7 lands (spec plan §2, C3's note): the helper's body is
// replaced by a real middleware drive in C7, and the goldens must not
// change. Until then this is the one place that builds the fixed request.
func goldenScope(ev *Event) {
	ev.Request = &RequestInfo{
		Method:      "GET",
		URL:         "http://api.example.test/items/42",
		QueryString: "id=7&token=[FILTERED]",
		Headers: map[string]string{
			"Accept":     "application/json",
			"User-Agent": "golden-agent/1.0",
			"Host":       "api.example.test",
		},
	}
	ev.Transaction = "/items/42"
	if ev.User == nil {
		ev.User = &UserInfo{IPAddress: "192.0.2.1"}
	} else if ev.User.IPAddress == "" {
		u := *ev.User
		u.IPAddress = "192.0.2.1"
		ev.User = &u
	}
}

// goldenBytes runs the transport's actual per-event pipeline (spec §3.5
// steps 1-4) and returns exactly the bytes it would put into a body.
func goldenBytes(t *testing.T, ev *Event, cfg Config) []byte {
	t.Helper()
	raw, err := prepareEvent(ev, cfg)
	if err != nil {
		t.Fatalf("prepareEvent: %v", err)
	}
	return raw
}

// compareOrWriteGolden compares got against testdata/name, writing it (and
// failing, so a regeneration is never silent) only when the file is
// missing or UPDATE_GOLDEN=1 is set - the construction step for this test,
// never a mutation it exists to catch (spec §10.2, "the golden rule").
func compareOrWriteGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	want, err := os.ReadFile(path)
	if os.IsNotExist(err) || os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		// Written exactly as sent (compact JSON, no reformatting): the root
		// half's array_size and publishCost are measured against these exact
		// bytes plus the two enclosing brackets (spec §10.2, "the golden rule").
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("write golden %s: %v", path, err)
		}
		t.Fatalf("golden %s did not exist; wrote it - re-run to verify", path)
	}
	if err != nil {
		t.Fatalf("read golden %s: %v", path, err)
	}
	var gotOne, wantOne interface{}
	if err := json.Unmarshal(got, &gotOne); err != nil {
		t.Fatalf("unmarshal got: %v", err)
	}
	if err := json.Unmarshal(want, &wantOne); err != nil {
		t.Fatalf("unmarshal want: %v", err)
	}
	gotJSON, _ := json.Marshal(gotOne)
	wantJSON, _ := json.Marshal(wantOne)
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("golden %s mismatch:\n got:  %s\n want: %s", path, gotJSON, wantJSON)
	}
}

// TestWireGoldenFull is P1.7's SDK half: an event built through §8's call
// order and pushed through a real HTTPMiddleware-driven request, so the
// golden and the root half (tests/unit: TestSDKGoWireGoldenMatchesIngestor)
// prove the whole SDK-to-ingestor wire, not just a hand-built scope.
func TestWireGoldenFull(t *testing.T) {
	cfg := Config{ProjectKey: "golden-key", Environment: "golden", ReleaseVersion: "1.2.3", ServerName: "golden-host"}

	rs := newRecordingServer(t)
	defer rs.Close()
	transport := newTestTransport(t, Config{Endpoint: rs.URL, MaxBufferSize: 10, BatchSize: 10, BatchWait: 10 * time.Millisecond})

	mw := NewHTTPMiddleware(MiddlewareOptions{CaptureClientIP: true})
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := WithUserInfo(r.Context(), UserInfo{ID: "u-1", Username: "golden-user", Email: "golden@example.test"})
		ctx = WithTag(ctx, "region", "eu")
		AddBreadcrumb(ctx, Breadcrumb{Message: "did a thing", Category: "test", Type: "default", Level: "info", Data: map[string]interface{}{"step": "one"}})

		ev := NewEvent(cfg, nil, nil)
		ApplyContext(ctx, ev)
		ev.Tags["tenant_id"] = "t-1"
		fixEventForGolden(ev, 3)

		raw, err := prepareEvent(ev, cfg)
		if err != nil {
			t.Fatalf("prepareEvent: %v", err)
		}
		w.WriteHeader(http.StatusAccepted)
		_ = raw
		transport.Push(ev)
	}))

	req := httptest.NewRequest("GET", "http://api.example.test/items/42?id=7&token=t", nil)
	req.RemoteAddr = "192.0.2.1:1234"
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "golden-agent/1.0")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if !transport.Flush(1 * time.Second) {
		t.Fatal("flush timed out")
	}

	if len(rs.bodies) != 1 || len(rs.bodies[0]) != 1 {
		t.Fatalf("expected one event, got %d bodies", len(rs.bodies))
	}
	got := []byte(rs.bodies[0][0])

	if bytes.Contains(got, []byte(strings.TrimPrefix(rs.URL, "http://"))) {
		t.Fatalf("recording server host:port leaked into body: %s", got)
	}

	compareOrWriteGolden(t, "wire_v2_full.golden.json", got)

	var mirror decodeMirror
	if err := json.Unmarshal(got, &mirror); err != nil {
		t.Fatalf("unmarshal decode mirror: %v", err)
	}
	var b Event
	if err := json.Unmarshal(got, &b); err != nil {
		t.Fatalf("unmarshal event: %v", err)
	}
	cost := publishCost(&b, &mirror)
	arraySize := len(got) + 2
	recordPublishCostValues(t, "wire_v2_full.golden.json", arraySize, cost)
}

// TestWireGoldenBounded is P1.8's typed-bound case: one event built over
// EVERY typed field bound in spec §3.4's table at once (plan.md P1.8), so
// this golden pins boundEvent's truncation/drop for each of them together -
// a fixture that only exercises the Struct-typed fields (metadata, contexts,
// request.data, breadcrumb data) proves nothing about a typed field's own
// bound (rv4: raising sdkMaxErrorClassLength or sdkMaxBreadcrumbMessageLen
// by one must turn this test red).
func TestWireGoldenBounded(t *testing.T) {
	cfg := goldenConfig()
	ev := NewEvent(cfg, nil, map[string]interface{}{"order_ref": "o-1"})

	ev.Message = strings.Repeat("m", 12000)           // > sdkMaxMessageLength (10,000 code points)
	ev.ErrorClass = strings.Repeat("e", 300)          // > sdkMaxErrorClassLength (255)
	ev.User = &UserInfo{ID: strings.Repeat("u", 300)} // > sdkMaxUserIDBytes (255): dropped, not truncated
	frames := make([]Frame, 120)                      // > sdkMaxStacktraceFrames (100)
	for i := range frames {
		frames[i] = Frame{File: "golden.go", Function: "golden", Line: 1, InApp: true}
	}
	ev.Stacktrace = frames
	ev.Tags = make(map[string]string, 62)
	for i := 0; i < 60; i++ {
		ev.Tags[fmt.Sprintf("tag%02d", i)] = "v"
	}
	ev.Tags["region"] = "eu"
	ev.Tags["tenant_id"] = "t-1"             // 62 tags total > sdkMaxTags (50); tenant_id always kept
	ev.Breadcrumbs = make([]Breadcrumb, 150) // > goldenConfig's MaxBreadcrumbs (100)
	for i := range ev.Breadcrumbs {
		ev.Breadcrumbs[i] = Breadcrumb{Message: strings.Repeat("c", 9000), Category: "test"} // > sdkMaxBreadcrumbMessageLen (8,192)
	}
	goldenScope(ev)
	headers := make(map[string]string, 70) // > sdkMaxRequestHeaders (64)
	for i := 0; i < 70; i++ {
		headers[headerName(i)] = strings.Repeat("h", 5000) // > sdkMaxRequestHeaderValueLen (4,096)
	}
	ev.Request.Headers = headers
	fixEventForGolden(ev, 1)
	ev.Stacktrace = frames // fixEventForGolden overwrites Stacktrace with a 1-frame golden list; restore

	got := goldenBytes(t, ev, cfg)
	compareOrWriteGolden(t, "wire_v2_bounded.golden.json", got)
	recordPublishCost(t, "wire_v2_bounded.golden.json", ev, cfg)
}

// TestWireGoldenEscapes is the rc14 note's plan addition: an event whose
// strings are dense with the characters an escaping-off encoder still
// escapes (", \, control characters, U+2028), so the root half gates the
// fix against the real proto.Marshal (spec §3, the sdkgo-rc14 note).
func TestWireGoldenEscapes(t *testing.T) {
	cfg := goldenConfig()
	escapeMix := "he said \"hi\" \\ then\n\x01  end"

	// headerUnit packs ", \, \x01 and U+2028 into every 6 bytes (spec
	// section 9's header list), so the 64*300-byte headers alone carry well
	// over 19,200 bytes of escape excess (the rc14 note's discriminating
	// case): each unit costs 10 escaped bytes for 6 raw ones.
	headerUnit := "\"\\\x01" + " "
	headerVal := strings.Repeat(headerUnit, 300/len(headerUnit))
	headers := make(map[string]string, 64)
	for i := 0; i < 64; i++ {
		headers[headerName(i)] = headerVal
	}
	ev := NewEvent(cfg, nil, nil)
	ev.Message = escapeMix
	ev.Tags["note"] = escapeMix
	// A map entry's framing keeps only a flat 6-byte allowance regardless of
	// key or value length (plan.md §3, "A map entry keeps its 6-byte
	// allowance"), the tightest margin publishCostPart1 has - so 49 more
	// short escaped tag values (the 50-tag cap, tenant_id aside) are where
	// the "forgetting the quotes" mutation (plan.md §3) has the best chance
	// of closing the gap to the real protobuf size.
	for i := 0; i < 49; i++ {
		ev.Tags[fmt.Sprintf("t%02d", i)] = "\""
	}
	ev.Breadcrumbs = []Breadcrumb{{Message: escapeMix, Category: "test"}}
	ev.Stacktrace = []Frame{{File: "a.go", Function: escapeMix, Line: 1, InApp: true}}
	goldenScope(ev)
	ev.Request.Headers = headers
	fixEventForGolden(ev, 2)

	got := goldenBytes(t, ev, cfg)
	compareOrWriteGolden(t, "wire_v2_escapes.golden.json", got)
	recordPublishCost(t, "wire_v2_escapes.golden.json", ev, cfg)
}

// recordPublishCost writes (or verifies) this golden's entry in
// wire_v2_publish_cost.json: the one-element array size and the publish
// cost, so the root half can gate publishCost against the real
// proto.Marshal size without recomputing it (spec §10.2 P1.8).
func recordPublishCost(t *testing.T, goldenName string, ev *Event, cfg Config) {
	t.Helper()
	raw := goldenBytes(t, ev, cfg)
	var b Event
	var mirror decodeMirror
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatalf("unmarshal bounded event: %v", err)
	}
	if err := json.Unmarshal(raw, &mirror); err != nil {
		t.Fatalf("unmarshal decode mirror: %v", err)
	}
	cost := publishCost(&b, &mirror)
	arraySize := len(raw) + 2
	recordPublishCostValues(t, goldenName, arraySize, cost)
}

// publishCostEntry is one golden's row in wire_v2_publish_cost.json.
type publishCostEntry struct {
	ArraySize   int `json:"array_size"`
	PublishCost int `json:"publish_cost"`
}

// recordPublishCostValues compares goldenName's already-computed array size
// and cost against its committed entry in testdata/wire_v2_publish_cost.json
// - a pin against publishCost, not a recomputation of it. It writes that
// entry only when the file or the entry is missing, or when UPDATE_GOLDEN=1
// is set (the construction step, spec §10.2 "the golden rule"); a plain test
// run that finds a committed entry only compares against it and never
// touches the file. Writing on every run (the sdkgo-rv3 bug) made this
// self-referential: it pinned publishCost's output to itself, so a mutation
// to publishCost - e.g. halving publishCostPart2's `ld(S(m.Metadata))` -
// still passed, because there was never a stored value to diverge from.
func recordPublishCostValues(t *testing.T, goldenName string, arraySize, cost int) {
	t.Helper()
	path := filepath.Join("testdata", "wire_v2_publish_cost.json")
	entries := map[string]publishCostEntry{}
	existing, readErr := os.ReadFile(path)
	if readErr == nil {
		if err := json.Unmarshal(existing, &entries); err != nil {
			t.Fatalf("unmarshal %s: %v", path, err)
		}
	} else if !os.IsNotExist(readErr) {
		t.Fatalf("read %s: %v", path, readErr)
	}

	want, ok := entries[goldenName]
	if !ok || os.Getenv("UPDATE_GOLDEN") == "1" {
		entries[goldenName] = publishCostEntry{arraySize, cost}
		out, err := json.MarshalIndent(entries, "", "  ")
		if err != nil {
			t.Fatalf("marshal publish cost entries: %v", err)
		}
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(path, out, 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		t.Fatalf("publish cost entry for %s did not exist (or UPDATE_GOLDEN=1); wrote it - re-run to verify", goldenName)
	}

	if arraySize != want.ArraySize {
		t.Errorf("array size for %s = %d, want the committed %d", goldenName, arraySize, want.ArraySize)
	}
	if cost != want.PublishCost {
		t.Errorf("publishCost for %s = %d, want the committed %d", goldenName, cost, want.PublishCost)
	}
	if cost > maxPublishBytes {
		t.Errorf("publishCost for %s = %d, want <= %d", goldenName, cost, maxPublishBytes)
	}
	if arraySize > maxBatchBodyBytes {
		t.Errorf("array size for %s = %d, want <= %d", goldenName, arraySize, maxBatchBodyBytes)
	}
}

// zerosList is a number-dense payload of n zero-valued numbers: cheap as
// JSON (each element one byte) but expensive as protobuf (spec §3.5, T40).
func zerosList(n int) []interface{} {
	l := make([]interface{}, n)
	for i := range l {
		l[i] = 0
	}
	return l
}

// solveCountForTarget returns the smallest n such that build(n) >= target,
// for a build that is non-decreasing in n. Used to size a golden's padding
// against the real json.Marshal length, exactly, instead of by hand (spec
// §10.2 P1.8: "the two zero lists sized by the test").
//
// maxProbe bounds the doubling search: a build padding a field that is
// itself capped (e.g. a request header value truncated at
// sdkMaxRequestHeaderValueLen) never reaches target no matter how large n
// grows, and an unbounded "hi *= 2" then tries to allocate a multi-gigabyte
// string and OOMs the test process - this happened twice with a header-padded
// build before it was rewritten to pad an uncapped field instead. 1<<24 is
// far more headroom than any P1.11/P1.8 target (at most ~5,242,880) needs
// from a per-byte-cost field.
const solveCountForTargetMaxProbe = 1 << 24

func solveCountForTarget(target int, build func(n int) int) int {
	lo, hi := 0, 1
	for build(hi) < target {
		hi *= 2
		if hi > solveCountForTargetMaxProbe {
			panic(fmt.Sprintf("solveCountForTarget: build(n) still under target %d at n=%d - target unreachable (a capped or non-monotonic field?)", target, hi))
		}
	}
	for lo < hi {
		mid := (lo + hi) / 2
		if build(mid) < target {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

// TestWireGoldenBoundedCrumbShape is P1.8's T28 case: 200 breadcrumbs whose
// 8,100-character message makes the list's ingestor shape (timestamp, type,
// category, level, message, data) the binding measure - not the SDK's own
// omitempty shape, which would let a 16th crumb through that the server then
// cuts (spec §10.2 P1.8, "TestWireGoldenBoundedCrumbShape").
func TestWireGoldenBoundedCrumbShape(t *testing.T) {
	cfg := goldenConfig()
	ev := NewEvent(cfg, nil, nil)
	fixedTS := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	ev.Breadcrumbs = make([]Breadcrumb, 200)
	for i := range ev.Breadcrumbs {
		ev.Breadcrumbs[i] = Breadcrumb{Message: strings.Repeat("m", 8100), Timestamp: fixedTS}
	}
	goldenScope(ev)
	fixEventForGolden(ev, 4)

	got := goldenBytes(t, ev, cfg)

	var mirror decodeMirror
	if err := json.Unmarshal(got, &mirror); err != nil {
		t.Fatalf("unmarshal decode mirror: %v", err)
	}
	if len(mirror.Breadcrumbs) != 15 {
		t.Fatalf("expected exactly 15 breadcrumbs kept (ingestor shape, T28), got %d", len(mirror.Breadcrumbs))
	}

	compareOrWriteGolden(t, "wire_v2_bounded_crumbs.golden.json", got)
	recordPublishCost(t, "wire_v2_bounded_crumbs.golden.json", ev, cfg)
}

// TestWireGoldenBoundedNumbers is P1.8's number-dense case: an event within
// every server bound whose JSON size vastly understates its protobuf size,
// because a number costs far more as a Struct's Value than as a JSON digit
// (spec §3.5, T40).
func TestWireGoldenBoundedNumbers(t *testing.T) {
	cfg := goldenConfig()
	ev := NewEvent(cfg, nil, map[string]interface{}{
		"request_id": "req-1",
		"samples":    zerosList(20000),
	})
	ev.Contexts["metrics"] = map[string]interface{}{"window": zerosList(10000)}
	goldenScope(ev)
	ev.Request.Data = map[string]interface{}{"ids": zerosList(8000)}
	ev.Breadcrumbs = make([]Breadcrumb, 20)
	for i := range ev.Breadcrumbs {
		ev.Breadcrumbs[i] = Breadcrumb{
			Category: "db",
			Message:  "q",
			Data:     map[string]interface{}{"rows": zerosList(2000)},
		}
	}
	fixEventForGolden(ev, 6)

	raw, err := prepareEvent(ev, cfg)
	if err != nil {
		t.Fatalf("prepareEvent: %v", err)
	}

	escOff := len(escOffJSON(ev))
	if escOff >= 200_000 {
		t.Fatalf("expected the escaping-off JSON size under 200,000, got %d", escOff)
	}

	var b Event
	var mirror decodeMirror
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatalf("unmarshal bounded event: %v", err)
	}
	if err := json.Unmarshal(raw, &mirror); err != nil {
		t.Fatalf("unmarshal decode mirror: %v", err)
	}
	cost := publishCost(&b, &mirror)
	if cost >= maxPublishBytes {
		t.Fatalf("expected publish cost under %d, got %d", maxPublishBytes, cost)
	}

	compareOrWriteGolden(t, "wire_v2_bounded_numbers.golden.json", raw)
	recordPublishCostValues(t, "wire_v2_bounded_numbers.golden.json", len(raw)+2, cost)
}

// TestWireGoldenShrunk is P1.8's shrink case: 33 non-well-known contexts plus
// a well-known one bring the count over the server's 32-context cap, and two
// non-exempt metadata keys bring metadata over its 61,440-byte bound; the
// SDK removes the largest first (contexts by count, then metadata by size),
// recording every removal (spec §3.4, §10.2 P1.8).
func TestWireGoldenShrunk(t *testing.T) {
	cfg := goldenConfig()
	ev := NewEvent(cfg, nil, map[string]interface{}{
		"request_id": zerosList(30000),
		"a":          strings.Repeat("s", 10000),
		"b":          zerosList(10000),
	})
	for i := 0; i < 33; i++ {
		ev.Contexts[fmt.Sprintf("c%02d", i)] = map[string]interface{}{"v": zerosList(900)}
	}
	ev.Contexts["blob"] = map[string]interface{}{"v": zerosList(40000)}
	goldenScope(ev)
	fixEventForGolden(ev, 5)

	got := goldenBytes(t, ev, cfg)

	var mirror struct {
		Metadata map[string]interface{}            `json:"metadata"`
		Contexts map[string]map[string]interface{} `json:"contexts"`
	}
	if err := json.Unmarshal(got, &mirror); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(mirror.Contexts) != 32 {
		t.Fatalf("expected 32 contexts kept, got %d: %v", len(mirror.Contexts), mirror.Contexts)
	}
	for _, missing := range []string{"blob", "c00", "c01", "c02", "c03"} {
		if _, ok := mirror.Contexts[missing]; ok {
			t.Errorf("expected context %q removed", missing)
		}
	}
	if _, ok := mirror.Metadata["request_id"]; !ok {
		t.Error("expected metadata.request_id kept (exempt)")
	}
	if _, ok := mirror.Metadata["a"]; ok {
		t.Error("expected metadata.a removed")
	}
	if _, ok := mirror.Metadata["b"]; ok {
		t.Error("expected metadata.b removed")
	}
	wantKeys := []interface{}{"contexts.blob", "contexts.c00", "contexts.c01", "contexts.c02", "contexts.c03", "b", "a"}
	tk, _ := mirror.Metadata["truncated_keys"].([]interface{})
	if len(tk) != len(wantKeys) {
		t.Fatalf("truncated_keys = %v, want %v", tk, wantKeys)
	}
	for i, k := range wantKeys {
		if tk[i] != k {
			t.Errorf("truncated_keys[%d] = %v, want %v", i, tk[i], k)
		}
	}

	compareOrWriteGolden(t, "wire_v2_shrunk.golden.json", got)
	recordPublishCost(t, "wire_v2_shrunk.golden.json", ev, cfg)
}

// TestWireGoldenNulAndMargin is P1.8's U+0000/collision case, at the margin:
// contexts and metadata are each sized to land just under the 61,440-byte
// bound (so neither shrinks for size), while a collided empty context name
// still triggers one dedup-recorded removal (spec §10.2 P1.8, review round
// 10's shapes).
func TestWireGoldenNulAndMargin(t *testing.T) {
	cfg := goldenConfig()
	ev := NewEvent(cfg, nil, map[string]interface{}{
		"request_id": "req-1",
	})
	ev.Contexts["a"] = map[string]interface{}{"v": "x"}
	ev.Contexts["a\x00"] = map[string]interface{}{"v": "y"}
	ev.Contexts[""] = map[string]interface{}{"v": 1}
	ev.Contexts["\x00"] = map[string]interface{}{"v": 2}
	ev.Contexts["n\x00b"] = map[string]interface{}{
		"k\x00": "v\x00w",
		"k":     "clean",
		"l":     []interface{}{0, "\x00", map[string]interface{}{"\x00": 1}},
	}
	goldenScope(ev)
	ev.Request.Data = map[string]interface{}{
		"q":     "a\x00",
		"m\x00": 1,
		"m":     2,
		"ids":   zerosList(8000),
	}
	ev.Breadcrumbs = make([]Breadcrumb, 5)
	for i := range ev.Breadcrumbs {
		ev.Breadcrumbs[i] = Breadcrumb{
			Category: "test",
			Message:  "crumb",
			Data:     map[string]interface{}{"rows": zerosList(2000), "k\x00": "x"},
		}
	}

	// Size "pad" and "samples" so contexts and metadata each land in
	// [61000, 61440] as sent, without a size-triggered shrink (the golden
	// rule: sized by the test, before Push).
	padTarget := 61_200
	padCount := solveCountForTarget(padTarget, func(n int) int {
		ev.Contexts["pad"] = map[string]interface{}{"w": zerosList(n)}
		body := 0
		for k, v := range ev.Contexts {
			body += entrySize(k, v)
		}
		return objectSize(body, len(ev.Contexts))
	})
	ev.Contexts["pad"] = map[string]interface{}{"w": zerosList(padCount)}
	contextsSize := func() int {
		body := 0
		for k, v := range ev.Contexts {
			body += entrySize(k, v)
		}
		return objectSize(body, len(ev.Contexts))
	}()
	if contextsSize < 61_000 || contextsSize > 61_440 {
		t.Fatalf("contexts size = %d, want [61000, 61440]", contextsSize)
	}

	metaTarget := 61_200
	sampleCount := solveCountForTarget(metaTarget, func(n int) int {
		ev.Metadata["samples"] = zerosList(n)
		return jsonSize(ev.Metadata)
	})
	ev.Metadata["samples"] = zerosList(sampleCount)
	metaSize := jsonSize(ev.Metadata)
	if metaSize < 61_000 || metaSize > 61_440 {
		t.Fatalf("metadata size = %d, want [61000, 61440]", metaSize)
	}

	fixEventForGolden(ev, 7)

	raw, err := prepareEvent(ev, cfg)
	if err != nil {
		t.Fatalf("prepareEvent: %v", err)
	}

	var b Event
	var mirror decodeMirror
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatalf("unmarshal bounded event: %v", err)
	}
	if err := json.Unmarshal(raw, &mirror); err != nil {
		t.Fatalf("unmarshal decode mirror: %v", err)
	}
	cost := publishCost(&b, &mirror)
	if cost >= maxPublishBytes {
		t.Fatalf("expected publish cost under %d (nothing shed), got %d", maxPublishBytes, cost)
	}

	var decoded struct {
		Metadata map[string]interface{}            `json:"metadata"`
		Contexts map[string]map[string]interface{} `json:"contexts"`
		Request  map[string]interface{}            `json:"request"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal decoded shape: %v", err)
	}
	if a, ok := decoded.Contexts["a"]; !ok || a["v"] != "x" {
		t.Errorf("contexts.a = %v, want {v: x}", a)
	}
	if nb, ok := decoded.Contexts["nb"]; ok {
		if nb["k"] != "clean" {
			t.Errorf("contexts.nb.k = %v, want clean", nb["k"])
		}
	} else {
		t.Error("expected contexts.nb to survive the NUL-stripped rename")
	}
	if _, ok := decoded.Contexts[""]; ok {
		t.Error("expected the empty context name removed")
	}
	tk, _ := decoded.Metadata["truncated_keys"].([]interface{})
	if len(tk) != 1 || tk[0] != "contexts." {
		t.Errorf("truncated_keys = %v, want [contexts.]", tk)
	}
	trunc, _ := decoded.Metadata["truncated"].([]interface{})
	if len(trunc) != 1 || trunc[0] != "contexts" {
		t.Errorf("truncated = %v, want [contexts]", trunc)
	}

	compareOrWriteGolden(t, "wire_v2_nul.golden.json", raw)
	recordPublishCostValues(t, "wire_v2_nul.golden.json", len(raw)+2, cost)
}
