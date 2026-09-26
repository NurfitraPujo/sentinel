package sentinel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestEventReferencesArePerEvent is P1.10's table: two events captured from
// one context, on both paths, must not share any per-event reference
// (guarantee 1's table) - writing into event 1's fields in place must not
// reach event 2, the context's own readers, or a third event captured
// afterward.
func TestEventReferencesArePerEvent(t *testing.T) {
	baseCtx := func() context.Context {
		ctx := WithBreadcrumbBuffer(context.Background(), 10)
		AddBreadcrumb(ctx, Breadcrumb{Message: "m", Data: map[string]interface{}{"k": 1}})
		ctx = WithTag(ctx, "region", "eu")
		ctx = WithUserInfo(ctx, UserInfo{ID: "u", Email: "e@example.test"})
		ctx = WithContextMap(ctx, map[string]interface{}{
			"order":  "o",
			"nested": map[string]interface{}{"k": "v"},
		})
		return ctx
	}

	mutate := func(t *testing.T, ev *Event) {
		t.Helper()
		ev.Tags["region"] = "us"
		ev.Contexts["os"]["name"] = "mutated"
		ev.Metadata["order"] = "mutated"
		if nested, ok := ev.Metadata["nested"].(map[string]interface{}); ok {
			nested["k"] = "mutated"
		}
		ev.User.Email = "mutated@example.test"
		ev.User.IPAddress = "9.9.9.9"
		if ev.Request != nil {
			ev.Request.Headers["Accept"] = "mutated"
		}
		if len(ev.Breadcrumbs) > 0 {
			ev.Breadcrumbs[0].Message = "mutated"
			delete(ev.Breadcrumbs[0].Data, "k")
		}
		if len(ev.Stacktrace) > 0 {
			ev.Stacktrace[0].Line = 999
		}
	}

	assertUnmutated := func(t *testing.T, label string, ev *Event) {
		t.Helper()
		if ev.Tags["region"] != "eu" {
			t.Errorf("%s: Tags[region] = %q, want eu", label, ev.Tags["region"])
		}
		if got := ev.Contexts["os"]["name"]; got == "mutated" {
			t.Errorf("%s: Contexts[os][name] leaked mutation", label)
		}
		if ev.Metadata["order"] != "o" {
			t.Errorf("%s: Metadata[order] = %v, want o", label, ev.Metadata["order"])
		}
		if nested, ok := ev.Metadata["nested"].(map[string]interface{}); ok && nested["k"] == "mutated" {
			t.Errorf("%s: Metadata[nested][k] leaked mutation", label)
		}
		if ev.User == nil || ev.User.Email != "e@example.test" {
			t.Errorf("%s: User.Email leaked mutation: %+v", label, ev.User)
		}
		if len(ev.Breadcrumbs) > 0 {
			if ev.Breadcrumbs[0].Message == "mutated" {
				t.Errorf("%s: Breadcrumbs[0].Message leaked mutation", label)
			}
			if _, ok := ev.Breadcrumbs[0].Data["k"]; !ok {
				t.Errorf("%s: Breadcrumbs[0].Data[k] deleted by another event's mutation", label)
			}
		}
	}

	t.Run("NewEvent+ApplyContext", func(t *testing.T) {
		ctx := baseCtx()

		ev1 := NewEvent(Config{}, nil, nil)
		ApplyContext(ctx, ev1)
		mutate(t, ev1)

		ev2 := NewEvent(Config{}, nil, nil)
		ApplyContext(ctx, ev2)
		assertUnmutated(t, "ev2", ev2)

		if tags := TagsFromContext(ctx); tags["region"] != "eu" {
			t.Errorf("TagsFromContext leaked mutation: %v", tags)
		}
		if u, _ := UserFromContext(ctx); u.Email != "e@example.test" {
			t.Errorf("UserFromContext leaked mutation: %+v", u)
		}
		if md := MetadataFromContext(ctx); md["order"] != "o" {
			t.Errorf("MetadataFromContext leaked mutation: %v", md)
		}
		if crumbs := BreadcrumbsFromContext(ctx); len(crumbs) > 0 {
			if _, ok := crumbs[0].Data["k"]; !ok {
				t.Errorf("BreadcrumbsFromContext leaked a deletion: %v", crumbs)
			}
		}

		ev3 := NewEvent(Config{}, nil, nil)
		ApplyContext(ctx, ev3)
		assertUnmutated(t, "ev3", ev3)
	})

	t.Run("Client.CaptureErrorContext", func(t *testing.T) {
		rs := newRecordingServer(t)
		defer rs.Close()

		var captured []*Event
		cfg := Config{
			ProjectKey: "pk_test", Endpoint: rs.URL, MaxBufferSize: 10, BatchSize: 10, BatchWait: 10 * time.Millisecond,
			BeforeSend: func(ev *Event) *Event {
				captured = append(captured, ev)
				if len(captured) == 1 {
					mutate(t, ev)
				}
				return ev
			},
		}
		c := Init(cfg)

		handler := NewHTTPMiddleware(MiddlewareOptions{})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := baseCtx()
			c.CaptureErrorContext(ctx, errTestBoom, nil)
			c.CaptureErrorContext(ctx, errTestBoom, nil)
			w.WriteHeader(http.StatusAccepted)
		}))

		req := httptest.NewRequest("GET", "/items/42", nil)
		req.Header.Set("Accept", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if !c.transport.Flush(2 * time.Second) {
			t.Fatal("flush timed out")
		}
		if len(captured) != 2 {
			t.Fatalf("expected 2 captures, got %d", len(captured))
		}
		assertUnmutated(t, "ev2", captured[1])
	})
}

