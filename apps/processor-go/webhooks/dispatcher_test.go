package webhooks

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NurfitraPujo/sentinel/apps/processor-go/store"
)

// fakeStore is an in-memory store.WebhookStore double, letting the dispatcher be tested without a
// live database — mirrors how OperationalDispatcher fakes dlqmonitor in tests/unit.
type fakeStore struct {
	mu sync.Mutex

	webhooks []store.WebhookRow
	events   []store.WebhookEvent // shared pool; filtered by org/afterSeq/eventTypes

	successCalls []casCall
	failureCalls []failCall

	// casResult lets a test force RecordDeliverySuccess to report "lost the race".
	forceCASFail bool
}

type casCall struct {
	webhookID      string
	oldSeq, newSeq int64
}

type failCall struct {
	webhookID string
	errMsg    string
	threshold int
}

func (f *fakeStore) ListActiveWebhooks(ctx context.Context) ([]store.WebhookRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]store.WebhookRow, len(f.webhooks))
	copy(out, f.webhooks)
	return out, nil
}

func (f *fakeStore) FetchEventsForWebhook(ctx context.Context, orgID string, afterSeq int64, eventTypes []string, limit int) ([]store.WebhookEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	typeSet := map[string]bool{}
	for _, t := range eventTypes {
		typeSet[t] = true
	}

	var out []store.WebhookEvent
	for _, e := range f.events {
		if e.Seq <= afterSeq {
			continue
		}
		if len(typeSet) > 0 && !typeSet[e.EventType] {
			continue
		}
		out = append(out, e)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (f *fakeStore) RecordDeliverySuccess(ctx context.Context, webhookID string, oldSeq, newSeq int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.successCalls = append(f.successCalls, casCall{webhookID, oldSeq, newSeq})
	if f.forceCASFail {
		return false, nil
	}
	for i := range f.webhooks {
		if f.webhooks[i].ID == webhookID && f.webhooks[i].LastDeliveredSeq == oldSeq {
			f.webhooks[i].LastDeliveredSeq = newSeq
			f.webhooks[i].ConsecutiveFailures = 0
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeStore) RecordDeliveryFailure(ctx context.Context, webhookID string, errMsg string, threshold int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failureCalls = append(f.failureCalls, failCall{webhookID, errMsg, threshold})
	for i := range f.webhooks {
		if f.webhooks[i].ID == webhookID {
			f.webhooks[i].ConsecutiveFailures++
			if f.webhooks[i].ConsecutiveFailures >= threshold {
				f.webhooks[i].Status = "failed"
			}
		}
	}
	return nil
}

func referenceSignature(secret string, ts int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(ts, 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	return fmt.Sprintf("t=%d,v1=%s", ts, hex.EncodeToString(mac.Sum(nil)))
}

func sampleEvent(seq int64, eventType string) store.WebhookEvent {
	return store.WebhookEvent{
		Seq:         seq,
		EventType:   eventType,
		ActorType:   "user",
		ActorID:     "user-1",
		OldValue:    json.RawMessage(`{"status":"unresolved"}`),
		NewValue:    json.RawMessage(`{"status":"resolved"}`),
		CreatedAt:   time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC),
		IssueID:     "issue-1",
		IssueTitle:  "boom: nil pointer",
		IssueStatus: "resolved",
		IssueType:   "system_error",
		ProjectID:   "project-1",
	}
}

func newTestDispatcher(st *fakeStore, client *http.Client) *Dispatcher {
	return &Dispatcher{
		Store:            st,
		Client:           client,
		Interval:         time.Millisecond,
		FailureThreshold: 3,
		MaxRetries:       3,
		Backoffs:         []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond},
	}
}

// TestDispatcher_SignatureAndPayloadShape verifies the signature header format against a reference
// HMAC implementation, and that the delivered JSON body's field names exactly match the golden
// shape (src/lib/db/queries/events.ts's OrgActivityEvent / GET /api/agent/events).
func TestDispatcher_SignatureAndPayloadShape(t *testing.T) {
	var gotBody []byte
	var gotSigHeader, gotDeliveryID string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		gotBody = body
		gotSigHeader = r.Header.Get("X-Sentinel-Signature")
		gotDeliveryID = r.Header.Get("X-Sentinel-Delivery-Id")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	fs := &fakeStore{
		webhooks: []store.WebhookRow{{
			ID: "wh-1", OrganizationID: "org-1", AgentID: "agent-1", URL: srv.URL, Secret: "sekrit",
			Status: "active", LastDeliveredSeq: 0,
		}},
		events: []store.WebhookEvent{sampleEvent(1, "status_changed")},
	}

	d := newTestDispatcher(fs, srv.Client())
	d.Tick(context.Background())

	if gotDeliveryID == "" {
		t.Fatalf("expected X-Sentinel-Delivery-Id to be set")
	}

	parts := strings.SplitN(gotSigHeader, ",", 2)
	if len(parts) != 2 || !strings.HasPrefix(parts[0], "t=") || !strings.HasPrefix(parts[1], "v1=") {
		t.Fatalf("signature header malformed: %q", gotSigHeader)
	}
	ts, err := strconv.ParseInt(strings.TrimPrefix(parts[0], "t="), 10, 64)
	if err != nil {
		t.Fatalf("bad timestamp in signature: %v", err)
	}
	wantSig := referenceSignature("sekrit", ts, gotBody)
	if gotSigHeader != wantSig {
		t.Fatalf("signature mismatch:\n got  %s\n want %s", gotSigHeader, wantSig)
	}

	var decoded map[string]any
	if err := json.Unmarshal(gotBody, &decoded); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}

	golden := `{
		"webhookId": "wh-1",
		"agentId": "agent-1",
		"cursor": 1,
		"events": [{
			"seq": 1,
			"eventType": "status_changed",
			"actorType": "user",
			"actorId": "user-1",
			"oldValue": {"status":"unresolved"},
			"newValue": {"status":"resolved"},
			"createdAt": "2026-08-14T12:00:00Z",
			"issue": {
				"id": "issue-1",
				"title": "boom: nil pointer",
				"status": "resolved",
				"issueType": "system_error",
				"projectId": "project-1"
			}
		}]
	}`
	var wantDecoded map[string]any
	if err := json.Unmarshal([]byte(golden), &wantDecoded); err != nil {
		t.Fatalf("golden fixture is not valid JSON: %v", err)
	}

	gotJSON, _ := json.Marshal(decoded)
	wantJSON, _ := json.Marshal(wantDecoded)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("payload shape mismatch:\n got  %s\n want %s", gotJSON, wantJSON)
	}
}

// TestDispatcher_CursorAdvancesOnlyOn2xx verifies the CAS cursor advances on a 2xx response and NOT
// on a 500, with the failure count incremented in the latter case.
func TestDispatcher_CursorAdvancesOnlyOn2xx(t *testing.T) {
	t.Run("2xx advances cursor", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		fs := &fakeStore{
			webhooks: []store.WebhookRow{{ID: "wh-1", OrganizationID: "org-1", AgentID: "a1", URL: srv.URL, Secret: "s", Status: "active"}},
			events:   []store.WebhookEvent{sampleEvent(1, "status_changed"), sampleEvent(2, "status_changed")},
		}
		d := newTestDispatcher(fs, srv.Client())
		d.Tick(context.Background())

		if len(fs.successCalls) != 1 {
			t.Fatalf("expected exactly 1 success call, got %d", len(fs.successCalls))
		}
		if fs.successCalls[0].newSeq != 2 {
			t.Fatalf("expected cursor to advance to seq 2, got %d", fs.successCalls[0].newSeq)
		}
		if fs.webhooks[0].LastDeliveredSeq != 2 {
			t.Fatalf("expected stored cursor 2, got %d", fs.webhooks[0].LastDeliveredSeq)
		}
		if len(fs.failureCalls) != 0 {
			t.Fatalf("expected no failure calls, got %d", len(fs.failureCalls))
		}
	})

	t.Run("500 does not advance cursor and records failure", func(t *testing.T) {
		var attempts int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			attempts++
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		fs := &fakeStore{
			webhooks: []store.WebhookRow{{ID: "wh-1", OrganizationID: "org-1", AgentID: "a1", URL: srv.URL, Secret: "s", Status: "active"}},
			events:   []store.WebhookEvent{sampleEvent(1, "status_changed")},
		}
		d := newTestDispatcher(fs, srv.Client())
		d.Tick(context.Background())

		if attempts != 3 {
			t.Fatalf("expected 3 in-tick retry attempts, got %d", attempts)
		}
		if len(fs.successCalls) != 0 {
			t.Fatalf("expected no success calls on persistent 500, got %d", len(fs.successCalls))
		}
		if fs.webhooks[0].LastDeliveredSeq != 0 {
			t.Fatalf("expected cursor to stay at 0, got %d", fs.webhooks[0].LastDeliveredSeq)
		}
		if len(fs.failureCalls) != 1 || fs.failureCalls[0].threshold != 3 {
			t.Fatalf("expected exactly 1 failure call with threshold 3, got %+v", fs.failureCalls)
		}
	})
}

