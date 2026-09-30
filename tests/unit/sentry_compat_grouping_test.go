package unit

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/mapping"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/sentrycompat"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/validation"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/event"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/symbolicate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// scFingerprint runs a Sentry event through the whole write path up to the processor's grouping decision:
// MapEvent, ValidatePayload (the bounds step), MapPayloadToEvent, proto.Marshal, event.Deserialize and
// ComputeFingerprint with the processor's default platform set. Two events with the same fingerprint are one
// issue (issue identity is the unique (project_id, environment, fingerprint), spec T13).
func scFingerprint(t *testing.T, js []byte) string {
	t.Helper()
	p, err := sentrycompat.MapEvent(js, sentrycompat.EnvelopeHeader{})
	require.NoError(t, err)
	p.ProjectKey = "proj-sc"
	require.True(t, validation.ValidatePayload(p).Valid)
	data, err := proto.Marshal(mapping.MapPayloadToEvent(p))
	require.NoError(t, err)
	evt, err := event.Deserialize(data)
	require.NoError(t, err)
	evt.ComputeFingerprint(symbolicate.DefaultPlatforms)
	require.NotEmpty(t, evt.Fingerprint)
	return evt.Fingerprint
}

func scJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}

func scFixtureMap(t *testing.T, name string) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(scFixtureEvent(t, name), &m))
	return m
}

func scDartEvent(frames ...map[string]any) []byte {
	fr := make([]any, len(frames))
	for i, f := range frames {
		fr[i] = f
	}
	b, _ := json.Marshal(map[string]any{
		"platform":  "other",
		"sdk":       map[string]any{"name": "sentry.dart.flutter", "version": "9.6.0"},
		"exception": map[string]any{"values": []any{map[string]any{"type": "TypeError", "value": "Null check operator used on a null value", "stacktrace": map[string]any{"frames": fr}}}},
	})
	return b
}

func scDartFrame(pkgPath, fn string, inApp bool) map[string]any {
	return map[string]any{"abs_path": pkgPath, "filename": pkgPath[strings.LastIndex(pkgPath, "/")+1:], "function": fn, "lineno": 10, "in_app": inApp}
}

// P5.1
func TestSentryDartSameErrorTwoDevicesOneIssue(t *testing.T) {
	a := scFixtureMap(t, "flutter-typeerror-unhandled.json")
	b := scFixtureMap(t, "flutter-typeerror-unhandled.json")
	b["event_id"] = "99999999999949998999999999999999"
	b["timestamp"] = 1790329999.5
	ctx := b["contexts"].(map[string]any)
	dev := ctx["device"].(map[string]any)
	dev["model"], dev["id"], dev["memory_size"] = "Pixel 9", "0000000000000777", 8589934592.0
	b["user"].(map[string]any)["id"] = "user-00000777"
	crumbs := b["breadcrumbs"].(map[string]any)["values"].([]any)
	b["breadcrumbs"].(map[string]any)["values"] = crumbs[12:]
	for _, f := range b["exception"].(map[string]any)["values"].([]any)[0].(map[string]any)["stacktrace"].(map[string]any)["frames"].([]any) {
		fm := f.(map[string]any)
		fm["lineno"] = fm["lineno"].(float64) + 7
	}
	assert.Equal(t, scFingerprint(t, scJSON(t, a)), scFingerprint(t, scJSON(t, b)))
}

// P5.2: the same error reached through one entry point; only the in-app frame just before the crash differs.
func TestSentryDartSameEntryTwoErrorsTwoIssues(t *testing.T) {
	// Sentry order: outermost first.
	outer := []map[string]any{
		scDartFrame("package:daya/main.dart", "main", true),
		scDartFrame("package:daya/app.dart", "App.run", true),
		scDartFrame("package:daya/router.dart", "Router.go", true),
	}
	crash := scDartFrame("package:flutter/src/widgets/framework.dart", "State.setState", false)
	build := func(inner map[string]any) []byte {
		fr := append(append([]map[string]any{}, outer...), inner, crash)
		return scDartEvent(fr...)
	}
	one := build(scDartFrame("package:daya/presentation/screens/visitation/visitation_camera.dart", "_VisitationCameraState._disposeCamera", true))
	two := build(scDartFrame("package:daya/presentation/screens/profile/profile_screen.dart", "_ProfileState.load", true))
	assert.NotEqual(t, scFingerprint(t, one), scFingerprint(t, two))
	assert.Equal(t, scFingerprint(t, one), scFingerprint(t, one), "and the same event is stable")
}

// P5.3
func TestSentryDartPackagesDoNotCollide(t *testing.T) {
	mk := func(pkg string) []byte {
		return scDartEvent(
			scDartFrame("package:"+pkg+"/utils.dart", "helper", true),
		)
	}
	assert.NotEqual(t, scFingerprint(t, mk("alpha")), scFingerprint(t, mk("beta")))
	// F1: abs_path is the file, so the two frames' shared filename does not make them one.
	a := scDartFrame("package:alpha/utils.dart", "helper", true)
	b := scDartFrame("package:beta/utils.dart", "helper", true)
	assert.Equal(t, "utils.dart", a["filename"])
	assert.Equal(t, a["filename"], b["filename"])
}

// P5.4
func TestSentryDartInAppDefaults(t *testing.T) {
	noFlag := func(path string) map[string]any {
		return map[string]any{"abs_path": path, "function": "f"}
	}
	frames := func(fs ...map[string]any) []validation.StackFrame {
		p, err := sentrycompat.MapEvent(scDartEvent(fs...), sentrycompat.EnvelopeHeader{})
		require.NoError(t, err)
		return p.Stacktrace
	}
	for _, path := range []string{"dart:async/zone.dart", "package:flutter/src/widgets/framework.dart", "package:daya/x.dart"} {
		got := frames(noFlag(path))
		require.Len(t, got, 1)
		assert.Falsef(t, got[0].InApp, "%s with no in_app must be false", path)
	}
	got := frames(map[string]any{"abs_path": "package:daya/x.dart", "function": "f", "in_app": true})
	assert.True(t, got[0].InApp)
	got = frames(map[string]any{"function": "f", "in_app": true})
	assert.False(t, got[0].InApp, "in_app without abs_path/filename is false")
	assert.Equal(t, "", got[0].File)
}
