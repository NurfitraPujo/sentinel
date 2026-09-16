package sentinel

import (
	"errors"
	"fmt"
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

type customSDKError struct {
	msg string
}

func (e *customSDKError) Error() string { return e.msg }

func TestExtractErrorClassUnwrapsChains(t *testing.T) {
	cfg := Config{ProjectKey: "pk_test"}
	base := &customSDKError{msg: "inner"}

	cases := []struct {
		name      string
		err       error
		wantClass string
	}{
		{"nil error", nil, "Error"},
		{"errors.New", errors.New("plain"), "*errors.errorString"},
		{"single wrap", fmt.Errorf("wrap: %w", base), "*sentinel.customSDKError"},
		{"double wrap", fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", base)), "*sentinel.customSDKError"},
		{"join wrap", errors.Join(fmt.Errorf("wrap: %w", base), errors.New("other")), "*sentinel.customSDKError"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := NewEvent(cfg, tc.err, nil)
			if ev.ErrorClass != tc.wantClass {
				t.Errorf("expected ErrorClass=%q, got %q", tc.wantClass, ev.ErrorClass)
			}
			if tc.err != nil && ev.ErrorClass == "*fmt.wrapError" {
				t.Errorf("error class must never be *fmt.wrapError")
			}
		})
	}
}

func TestNewEventOptionsAndOverrides(t *testing.T) {
	cfg := Config{ProjectKey: "pk_test"}
	err := fmt.Errorf("wrap: %w", &customSDKError{msg: "foo"})

	// Context tags override
	tags := map[string]interface{}{
		"fingerprint": "custom-tag-fp",
		"error_class": "ExplicitTagClass",
	}
	evTags := NewEvent(cfg, err, tags)
	if evTags.Fingerprint != "custom-tag-fp" {
		t.Errorf("expected Fingerprint=custom-tag-fp, got %q", evTags.Fingerprint)
	}
	if evTags.ErrorClass != "ExplicitTagClass" {
		t.Errorf("expected ErrorClass=ExplicitTagClass, got %q", evTags.ErrorClass)
	}

	// Functional options override
	evOpts := NewEvent(cfg, err, tags,
		WithEventFingerprint("fp-from-opt"),
		WithEventErrorClass("ClassFromOpt"),
	)
	if evOpts.Fingerprint != "fp-from-opt" {
		t.Errorf("expected Fingerprint=fp-from-opt, got %q", evOpts.Fingerprint)
	}
	if evOpts.ErrorClass != "ClassFromOpt" {
		t.Errorf("expected ErrorClass=ClassFromOpt, got %q", evOpts.ErrorClass)
	}
}
