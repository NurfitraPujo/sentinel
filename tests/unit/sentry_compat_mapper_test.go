package unit

import (
	"context"
	"encoding/json"
	"flag"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/mapping"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/sentrycompat"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/validation"
	sentinelv1 "github.com/NurfitraPujo/sentinel/gen/sentinel/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

var scUpdate = flag.Bool("update", false, "rewrite the sentry-compat golden files (the test then fails on purpose)")

func scMapRaw(t *testing.T, payload []byte, env sentrycompat.EnvelopeHeader) *validation.ErrorPayload {
	t.Helper()
	p, err := sentrycompat.MapEvent(payload, env)
	require.NoError(t, err)
	return p
}

func scMap(t *testing.T, payload string) *validation.ErrorPayload {
	t.Helper()
	return scMapRaw(t, []byte(payload), sentrycompat.EnvelopeHeader{})
}

func scFixtureEvent(t *testing.T, name string) []byte { return scReadFixture(t, name) }

// P4.1
func TestSentryCompatFlutterGolden(t *testing.T) {
	p := scMapRaw(t, scFixtureEvent(t, "flutter-typeerror-unhandled.json"), sentrycompat.EnvelopeHeader{})
	got, err := json.MarshalIndent(p, "", "  ")
	require.NoError(t, err)
	got = append(got, '\n')
	golden := filepath.Join(filepath.FromSlash(scFixtureDir), "flutter-typeerror-unhandled.payload.golden.json")
	if *scUpdate {
		require.NoError(t, os.WriteFile(golden, got, 0o644))
		t.Fatal("golden rewritten; rerun without -update")
	}
	want, err := os.ReadFile(golden)
	require.NoError(t, err)
	assert.Equal(t, string(want), string(got), "the mapper's output changed; review the diff, then regenerate with -args -update")
}

// scIngest runs the mapped payload through the REAL IngestAt (the /ingest pipeline) and returns the one
// published proto. The scope stub sets what Config.Scope sets in production.
func scIngest(t *testing.T, p *validation.ErrorPayload) *sentinelv1.ErrorEvent {
	t.Helper()
	svc, pub := newTestIngestService(t)
	p.ProjectKey, p.ProjectID = "proj-sc", "0192f3a4-5b6c-4d7e-8f90-a1b2c3d4e5f6"
	require.NoError(t, svc.IngestAt(context.Background(), p, time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)))
	return publishedEvent(t, pub)
}

func scStruct(t *testing.T, e *sentinelv1.ErrorEvent, ctxName string) map[string]any {
	t.Helper()
	c, ok := e.Contexts[ctxName]
	require.Truef(t, ok, "context %q missing", ctxName)
	return c.AsMap()
}

