package unit

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"buf.build/go/protovalidate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/protobuf/proto"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/mapping"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/validation"
	"github.com/NurfitraPujo/sentinel/packages/shared-go/obs"
)

// sentinel-event-schema-v2 spec §4.2-§4.4, §9.2 V3a: the ingestor's per-field bounds step
// (validation.ValidatePayload, wired into service.IngestAt). One subtest per §4.2 row, asserting the
// output value AND the metadata.server_truncated entry, then the §4.4 cases I1-I13 (with I7b, I7c) by
// name. Every row's output is also run through protovalidate: the CEL rules are tripwires the step must
// never trip (spec §3.3).

// boundedPayload runs the bounds step on p, requires it to accept, and requires the result to pass
// protovalidate - a cut that still violated a CEL rule would 400 the event the step exists to keep.
func boundedPayload(t *testing.T, p *validation.ErrorPayload) *validation.ErrorPayload {
	t.Helper()
	res := validation.ValidatePayload(p)
	require.True(t, res.Valid, "the bounds step must not reject: %+v", res.Errors)
	require.NoError(t, protovalidate.Validate(mapping.MapPayloadToEvent(p)), "the step's output must satisfy every CEL tripwire")
	return p
}

func stringList(t *testing.T, v interface{}) []string {
	t.Helper()
	if v == nil {
		return nil
	}
	list, ok := v.([]interface{})
	require.True(t, ok, "a server marker must be a []interface{} of strings, got %T", v)
	out := make([]string, len(list))
	for i, e := range list {
		s, ok := e.(string)
		require.True(t, ok, "marker entry %d is %T", i, e)
		out[i] = s
	}
	return out
}

func serverTruncated(t *testing.T, p *validation.ErrorPayload) []string {
	t.Helper()
	return stringList(t, p.Metadata[validation.MetadataKeyServerTruncated])
}

func serverTruncatedKeys(t *testing.T, p *validation.ErrorPayload) []string {
	t.Helper()
	return stringList(t, p.Metadata[validation.MetadataKeyServerTruncatedKeys])
}

func marshalledSize(t *testing.T, v interface{}) int {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return len(b)
}

// stringRule is one truncate-or-drop row of the §4.2 table for a single string.
type stringRule struct {
	field  string // the §4.2 field name, for the subtest name
	bound  int
	marker string
	drop   bool // identifiers are dropped, everything else is truncated
	set    func(p *validation.ErrorPayload, s string)
	get    func(p *validation.ErrorPayload) string
}

func chainFrame(p *validation.ErrorPayload) *validation.StackFrame {
	if p.Exception == nil {
		p.Exception = &validation.ExceptionInterface{Values: []validation.ExceptionValue{{
			Type: "E", Stacktrace: []validation.StackFrame{{File: "a.py", Function: "f"}},
		}}}
	}
	return &p.Exception.Values[0].Stacktrace[0]
}

func topFrame(p *validation.ErrorPayload) *validation.StackFrame {
	if len(p.Stacktrace) == 0 {
		p.Stacktrace = []validation.StackFrame{{File: "main.go", Line: 1, Function: "main"}}
	}
	return &p.Stacktrace[0]
}

func ensureUser(p *validation.ErrorPayload) *validation.UserInfo {
	if p.User == nil {
		p.User = &validation.UserInfo{}
	}
	return p.User
}

func ensureGeo(p *validation.ErrorPayload) *validation.GeoInfo {
	u := ensureUser(p)
	if u.Geo == nil {
		u.Geo = &validation.GeoInfo{}
	}
	return u.Geo
}

func ensureRequest(p *validation.ErrorPayload) *validation.RequestInfo {
	if p.Request == nil {
		p.Request = &validation.RequestInfo{}
	}
	return p.Request
}

func ensureCrumb(p *validation.ErrorPayload) *validation.Breadcrumb {
	if len(p.Breadcrumbs) == 0 {
		p.Breadcrumbs = []validation.Breadcrumb{{Type: "default"}}
	}
	return &p.Breadcrumbs[0]
}

func ensureSDK(p *validation.ErrorPayload) *validation.SDKInfo {
	if p.SDK == nil {
		p.SDK = &validation.SDKInfo{}
	}
	return p.SDK
}

var stringRules = []stringRule{
	{"logger", validation.MaxLoggerLength, "logger", false,
		func(p *validation.ErrorPayload, s string) { p.Logger = s }, func(p *validation.ErrorPayload) string { return p.Logger }},
	{"server_name", validation.MaxServerNameLength, "server_name", false,
		func(p *validation.ErrorPayload, s string) { p.ServerName = s }, func(p *validation.ErrorPayload) string { return p.ServerName }},
	{"transaction", validation.MaxTransactionLength, "transaction", false,
		func(p *validation.ErrorPayload, s string) { p.Transaction = s }, func(p *validation.ErrorPayload) string { return p.Transaction }},
	{"dist", validation.MaxDistLength, "dist", false,
		func(p *validation.ErrorPayload, s string) { p.Dist = s }, func(p *validation.ErrorPayload) string { return p.Dist }},
	{"source", validation.MaxSourceLength, "source", false,
		func(p *validation.ErrorPayload, s string) { p.Source = s }, func(p *validation.ErrorPayload) string { return p.Source }},
	{"sdk.name", validation.MaxSDKNameLength, "sdk", false,
		func(p *validation.ErrorPayload, s string) { ensureSDK(p).Name = s }, func(p *validation.ErrorPayload) string { return p.SDK.Name }},
	{"sdk.version", validation.MaxSDKVersionLength, "sdk", false,
		func(p *validation.ErrorPayload, s string) { ensureSDK(p).Version = s }, func(p *validation.ErrorPayload) string { return p.SDK.Version }},
	{"user.username", validation.MaxUsernameLength, "user.username", false,
		func(p *validation.ErrorPayload, s string) { ensureUser(p).Username = s }, func(p *validation.ErrorPayload) string { return p.User.Username }},
	{"user.email", validation.MaxUserEmailLength, "user.email", true,
		func(p *validation.ErrorPayload, s string) { ensureUser(p).Email = s }, func(p *validation.ErrorPayload) string { return p.User.Email }},
	{"user.ip_address", validation.MaxUserIPAddressLength, "user.ip_address", true,
		func(p *validation.ErrorPayload, s string) { ensureUser(p).IPAddress = s }, func(p *validation.ErrorPayload) string { return p.User.IPAddress }},
	{"user.geo.city", validation.MaxGeoTextLength, "user.geo", false,
		func(p *validation.ErrorPayload, s string) { ensureGeo(p).City = s }, func(p *validation.ErrorPayload) string { return p.User.Geo.City }},
	{"user.geo.region", validation.MaxGeoTextLength, "user.geo", false,
		func(p *validation.ErrorPayload, s string) { ensureGeo(p).Region = s }, func(p *validation.ErrorPayload) string { return p.User.Geo.Region }},
	{"user.geo.subdivision", validation.MaxGeoTextLength, "user.geo", false,
		func(p *validation.ErrorPayload, s string) { ensureGeo(p).Subdivision = s }, func(p *validation.ErrorPayload) string { return p.User.Geo.Subdivision }},
	{"user.geo.country_code", validation.MaxGeoCountryCodeLength, "user.geo", true,
		func(p *validation.ErrorPayload, s string) { ensureGeo(p).CountryCode = s }, func(p *validation.ErrorPayload) string { return p.User.Geo.CountryCode }},
	{"request.method", validation.MaxRequestMethodLength, "request.method", false,
		func(p *validation.ErrorPayload, s string) { ensureRequest(p).Method = s }, func(p *validation.ErrorPayload) string { return p.Request.Method }},
	{"request.url", validation.MaxRequestURLLength, "request.url", false,
		func(p *validation.ErrorPayload, s string) { ensureRequest(p).URL = s }, func(p *validation.ErrorPayload) string { return p.Request.URL }},
	{"request.query_string", validation.MaxRequestQueryStringLength, "request.query_string", false,
		func(p *validation.ErrorPayload, s string) { ensureRequest(p).QueryString = s }, func(p *validation.ErrorPayload) string { return p.Request.QueryString }},
	{"request.headers value", validation.MaxRequestHeaderValueLength, "request.headers", false,
		func(p *validation.ErrorPayload, s string) { ensureRequest(p).Headers = map[string]string{"X-Long": s} },
		func(p *validation.ErrorPayload) string { return p.Request.Headers["X-Long"] }},
	{"request.env value", validation.MaxRequestEnvValueLength, "request.env", false,
		func(p *validation.ErrorPayload, s string) { ensureRequest(p).Env = map[string]string{"LONG": s} },
		func(p *validation.ErrorPayload) string { return p.Request.Env["LONG"] }},
	{"breadcrumbs[].type", validation.MaxBreadcrumbTypeLength, "breadcrumbs.type", false,
		func(p *validation.ErrorPayload, s string) { ensureCrumb(p).Type = s }, func(p *validation.ErrorPayload) string { return p.Breadcrumbs[0].Type }},
	{"breadcrumbs[].category", validation.MaxBreadcrumbCategoryLength, "breadcrumbs.category", false,
		func(p *validation.ErrorPayload, s string) { ensureCrumb(p).Category = s }, func(p *validation.ErrorPayload) string { return p.Breadcrumbs[0].Category }},
	{"breadcrumbs[].message", validation.MaxBreadcrumbMessageLength, "breadcrumbs.message", false,
		func(p *validation.ErrorPayload, s string) { ensureCrumb(p).Message = s }, func(p *validation.ErrorPayload) string { return p.Breadcrumbs[0].Message }},
	{"exception.values[].type", validation.MaxExceptionTypeLength, "exception.type", false,
		func(p *validation.ErrorPayload, s string) { chainFrame(p); p.Exception.Values[0].Type = s },
		func(p *validation.ErrorPayload) string { return p.Exception.Values[0].Type }},
	{"exception.values[].value", validation.MaxExceptionValueLength, "exception.value", false,
		func(p *validation.ErrorPayload, s string) { chainFrame(p); p.Exception.Values[0].Value = s },
		func(p *validation.ErrorPayload) string { return p.Exception.Values[0].Value }},
	{"exception.values[].mechanism.type", validation.MaxMechanismTypeLength, "exception.mechanism", false,
		func(p *validation.ErrorPayload, s string) {
			chainFrame(p)
			p.Exception.Values[0].Mechanism = &validation.Mechanism{Type: s}
		},
		func(p *validation.ErrorPayload) string { return p.Exception.Values[0].Mechanism.Type }},
	{"chain frame .file", validation.MaxFilePathLength, "exception.stacktrace", false,
		func(p *validation.ErrorPayload, s string) { chainFrame(p).File = s },
		func(p *validation.ErrorPayload) string { return p.Exception.Values[0].Stacktrace[0].File }},
	{"chain frame .function", validation.MaxFunctionNameLength, "exception.stacktrace", false,
		func(p *validation.ErrorPayload, s string) { chainFrame(p).Function = s },
		func(p *validation.ErrorPayload) string { return p.Exception.Values[0].Stacktrace[0].Function }},
	{"top-level frame .module", validation.MaxFrameModuleLength, "stacktrace.module", false,
		func(p *validation.ErrorPayload, s string) { topFrame(p).Module = s }, func(p *validation.ErrorPayload) string { return p.Stacktrace[0].Module }},
	{"top-level frame .abs_path", validation.MaxFrameAbsPathLength, "stacktrace.abs_path", false,
		func(p *validation.ErrorPayload, s string) { topFrame(p).AbsPath = s }, func(p *validation.ErrorPayload) string { return p.Stacktrace[0].AbsPath }},
	{"top-level frame .context_line", validation.MaxFrameContextLineLength, "stacktrace.context", false,
		func(p *validation.ErrorPayload, s string) { topFrame(p).ContextLine = s }, func(p *validation.ErrorPayload) string { return p.Stacktrace[0].ContextLine }},
	{"top-level frame .pre_context line", validation.MaxFrameContextLineLength, "stacktrace.context", false,
		func(p *validation.ErrorPayload, s string) { topFrame(p).PreContext = []string{s} }, func(p *validation.ErrorPayload) string { return p.Stacktrace[0].PreContext[0] }},
	{"top-level frame .post_context line", validation.MaxFrameContextLineLength, "stacktrace.context", false,
		func(p *validation.ErrorPayload, s string) { topFrame(p).PostContext = []string{s} }, func(p *validation.ErrorPayload) string { return p.Stacktrace[0].PostContext[0] }},
	{"chain frame .module", validation.MaxFrameModuleLength, "stacktrace.module", false,
		func(p *validation.ErrorPayload, s string) { chainFrame(p).Module = s },
		func(p *validation.ErrorPayload) string { return p.Exception.Values[0].Stacktrace[0].Module }},
}

