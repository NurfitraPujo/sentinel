package sentinel

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fixEventForGolden assigns an event's volatile fields (event_id, timestamp,
// stacktrace, and the environment-dependent runtime/os/device context
// fields) fixed values so a test can compare against a committed golden or a
// literal integer (spec §10.2, "the golden rule"). Shared by bodycap_test.go
// (spec §10.2 P1.11's near-limit body-cap tests) and wire_golden_test.go, so
// it lives here rather than in either.
func fixEventForGolden(ev *Event, n int) {
	ev.EventID = fmt.Sprintf("00000000-0000-4000-8000-%012d", n)
	ev.Timestamp = time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	for i := range ev.Breadcrumbs {
		ev.Breadcrumbs[i].Timestamp = ev.Timestamp
	}
	ev.Stacktrace = []Frame{{File: "golden.go", Function: "golden", Line: 1, InApp: true}}
	if ev.Contexts != nil {
		if rt, ok := ev.Contexts["runtime"]; ok {
			rt["version"] = "0.0.0-golden"
		}
		if os, ok := ev.Contexts["os"]; ok {
			os["name"] = "golden-os"
		}
		if dev, ok := ev.Contexts["device"]; ok {
			dev["arch"] = "golden-arch"
		}
	}
}

// fixedTestConfig is a golden-scope-independent test config (P1.11's cases
// build their events directly, per the plan; the shared middleware-driven
// golden helper lives in wire_golden_test.go's goldenConfig). Shared for the
// same reason fixEventForGolden is.
func fixedTestConfig(endpoint string) Config {
	return Config{
		ProjectKey:    "pk_test",
		Endpoint:      endpoint,
		ServerName:    "golden-host",
		MaxBufferSize: 512,
		BatchSize:     1,
		BatchWait:     time.Minute,
	}
}

