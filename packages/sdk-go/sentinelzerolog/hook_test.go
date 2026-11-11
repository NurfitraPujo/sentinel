package sentinelzerolog

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	sentinel "github.com/NurfitraPujo/sentinel/packages/sdk-go"
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

// TestLevelFromZerolog is P1.4: every zerolog level, NoLevel and Disabled
// included.
func TestLevelFromZerolog(t *testing.T) {
	cases := map[zerolog.Level]string{
		zerolog.TraceLevel: sentinel.LevelDebug,
		zerolog.DebugLevel: sentinel.LevelDebug,
		zerolog.InfoLevel:  sentinel.LevelInfo,
		zerolog.WarnLevel:  sentinel.LevelWarning,
		zerolog.ErrorLevel: sentinel.LevelError,
		zerolog.FatalLevel: sentinel.LevelFatal,
		zerolog.PanicLevel: sentinel.LevelFatal,
		zerolog.NoLevel:    "",
		zerolog.Disabled:   "",
		zerolog.Level(-2):  sentinel.LevelDebug,
	}
	for level, want := range cases {
		if got := LevelFromZerolog(level); got != want {
			t.Errorf("LevelFromZerolog(%v) = %q, want %q", level, got, want)
		}
	}
}

// TestLevelFromZerologIgnoresLevelValueGlobals proves the mapper reads the
// zerolog.Level enum, not Level.String(), which depends on mutable package
// globals a program may reassign (T31).
func TestLevelFromZerologIgnoresLevelValueGlobals(t *testing.T) {
	origWarn, origPanic := zerolog.LevelWarnValue, zerolog.LevelPanicValue
	t.Cleanup(func() {
		zerolog.LevelWarnValue = origWarn
		zerolog.LevelPanicValue = origPanic
	})
	zerolog.LevelWarnValue = "WRN"
	zerolog.LevelPanicValue = "boom"

	if got := LevelFromZerolog(zerolog.WarnLevel); got != sentinel.LevelWarning {
		t.Errorf("LevelFromZerolog(WarnLevel) = %q, want %q even with LevelWarnValue reassigned", got, sentinel.LevelWarning)
	}
	if got := LevelFromZerolog(zerolog.PanicLevel); got != sentinel.LevelFatal {
		t.Errorf("LevelFromZerolog(PanicLevel) = %q, want %q even with LevelPanicValue reassigned", got, sentinel.LevelFatal)
	}
}

// TestHookSendsTypedLevel is P1.4's hook proof: Error/Fatal/Panic capture
// with the typed level; Log() (NoLevel) and Warn() do not capture at all
// (D2).
func TestHookSendsTypedLevel(t *testing.T) {
	var mu sync.Mutex
	var bodies [][]map[string]interface{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var batch []map[string]interface{}
		json.NewDecoder(r.Body).Decode(&batch)
		mu.Lock()
		bodies = append(bodies, batch)
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer ts.Close()

	sentinel.Init(sentinel.Config{
		ProjectKey:    "pk_test",
		Endpoint:      ts.URL,
		MaxBufferSize: 10,
		BatchSize:     1,
		BatchWait:     50 * time.Millisecond,
	})
	logger := zerolog.New(os.Stdout).Hook(NewHook())

	logger.WithLevel(zerolog.PanicLevel).Msg("panic line")
	logger.Error().Msg("error line")
	logger.Log().Msg("no-level line")
	logger.Warn().Msg("warn line")

	sentinel.Flush(2 * time.Second)

	mu.Lock()
	defer mu.Unlock()
	var levels []string
	for _, b := range bodies {
		for _, ev := range b {
			levels = append(levels, ev["level"].(string))
		}
	}
	wantFatal, wantError := false, false
	for _, l := range levels {
		if l == "fatal" {
			wantFatal = true
		}
		if l == "error" {
			wantError = true
		}
	}
	if !wantFatal || !wantError {
		t.Errorf("expected a fatal and an error level among %v", levels)
	}
	if len(levels) != 2 {
		t.Errorf("expected exactly 2 captured events (Log() and Warn() must not capture), got %d: %v", len(levels), levels)
	}
}