// P4.2
func TestSentryCompatFlutterFieldsLand(t *testing.T) {
	e := scIngest(t, scMapRaw(t, scFixtureEvent(t, "flutter-typeerror-unhandled.json"), sentrycompat.EnvelopeHeader{}))
	assert.Equal(t, "dart", e.Platform)
	assert.Equal(t, "fatal", e.Level)
	assert.Equal(t, "TypeError", e.ErrorClass)
	assert.Equal(t, "Null check operator used on a null value", e.Message)
	assert.Equal(t, "production", e.Environment)
	assert.Equal(t, "zasri.zasri@1.5.28+216", e.ReleaseVersion)
	assert.Equal(t, "216", e.Dist)
	assert.Equal(t, "sentry-compat", e.Source)
	assert.Equal(t, "2026-09-25T09:41:52.838Z", e.Timestamp.AsTime().UTC().Format("2006-01-02T15:04:05.000Z"))
	assert.Equal(t, "0000000000000000000000000000c0de", e.TraceId)
	assert.Equal(t, "00000000000000a1", e.SpanId)
	assert.Equal(t, "sentry.dart.flutter", e.Sdk.Name)
	assert.Equal(t, "9.6.0", e.Sdk.Version)
	assert.Equal(t, "user-00000001", e.User.Id)
	assert.Equal(t, "", e.User.IpAddress, "{{auto}} is blanked: the unit context carries no caller IP (T12 I2)")
	assert.Equal(t, "Example City", e.User.Geo.City)
	assert.Equal(t, "Example Region", e.User.Geo.Region)
	assert.Equal(t, "ZZ", e.User.Geo.CountryCode)
	assert.Len(t, e.Tags, 22)
	assert.Equal(t, "PlatformDispatcher.onError", e.Tags["mechanism"])
	assert.Len(t, e.Contexts, 11)
	assert.Equal(t, "Infinix", scStruct(t, e, "device")["family"])
	require.Len(t, e.Breadcrumbs, 70)
	b0 := e.Breadcrumbs[0]
	assert.Equal(t, "2026-09-25T07:20:41.195Z", b0.Timestamp.AsTime().UTC().Format("2006-01-02T15:04:05.000Z"))
	assert.Equal(t, "navigation", b0.Type)
	assert.Equal(t, "app.lifecycle", b0.Category)
	assert.Equal(t, "info", b0.Level)
	assert.Equal(t, "foreground", b0.Data.AsMap()["state"])
	require.Len(t, e.Exception.Values, 1)
	v := e.Exception.Values[0]
	assert.Equal(t, "TypeError", v.Type)
	assert.Equal(t, "PlatformDispatcher.onError", v.Mechanism.Type)
	assert.False(t, v.Mechanism.GetHandled())
	require.Len(t, e.Stacktrace, 2)
	assert.Equal(t, "State.setState", e.Stacktrace[0].Function)
	assert.Equal(t, "package:flutter/src/widgets/framework.dart", e.Stacktrace[0].File)
	assert.EqualValues(t, 1219, e.Stacktrace[0].Line)
	assert.False(t, e.Stacktrace[0].InApp)
	assert.Equal(t, "_VisitationCameraState._disposeCamera", e.Stacktrace[1].Function)
	assert.Equal(t, "package:daya/presentation/screens/visitation/visitation_camera.dart", e.Stacktrace[1].File)
	assert.EqualValues(t, 91, e.Stacktrace[1].Line)
	assert.True(t, e.Stacktrace[1].InApp)
	assert.Equal(t, "", e.Fingerprint)
	if e.Metadata != nil {
		assert.NotContains(t, e.Metadata.AsMap(), "server_truncated")
	}

	d := scIngest(t, scMapRaw(t, scFixtureEvent(t, "flutter-dio-error.json"), sentrycompat.EnvelopeHeader{}))
	assert.Equal(t, "dart", d.Platform)
	assert.Equal(t, "error", d.Level)
	assert.Equal(t, "POST /auths", d.Transaction)
	assert.Equal(t, "POST", d.Request.Method)
	assert.Equal(t, "https://api.example.invalid/api/v1/auths/refresh-token", d.Request.Url)
	assert.Len(t, d.Contexts, 12)
	assert.Contains(t, d.Contexts, "API Request Info")
	assert.Len(t, d.Breadcrumbs, 55)
	require.Len(t, d.Exception.Values, 1)
	assert.Nil(t, d.Exception.Values[0].Mechanism, "the Dio event carries no mechanism")
	require.Len(t, d.Stacktrace, 12)
	assert.Equal(t, "DioMixin.request", d.Stacktrace[0].Function)
	assert.Equal(t, "package:dio/src/dio_mixin.dart", d.Stacktrace[0].File)
}