var errTestBoom = &testBoomError{}

type testBoomError struct{}

func (*testBoomError) Error() string { return "boom" }

// TestSDKNeverWritesCallerValues is P1.10, path (i): a caller's own Tags map
// and *UserInfo, shared across many events via an EventOption, must never
// be written by ApplyContext or the transport -- proved under -race with
// concurrent pushes on a shared transport.
func TestSDKNeverWritesCallerValues(t *testing.T) {
	rs := newRecordingServer(t)
	defer rs.Close()

	sharedTags := map[string]string{"service": "checkout"}
	sharedUser := &UserInfo{ID: "svc-account"}
	withShared := func(ev *Event) {
		ev.Tags = sharedTags
		ev.User = sharedUser
	}

	tr := newTestTransport(t, Config{
		ProjectKey:    "pk_test",
		Endpoint:      rs.URL,
		MaxBufferSize: 512,
		BatchSize:     1,
		BatchWait:     time.Second,
	})

	const n = 200
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < n; i++ {
			ev := NewEvent(Config{}, nil, nil, EventOption(withShared))
			ApplyContext(nil, ev)
			tr.Push(ev)
		}
	}()
	// Concurrently read the shared map/pointer the way another goroutine
	// building its own events would, to let -race catch a write into them.
	for i := 0; i < n; i++ {
		_ = sharedTags["service"]
		_ = sharedUser.ID
	}
	<-done

	if !tr.Flush(5 * time.Second) {
		t.Fatal("flush timed out")
	}
	if tr.droppedCount != 0 {
		t.Errorf("droppedCount = %d, want 0", tr.droppedCount)
	}
	if sharedTags["service"] != "checkout" {
		t.Error("the shared Tags map must not have been modified")
	}
	if sharedUser.ID != "svc-account" {
		t.Error("the shared *UserInfo must not have been modified")
	}
}

