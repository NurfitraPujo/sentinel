package sentinellog

import (
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	sentinel "github.com/NurfitraPujo/sentinel/packages/sdk-go"
)

func TestStandardLogWriter(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer ts.Close()

	sentinel.Init(sentinel.Config{
		ProjectKey: "pk_test",
		Endpoint:   ts.URL,
	})

	writer := NewWriter(os.Stdout)
	stdLogger := log.New(writer, "[APP] ", log.LstdFlags)

	stdLogger.Println("standard logger error captured")

	sentinel.Flush(1 * time.Second)
}