// TestIngestorBounds_StringRules is every single-string §4.2 row: at the bound the value is untouched
// and no marker is written; one code point over it is truncated to a clean prefix (or dropped, for an
// identifier) and the field's marker is written. The value is built from a 3-byte character so a
// byte-counting regression would cut far too early and fail the at-bound row.
func TestIngestorBounds_StringRules(t *testing.T) {
	for _, r := range stringRules {
		t.Run(r.field+"/at the bound", func(t *testing.T) {
			p := validPayload()
			v := strings.Repeat("界", r.bound)
			r.set(p, v)
			boundedPayload(t, p)
			assert.Equal(t, v, r.get(p))
			assert.Nil(t, serverTruncated(t, p))
		})
		t.Run(r.field+"/over the bound", func(t *testing.T) {
			p := validPayload()
			r.set(p, strings.Repeat("界", r.bound+1))
			boundedPayload(t, p)
			if r.drop {
				assert.Equal(t, "", r.get(p), "an identifier is dropped, never truncated")
			} else {
				assert.Equal(t, strings.Repeat("界", r.bound), r.get(p), "truncated on a code-point boundary, no ellipsis")
			}
			assert.Equal(t, []string{r.marker}, serverTruncated(t, p))
		})
	}
}

func TestIngestorBounds_Level(t *testing.T) {
	for _, lvl := range []string{"", "fatal", "error", "warning", "info", "debug"} {
		t.Run("kept/"+lvl, func(t *testing.T) {
			p := validPayload()
			p.Level = lvl
			boundedPayload(t, p)
			assert.Equal(t, lvl, p.Level)
			assert.Nil(t, serverTruncated(t, p))
		})
	}
	for _, lvl := range []string{"WARN", "Error", "critical", "warning "} {
		t.Run("replaced/"+lvl, func(t *testing.T) {
			p := validPayload()
			p.Level = lvl
			boundedPayload(t, p)
			assert.Equal(t, "", p.Level)
			assert.Equal(t, []string{"level"}, serverTruncated(t, p))
		})
	}
	t.Run("breadcrumbs[].level replaced", func(t *testing.T) {
		p := validPayload()
		p.Breadcrumbs = []validation.Breadcrumb{{Level: "info"}, {Level: "WARN"}}
		boundedPayload(t, p)
		assert.Equal(t, "info", p.Breadcrumbs[0].Level)
		assert.Equal(t, "", p.Breadcrumbs[1].Level)
		assert.Equal(t, []string{"breadcrumbs.level"}, serverTruncated(t, p))
	})
}

func TestIngestorBounds_UserID(t *testing.T) {
	t.Run("exactly 255 bytes is kept", func(t *testing.T) {
		p := validPayload()
		p.User = &validation.UserInfo{ID: strings.Repeat("u", 255)}
		boundedPayload(t, p)
		assert.Equal(t, strings.Repeat("u", 255), p.User.ID)
		assert.Nil(t, serverTruncated(t, p))
	})
	t.Run("256 bytes is dropped", func(t *testing.T) {
		p := validPayload()
		p.User = &validation.UserInfo{ID: strings.Repeat("u", 256), Username: "kept"}
		boundedPayload(t, p)
		assert.Equal(t, "", p.User.ID)
		assert.Equal(t, "kept", p.User.Username)
		assert.Equal(t, []string{"user.id"}, serverTruncated(t, p))
	})
	t.Run("a 100-character CJK id (300 bytes) is dropped: the bound is bytes", func(t *testing.T) {
		p := validPayload()
		id := strings.Repeat("界", 100)
		require.Equal(t, 300, len(id))
		p.User = &validation.UserInfo{ID: id}
		boundedPayload(t, p)
		assert.Equal(t, "", p.User.ID)
		assert.Equal(t, []string{"user.id"}, serverTruncated(t, p))
	})
}