// pushOneAndUnmarshal pushes ev alone through tr, flushes, and returns the
// single event the recording server received. Shared for the same reason
// fixEventForGolden is.
func pushOneAndUnmarshal(t *testing.T, tr *Transport, rs *recordingServer, ev *Event) Event {
	t.Helper()
	tr.Push(ev)
	if !tr.Flush(5 * time.Second) {
		t.Fatal("flush timed out")
	}
	if len(rs.bodies) != 1 || len(rs.bodies[0]) != 1 {
		t.Fatalf("expected one body of one event, got %d bodies", len(rs.bodies))
	}
	var got Event
	if err := json.Unmarshal(rs.bodies[0][0], &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return got
}

// headerName returns a stable, collision-free request header name for the
// i'th of a set of padding headers. Shared by bodycap_test.go and
// wire_golden_test.go for the same reason fixEventForGolden is.
func headerName(i int) string {
	const letters = "abcdefghijklmnopqrstuvwxyz"
	return "X-H-" + string(letters[i%26]) + string(letters[(i/26)%26])
}

// serverMaxObjectSize is the bound recordingServer enforces on contexts and
// metadata: the ingestor's own 65,536-byte bound less the SDK's 4,096-byte
// margin (spec §3.5, "The margin"). Written as the literal 61,440, not as
// sdkMaxContextsSize/sdkMaxMetadataSize, so a mutation to either constant in
// bounds.go cannot also loosen the check that is supposed to catch it (spec
// §10.2 P1.11, sdkgo-rv2b: standing in for the ingestor's own ValidatePayload,
// which knows nothing of the SDK's constants either).
const serverMaxObjectSize = 61440

// containsNUL reports whether v holds a U+0000 anywhere - in a string value,
// or in a map key - at any depth, mirroring what the ingestor's own cuts
// package refuses to see once the SDK has bounded an event (spec §3.4).
func containsNUL(v interface{}) bool {
	switch t := v.(type) {
	case string:
		return strings.IndexByte(t, 0) >= 0
	case map[string]interface{}:
		for k, val := range t {
			if strings.IndexByte(k, 0) >= 0 {
				return true
			}
			if containsNUL(val) {
				return true
			}
		}
	case []interface{}:
		for _, e := range t {
			if containsNUL(e) {
				return true
			}
		}
	}
	return false
}

// newTestTransport builds a Transport for a test, refusing a zero
// MaxBufferSize, BatchSize or BatchWait: NewTransport applies no defaults
// (unlike Init), so an unset MaxBufferSize is an unbuffered channel that
// silently drops every event the worker is not already waiting for (spec
// §10.1, "the eviction trap", T36). Every test that calls NewTransport must
// go through this helper.
func newTestTransport(t *testing.T, cfg Config) *Transport {
	t.Helper()
	if cfg.MaxBufferSize == 0 {
		t.Fatal("newTestTransport: Config.MaxBufferSize must be set explicitly")
	}
	if cfg.BatchSize == 0 {
		t.Fatal("newTestTransport: Config.BatchSize must be set explicitly")
	}
	if cfg.BatchWait == 0 {
		t.Fatal("newTestTransport: Config.BatchWait must be set explicitly")
	}
	return NewTransport(cfg)
}

// recordingServer is an httptest.Server that stands in for the real
// ingestor's batch endpoint (spec §10.2 P1.11): it decodes every body it
// receives as a JSON array (§3.5: every body, a lone event included, is
// posted to {endpoint}/batch as an array) and appends it to bodies, but
// first enforces exactly the caps handleBatchIngest does - a body over
// maxBatchBodyBytes or an array over maxBatchEvents items answers 413, any
// path but /batch answers 404 - and, per element, decodes it into the
// package's own Event/decodeMirror and fails the test if that element's
// publishCost is over maxPublishBytes, standing in for the NATS publish a
// test in this module cannot reach (T39, T40).
type recordingServer struct {
	*httptest.Server
	bodies [][]json.RawMessage
}

func newRecordingServer(t *testing.T) *recordingServer {
	t.Helper()
	rs := &recordingServer{}
	rs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/batch" {
			t.Errorf("recordingServer: unexpected request path %q; the SDK must always post to {endpoint}/batch", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}

		limited := http.MaxBytesReader(w, r.Body, maxBatchBodyBytes)
		var batch []json.RawMessage
		if err := json.NewDecoder(limited).Decode(&batch); err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				w.WriteHeader(http.StatusRequestEntityTooLarge)
				return
			}
			t.Errorf("recordingServer: body did not decode as a JSON array: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if len(batch) > maxBatchEvents {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}

		for _, raw := range batch {
			var ev Event
			var mirror decodeMirror
			if err := json.Unmarshal(raw, &ev); err != nil {
				t.Errorf("recordingServer: element did not decode as an Event: %v", err)
				continue
			}
			if err := json.Unmarshal(raw, &mirror); err != nil {
				t.Errorf("recordingServer: element did not decode as a decodeMirror: %v", err)
				continue
			}
			if cost := publishCost(&ev, &mirror); cost > maxPublishBytes {
				t.Errorf("recordingServer: element %s publish cost %d over %d (would be lost at the NATS publish, T39/T40)", ev.EventID, cost, maxPublishBytes)
			}

			// Standing in for the ingestor's own ValidatePayload/cuts pass
			// (spec §10.2 P1.11, sdkgo-rv2b): an SDK-bounded event must
			// already satisfy the server's own rules, so nothing here should
			// ever fire on a correctly-bounded event.
			for name, ctx := range mirror.Contexts {
				if strings.IndexByte(name, 0) >= 0 {
					t.Errorf("recordingServer: element %s: context name %q holds U+0000", ev.EventID, name)
				}
				if containsNUL(ctx) {
					t.Errorf("recordingServer: element %s: context %q holds U+0000 in its data", ev.EventID, name)
				}
			}
			if containsNUL(mirror.Metadata) {
				t.Errorf("recordingServer: element %s: metadata holds U+0000", ev.EventID)
			}
			if mirror.Request != nil && containsNUL(mirror.Request.Data) {
				t.Errorf("recordingServer: element %s: request.data holds U+0000", ev.EventID)
			}
			for i, c := range mirror.Breadcrumbs {
				if containsNUL(c.Data) {
					t.Errorf("recordingServer: element %s: breadcrumb %d data holds U+0000", ev.EventID, i)
				}
			}
			if s := jsonSize(mirror.Contexts); s > serverMaxObjectSize {
				t.Errorf("recordingServer: element %s: contexts %d bytes over %d", ev.EventID, s, serverMaxObjectSize)
			}
			if s := jsonSize(mirror.Metadata); s > serverMaxObjectSize {
				t.Errorf("recordingServer: element %s: metadata %d bytes over %d", ev.EventID, s, serverMaxObjectSize)
			}
		}

		rs.bodies = append(rs.bodies, batch)
		w.WriteHeader(http.StatusAccepted)
	}))
	return rs
}

// mustUnmarshal decodes raw into v or fails the test.
func mustUnmarshal(t *testing.T, raw json.RawMessage, v interface{}) {
	t.Helper()
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("mustUnmarshal: %v", err)
	}
}

// decodingServer answers 400, as handleBatchIngest does, when a body fails
// to decode as a JSON array (spec §3.5 step 2, T41).
func newDecodingServer(t *testing.T) *recordingServer {
	t.Helper()
	rs := &recordingServer{}
	rs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var batch []json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":"Invalid JSON payload array"}`))
			return
		}
		rs.bodies = append(rs.bodies, batch)
		w.WriteHeader(http.StatusAccepted)
	}))
	return rs
}
