package sentinel

import (
	"errors"
	"path/filepath"
	"runtime"
	"testing"
)

func TestIsInAppFrame(t *testing.T) {
	goroot := filepath.ToSlash(runtime.GOROOT())

	cases := []struct {
		name string
		file string
		want bool
	}{
		{"empty", "", false},
		{"stdlib", goroot + "/src/fmt/print.go", false},
		{"module cache", "/home/user/go/pkg/mod/github.com/example/pkg@v1.2.3/foo.go", false},
		{"vendor dir", "/home/user/project/vendor/github.com/example/pkg/foo.go", false},
		{"application code", "/home/user/project/internal/worker/worker.go", true},
		{"application code, different root", "/build/myapp/main.go", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isInAppFrame(tc.file)
			if got != tc.want {
				t.Errorf("isInAppFrame(%q) = %v, want %v", tc.file, got, tc.want)
			}
		})
	}
}

func TestExtractStacktracePopulatesInApp(t *testing.T) {
	frames := ExtractStacktrace(1)
	if len(frames) == 0 {
		t.Fatalf("expected at least one frame")
	}

	// The first frame is this test function's own file, which lives in the
	// SDK's source tree (not GOROOT, not a module cache, not vendor/), so it
	// must be reported in-app. Before this fix, Frame had no InApp field at
	// all and every frame was implicitly not-in-app server-side (S11).
	if !frames[0].InApp {
		t.Errorf("expected first frame (%s) to be InApp, got false", frames[0].File)
	}
}

func TestNewEventFieldsMatchWireContract(t *testing.T) {
	cfg := Config{
		ProjectKey:     "pk_test",
		Environment:    "production",
		ReleaseVersion: "1.2.3",
	}

	ev := NewEvent(cfg, errors.New("boom"), map[string]interface{}{"user_id": "u1"})

	if ev.Platform != "go" {
		t.Errorf("expected Platform=go, got %q", ev.Platform)
	}
	if ev.Message != "boom" {
		t.Errorf("expected Message=boom, got %q", ev.Message)
	}
	if ev.ReleaseVersion != "1.2.3" {
		t.Errorf("expected ReleaseVersion=1.2.3, got %q", ev.ReleaseVersion)
	}
	if ev.Metadata["user_id"] != "u1" {
		t.Errorf("expected Metadata[user_id]=u1, got %v", ev.Metadata["user_id"])
	}
}