func TestIngestorBounds_Tags(t *testing.T) {
	t.Run("valid tags are kept", func(t *testing.T) {
		p := validPayload()
		p.Tags = map[string]string{"tenant_id": "acme", "feature.flag": "on", "a:b-c_d": strings.Repeat("v", validation.MaxTagValueLength)}
		want := map[string]string{}
		for k, v := range p.Tags {
			want[k] = v
		}
		boundedPayload(t, p)
		assert.Equal(t, want, p.Tags)
		assert.Nil(t, serverTruncated(t, p))
	})
	t.Run("every invalid entry is dropped and named", func(t *testing.T) {
		p := validPayload()
		p.Tags = map[string]string{
			"ok":                    "v",
			strings.Repeat("k", 33): "key too long",
			"bad key":               "space in key",
			"empty":                 "",
			"long":                  strings.Repeat("v", validation.MaxTagValueLength+1),
			"newline":               "a\nb",
		}
		boundedPayload(t, p)
		assert.Equal(t, map[string]string{"ok": "v"}, p.Tags)
		assert.Equal(t, []string{"tags"}, serverTruncated(t, p))
		assert.Equal(t, []string{"tags.bad key", "tags.empty", "tags." + strings.Repeat("k", 33), "tags.long", "tags.newline"}, serverTruncatedKeys(t, p))
	})
	t.Run("over 50 keeps the 50 lexically smallest keys", func(t *testing.T) {
		p := validPayload()
		p.Tags = map[string]string{}
		for i := 0; i < 52; i++ {
			p.Tags[fmt.Sprintf("t%02d", i)] = "v"
		}
		boundedPayload(t, p)
		require.Len(t, p.Tags, validation.MaxTags)
		assert.Contains(t, p.Tags, "t49")
		assert.NotContains(t, p.Tags, "t50")
		assert.Equal(t, []string{"tags.t50", "tags.t51"}, serverTruncatedKeys(t, p))
		assert.Equal(t, []string{"tags"}, serverTruncated(t, p))
	})
}

func TestIngestorBounds_Contexts(t *testing.T) {
	t.Run("an invalid name is dropped", func(t *testing.T) {
		p := validPayload()
		p.Contexts = map[string]map[string]interface{}{
			"":                      {"a": "b"},
			strings.Repeat("c", 65): {"a": "b"},
			strings.Repeat("d", 64): {"a": "b"},
			"device":                {"model": "Pixel 8"},
		}
		boundedPayload(t, p)
		assert.Len(t, p.Contexts, 2)
		assert.Contains(t, p.Contexts, strings.Repeat("d", 64))
		assert.Contains(t, p.Contexts, "device")
		assert.Equal(t, []string{"contexts"}, serverTruncated(t, p))
		assert.Equal(t, []string{"contexts.", "contexts." + strings.Repeat("c", 65)}, serverTruncatedKeys(t, p))
	})
	t.Run("over 32 removes a non-well-known context first, the largest", func(t *testing.T) {
		p := validPayload()
		p.Contexts = map[string]map[string]interface{}{}
		for _, k := range []string{"device", "os", "app", "runtime", "browser", "culture"} {
			p.Contexts[k] = map[string]interface{}{"big": strings.Repeat("x", 500)}
		}
		for i := 0; i < 27; i++ {
			p.Contexts[fmt.Sprintf("c%02d", i)] = map[string]interface{}{"v": "small"}
		}
		p.Contexts["c05"] = map[string]interface{}{"v": strings.Repeat("y", 100)}
		require.Len(t, p.Contexts, 33)
		boundedPayload(t, p)
		assert.Len(t, p.Contexts, validation.MaxContexts)
		assert.NotContains(t, p.Contexts, "c05", "the largest non-well-known context goes first, although six well-known ones are larger")
		assert.Equal(t, []string{"contexts.c05"}, serverTruncatedKeys(t, p))
		assert.Equal(t, []string{"contexts"}, serverTruncated(t, p))
	})
	t.Run("ties go to the lexically smallest name", func(t *testing.T) {
		p := validPayload()
		p.Contexts = map[string]map[string]interface{}{}
		for i := 0; i < 33; i++ {
			p.Contexts[fmt.Sprintf("c%02d", i)] = map[string]interface{}{"v": "same"}
		}
		boundedPayload(t, p)
		assert.NotContains(t, p.Contexts, "c00")
		assert.Equal(t, []string{"contexts.c00"}, serverTruncatedKeys(t, p))
	})
	t.Run("over 64 KiB removes contexts until it fits, well-known last", func(t *testing.T) {
		p := validPayload()
		p.Contexts = map[string]map[string]interface{}{
			"device": {"blob": strings.Repeat("d", 40000)},
			"custom": {"blob": strings.Repeat("c", 30000)},
			"os":     {"name": "Android"},
		}
		boundedPayload(t, p)
		assert.Contains(t, p.Contexts, "device", "a well-known context is removed only when nothing else is left")
		assert.NotContains(t, p.Contexts, "custom")
		assert.LessOrEqual(t, marshalledSize(t, p.Contexts), validation.MaxContextsSize)
		assert.Equal(t, []string{"contexts.custom"}, serverTruncatedKeys(t, p))
	})
	t.Run("only well-known contexts over 64 KiB: the largest well-known one goes", func(t *testing.T) {
		p := validPayload()
		p.Contexts = map[string]map[string]interface{}{
			"device": {"blob": strings.Repeat("d", 70000)},
			"os":     {"name": "Android"},
		}
		boundedPayload(t, p)
		assert.Equal(t, map[string]map[string]interface{}{"os": {"name": "Android"}}, p.Contexts)
		assert.Equal(t, []string{"contexts.device"}, serverTruncatedKeys(t, p))
	})
}

func TestIngestorBounds_RequestMaps(t *testing.T) {
	t.Run("headers: invalid keys dropped", func(t *testing.T) {
		p := validPayload()
		p.Request = &validation.RequestInfo{Headers: map[string]string{
			"":                       "empty key",
			strings.Repeat("h", 257): "long key",
			strings.Repeat("k", 256): "at bound",
			"Accept":                 "*/*",
		}}
		boundedPayload(t, p)
		assert.Equal(t, map[string]string{strings.Repeat("k", 256): "at bound", "Accept": "*/*"}, p.Request.Headers)
		assert.Equal(t, []string{"request.headers"}, serverTruncated(t, p))
	})
	t.Run("headers: past 64 the lexically largest are dropped", func(t *testing.T) {
		p := validPayload()
		p.Request = &validation.RequestInfo{Headers: map[string]string{}}
		for i := 0; i < 66; i++ {
			p.Request.Headers[fmt.Sprintf("h%02d", i)] = "v"
		}
		boundedPayload(t, p)
		assert.Len(t, p.Request.Headers, validation.MaxRequestHeaders)
		assert.Contains(t, p.Request.Headers, "h63")
		assert.NotContains(t, p.Request.Headers, "h64")
		assert.NotContains(t, p.Request.Headers, "h65")
		assert.Equal(t, []string{"request.headers"}, serverTruncated(t, p))
	})
	t.Run("env: invalid keys dropped, past 64 the lexically largest are dropped", func(t *testing.T) {
		p := validPayload()
		p.Request = &validation.RequestInfo{Env: map[string]string{"": "x"}}
		for i := 0; i < 65; i++ {
			p.Request.Env[fmt.Sprintf("E%02d", i)] = "v"
		}
		boundedPayload(t, p)
		assert.Len(t, p.Request.Env, validation.MaxRequestEnv)
		assert.NotContains(t, p.Request.Env, "E64")
		assert.NotContains(t, p.Request.Env, "")
		assert.Equal(t, []string{"request.env"}, serverTruncated(t, p))
	})
	t.Run("data: at most 16 KiB is kept, over it is dropped", func(t *testing.T) {
		p := validPayload()
		small := map[string]interface{}{"amount": 4200.0, "items": []interface{}{"a", "b"}}
		p.Request = &validation.RequestInfo{Data: small}
		boundedPayload(t, p)
		assert.Equal(t, small, p.Request.Data)
		assert.Nil(t, serverTruncated(t, p))

		p = validPayload()
		p.Request = &validation.RequestInfo{Method: "POST", Data: map[string]interface{}{"blob": strings.Repeat("x", validation.MaxRequestDataSize)}}
		boundedPayload(t, p)
		assert.Nil(t, p.Request.Data)
		assert.Equal(t, "POST", p.Request.Method)
		assert.Equal(t, []string{"request.data"}, serverTruncated(t, p))
	})
}