// P4.3: one case per row of §5.2-§5.5.
func TestSentryCompatMappingRows(t *testing.T) {
	m := func(js string) *validation.ErrorPayload { return scMap(t, js) }

	t.Run("M1 event id", func(t *testing.T) {
		assert.Equal(t, "abc", m(`{"event_id":"abc"}`).EventID)
		assert.Equal(t, "hdr", scMapRaw(t, []byte(`{}`), sentrycompat.EnvelopeHeader{EventID: "hdr"}).EventID)
		assert.Equal(t, "hdr", scMapRaw(t, []byte(`{"event_id":7}`), sentrycompat.EnvelopeHeader{EventID: "hdr"}).EventID)
		assert.Equal(t, "payload", scMapRaw(t, []byte(`{"event_id":"payload"}`), sentrycompat.EnvelopeHeader{EventID: "hdr"}).EventID)
		assert.Equal(t, "", m(`{}`).EventID)
		assert.Equal(t, "a\x00b", m(`{"event_id":"a\u0000b"}`).EventID, "passed raw so IngestAt re-mints it")
	})
	t.Run("M2 timestamps", func(t *testing.T) {
		want := time.Date(2026, 9, 25, 9, 41, 52, 838_000_000, time.UTC)
		for name, js := range map[string]string{
			"float":          `{"timestamp":1790329312.838}`,
			"rfc3339":        `{"timestamp":"2026-09-25T09:41:52.838Z"}`,
			"rfc3339 zone":   `{"timestamp":"2026-09-25T16:41:52.838+07:00"}`,
			"zone-less":      `{"timestamp":"2026-09-25T09:41:52.838"}`,
			"numeric string": `{"timestamp":"1790329312.838"}`,
		} {
			assert.True(t, want.Equal(m(js).Timestamp), name)
			assert.Equal(t, time.UTC, m(js).Timestamp.Location(), name)
		}
		for name, js := range map[string]string{
			"garbage": `{"timestamp":"yesterday"}`, "zero": `{"timestamp":0}`, "negative": `{"timestamp":-5}`,
			"year 10000": `{"timestamp":253402300800}`, "bool": `{"timestamp":true}`, "absent": `{}`, "nan string": `{"timestamp":"NaN"}`,
		} {
			assert.True(t, m(js).Timestamp.IsZero(), name)
		}
	})
	t.Run("M3 platform", func(t *testing.T) {
		assert.Equal(t, "python", m(`{"platform":"python"}`).Platform)
		assert.Equal(t, "other", m(`{"platform":"Flutter!"}`).Platform)
		assert.Equal(t, "other", m(`{"platform":"Dart"}`).Platform)
		assert.Equal(t, "other", m(`{}`).Platform)
		assert.Equal(t, "other", m(`{"platform":"`+strings.Repeat("a", 51)+`"}`).Platform)
		assert.Equal(t, "dart", m(`{"platform":"other","sdk":{"name":"sentry.dart.flutter"}}`).Platform)
		assert.Equal(t, "dart", m(`{"platform":"Dart","sdk":{"name":"sentry.dart"}}`).Platform)
		assert.Equal(t, "dart", scMapRaw(t, []byte(`{"platform":"other"}`), sentrycompat.EnvelopeHeader{SDK: &validation.SDKInfo{Name: "sentry.dart.flutter"}}).Platform)
		assert.Equal(t, "javascript", m(`{"platform":"javascript","sdk":{"name":"sentry.dart"}}`).Platform)
	})
	t.Run("M4 level", func(t *testing.T) {
		assert.Equal(t, "info", m(`{"level":"log"}`).Level)
		assert.Equal(t, "fatal", m(`{"level":" FATAL "}`).Level)
		assert.Equal(t, "weird", m(`{"level":"weird"}`).Level)
		assert.Equal(t, "", m(`{}`).Level)
	})
	t.Run("M5 M6 M7", func(t *testing.T) {
		p := m(`{"logger":"l","server_name":"s","transaction":"t","dist":"9","release":"` + strings.Repeat("r", 150) + `"}`)
		assert.Equal(t, []string{"l", "s", "t", "9"}, []string{p.Logger, p.ServerName, p.Transaction, p.Dist})
		assert.Len(t, p.ReleaseVersion, 100)
		assert.Equal(t, "production", m(`{"environment":"Production "}`).Environment)
		assert.Equal(t, "staging", m(`{"environment":" Staging "}`).Environment)
		assert.Equal(t, "production", m(`{"environment":"staging area"}`).Environment)
		assert.Equal(t, "production", m(`{}`).Environment)
		assert.Equal(t, "qa.1_x-y", m(`{"environment":"qa.1_x-y"}`).Environment)
	})
	t.Run("M8 M10 message", func(t *testing.T) {
		for name, tc := range map[string]struct{ js, msg, class string }{
			"string":             {`{"message":"boom\nline2"}`, "boom\nline2", "boom"},
			"message object":     {`{"message":{"formatted":"fmt","message":"raw"}}`, "fmt", "fmt"},
			"message object raw": {`{"message":{"message":"raw"}}`, "raw", "raw"},
			"logentry formatted": {`{"logentry":{"formatted":"le","message":"lm"},"message":"m"}`, "le", "le"},
			"logentry message":   {`{"logentry":{"message":"lm"},"message":"m"}`, "lm", "lm"},
			"blank first line":   {`{"message":"\n  \n  hello  \nx"}`, "\n  \n  hello  \nx", "hello"},
			"nothing":            {`{}`, "", "UnknownError"},
			"empty object":       {`{}`, "", "UnknownError"},
		} {
			p := m(tc.js)
			assert.Equal(t, tc.msg, p.Message, name)
			assert.Equal(t, tc.class, p.ErrorClass, name)
			assert.Nil(t, p.Exception, name)
		}
		long := m(`{"message":"` + strings.Repeat("é", 20000) + `"}`)
		assert.Equal(t, 10000, utf8.RuneCountInString(long.Message))
		assert.Equal(t, 255, utf8.RuneCountInString(long.ErrorClass))
	})
	t.Run("E1-E4 exception", func(t *testing.T) {
		p := m(`{"exception":{"values":[{"type":"A","value":"first"},{"type":" TypeError ","value":"raised","module":"m","mechanism":{"type":"generic","handled":true}}]},"message":"m"}`)
		require.Len(t, p.Exception.Values, 2)
		assert.Equal(t, "A", p.Exception.Values[0].Type)
		assert.Equal(t, "TypeError", p.ErrorClass, "the LAST value titles the event (D3)")
		assert.Equal(t, "raised", p.Message)
		require.NotNil(t, p.Exception.Values[1].Mechanism.Handled)
		assert.True(t, *p.Exception.Values[1].Mechanism.Handled)
		assert.Nil(t, p.Exception.Values[0].Mechanism)
		// an array instead of {values}; non-object elements skipped
		p = m(`{"exception":[1,{"type":"E","value":"v"}]}`)
		require.Len(t, p.Exception.Values, 1)
		// handled that is not a bool is unset
		p = m(`{"exception":{"values":[{"type":"E","mechanism":{"type":"x","handled":"yes"}}]}}`)
		assert.Nil(t, p.Exception.Values[0].Mechanism.Handled)
		// no type: the first line of the value (V2)
		p = m(`{"exception":{"values":[{"value":"\n  Bad thing\nmore"}]}}`)
		assert.Equal(t, "Bad thing", p.ErrorClass)
		// no type and no value: the message, then UnknownError
		p = m(`{"exception":{"values":[{}]},"message":"from message"}`)
		assert.Equal(t, "from message", p.ErrorClass)
		assert.Equal(t, "from message", p.Message)
		p = m(`{"exception":{"values":[{}]}}`)
		assert.Equal(t, "UnknownError", p.ErrorClass)
		// a value-less exception falls back to the message text for message
		p = m(`{"exception":{"values":[{"type":"T"}]},"message":"msg"}`)
		assert.Equal(t, "msg", p.Message)
		// no element at all: exception nil, M10 applies
		p = m(`{"exception":{"values":[1,2]},"message":"only message"}`)
		assert.Nil(t, p.Exception)
		assert.Equal(t, "only message", p.ErrorClass)
		// a class of 400 chars is cut to 255
		p = m(`{"exception":{"values":[{"type":"` + strings.Repeat("T", 400) + `"}]}}`)
		assert.Len(t, p.ErrorClass, 255)
	})
	t.Run("S1-S3 stacktrace", func(t *testing.T) {
		frames := func(names ...string) string {
			var fs []string
			for _, n := range names {
				fs = append(fs, `{"function":"`+n+`"}`)
			}
			return `{"frames":[` + strings.Join(fs, ",") + `]}`
		}
		fn := func(fr []validation.StackFrame) []string {
			var out []string
			for _, f := range fr {
				out = append(out, f.Function)
			}
			return out
		}
		p := m(`{"exception":{"values":[{"type":"T","stacktrace":` + frames("outer", "mid", "crash") + `}]}}`)
		assert.Equal(t, []string{"crash", "mid", "outer"}, fn(p.Stacktrace), "S1: crash-first")
		assert.Equal(t, []string{"crash", "mid", "outer"}, fn(p.Exception.Values[0].Stacktrace), "chain frames are reversed too")
		// threads fallback: crashed > current > first
		th := func(order string) string {
			return `{"exception":{"values":[{"type":"T"}]},"threads":` + order + `}`
		}
		crashed := `{"values":[{"stacktrace":` + frames("first") + `},{"current":true,"stacktrace":` + frames("cur") + `},{"crashed":true,"stacktrace":` + frames("a", "b") + `}]}`
		assert.Equal(t, []string{"b", "a"}, fn(m(th(crashed)).Stacktrace))
		current := `[{"stacktrace":` + frames("first") + `},{"current":true,"stacktrace":` + frames("cur") + `}]`
		assert.Equal(t, []string{"cur"}, fn(m(th(current)).Stacktrace))
		first := `[{"stacktrace":{"frames":[]}},{"stacktrace":` + frames("x") + `}]`
		assert.Equal(t, []string{"x"}, fn(m(th(first)).Stacktrace))
		assert.Nil(t, m(th(`[{"crashed":true,"stacktrace":{"frames":[]}}]`)).Stacktrace)
		// the exception's own frames win over threads
		p = m(`{"exception":{"values":[{"type":"T","stacktrace":` + frames("e") + `}]},"threads":` + current + `}`)
		assert.Equal(t, []string{"e"}, fn(p.Stacktrace))
		// S3: over 100 keeps the crash end (first 100 after reversal)
		var many []string
		for i := 0; i < 150; i++ {
			many = append(many, "f"+strconvI(i))
		}
		p = m(`{"exception":{"values":[{"type":"T","stacktrace":` + frames(many...) + `}]}}`)
		require.Len(t, p.Stacktrace, 100)
		assert.Equal(t, "f149", p.Stacktrace[0].Function)
		assert.Len(t, p.Exception.Values[0].Stacktrace, 150, "chain frames are left to the bounds step")
	})
	t.Run("F1-F8 frames", func(t *testing.T) {
		fr := func(js string) validation.StackFrame {
			p := m(`{"exception":{"values":[{"type":"T","stacktrace":{"frames":[` + js + `]}}]}}`)
			require.Len(t, p.Stacktrace, 1)
			return p.Stacktrace[0]
		}
		f := fr(`{"abs_path":"package:a/x.dart","filename":"x.dart","function":"fn","module":"mod","context_line":"cl","lineno":12,"colno":3,"in_app":true,"pre_context":["a",1,"c"],"post_context":"no","platform":"dart","vars":{}}`)
		assert.Equal(t, validation.StackFrame{File: "package:a/x.dart", AbsPath: "package:a/x.dart", Function: "fn", Module: "mod", ContextLine: "cl", Line: 12, Column: 3, InApp: true, PreContext: []string{"a", "", "c"}}, f)
		assert.Equal(t, "x.dart", fr(`{"filename":"x.dart"}`).File)
		assert.Equal(t, "", fr(`{"filename":"x.dart"}`).AbsPath)
		assert.EqualValues(t, 0, fr(`{"colno":-3}`).Column)
		assert.EqualValues(t, 0, fr(`{"lineno":3000000000}`).Line)
		assert.EqualValues(t, 0, fr(`{"lineno":0}`).Line)
		assert.EqualValues(t, 7, fr(`{"lineno":7.9}`).Line)
		assert.EqualValues(t, math.MaxInt32, fr(`{"lineno":2147483647}`).Line)
		assert.False(t, fr(`{"in_app":true}`).InApp, "in_app without a file is false (F7)")
		assert.False(t, fr(`{"filename":"x","in_app":"true"}`).InApp)
		assert.False(t, fr(`{"filename":"x"}`).InApp)
		assert.Len(t, fr(`{"abs_path":"`+strings.Repeat("é", 600)+`"}`).File, 512, "file is cut on a rune boundary")
		assert.Len(t, fr(`{"function":"`+strings.Repeat("日", 300)+`"}`).Function, 255)
	})
	t.Run("M12 breadcrumbs", func(t *testing.T) {
		vals := `[{"timestamp":1790329312.5,"type":"t","category":"c","level":"log","message":"m","data":{"a":[1,{"b":null}]}},3,{"timestamp":"junk","data":[1]}]`
		for name, js := range map[string]string{"values": `{"breadcrumbs":{"values":` + vals + `}}`, "array": `{"breadcrumbs":` + vals + `}`} {
			p := m(js)
			require.Len(t, p.Breadcrumbs, 2, name)
			assert.Equal(t, "info", p.Breadcrumbs[0].Level)
			assert.Equal(t, "m", p.Breadcrumbs[0].Message)
			assert.Equal(t, map[string]interface{}{"a": []interface{}{1.0, map[string]interface{}{"b": nil}}}, p.Breadcrumbs[0].Data)
			assert.True(t, p.Breadcrumbs[1].Timestamp.IsZero())
			assert.Nil(t, p.Breadcrumbs[1].Data)
		}
		assert.Nil(t, m(`{"breadcrumbs":{"values":[]}}`).Breadcrumbs)
	})
	t.Run("M13 user", func(t *testing.T) {
		p := m(`{"user":{"id":12345678901234567890,"username":"u","email":"e@example.test","ip_address":"{{auto}}","geo":{"city":"c","region":"r","country_code":"ZZ","subdivision":"s"},"name":"dropped"}}`)
		assert.Equal(t, &validation.UserInfo{ID: "12345678901234567890", Username: "u", Email: "e@example.test", IPAddress: "{{auto}}",
			Geo: &validation.GeoInfo{City: "c", Region: "r", CountryCode: "ZZ", Subdivision: "s"}}, p.User)
		assert.Nil(t, m(`{"user":{"name":"x"}}`).User)
		assert.Nil(t, m(`{"user":"x"}`).User)
		assert.NotNil(t, m(`{"user":{"geo":{}}}`).User)
		assert.Equal(t, "true", m(`{"user":{"id":true}}`).User.ID)
	})
	t.Run("M14 sdk", func(t *testing.T) {
		assert.Equal(t, &validation.SDKInfo{Name: "n", Version: "1"}, m(`{"sdk":{"name":"n","version":"1","integrations":["x"]}}`).SDK)
		assert.Nil(t, m(`{"sdk":{}}`).SDK)
		p := scMapRaw(t, []byte(`{}`), sentrycompat.EnvelopeHeader{SDK: &validation.SDKInfo{Name: "hdr", Version: "2"}})
		assert.Equal(t, &validation.SDKInfo{Name: "hdr", Version: "2"}, p.SDK)
	})
	t.Run("M15 tags", func(t *testing.T) {
		assert.Equal(t, map[string]string{"a": "1", "b": "true", "c": "x", "d": "1.0"}, m(`{"tags":{"a":1,"b":true,"c":"x","d":1.0,"e":null,"f":[1],"g":{}}}`).Tags)
		assert.Equal(t, map[string]string{"a": "2", "b": "x"}, m(`{"tags":[["a","1"],["a",2],["b","x"],["c"],[1,"x"],"junk",["d",null]]}`).Tags, "pairs: a later pair wins; junk skipped")
		assert.Nil(t, m(`{"tags":{}}`).Tags)
		assert.Nil(t, m(`{"tags":"x"}`).Tags)
		// D14: a body relay_auth is always removed, in both forms
		assert.Equal(t, map[string]string{"a": "b"}, m(`{"tags":{"relay_auth":"user","a":"b"}}`).Tags)
		assert.Equal(t, map[string]string{"a": "b"}, m(`{"tags":[["relay_auth","user"],["a","b"]]}`).Tags)
		// finding d14-nul-tag-key: NUL is stripped from keys BEFORE the delete, both forms
		want := map[string]string{"tenant_id": "victim-tenant", "env": "b"}
		assert.Equal(t, want, m(`{"tags":{"relay_auth\u0000":"user","tenant_id\u0000":"victim-tenant","env\u0000":"a","env":"b"}}`).Tags)
		assert.Equal(t, want, m(`{"tags":[["relay_auth\u0000","user"],["tenant_id\u0000","victim-tenant"],["env\u0000","a"],["env","b"]]}`).Tags)
		// several NUL-bearing keys that clean to one key: the first in sorted raw-key order wins
		assert.Equal(t, map[string]string{"k": "first"}, m(`{"tags":{"k\u0000\u0000":"second","k\u0000":"first"}}`).Tags)
		// and a NUL in a value is removed
		assert.Equal(t, map[string]string{"k": "ab"}, m(`{"tags":{"k":"a\u0000b"}}`).Tags)
	})
	t.Run("M16 M17 contexts", func(t *testing.T) {
		p := m(`{"contexts":{"trace":{"trace_id":"t1","span_id":"s1","n":{"x":[1,2]}},"device":{"family":"F"},"bad":"str","arr":[1],"nul\u0000":{"a\u0000b":"c\u0000d"}}}`)
		assert.Equal(t, []string{"device", "nul", "trace"}, sortedKeys(p.Contexts))
		assert.Equal(t, map[string]interface{}{"ab": "cd"}, p.Contexts["nul"])
		assert.Equal(t, "t1", p.TraceID)
		assert.Equal(t, "s1", p.SpanID)
		p = m(`{"contexts":{"trace":{"trace_id":"` + strings.Repeat("a", 65) + `","span_id":7}}}`)
		assert.Equal(t, "", p.TraceID)
		assert.Equal(t, "", p.SpanID)
		assert.NotNil(t, p.Contexts["trace"])
		assert.Nil(t, m(`{"contexts":{}}`).Contexts)
	})
	t.Run("Q1-Q5 request", func(t *testing.T) {
		p := m(`{"request":{"method":"POST","url":"https://x.example.invalid/a","query_string":"?a=1&b=2","headers":{"A":"1","B":2,"C":null},"data":{"k":[1]},"env":{"REMOTE_ADDR":"x","N":1},"cookies":"dropped"}}`)
		assert.Equal(t, &validation.RequestInfo{Method: "POST", URL: "https://x.example.invalid/a", QueryString: "a=1&b=2",
			Headers: map[string]string{"A": "1", "B": "2"}, Data: map[string]interface{}{"k": []interface{}{1.0}}, Env: map[string]string{"REMOTE_ADDR": "x", "N": "1"}}, p.Request)
		assert.Equal(t, "a=1&b=x+y", m(`{"request":{"query_string":{"b":"x y","a":1}}}`).Request.QueryString)
		assert.Equal(t, "b=2&a=1&b=3", m(`{"request":{"query_string":[["b",2],["a","1"],["b",3]]}}`).Request.QueryString)
		assert.Equal(t, map[string]string{"Accept": "a, b", "X": "1"}, m(`{"request":{"headers":[["Accept","a"],["X",1],["Accept","b"]]}}`).Request.Headers)
		assert.Nil(t, m(`{"request":{"data":null}}`).Request)
		assert.Nil(t, m(`{"request":{}}`).Request)
		assert.Nil(t, m(`{"request":"x"}`).Request)
	})
	t.Run("M19 extra", func(t *testing.T) {
		p := m(`{"extra":{"a":1,"b":{"c":[true,null]},"n\u0000":"v\u0000"}}`)
		assert.Equal(t, map[string]interface{}{"a": 1.0, "b": map[string]interface{}{"c": []interface{}{true, nil}}, "n": "v"}, p.Metadata)
		assert.Nil(t, m(`{"extra":[1]}`).Metadata)
		assert.Nil(t, m(`{"extra":{}}`).Metadata)
	})
	t.Run("M20 fingerprint", func(t *testing.T) {
		assert.Equal(t, "", m(`{"fingerprint":["{{ default }}"]}`).Fingerprint)
		assert.Equal(t, "", m(`{"fingerprint":["{{default}}"]}`).Fingerprint)
		assert.Equal(t, "", m(`{"fingerprint":["{{ DEFAULT }}","x"]}`).Fingerprint, "no partial-default composition (D5)")
		assert.Equal(t, "eb3452a2abffd1c9c66915517b6ac1b0", m(`{"fingerprint":["checkout","timeout"]}`).Fingerprint)
		assert.Equal(t, m(`{"fingerprint":["checkout"]}`).Fingerprint, m(`{"fingerprint":"checkout"}`).Fingerprint)
		assert.Equal(t, "", m(`{"fingerprint":[]}`).Fingerprint)
		assert.Equal(t, "", m(`{"fingerprint":[null,{}]}`).Fingerprint)
		assert.Equal(t, "", m(`{"fingerprint":[""]}`).Fingerprint, "only empty strings: the server computes")
		assert.Len(t, m(`{"fingerprint":[1,2]}`).Fingerprint, 32)
	})
	t.Run("M21 M22 M23", func(t *testing.T) {
		p := m(`{"modules":{"a":"1"},"debug_meta":{},"culprit":"c","type":"error","spans":[],"project_key":"x","project_id":"y","trace_flags":3,"fingerprint_override":true}`)
		assert.Equal(t, "sentry-compat", p.Source)
		assert.Equal(t, "", p.ProjectKey)
		assert.Equal(t, "", p.ProjectID)
		assert.Zero(t, p.TraceFlags)
		assert.False(t, p.FingerprintOverride)
		assert.True(t, p.ReceivedAt.IsZero())
	})
	t.Run("not an object", func(t *testing.T) {
		for _, js := range []string{`[1]`, `"x"`, `null`, `7`, ``, `{`, `not json`} {
			_, err := sentrycompat.MapEvent([]byte(js), sentrycompat.EnvelopeHeader{})
			assert.ErrorIs(t, err, sentrycompat.ErrInvalidEvent, js)
		}
	})
}

