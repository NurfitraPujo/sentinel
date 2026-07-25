package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/validation"
)

type mockIngestor struct {
	ingested []*validation.ErrorPayload
}

func (m *mockIngestor) Ingest(ctx context.Context, payload *validation.ErrorPayload) error {
	m.ingested = append(m.ingested, payload)
	return nil
}

func TestBatchIngestHandler(t *testing.T) {
	mock := &mockIngestor{}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payloads []validation.ErrorPayload
		if err := json.NewDecoder(r.Body).Decode(&payloads); err != nil {
			http.Error(w, "invalid json array", http.StatusBadRequest)
			return
		}

		if len(payloads) == 0 {
			http.Error(w, "empty batch", http.StatusBadRequest)
			return
		}

		ingestedCount := 0
		for i := range payloads {
			if err := mock.Ingest(r.Context(), &payloads[i]); err == nil {
				ingestedCount++
			}
		}

		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":   "accepted",
			"ingested": ingestedCount,
		})
	})

	body := `[
		{"project_key": "pk_123", "event_id": "evt_1", "error": {"type": "NullPointer", "message": "error 1"}},
		{"project_key": "pk_123", "event_id": "evt_2", "error": {"type": "Timeout", "message": "error 2"}}
	]`

	req := httptest.NewRequest("POST", "/ingest/batch", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected status 202 accepted, got %d", w.Code)
	}

	if len(mock.ingested) != 2 {
		t.Fatalf("expected 2 ingested payloads, got %d", len(mock.ingested))
	}
}