func TestIngestorBounds_Breadcrumbs(t *testing.T) {
	t.Run("data over 4 KiB is dropped from that crumb only", func(t *testing.T) {
		p := validPayload()
		p.Breadcrumbs = []validation.Breadcrumb{
			{Message: "small", Data: map[string]interface{}{"k": "v"}},
			{Message: "big", Data: map[string]interface{}{"blob": strings.Repeat("x", validation.MaxBreadcrumbDataSize)}},
		}
		boundedPayload(t, p)
		assert.Equal(t, map[string]interface{}{"k": "v"}, p.Breadcrumbs[0].Data)
		assert.Nil(t, p.Breadcrumbs[1].Data)
		assert.Equal(t, "big", p.Breadcrumbs[1].Message)
		assert.Equal(t, []string{"breadcrumbs.data"}, serverTruncated(t, p))
	})
	t.Run("over the 128 KiB total the oldest are dropped", func(t *testing.T) {
		p := validPayload()
		for i := 0; i < 40; i++ {
			p.Breadcrumbs = append(p.Breadcrumbs, validation.Breadcrumb{Message: fmt.Sprintf("%02d", i) + strings.Repeat("m", 4000)})
		}
		boundedPayload(t, p)
		assert.LessOrEqual(t, marshalledSize(t, p.Breadcrumbs), validation.MaxBreadcrumbsSize)
		assert.Less(t, len(p.Breadcrumbs), 40)
		assert.True(t, strings.HasPrefix(p.Breadcrumbs[len(p.Breadcrumbs)-1].Message, "39"), "the newest crumb is kept")
		assert.False(t, strings.HasPrefix(p.Breadcrumbs[0].Message, "00"), "the oldest crumb goes first")
		assert.Equal(t, []string{"breadcrumbs"}, serverTruncated(t, p))
	})
}

func TestIngestorBounds_Exception(t *testing.T) {
	t.Run("over 20 values keeps the last 20", func(t *testing.T) {
		p := validPayload()
		p.Exception = &validation.ExceptionInterface{}
		for i := 0; i < 22; i++ {
			p.Exception.Values = append(p.Exception.Values, validation.ExceptionValue{Type: fmt.Sprintf("E%02d", i)})
		}
		boundedPayload(t, p)
		require.Len(t, p.Exception.Values, validation.MaxExceptionValues)
		assert.Equal(t, "E02", p.Exception.Values[0].Type)
		assert.Equal(t, "E21", p.Exception.Values[19].Type, "the raised exception (last) is kept")
		assert.Equal(t, []string{"exception"}, serverTruncated(t, p))
	})
	t.Run("a chain stacktrace over 100 frames keeps the first 100", func(t *testing.T) {
		p := validPayload()
		frames := make([]validation.StackFrame, 101)
		for i := range frames {
			frames[i] = validation.StackFrame{File: "a.py", Line: int32(i + 1), Function: "f"}
		}
		p.Exception = &validation.ExceptionInterface{Values: []validation.ExceptionValue{{Type: "E", Stacktrace: frames}}}
		boundedPayload(t, p)
		st := p.Exception.Values[0].Stacktrace
		require.Len(t, st, validation.MaxExceptionFrames)
		assert.Equal(t, int32(1), st[0].Line, "the crashing end is kept")
		assert.Equal(t, int32(100), st[99].Line)
		assert.Equal(t, []string{"exception.stacktrace"}, serverTruncated(t, p))
	})
}

func TestIngestorBounds_FrameContext(t *testing.T) {
	lines := func(prefix string, n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("%s%d", prefix, i)
		}
		return out
	}
	p := validPayload()
	p.Stacktrace = []validation.StackFrame{{File: "main.go", Line: 10, Function: "main", PreContext: lines("pre", 7), PostContext: lines("post", 7)}}
	boundedPayload(t, p)
	assert.Equal(t, []string{"pre2", "pre3", "pre4", "pre5", "pre6"}, p.Stacktrace[0].PreContext, "the last 5 of pre (nearest the line)")
	assert.Equal(t, []string{"post0", "post1", "post2", "post3", "post4"}, p.Stacktrace[0].PostContext, "the first 5 of post (nearest the line)")
	assert.Equal(t, []string{"stacktrace.context"}, serverTruncated(t, p))
}

// Top-level frames: file and function are fingerprint inputs and are never cut by the server (S4); a
// negative column keeps its 400 (it is not a v2 field).
func TestIngestorBounds_TopLevelFrames(t *testing.T) {
	t.Run("file and function over the SDK-side bounds are not touched", func(t *testing.T) {
		p := validPayload()
		file, fn := strings.Repeat("f", validation.MaxFilePathLength+100), strings.Repeat("g", validation.MaxFunctionNameLength+100)
		p.Stacktrace = []validation.StackFrame{{File: file, Line: 1, Function: fn, InApp: true}}
		boundedPayload(t, p)
		assert.Equal(t, file, p.Stacktrace[0].File)
		assert.Equal(t, fn, p.Stacktrace[0].Function)
		assert.Nil(t, serverTruncated(t, p))
	})
	t.Run("a negative column is the one rule the step rejects on", func(t *testing.T) {
		p := validPayload()
		p.Stacktrace = []validation.StackFrame{{File: "a.js", Line: 1, Column: -1, Function: "f"}}
		res := validation.ValidatePayload(p)
		require.False(t, res.Valid)
		require.Len(t, res.Errors, 1)
		assert.Equal(t, "stacktrace[0].column", res.Errors[0].Field)

		svc, pub := newTestIngestService(t)
		p = validPayload()
		p.Stacktrace = []validation.StackFrame{{File: "a.js", Line: 1, Column: -1, Function: "f"}}
		err := svc.Ingest(context.Background(), p)
		require.EqualError(t, err, "validation failed: stacktrace[0].column must be >= 0")
		assert.Empty(t, pub.published)
	})
	t.Run("101 top-level frames are a 400 from protovalidate", func(t *testing.T) {
		p := validPayload()
		for i := 0; i < 101; i++ {
			p.Stacktrace = append(p.Stacktrace, validation.StackFrame{File: "a.go", Line: int32(i + 1), Function: "f"})
		}
		require.Equal(t, []ingestViolation{{"stacktrace", "repeated.max_items"}}, ingestViolations(t, p))
	})
}

// Every existing reject row is unchanged: the same inputs still 400 through IngestAt, by protovalidate.
func TestIngestorBounds_ExistingRejectRowsUnchanged(t *testing.T) {
	rows := []struct {
		name   string
		mutate func(p *validation.ErrorPayload)
		want   ingestViolation
	}{
		{"project_key 65", func(p *validation.ErrorPayload) { p.ProjectKey = strings.Repeat("a", 65) }, ingestViolation{"", "error_event.project_key"}},
		{"platform uppercase", func(p *validation.ErrorPayload) { p.Platform = "Go" }, ingestViolation{"", "error_event.platform"}},
		{"environment leading dash", func(p *validation.ErrorPayload) { p.Environment = "-prod" }, ingestViolation{"", "error_event.environment"}},
		{"message 10001 code points", func(p *validation.ErrorPayload) { p.Message = strings.Repeat("界", 10001) }, ingestViolation{"", "error_event.message"}},
		{"error_class 256", func(p *validation.ErrorPayload) { p.ErrorClass = strings.Repeat("E", 256) }, ingestViolation{"", "error_event.error_class"}},
		{"trace_id 65", func(p *validation.ErrorPayload) { p.TraceID = strings.Repeat("a", 65) }, ingestViolation{"", "error_event.trace_id"}},
		{"span_id 65", func(p *validation.ErrorPayload) { p.SpanID = strings.Repeat("a", 65) }, ingestViolation{"", "error_event.span_id"}},
		{"release_version 101", func(p *validation.ErrorPayload) { p.ReleaseVersion = strings.Repeat("1", 101) }, ingestViolation{"", "error_event.release_version"}},
		{"fingerprint 65", func(p *validation.ErrorPayload) { p.Fingerprint = strings.Repeat("f", 65) }, ingestViolation{"", "error_event.fingerprint"}},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			p := validPayload()
			r.mutate(p)
			require.Equal(t, []ingestViolation{r.want}, ingestViolations(t, p))
		})
	}
	t.Run("event_id over 64 is replaced, not rejected", func(t *testing.T) {
		svc, pub := newTestIngestService(t)
		p := validPayload()
		p.EventID = strings.Repeat("e", 65)
		require.NoError(t, svc.Ingest(context.Background(), p))
		assert.NotEqual(t, strings.Repeat("e", 65), publishedEvent(t, pub).GetEventId())
	})
	t.Run("a timestamp out of the window is replaced, not rejected", func(t *testing.T) {
		svc, pub := newTestIngestService(t)
		p := validPayload()
		p.Timestamp = tsRecv.Add(time.Hour)
		require.NoError(t, svc.IngestAt(context.Background(), p, tsRecv))
		assert.True(t, publishedEvent(t, pub).GetTimestamp().AsTime().Equal(tsRecv))
	})
}