func strconvI(i int) string { b, _ := json.Marshal(i); return string(b) }

func sortedKeys[V any](m map[string]V) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	for i := range ks {
		for j := i + 1; j < len(ks); j++ {
			if ks[j] < ks[i] {
				ks[i], ks[j] = ks[j], ks[i]
			}
		}
	}
	return ks
}

// P4.4
func TestSentryCompatFreeFormSurvivesStructpb(t *testing.T) {
	nested := `{"s":"x","n":1.5,"b":true,"z":null,"arr":[1,"a",{"k":[false]}],"o":{"p":{"q":2}}}`
	js := `{"contexts":{"c":` + nested + `},"extra":` + nested + `,"request":{"data":` + nested + `},"breadcrumbs":[{"message":"m","data":` + nested + `}]}`
	p := scMap(t, js)
	ev := mapping.MapPayloadToEvent(p)
	require.NotNil(t, ev.Metadata, "metadata must not be dropped silently (T11)")
	require.Contains(t, ev.Contexts, "c")
	require.NotNil(t, ev.Request.Data)
	require.Len(t, ev.Breadcrumbs, 1)
	require.NotNil(t, ev.Breadcrumbs[0].Data)
	var want map[string]any
	require.NoError(t, json.Unmarshal([]byte(nested), &want))
	assert.Equal(t, want, ev.Metadata.AsMap())
	assert.Equal(t, want, ev.Contexts["c"].AsMap())
	assert.Equal(t, want, ev.Request.Data.AsInterface())
	assert.Equal(t, want, ev.Breadcrumbs[0].Data.AsMap())

	// D-b's unit of loss: a value encoding/json cannot hold drops that one key / context, never the whole field.
	p = scMap(t, `{"extra":{"huge":1e400,"keep":1},"contexts":{"bad":{"n":1e400},"good":{"n":1}}}`)
	assert.Equal(t, map[string]interface{}{"keep": 1.0}, p.Metadata)
	assert.Equal(t, []string{"good"}, sortedKeys(p.Contexts))
	ev = mapping.MapPayloadToEvent(p)
	require.NotNil(t, ev.Metadata)
	assert.Contains(t, ev.Contexts, "good")
}

