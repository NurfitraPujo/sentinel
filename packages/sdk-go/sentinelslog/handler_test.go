package sentinelslog

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	sentinel "github.com/NurfitraPujo/sentinel/packages/sdk-go"
)

func TestSlogHandler(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer ts.Close()

	sentinel.Init(sentinel.Config{
		ProjectKey: "pk_test",
		Endpoint:   ts.URL,
	})

	baseHandler := slog.NewTextHandler(os.Stdout, nil)
	slogHandler := NewHandler(baseHandler)
	logger := slog.New(slogHandler)

	logger.ErrorContext(context.Background(), "slog error captured", "err", errors.New("slog internal error"), "user_id", "usr_1")

	sentinel.Flush(1 * time.Second)
}
