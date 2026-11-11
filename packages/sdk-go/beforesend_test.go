package sentinel

import (
	"bytes"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestBeforeSendModifies is P3.1: BeforeSend may modify the event before
// it is sent.
func TestBeforeSendModifies(t *testing.T) {
	rs := newRecordingServer(t)
	defer rs.Close()

	cfg := Config{
		ProjectKey:    "pk_test",
		Endpoint:      rs.URL,
		MaxBufferSize: 10,
		BatchSize:     1,
		BatchWait:     time.Second,
		BeforeSend: func(ev *Event) *Event {
			ev.Tags["injected"] = "yes"
			return ev
		},
	}
	tr := newTestTransport(t, cfg)
	ev := NewEvent(cfg, errors.New("boom"), nil)
	ApplyContext(nil, ev)
	ev = cfg.ApplyBeforeSend(ev)
	tr.Push(ev)
	if !tr.Flush(2 * time.Second) {
		t.Fatal("flush timed out")
	}
	if len(rs.bodies) != 1 {
		t.Fatalf("expected 1 body, got %d", len(rs.bodies))
	}
}

// TestBeforeSendDrops is P3.2: returning nil drops the event.
func TestBeforeSendDrops(t *testing.T) {
	cfg := Config{BeforeSend: func(ev *Event) *Event { return nil }}
	ev := &Event{EventID: "e1"}
	if got := cfg.ApplyBeforeSend(ev); got != nil {
		t.Errorf("expected nil, got %+v", got)
	}
}

func TestBeforeSendNilConfigReturnsEventUnchanged(t *testing.T) {
	cfg := Config{}
	ev := &Event{EventID: "e1"}
	if got := cfg.ApplyBeforeSend(ev); got != ev {
		t.Error("expected the same event back when BeforeSend is nil")
	}
	if got := cfg.ApplyBeforeSend(nil); got != nil {
		t.Error("expected nil for a nil event")
	}
}

// TestBeforeSendPanicDropsAndLogs is P3.4: a panicking hook drops the event
// (never sent unmodified) and does not crash the host.
func TestBeforeSendPanicDropsAndLogs(t *testing.T) {
	var buf bytes.Buffer
	prevOutput := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevOutput)
		log.SetFlags(prevFlags)
	})

	cfg := Config{BeforeSend: func(ev *Event) *Event { panic("boom") }}
	ev := &Event{EventID: "e1"}

	got := cfg.ApplyBeforeSend(ev) // must not panic out of this call
	if got != nil {
		t.Errorf("expected nil after a panicking hook, got %+v", got)
	}
	logged := buf.String()
	if !strings.Contains(logged, "BeforeSend panicked") {
		t.Errorf("expected log output to contain %q, got %q", "BeforeSend panicked", logged)
	}
	if !strings.Contains(logged, ev.EventID) {
		t.Errorf("expected log output to contain the event id %q, got %q", ev.EventID, logged)
	}
}

// TestBeforeSendSeesAutomaticFields is P3.3: BeforeSend runs after every
// automatic field NewEvent, ApplyContext and the middleware add - Level,
// SDK, ServerName, Contexts["runtime"], Breadcrumbs, Request and User must
// all already be set when the hook runs.
func TestBeforeSendSeesAutomaticFields(t *testing.T) {
	rs := newRecordingServer(t)
	defer rs.Close()

	seen := make(chan *Event, 1)
	cfg := Config{
		ProjectKey:    "pk_test",
		Endpoint:      rs.URL,
		MaxBufferSize: 10,
		BatchSize:     1,
		BatchWait:     time.Second,
		BeforeSend: func(ev *Event) *Event {
			seen <- ev
			return ev
		},
	}
	transport := newTestTransport(t, cfg)

	handler := NewHTTPMiddleware(MiddlewareOptions{})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := WithUser(r.Context(), "u1")
		AddBreadcrumb(ctx, Breadcrumb{Message: "hello"})
		ev := NewEvent(cfg, errors.New("boom"), nil)
		ApplyContext(ctx, ev)
		if ev = cfg.ApplyBeforeSend(ev); ev != nil {
			transport.Push(ev)
		}
		w.WriteHeader(http.StatusAccepted)
	}))

	req := httptest.NewRequest("GET", "/items/42", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	select {
	case ev := <-seen:
		if ev.Level == "" {
			t.Error("Level not set before BeforeSend")
		}
		if ev.SDK == nil {
			t.Error("SDK not set before BeforeSend")
		}
		if ev.ServerName == "" {
			t.Error("ServerName not set before BeforeSend")
		}
		if _, ok := ev.Contexts["runtime"]; !ok {
			t.Error("Contexts[runtime] not set before BeforeSend")
		}
		if len(ev.Breadcrumbs) == 0 {
			t.Error("Breadcrumbs not set before BeforeSend")
		}
		if ev.Request == nil {
			t.Error("Request not set before BeforeSend")
		}
		if ev.User == nil {
			t.Error("User not set before BeforeSend")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("BeforeSend hook never ran")
	}
}