// scHostile is a table of events built to make IngestAt reject or fail.
func scHostileEvents() map[string]string {
	rep := strings.Repeat
	frames := func(n int, extra string) string {
		fs := make([]string, n)
		for i := range fs {
			fs[i] = `{"function":"f","in_app":true` + extra + `}`
		}
		return `{"frames":[` + strings.Join(fs, ",") + `]}`
	}
	values := make([]string, 25)
	for i := range values {
		values[i] = `{"type":"E` + strconvI(i) + `","value":"v","stacktrace":` + frames(3, "") + `}`
	}
	return map[string]string{
		"every field the wrong type": `{"event_id":7,"timestamp":[],"platform":1,"level":2,"logger":[],"server_name":{},"transaction":3,"dist":4,"release":5,"environment":6,"message":7,"logentry":8,"exception":"x","threads":"x","breadcrumbs":"x","user":"x","sdk":9,"tags":10,"contexts":11,"request":12,"extra":13,"fingerprint":14}`,
		"150 frames":                 `{"exception":{"values":[{"type":"T","stacktrace":` + frames(150, "") + `}]}}`,
		"25 exception values":        `{"exception":{"values":[` + strings.Join(values, ",") + `]}}`,
		"class 400 chars":            `{"exception":{"values":[{"type":"` + rep("C", 400) + `"}]}}`,
		"message 20000":              `{"message":"` + rep("m", 20000) + `"}`,
		"release 150":                `{"release":"` + rep("r", 150) + `"}`,
		"env PROD ENV":               `{"environment":"PROD ENV"}`,
		"platform Flutter.Mobile":    `{"platform":"Flutter.Mobile"}`,
		"trace 100":                  `{"contexts":{"trace":{"trace_id":"` + rep("t", 100) + `","span_id":"` + rep("s", 100) + `"}}}`,
		"in_app without file":        `{"exception":{"values":[{"type":"T","stacktrace":` + frames(3, "") + `}]}}`,
		"colno -1":                   `{"exception":{"values":[{"type":"T","stacktrace":` + frames(2, `,"colno":-1,"lineno":-5`) + `}]}}`,
		"NUL in every string":        `{"event_id":"a\u0000b","release":"r\u0000","environment":"p\u0000","message":"m\u0000","logger":"l\u0000","exception":{"values":[{"type":"T\u0000","value":"v\u0000","stacktrace":{"frames":[{"function":"f\u0000","abs_path":"p\u0000","module":"m\u0000","context_line":"c\u0000","pre_context":["x\u0000"]}]}}]},"tags":{"k\u0000":"v\u0000"},"extra":{"k\u0000":"v\u0000","n":{"i\u0000":"\u0000"}},"user":{"id":"u\u0000","email":"e\u0000"},"contexts":{"c\u0000":{"k\u0000":"v\u0000"}},"breadcrumbs":[{"message":"m\u0000","data":{"k\u0000":"v\u0000"}}],"request":{"url":"u\u0000","headers":{"h\u0000":"v\u0000"},"data":"d\u0000"}}`,
		"invalid UTF-8":              "{\"message\":\"a\xff\xfeb\",\"logger\":\"\xc3\x28\",\"tags\":{\"k\xff\":\"v\xfe\"},\"extra\":{\"\xff\":\"\xfe\"}}",
		"empty object":               `{}`,
		"long file and function":     `{"exception":{"values":[{"type":"T","stacktrace":{"frames":[{"abs_path":"a` + rep("é", 600) + `","function":"ab` + rep("日", 300) + `","in_app":true}]}}]}}`,
		"type is only NUL":           `{"exception":{"values":[{"type":"\u0000","value":"the value"}]}}`,
		"in_app with NUL-only path":  `{"exception":{"values":[{"type":"T","stacktrace":{"frames":[{"in_app":true,"abs_path":"\u0000"}]}}]}}`,
	}
}

