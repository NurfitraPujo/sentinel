package sentinelzerolog

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/rs/zerolog"
	sentinel "github.com/tuju-ai/sentinel/packages/sdk-go"
)

func TestZerologHook(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer ts.Close()

	sentinel.Init(sentinel.Config{
		ProjectKey: "pk_test",
		Endpoint:   ts.URL,
	})

	logger := zerolog.New(os.Stdout).Hook(NewHook())
	logger.Error().Msg("zerolog error captured")

	sentinel.Flush(1 * time.Second)
}