// TestDispatcher_StatusFlipsToFailedAtThreshold verifies the store-level failure accounting the
// dispatcher drives trips 'failed' once consecutive_failures reaches the threshold — exercised here
// via three separate ticks against a fake that carries state across calls, the same way the real
// UPDATE ... CASE WHEN ... >= $3 THEN 'failed' guard would after three real ticks.
func TestDispatcher_StatusFlipsToFailedAtThreshold(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	fs := &fakeStore{
		webhooks: []store.WebhookRow{{ID: "wh-1", OrganizationID: "org-1", AgentID: "a1", URL: srv.URL, Secret: "s", Status: "active"}},
		events:   []store.WebhookEvent{sampleEvent(1, "status_changed")},
	}
	d := newTestDispatcher(fs, srv.Client())
	d.FailureThreshold = 2

	d.Tick(context.Background()) // failure 1
	if fs.webhooks[0].Status != "active" {
		t.Fatalf("expected still active after 1 failure, got %q", fs.webhooks[0].Status)
	}

	d.Tick(context.Background()) // failure 2 -> trips
	if fs.webhooks[0].Status != "failed" {
		t.Fatalf("expected status 'failed' after reaching threshold, got %q", fs.webhooks[0].Status)
	}
}

// TestDispatcher_EventTypesFilterHonored verifies a webhook subscribed to a subset of event types
// only receives matching events.
func TestDispatcher_EventTypesFilterHonored(t *testing.T) {
	var deliveredTypes []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body deliveryBody
		_ = json.NewDecoder(r.Body).Decode(&body)
		for _, e := range body.Events {
			deliveredTypes = append(deliveredTypes, e.EventType)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	fs := &fakeStore{
		webhooks: []store.WebhookRow{{
			ID: "wh-1", OrganizationID: "org-1", AgentID: "a1", URL: srv.URL, Secret: "s", Status: "active",
			EventTypes: []string{"assigned"},
		}},
		events: []store.WebhookEvent{
			sampleEvent(1, "status_changed"),
			sampleEvent(2, "assigned"),
		},
	}
	d := newTestDispatcher(fs, srv.Client())
	d.Tick(context.Background())

	if len(deliveredTypes) != 1 || deliveredTypes[0] != "assigned" {
		t.Fatalf("expected only 'assigned' event delivered, got %v", deliveredTypes)
	}
}

// TestDispatcher_EmptyEventsSendsNothing verifies a webhook with no due events triggers no HTTP call
// and no store mutation at all.
func TestDispatcher_EmptyEventsSendsNothing(t *testing.T) {
	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	fs := &fakeStore{
		webhooks: []store.WebhookRow{{ID: "wh-1", OrganizationID: "org-1", AgentID: "a1", URL: srv.URL, Secret: "s", Status: "active", LastDeliveredSeq: 5}},
		events:   []store.WebhookEvent{sampleEvent(1, "status_changed"), sampleEvent(5, "status_changed")},
	}
	d := newTestDispatcher(fs, srv.Client())
	d.Tick(context.Background())

	if called {
		t.Fatalf("expected no HTTP call when there are no due events")
	}
	if len(fs.successCalls) != 0 || len(fs.failureCalls) != 0 {
		t.Fatalf("expected no store mutation for an empty-events tick")
	}
}

// TestDispatcher_CASLossIsNotTreatedAsFailure verifies that when RecordDeliverySuccess reports it
// lost the compare-and-swap race (another instance already advanced the cursor), the dispatcher
// does NOT record a delivery failure — the HTTP delivery itself succeeded.
func TestDispatcher_CASLossIsNotTreatedAsFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	fs := &fakeStore{
		webhooks:     []store.WebhookRow{{ID: "wh-1", OrganizationID: "org-1", AgentID: "a1", URL: srv.URL, Secret: "s", Status: "active"}},
		events:       []store.WebhookEvent{sampleEvent(1, "status_changed")},
		forceCASFail: true,
	}
	d := newTestDispatcher(fs, srv.Client())
	d.Tick(context.Background())

	if len(fs.successCalls) != 1 {
		t.Fatalf("expected 1 success call attempted, got %d", len(fs.successCalls))
	}
	if len(fs.failureCalls) != 0 {
		t.Fatalf("expected no failure recorded on a CAS loss (delivery itself succeeded), got %d", len(fs.failureCalls))
	}
}

// TestSign_MatchesReferenceImplementation is a focused unit test on the signing helper alone.
func TestSign_MatchesReferenceImplementation(t *testing.T) {
	body := []byte(`{"hello":"world"}`)
	ts := int64(1755000000)
	got := Sign("my-secret", ts, body)
	want := referenceSignature("my-secret", ts, body)
	if got != want {
		t.Fatalf("Sign mismatch:\n got  %s\n want %s", got, want)
	}
}