func scWalkStrings(t *testing.T, m protoreflect.Message, path string, fn func(path, s string)) {
	t.Helper()
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		p := path + "." + string(fd.Name())
		var visit func(fd protoreflect.FieldDescriptor, v protoreflect.Value, p string)
		visit = func(fd protoreflect.FieldDescriptor, v protoreflect.Value, p string) {
			switch {
			case fd.IsList():
				l := v.List()
				for i := 0; i < l.Len(); i++ {
					visitElem(t, fd, l.Get(i), p, fn)
				}
			case fd.IsMap():
				mm := v.Map()
				mm.Range(func(k protoreflect.MapKey, mv protoreflect.Value) bool {
					fn(p+"{key}", k.String())
					visitElem(t, fd.MapValue(), mv, p, fn)
					return true
				})
			default:
				visitElem(t, fd, v, p, fn)
			}
		}
		visit(fd, v, p)
		return true
	})
}

func visitElem(t *testing.T, fd protoreflect.FieldDescriptor, v protoreflect.Value, p string, fn func(path, s string)) {
	t.Helper()
	switch fd.Kind() {
	case protoreflect.StringKind:
		fn(p, v.String())
	case protoreflect.MessageKind, protoreflect.GroupKind:
		msg := v.Message()
		if msg.Descriptor().FullName() == "google.protobuf.Value" || msg.Descriptor().FullName() == "google.protobuf.Struct" {
			// walk structpb through its Go accessors
			scWalkStructpb(msg.Interface(), p, fn)
			return
		}
		scWalkStrings(t, msg, p, fn)
	}
}