func TestIngestorBounds_Metadata(t *testing.T) {
	t.Run("the largest non-exempt key goes first; exempt keys and the markers survive", func(t *testing.T) {
		p := validPayload()
		p.Metadata = map[string]interface{}{
			"user_id": "u-1",
			"blob":    strings.Repeat("b", 70000),
			"small":   "s",
		}
		boundedPayload(t, p)
		assert.Equal(t, "u-1", p.Metadata["user_id"])
		assert.Equal(t, "s", p.Metadata["small"])
		assert.NotContains(t, p.Metadata, "blob")
		assert.Equal(t, []string{"metadata"}, serverTruncated(t, p))
		assert.Equal(t, []string{"blob"}, serverTruncatedKeys(t, p))
		assert.LessOrEqual(t, marshalledSize(t, p.Metadata), validation.MaxMetadataSize)
	})
	t.Run("an exempt key goes only when nothing else is left", func(t *testing.T) {
		p := validPayload()
		p.Metadata = map[string]interface{}{
			"user":  strings.Repeat("u", 70000),
			"small": "s",
		}
		boundedPayload(t, p)
		assert.NotContains(t, p.Metadata, "user")
		assert.NotContains(t, p.Metadata, "small", "every non-exempt key is removed before an exempt one")
		assert.Equal(t, []string{"small", "user"}, serverTruncatedKeys(t, p))
	})
	t.Run("ties go to the lexically smallest key", func(t *testing.T) {
		p := validPayload()
		p.Metadata = map[string]interface{}{
			"b": strings.Repeat("x", 40000),
			"a": strings.Repeat("x", 40000),
		}
		boundedPayload(t, p)
		assert.NotContains(t, p.Metadata, "a")
		assert.Contains(t, p.Metadata, "b")
	})
	t.Run("at the bound nothing is removed", func(t *testing.T) {
		p := validPayload()
		p.Metadata = map[string]interface{}{"k": ""}
		p.Metadata["k"] = strings.Repeat("x", validation.MaxMetadataSize-marshalledSize(t, p.Metadata))
		require.Equal(t, validation.MaxMetadataSize, marshalledSize(t, p.Metadata))
		boundedPayload(t, p)
		assert.Contains(t, p.Metadata, "k")
		assert.Nil(t, serverTruncated(t, p))
	})
}

// Markers are deduplicated and in vocabulary order, whatever order the cuts happen in.
func TestIngestorBounds_MarkerOrderAndDedup(t *testing.T) {
	p := validPayload()
	p.Metadata = map[string]interface{}{"blob": strings.Repeat("b", 70000)}
	p.Tags = map[string]string{"bad key": "v"}
	p.Level = "LOUD"
	p.Breadcrumbs = []validation.Breadcrumb{{Level: "x"}, {Level: "y"}}
	boundedPayload(t, p)
	assert.Equal(t, []string{"level", "tags", "breadcrumbs.level", "metadata"}, serverTruncated(t, p))
	assert.Equal(t, []string{"tags.bad key", "blob"}, serverTruncatedKeys(t, p))
}

// assertNoNULInMarkers: the server markers are stored in error_occurrences.metadata JSONB, which rejects
// \u0000 with 22P05 (Permanent: the event is dead-lettered).
func assertNoNULInMarkers(t *testing.T, p *validation.ErrorPayload) {
	t.Helper()
	for _, k := range []string{validation.MetadataKeyServerTruncated, validation.MetadataKeyServerTruncatedKeys} {
		b, err := json.Marshal(p.Metadata[k])
		require.NoError(t, err)
		assert.NotContains(t, string(b), `\u0000`, "metadata.%s must never carry a NUL", k)
	}
}

func TestIngestorBounds_NULStrip(t *testing.T) {
	p := validPayload()
	p.Logger = "a\x00b"
	p.User = &validation.UserInfo{ID: "u\x001", Username: "x\x00"}
	p.Contexts = map[string]map[string]interface{}{"dev\x00ice": {"mo\x00del": "Pix\x00el", "nested": []interface{}{"a\x00"}}}
	p.Request = &validation.RequestInfo{Headers: map[string]string{"X-\x00H": "v\x00"}, Data: map[string]interface{}{"k\x00": "v\x00"}}
	p.Exception = &validation.ExceptionInterface{Values: []validation.ExceptionValue{{Type: "E\x00", Value: "v\x00"}}}
	boundedPayload(t, p)
	assert.Equal(t, "ab", p.Logger)
	assert.Equal(t, "u1", p.User.ID)
	assert.Equal(t, "x", p.User.Username)
	assert.Equal(t, map[string]map[string]interface{}{"device": {"model": "Pixel", "nested": []interface{}{"a"}}}, p.Contexts)
	assert.Equal(t, map[string]string{"X-H": "v"}, p.Request.Headers)
	assert.Equal(t, map[string]interface{}{"k": "v"}, p.Request.Data)
	assert.Equal(t, "E", p.Exception.Values[0].Type)
	assert.Equal(t, "v", p.Exception.Values[0].Value)
	assert.Equal(t, []string{"logger", "user.id", "user.username", "contexts", "request.headers", "request.data", "exception.type", "exception.value"}, serverTruncated(t, p))

	// Review finding esv2-rev-nul-tag-key-dlq: a NUL in a tag key or a context name reached
	// metadata.server_truncated_keys raw, Postgres rejects \u0000 in jsonb (22P05, Permanent), and the
	// event was dead-lettered. A key is stripped to its valid form, and no marker ever carries a NUL.
	t.Run("a NUL in a tag key is stripped, not dropped", func(t *testing.T) {
		p := validPayload()
		p.Tags = map[string]string{"env\x00x": "v"}
		boundedPayload(t, p)
		assert.Equal(t, map[string]string{"envx": "v"}, p.Tags)
		assert.Equal(t, []string{"tags"}, serverTruncated(t, p))
		assert.Nil(t, serverTruncatedKeys(t, p), "nothing was removed")
		assertNoNULInMarkers(t, p)
	})
	t.Run("a stripped tag key colliding with a sent one: the sent one wins, the marker is NUL-free", func(t *testing.T) {
		p := validPayload()
		p.Tags = map[string]string{"env\x00x": "stripped", "envx": "sent"}
		boundedPayload(t, p)
		assert.Equal(t, map[string]string{"envx": "sent"}, p.Tags)
		assert.Equal(t, []string{"tags"}, serverTruncated(t, p))
		assert.Nil(t, serverTruncatedKeys(t, p), "tags.envx is stored, so it is not recorded as removed")
		assertNoNULInMarkers(t, p)
	})
	t.Run("a tag key still invalid after the strip is dropped under its stripped name", func(t *testing.T) {
		p := validPayload()
		p.Tags = map[string]string{"bad\x00 key": "v"}
		boundedPayload(t, p)
		assert.Empty(t, p.Tags)
		assert.Equal(t, []string{"tags.bad key"}, serverTruncatedKeys(t, p))
		assertNoNULInMarkers(t, p)
	})
	t.Run("a stripped context name colliding with a sent one is recorded NUL-free", func(t *testing.T) {
		p := validPayload()
		p.Contexts = map[string]map[string]interface{}{"dev\x00ice": {"model": "a"}, "device": {"model": "b"}}
		boundedPayload(t, p)
		assert.Equal(t, map[string]map[string]interface{}{"device": {"model": "b"}}, p.Contexts)
		assert.Equal(t, []string{"contexts"}, serverTruncated(t, p))
		assert.Nil(t, serverTruncatedKeys(t, p), "contexts.device is stored, so it is not recorded as removed")
		assertNoNULInMarkers(t, p)
	})

	// Review finding esv2-rev2-nul-collision-markers: the three NUL-collision paths disagreed. All three
	// now keep the key the client sent, and a name is recorded in server_truncated_keys only when no
	// entry of that name was stored, and then once.
	t.Run("tags: the sent key wins whichever side of it the NUL sorts, and is not recorded", func(t *testing.T) {
		for _, nulKey := range []string{"a\x00", "\x00a"} {
			p := validPayload()
			p.Tags = map[string]string{"a": "sent", nulKey: "nul"}
			boundedPayload(t, p)
			assert.Equal(t, map[string]string{"a": "sent"}, p.Tags, "%q", nulKey)
			assert.Equal(t, []string{"tags"}, serverTruncated(t, p), "%q", nulKey)
			assert.Nil(t, serverTruncatedKeys(t, p), "%q: tags.a is stored", nulKey)
		}
	})
	t.Run("tags: a name lost by both the sent and the stripped key is recorded once", func(t *testing.T) {
		p := validPayload()
		p.Tags = map[string]string{"a": "", "a\x00": "nul"}
		boundedPayload(t, p)
		assert.Empty(t, p.Tags)
		assert.Equal(t, []string{"tags.a"}, serverTruncatedKeys(t, p))
	})
	t.Run("contexts: an empty name and its NUL twin are recorded once", func(t *testing.T) {
		p := validPayload()
		p.Contexts = map[string]map[string]interface{}{"": {"k": "sent"}, "\x00": {"k": "nul"}}
		boundedPayload(t, p)
		assert.Empty(t, p.Contexts)
		assert.Equal(t, []string{"contexts"}, serverTruncated(t, p))
		assert.Equal(t, []string{"contexts."}, serverTruncatedKeys(t, p))
		assertNoNULInMarkers(t, p)
	})
	t.Run("request headers and env: the key the client sent wins", func(t *testing.T) {
		p := validPayload()
		p.Request = &validation.RequestInfo{
			Headers: map[string]string{"Cookie": "sent", "\x00Cookie": "nul-one"},
			Env:     map[string]string{"HOME": "sent", "HOME\x00": "nul-one"},
		}
		boundedPayload(t, p)
		assert.Equal(t, map[string]string{"Cookie": "sent"}, p.Request.Headers)
		assert.Equal(t, map[string]string{"HOME": "sent"}, p.Request.Env)
		assert.Equal(t, []string{"request.headers", "request.env"}, serverTruncated(t, p))
	})
	t.Run("request headers: between two stripped keys the lexically first original wins", func(t *testing.T) {
		p := validPayload()
		p.Request = &validation.RequestInfo{Headers: map[string]string{"\x00Cookie": "first", "Coo\x00kie": "second"}}
		boundedPayload(t, p)
		assert.Equal(t, map[string]string{"Cookie": "first"}, p.Request.Headers)
	})
	t.Run("a metadata key removed by the shrink is recorded NUL-free", func(t *testing.T) {
		p := validPayload()
		p.Metadata = map[string]interface{}{"bl\x00ob": strings.Repeat("b", 70000)}
		res := validation.ValidatePayload(p)
		require.True(t, res.Valid)
		assert.Equal(t, []string{"blob"}, serverTruncatedKeys(t, p))
		assertNoNULInMarkers(t, p)
	})
	t.Run("the strip runs before the length rule", func(t *testing.T) {
		p := validPayload()
		p.Logger = strings.Repeat("a", validation.MaxLoggerLength) + "\x00"
		boundedPayload(t, p)
		assert.Equal(t, strings.Repeat("a", validation.MaxLoggerLength), p.Logger, "measured on what is stored")
		assert.Equal(t, []string{"logger"}, serverTruncated(t, p))
	})
	t.Run("metadata and the top-level message are not v2 fields and are left as sent", func(t *testing.T) {
		p := validPayload()
		p.Message = "m\x00"
		p.Metadata = map[string]interface{}{"k": "v\x00"}
		res := validation.ValidatePayload(p)
		require.True(t, res.Valid)
		assert.Equal(t, "m\x00", p.Message)
		assert.Equal(t, "v\x00", p.Metadata["k"])
	})
}