// TestSDKNeverWritesCallerValuesViaBeforeSend is P1.10 path (ii): a
// BeforeSend hook that ITSELF assigns a shared Tags map / *UserInfo to many
// events must not have those values written by the transport either.
func TestSDKNeverWritesCallerValuesViaBeforeSend(t *testing.T) {
	rs := newRecordingServer(t)
	defer rs.Close()

	sharedTags := map[string]string{"service": "checkout"}
	sharedUser := &UserInfo{ID: "svc-account", IPAddress: "203.0.113.9"} // deliberately over-bound-looking

	cfg := Config{
		ProjectKey:    "pk_test",
		Endpoint:      rs.URL,
		MaxBufferSize: 512,
		BatchSize:     1,
		BatchWait:     time.Second,
		BeforeSend: func(ev *Event) *Event {
			ev.Tags = sharedTags
			ev.User = sharedUser
			return ev
		},
	}
	tr := newTestTransport(t, cfg)

	for i := 0; i < 200; i++ {
		ev := NewEvent(cfg, nil, nil)
		ApplyContext(nil, ev)
		ev = cfg.ApplyBeforeSend(ev)
		tr.Push(ev)
	}
	if !tr.Flush(10 * time.Second) {
		t.Fatal("flush timed out")
	}
	if tr.droppedCount != 0 {
		t.Errorf("droppedCount = %d, want 0", tr.droppedCount)
	}
	if sharedTags["service"] != "checkout" || len(sharedTags) != 1 {
		t.Errorf("the shared Tags map must not have been modified, got %v", sharedTags)
	}
	if sharedUser.ID != "svc-account" || sharedUser.IPAddress != "203.0.113.9" {
		t.Errorf("the shared *UserInfo must not have been modified, got %+v", sharedUser)
	}
}

// TestBreadcrumbDataRaceBeforeSend is P1.10: BeforeSend writing into one
// event's crumb Data must never race another event's crumb Data from the
// same request buffer.
func TestBreadcrumbDataRaceBeforeSend(t *testing.T) {
	rs := newRecordingServer(t)
	defer rs.Close()

	ctx := WithBreadcrumbBuffer(context.Background(), 20)
	for i := 0; i < 5; i++ {
		AddBreadcrumb(ctx, Breadcrumb{Message: "m", Data: map[string]interface{}{"k": "v"}})
	}

	cfg := Config{
		ProjectKey:    "pk_test",
		Endpoint:      rs.URL,
		MaxBufferSize: 512,
		BatchSize:     1,
		BatchWait:     time.Second,
		BeforeSend: func(ev *Event) *Event {
			for i := range ev.Breadcrumbs {
				if ev.Breadcrumbs[i].Data != nil {
					ev.Breadcrumbs[i].Data["mutated"] = true
				}
			}
			return ev
		},
	}
	tr := newTestTransport(t, cfg)
	for i := 0; i < 200; i++ {
		ev := NewEvent(cfg, nil, nil)
		ApplyContext(ctx, ev)
		ev = cfg.ApplyBeforeSend(ev)
		tr.Push(ev)
	}
	if !tr.Flush(10 * time.Second) {
		t.Fatal("flush timed out")
	}
	if tr.droppedCount != 0 {
		t.Errorf("droppedCount = %d, want 0", tr.droppedCount)
	}
}

// TestMetadataListRaceBeforeSend is P1.10: BeforeSend mutating a metadata
// list's map entries on one event must never race another event's.
func TestMetadataListRaceBeforeSend(t *testing.T) {
	rs := newRecordingServer(t)
	defer rs.Close()

	cfg := Config{
		ProjectKey:    "pk_test",
		Endpoint:      rs.URL,
		MaxBufferSize: 512,
		BatchSize:     1,
		BatchWait:     time.Second,
		BeforeSend: func(ev *Event) *Event {
			if items, ok := ev.Metadata["items"].([]interface{}); ok {
				for _, it := range items {
					if m, ok := it.(map[string]interface{}); ok {
						m["touched"] = true
					}
				}
			}
			return ev
		},
	}
	tr := newTestTransport(t, cfg)
	for i := 0; i < 150; i++ {
		ctxTags := map[string]interface{}{
			"items": []interface{}{map[string]interface{}{"k": "v"}},
		}
		ev := NewEvent(cfg, nil, ctxTags)
		ApplyContext(nil, ev)
		ev = cfg.ApplyBeforeSend(ev)
		tr.Push(ev)
	}
	if !tr.Flush(10 * time.Second) {
		t.Fatal("flush timed out")
	}
	if tr.droppedCount != 0 {
		t.Errorf("droppedCount = %d, want 0", tr.droppedCount)
	}
}