func scWalkStructpb(m proto.Message, p string, fn func(path, s string)) {
	b, err := protojson.Marshal(m)
	if err != nil {
		return
	}
	var v any
	if json.Unmarshal(b, &v) == nil {
		scWalkJSON(v, p, fn)
	}
}

func scWalkJSON(v any, p string, fn func(path, s string)) {
	switch x := v.(type) {
	case string:
		fn(p, x)
	case []any:
		for _, e := range x {
			scWalkJSON(e, p, fn)
		}
	case map[string]any:
		for k, e := range x {
			fn(p+"{key}", k)
			scWalkJSON(e, p, fn)
		}
	}
}

// P4.5
func TestSentryCompatNeverRejected(t *testing.T) {
	for name, js := range scHostileEvents() {
		t.Run(name, func(t *testing.T) {
			p, err := sentrycompat.MapEvent([]byte(js), sentrycompat.EnvelopeHeader{})
			require.NoError(t, err)
			ev := scIngest(t, p) // require.NoError inside: IngestAt accepted it and published exactly one message
			scWalkStrings(t, ev.ProtoReflect(), "event", func(path, s string) {
				assert.Falsef(t, strings.ContainsRune(s, 0), "%s carries U+0000: %q", path, s)
				assert.Truef(t, utf8.ValidString(s), "%s is not valid UTF-8: %q", path, s)
			})
			for _, f := range ev.Stacktrace {
				assert.LessOrEqual(t, len(f.File), 512)
				assert.LessOrEqual(t, len(f.Function), 255)
			}
			if name == "empty object" {
				assert.Equal(t, "UnknownError", ev.ErrorClass)
				assert.Equal(t, "other", ev.Platform)
				assert.Equal(t, "production", ev.Environment)
			}
			if name == "type is only NUL" {
				assert.Equal(t, "the value", ev.ErrorClass)
			}
			if name == "in_app with NUL-only path" {
				for _, f := range ev.Stacktrace {
					assert.False(t, f.InApp)
				}
			}
		})
	}
}