// ---- §4.4, case by case.

func installFieldsTruncatedReader(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	return installTimestampManualReader(t)
}

func collectFieldsTruncated(t *testing.T, reader *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != obs.MetricIngestFieldsTruncated {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			require.True(t, ok)
			for _, dp := range sum.DataPoints {
				v, ok := dp.Attributes.Value(obs.LabelField)
				require.True(t, ok)
				require.Equal(t, 1, dp.Attributes.Len(), "the field label is the only label")
				out[v.AsString()] = dp.Value
			}
		}
	}
	return out
}

func TestIngestorBounds_Cases(t *testing.T) {
	t.Run("I1 a valid pre-v2 payload is published unchanged except received_at", func(t *testing.T) {
		svc, pub := newTestIngestService(t)
		p := validPayload()
		p.Timestamp = tsRecv
		p.EventID = "evt-i1"
		p.Metadata = map[string]interface{}{"user_id": "u", "n": 1.0}
		p.Stacktrace = []validation.StackFrame{{File: "main.go", Line: 3, Function: "main", InApp: true}}
		want := mapping.MapPayloadToEvent(p)
		require.NoError(t, svc.IngestAt(context.Background(), p, tsRecv))

		got := publishedEvent(t, pub)
		require.True(t, got.GetReceivedAt().AsTime().Equal(tsRecv), "field 30 carries T_recv")
		got.ReceivedAt = nil
		assert.True(t, proto.Equal(want, got), "no v2 key, metadata under the bound: nothing but received_at differs\nwant %v\ngot  %v", want, got)
		assert.NotContains(t, got.GetMetadata().AsMap(), validation.MetadataKeyServerTruncated)
	})

	t.Run("I2 100 KB metadata is shrunk to 64 KiB and counted once", func(t *testing.T) {
		reader := installFieldsTruncatedReader(t)
		svc, pub := newTestIngestService(t)
		p := validPayload()
		p.Metadata = map[string]interface{}{"user_id": "u-1", "tenant_id": "t-1"}
		for i := 0; i < 10; i++ {
			p.Metadata[fmt.Sprintf("blob%d", i)] = strings.Repeat("x", 10000)
		}
		require.Greater(t, marshalledSize(t, p.Metadata), 100000)
		require.NoError(t, svc.IngestAt(context.Background(), p, tsRecv))

		md := publishedEvent(t, pub).GetMetadata().AsMap()
		assert.LessOrEqual(t, marshalledSize(t, md), validation.MaxMetadataSize)
		assert.Equal(t, []interface{}{"metadata"}, md[validation.MetadataKeyServerTruncated])
		assert.Equal(t, "u-1", md["user_id"])
		assert.Equal(t, "t-1", md["tenant_id"])
		removed := stringList(t, md[validation.MetadataKeyServerTruncatedKeys])
		assert.NotEmpty(t, removed)
		for _, k := range removed {
			assert.True(t, strings.HasPrefix(k, "blob"), "only non-exempt keys were needed: %s", k)
			assert.NotContains(t, md, k)
		}
		assert.Equal(t, map[string]int64{"metadata": 1}, collectFieldsTruncated(t, reader))
	})

	t.Run("I3 every v2 field within bounds reaches the published proto, no marker", func(t *testing.T) {
		reader := installFieldsTruncatedReader(t)
		svc, pub := newTestIngestService(t)
		p := loadEventV2Full(t)
		p.ProjectKey = "proj-bounds"
		require.NoError(t, svc.IngestAt(context.Background(), p, tsRecv))
		ev := publishedEvent(t, pub)
		assert.Equal(t, "warning", ev.GetLevel())
		assert.Equal(t, "onerror", ev.GetSource())
		assert.NotEmpty(t, ev.GetBreadcrumbs())
		assert.NotEmpty(t, ev.GetException().GetValues())
		assert.NotEmpty(t, ev.GetTags())
		assert.NotEmpty(t, ev.GetContexts())
		assert.NotNil(t, ev.GetRequest())
		assert.NotContains(t, ev.GetMetadata().AsMap(), validation.MetadataKeyServerTruncated)
		assert.Empty(t, collectFieldsTruncated(t, reader))
	})

	t.Run("I4 150 breadcrumbs keep the last 100", func(t *testing.T) {
		p := validPayload()
		for i := 0; i < 150; i++ {
			p.Breadcrumbs = append(p.Breadcrumbs, validation.Breadcrumb{Message: fmt.Sprintf("crumb-%03d", i)})
		}
		boundedPayload(t, p)
		require.Len(t, p.Breadcrumbs, validation.MaxBreadcrumbs)
		assert.Equal(t, "crumb-050", p.Breadcrumbs[0].Message)
		assert.Equal(t, "crumb-149", p.Breadcrumbs[99].Message)
		assert.Contains(t, serverTruncated(t, p), "breadcrumbs")
	})

	t.Run("I5 60 tags, 3 with a 40-char key: 3 dropped, then the 50 smallest of 57", func(t *testing.T) {
		p := validPayload()
		p.Tags = map[string]string{}
		for i := 0; i < 57; i++ {
			p.Tags[fmt.Sprintf("tag%02d", i)] = "v"
		}
		for i := 0; i < 3; i++ {
			p.Tags[strings.Repeat(string(rune('a'+i)), 40)] = "v"
		}
		require.Len(t, p.Tags, 60)
		boundedPayload(t, p)
		assert.Len(t, p.Tags, 50)
		assert.Contains(t, p.Tags, "tag49")
		assert.NotContains(t, p.Tags, "tag50")
		assert.Equal(t, []string{"tags"}, serverTruncated(t, p))
		assert.Len(t, serverTruncatedKeys(t, p), 10)
	})

	t.Run("I6 level WARN is replaced with empty (stored as error)", func(t *testing.T) {
		svc, pub := newTestIngestService(t)
		p := validPayload()
		p.Level = "WARN"
		require.NoError(t, svc.Ingest(context.Background(), p))
		ev := publishedEvent(t, pub)
		assert.Equal(t, "", ev.GetLevel())
		assert.Equal(t, []interface{}{"level"}, ev.GetMetadata().AsMap()[validation.MetadataKeyServerTruncated])
	})

	t.Run("I7 user.id of 256 bytes or a 100-char CJK id is absent; 255 bytes is kept", func(t *testing.T) {
		for _, id := range []string{strings.Repeat("u", 256), strings.Repeat("界", 100)} {
			svc, pub := newTestIngestService(t)
			p := validPayload()
			p.User = &validation.UserInfo{ID: id}
			p.Metadata = map[string]interface{}{"user_id": "meta-user"}
			require.NoError(t, svc.Ingest(context.Background(), p))
			ev := publishedEvent(t, pub)
			assert.Equal(t, "", ev.GetUser().GetId())
			md := ev.GetMetadata().AsMap()
			assert.Equal(t, []interface{}{"user.id"}, md[validation.MetadataKeyServerTruncated])
			assert.Equal(t, "meta-user", md["user_id"], "the metadata fallback the processor reads is untouched")
		}
		svc, pub := newTestIngestService(t)
		p := validPayload()
		p.User = &validation.UserInfo{ID: strings.Repeat("u", 255)}
		require.NoError(t, svc.Ingest(context.Background(), p))
		assert.Equal(t, strings.Repeat("u", 255), publishedEvent(t, pub).GetUser().GetId())
	})

	t.Run("I7b chain frames are repaired, never rejected", func(t *testing.T) {
		svc, pub := newTestIngestService(t)
		p := validPayload()
		p.Exception = &validation.ExceptionInterface{Values: []validation.ExceptionValue{{
			Type: "AuthError",
			Stacktrace: []validation.StackFrame{
				{Module: "app.auth", AbsPath: "/srv/app/auth.py", InApp: true},
				{File: "b.py", Function: "g", Line: 2, Column: -3},
			},
		}}}
		require.NoError(t, svc.Ingest(context.Background(), p))
		st := publishedEvent(t, pub).GetException().GetValues()[0].GetStacktrace()
		require.Len(t, st, 2)
		assert.False(t, st[0].GetInApp())
		assert.Equal(t, "app.auth", st[0].GetModule())
		assert.Equal(t, "/srv/app/auth.py", st[0].GetAbsPath())
		assert.Equal(t, int32(0), st[1].GetColumn())
		assert.Equal(t, []string{"exception.stacktrace"}, serverTruncated(t, p))
	})

	t.Run("I7c NUL in a breadcrumb message and a tag value is stripped", func(t *testing.T) {
		svc, pub := newTestIngestService(t)
		p := decodeIngestPayload(t, `{"project_key":"proj","platform":"go","environment":"production","error_class":"E",
			"breadcrumbs":[{"message":"a\u0000b"}],"tags":{"k":"v\u0000"}}`)
		require.NoError(t, svc.Ingest(context.Background(), p))
		ev := publishedEvent(t, pub)
		assert.Equal(t, "ab", ev.GetBreadcrumbs()[0].GetMessage())
		assert.Equal(t, "v", ev.GetTags()["k"])
		assert.Equal(t, []interface{}{"tags", "breadcrumbs.message"}, ev.GetMetadata().AsMap()[validation.MetadataKeyServerTruncated])
	})

	t.Run("I8 a numeric tag value fails the whole decode", func(t *testing.T) {
		var p validation.ErrorPayload
		err := json.Unmarshal([]byte(`{"project_key":"p","platform":"go","environment":"production","error_class":"E","tags":{"a":1}}`), &p)
		require.Error(t, err, "a type mismatch is a 400 Invalid JSON payload, as for every field")
	})

	t.Run("I9 a JSON fingerprint reaches the proto verbatim", func(t *testing.T) {
		svc, pub := newTestIngestService(t)
		p := decodeIngestPayload(t, `{"project_key":"proj","platform":"go","environment":"production","error_class":"E","fingerprint":"x"}`)
		require.NoError(t, svc.Ingest(context.Background(), p))
		assert.Equal(t, "x", publishedEvent(t, pub).GetFingerprint())
	})

	t.Run("I10 source over JSON sets field 14", func(t *testing.T) {
		svc, pub := newTestIngestService(t)
		p := decodeIngestPayload(t, `{"project_key":"proj","platform":"go","environment":"production","error_class":"E","source":"onerror"}`)
		require.NoError(t, svc.Ingest(context.Background(), p))
		assert.Equal(t, "onerror", publishedEvent(t, pub).GetSource())
	})

	t.Run("I11 client received_at is ignored; client server markers are overwritten or removed", func(t *testing.T) {
		svc, pub := newTestIngestService(t)
		p := decodeIngestPayload(t, `{"project_key":"proj","platform":"go","environment":"production","error_class":"E",
			"received_at":"2001-01-01T00:00:00Z",
			"metadata":{"server_truncated":["forged"],"server_truncated_keys":["forged"],"k":"v"}}`)
		require.NoError(t, svc.IngestAt(context.Background(), p, tsRecv))
		ev := publishedEvent(t, pub)
		assert.True(t, ev.GetReceivedAt().AsTime().Equal(tsRecv))
		md := ev.GetMetadata().AsMap()
		assert.NotContains(t, md, validation.MetadataKeyServerTruncated, "nothing was cut, so a client marker is removed")
		assert.NotContains(t, md, validation.MetadataKeyServerTruncatedKeys)
		assert.Equal(t, "v", md["k"])

		svc, pub = newTestIngestService(t)
		p = decodeIngestPayload(t, `{"project_key":"proj","platform":"go","environment":"production","error_class":"E","level":"LOUD",
			"metadata":{"server_truncated":["forged"],"server_truncated_keys":["forged"]}}`)
		require.NoError(t, svc.IngestAt(context.Background(), p, tsRecv))
		md = publishedEvent(t, pub).GetMetadata().AsMap()
		assert.Equal(t, []interface{}{"level"}, md[validation.MetadataKeyServerTruncated], "the server's value replaces the client's")
		assert.NotContains(t, md, validation.MetadataKeyServerTruncatedKeys)
	})

	t.Run("I12 an exception chain without error_class is a 400 on error_class/required", func(t *testing.T) {
		p := validPayload()
		p.ErrorClass = ""
		p.Exception = &validation.ExceptionInterface{Values: []validation.ExceptionValue{{Type: "ValueError", Value: "bad"}}}
		assert.Contains(t, ingestViolations(t, p), ingestViolation{"error_class", "required"})
	})

	t.Run("I13 a batch item with a negative column fails alone", func(t *testing.T) {
		svc, pub := newTestIngestService(t)
		items := []*validation.ErrorPayload{validPayload(), validPayload(), validPayload()}
		items[1].Stacktrace = []validation.StackFrame{{File: "a.js", Line: 1, Column: -1, Function: "f"}}
		errs := make([]error, len(items))
		for i, it := range items {
			errs[i] = svc.IngestAt(context.Background(), it, tsRecv)
		}
		assert.NoError(t, errs[0])
		assert.EqualError(t, errs[1], "validation failed: stacktrace[0].column must be >= 0")
		assert.NoError(t, errs[2])
		assert.Len(t, pub.published, 2)
	})
}

// TestIngestorBounds_UTF8Safe: a truncation never splits a multi-byte character, so every truncated
// value is still valid UTF-8 (proto3 strings must be, or proto.Marshal fails).
func TestIngestorBounds_UTF8Safe(t *testing.T) {
	p := validPayload()
	p.Logger = strings.Repeat("é", validation.MaxLoggerLength) + "界界"
	boundedPayload(t, p)
	assert.True(t, utf8.ValidString(p.Logger))
	assert.Equal(t, validation.MaxLoggerLength, utf8.RuneCountInString(p.Logger))
}

// TestIngestorBounds_ShrinkIsNotQuadratic is the regression test for review finding
// esv2-rev-bounds-quadratic-dos. The contexts and metadata shrinks used to re-sort every key and
// re-marshal the whole map on each removal, O(n^2 log n) in the key count: at 40ada7d ValidatePayload
// took 28 s on 8,000 empty contexts (an 87 KB body) and 137 s on 20,000 one-int metadata keys (209 KB),
// synchronously on /ingest. The spec §4.2 rules need one sort and running sizes. The results are also
// checked, so a fast step that stopped shrinking would not pass either.
func TestIngestorBounds_ShrinkIsNotQuadratic(t *testing.T) {
	const budget = time.Second
	t.Run("20,000 metadata keys", func(t *testing.T) {
		p := validPayload()
		p.Metadata = map[string]interface{}{"user_id": "u-1", "tenant_id": "t-1"}
		for i := 0; i < 20000; i++ {
			p.Metadata[fmt.Sprintf("k%05d", i)] = float64(i)
		}
		require.Greater(t, marshalledSize(t, p.Metadata), 200*1024)
		start := time.Now()
		res := validation.ValidatePayload(p)
		elapsed := time.Since(start)
		require.True(t, res.Valid)
		assert.Less(t, elapsed, budget, "the metadata shrink must be O(n log n)")
		assert.Equal(t, []string{"metadata"}, serverTruncated(t, p))
		assert.LessOrEqual(t, marshalledSize(t, p.Metadata), validation.MaxMetadataSize)
		assert.Equal(t, "u-1", p.Metadata["user_id"], "the capped markers never push an exempt key out")
		assert.Equal(t, "t-1", p.Metadata["tenant_id"])
	})
	t.Run("the running size stops the shrink exactly: fits, and one fewer removal would not", func(t *testing.T) {
		p := validPayload()
		p.Metadata = map[string]interface{}{}
		for i := 0; i < 100; i++ {
			p.Metadata[fmt.Sprintf("k%03d", i)] = strings.Repeat("x", 1000)
		}
		boundedPayload(t, p)
		size := marshalledSize(t, p.Metadata)
		assert.LessOrEqual(t, size, validation.MaxMetadataSize)
		// Each removal frees a ~1,010-byte entry and adds a 6-byte marker entry, so a shrink that removed
		// one key too many would leave the map more than ~1,000 bytes under the bound.
		assert.Greater(t, size, validation.MaxMetadataSize-1000)
	})
	t.Run("8,000 contexts", func(t *testing.T) {
		p := validPayload()
		p.Contexts = map[string]map[string]interface{}{"device": {"model": "Pixel 8"}}
		for i := 0; i < 8000; i++ {
			p.Contexts[fmt.Sprintf("c%04d", i)] = map[string]interface{}{}
		}
		start := time.Now()
		res := validation.ValidatePayload(p)
		elapsed := time.Since(start)
		require.True(t, res.Valid)
		assert.Less(t, elapsed, budget, "the contexts shrink must be O(n log n)")
		require.Len(t, p.Contexts, validation.MaxContexts)
		assert.Contains(t, p.Contexts, "device", "a well-known context goes last")
		// Every candidate is the same size, so ties go to the lexically smallest: c7969..c7999 are kept.
		assert.NotContains(t, p.Contexts, "c7968")
		assert.Contains(t, p.Contexts, "c7969")
		assert.Contains(t, serverTruncated(t, p), "contexts")
	})
	// Review finding esv2-rev2-breadcrumbs-byte-loop-quadratic: the breadcrumbs byte loop re-marshalled the
	// whole list per dropped crumb. json.Marshal escapes each '<' to 6 bytes, so these 100 crumbs are an
	// 822 KB body that cost 300-440 ms of ValidatePayload at df38321; the running size makes it one pass.
	t.Run("100 breadcrumbs of 8,192 '<'", func(t *testing.T) {
		const crumbBudget = 100 * time.Millisecond
		p := validPayload()
		for i := 0; i < validation.MaxBreadcrumbs; i++ {
			p.Breadcrumbs = append(p.Breadcrumbs, validation.Breadcrumb{
				Category: fmt.Sprintf("c%03d", i),
				Message:  strings.Repeat("<", validation.MaxBreadcrumbMessageLength),
			})
		}
		sent := append([]validation.Breadcrumb(nil), p.Breadcrumbs...)
		require.Greater(t, marshalledSize(t, sent), 800*1024)
		start := time.Now()
		res := validation.ValidatePayload(p)
		elapsed := time.Since(start)
		require.True(t, res.Valid)
		assert.Less(t, elapsed, crumbBudget, "the breadcrumbs byte loop must not re-marshal per removal")
		// The same crumbs as the re-marshalling loop kept: the longest newest suffix that fits.
		n := len(p.Breadcrumbs)
		require.Greater(t, n, 0)
		assert.Equal(t, sent[len(sent)-n:], p.Breadcrumbs)
		assert.LessOrEqual(t, marshalledSize(t, p.Breadcrumbs), validation.MaxBreadcrumbsSize)
		assert.Greater(t, marshalledSize(t, sent[len(sent)-n-1:]), validation.MaxBreadcrumbsSize, "one more crumb would not fit")
		assert.Equal(t, []string{"breadcrumbs"}, serverTruncated(t, p))
	})
}

// TestIngestorBounds_MarkerKeysCapped is review finding esv2-rev-markers-unbounded-evict-exempt:
// server_truncated_keys had no count or length cap and the shrink never removes the markers, so they
// alone could exceed the 64 KiB metadata bound and push the exempt routing keys out.
func TestIngestorBounds_MarkerKeysCapped(t *testing.T) {
	t.Run("the reviewer's reproduction: 400 invalid 213-char tag keys", func(t *testing.T) {
		p := validPayload()
		p.Metadata = map[string]interface{}{"user_id": "u-1", "tenant_id": "t-1"}
		p.Tags = map[string]string{}
		for i := 0; i < 400; i++ {
			p.Tags[fmt.Sprintf("%03d", i)+strings.Repeat("k", 210)] = "v"
		}
		boundedPayload(t, p)
		assert.Empty(t, p.Tags)
		assert.LessOrEqual(t, marshalledSize(t, p.Metadata), validation.MaxMetadataSize)
		assert.Equal(t, "u-1", p.Metadata["user_id"], "an exempt key is never evicted to make room for the markers")
		assert.Equal(t, "t-1", p.Metadata["tenant_id"])
		assert.Equal(t, []string{"tags"}, serverTruncated(t, p), "the capped markers fit without a shrink")

		keys := serverTruncatedKeys(t, p)
		require.Len(t, keys, validation.MaxServerTruncatedKeys+1, "the first MaxServerTruncatedKeys, then one overflow entry")
		assert.Equal(t, fmt.Sprintf("+%d more", 400-validation.MaxServerTruncatedKeys), keys[len(keys)-1])
		for _, k := range keys[:validation.MaxServerTruncatedKeys] {
			assert.LessOrEqual(t, len(k), validation.MaxServerTruncatedKeyBytes)
			assert.True(t, strings.HasPrefix(k, "tags."), k)
			assert.True(t, utf8.ValidString(k))
		}
		assert.Equal(t, "tags.000"+strings.Repeat("k", validation.MaxServerTruncatedKeyBytes-len("tags.000")), keys[0])
	})
	t.Run("an entry is cut on a UTF-8 boundary", func(t *testing.T) {
		p := validPayload()
		p.Metadata = map[string]interface{}{strings.Repeat("界", 100): strings.Repeat("x", 70000)}
		boundedPayload(t, p)
		keys := serverTruncatedKeys(t, p)
		require.Len(t, keys, 1)
		assert.True(t, utf8.ValidString(keys[0]))
		assert.LessOrEqual(t, len(keys[0]), validation.MaxServerTruncatedKeyBytes)
		assert.Equal(t, strings.Repeat("界", validation.MaxServerTruncatedKeyBytes/3), keys[0])
	})
	t.Run("the overflow entry is counted inside the 64 KiB bound", func(t *testing.T) {
		p := validPayload()
		p.Metadata = map[string]interface{}{"user_id": "u-1"}
		for i := 0; i < 5000; i++ {
			p.Metadata[fmt.Sprintf("k%04d", i)] = strings.Repeat("x", 20)
		}
		boundedPayload(t, p)
		assert.LessOrEqual(t, marshalledSize(t, p.Metadata), validation.MaxMetadataSize)
		assert.Equal(t, "u-1", p.Metadata["user_id"])
		keys := serverTruncatedKeys(t, p)
		require.Len(t, keys, validation.MaxServerTruncatedKeys+1)
		assert.Regexp(t, `^\+\d+ more$`, keys[len(keys)-1])
	})
	t.Run("the worst case of both markers stays far inside the metadata bound", func(t *testing.T) {
		// JSON escapes one byte to at most six (\u00XX); each entry adds its quotes and a comma.
		worstKeys := validation.MaxServerTruncatedKeys*(6*validation.MaxServerTruncatedKeyBytes+3) + len(`"+99999999 more"`)
		all := make([]interface{}, 0)
		for _, f := range obs.TruncatedFieldVocabulary() {
			all = append(all, f)
		}
		worstFields := marshalledSize(t, map[string]interface{}{validation.MetadataKeyServerTruncated: all})
		assert.Less(t, worstKeys+worstFields, validation.MaxMetadataSize*2/3)
	})
}
