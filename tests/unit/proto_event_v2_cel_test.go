package unit

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"buf.build/go/protovalidate"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"

	sentinelv1 "github.com/NurfitraPujo/sentinel/gen/sentinel/v1"
)

// V1a (sentinel-event-schema-v2 spec §9.2, TASK-01 criterion 1): every v2 CEL id of spec §3.3 has one
// accepting row (the value AT the bound) and one rejecting row (one past it), asserted by rule id over the
// descriptor compiled into gen/ — so a .proto edit without `buf generate` fails here, exactly like
// proto_environment_cel_test.go. The ingestor truncates to within every one of these first (§4.2), which
// makes them tripwires; this test proves the tripwire is armed.

// v2RuleIDs returns every rule id a validation error carries.
func v2RuleIDs(t *testing.T, err error) []string {
	t.Helper()
	var verr *protovalidate.ValidationError
	require.True(t, errors.As(err, &verr), "want a *protovalidate.ValidationError, got %T: %v", err, err)
	ids := make([]string, 0, len(verr.Violations))
	for _, v := range verr.Violations {
		ids = append(ids, v.Proto.GetRuleId())
	}
	return ids
}

func TestEventV2CEL_EveryRuleAcceptsAtAndRejectsPastItsBound(t *testing.T) {
	v, err := protovalidate.New()
	require.NoError(t, err)

	rep := strings.Repeat
	tagsOf := func(n int) map[string]string {
		m := make(map[string]string, n)
		for i := 0; i < n; i++ {
			m[fmt.Sprintf("k%02d", i)] = "v"
		}
		return m
	}
	stringMap := func(n int, key, value string) map[string]string {
		m := make(map[string]string, n)
		for i := 0; i < n; i++ {
			m[fmt.Sprintf("%s%02d", key, i)] = value
		}
		return m
	}
	contextsOf := func(n int) map[string]*structpb.Struct {
		m := make(map[string]*structpb.Struct, n)
		for i := 0; i < n; i++ {
			m[fmt.Sprintf("c%02d", i)] = &structpb.Struct{}
		}
		return m
	}
	lines := func(n int, s string) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = s
		}
		return out
	}
	withChainFrame := func(f *sentinelv1.StackFrame) func(e *sentinelv1.ErrorEvent) {
		return func(e *sentinelv1.ErrorEvent) {
			e.Exception = &sentinelv1.ExceptionInterface{Values: []*sentinelv1.ExceptionValue{{Type: "E", Stacktrace: []*sentinelv1.StackFrame{f}}}}
		}
	}

	cases := []struct {
		id     string
		accept func(e *sentinelv1.ErrorEvent)
		reject func(e *sentinelv1.ErrorEvent)
	}{
		{"error_event.level", func(e *sentinelv1.ErrorEvent) { e.Level = "warning" }, func(e *sentinelv1.ErrorEvent) { e.Level = "WARN" }},
		{"error_event.logger", func(e *sentinelv1.ErrorEvent) { e.Logger = rep("é", 64) }, func(e *sentinelv1.ErrorEvent) { e.Logger = rep("a", 65) }},
		{"error_event.server_name", func(e *sentinelv1.ErrorEvent) { e.ServerName = rep("a", 256) }, func(e *sentinelv1.ErrorEvent) { e.ServerName = rep("a", 257) }},
		{"error_event.transaction", func(e *sentinelv1.ErrorEvent) { e.Transaction = rep("a", 200) }, func(e *sentinelv1.ErrorEvent) { e.Transaction = rep("a", 201) }},
		{"error_event.dist", func(e *sentinelv1.ErrorEvent) { e.Dist = rep("a", 64) }, func(e *sentinelv1.ErrorEvent) { e.Dist = rep("a", 65) }},
		{"error_event.source", func(e *sentinelv1.ErrorEvent) { e.Source = rep("a", 64) }, func(e *sentinelv1.ErrorEvent) { e.Source = rep("a", 65) }},
		{"error_event.tags", func(e *sentinelv1.ErrorEvent) { e.Tags = tagsOf(50) }, func(e *sentinelv1.ErrorEvent) { e.Tags = tagsOf(51) }},
		{"error_event.tags", func(e *sentinelv1.ErrorEvent) { e.Tags = map[string]string{rep("k", 32): rep("v", 200)} }, func(e *sentinelv1.ErrorEvent) { e.Tags = map[string]string{rep("k", 33): "v"} }},
		{"error_event.tags", func(e *sentinelv1.ErrorEvent) { e.Tags = map[string]string{"a_b.c:d-e": "v"} }, func(e *sentinelv1.ErrorEvent) { e.Tags = map[string]string{"a b": "v"} }},
		{"error_event.tags", func(e *sentinelv1.ErrorEvent) { e.Tags = map[string]string{"k": "v"} }, func(e *sentinelv1.ErrorEvent) { e.Tags = map[string]string{"k": rep("v", 201)} }},
		{"error_event.tags", func(e *sentinelv1.ErrorEvent) { e.Tags = map[string]string{"k": "a\tb"} }, func(e *sentinelv1.ErrorEvent) { e.Tags = map[string]string{"k": "a\nb"} }},
		{"error_event.tags", func(e *sentinelv1.ErrorEvent) { e.Tags = map[string]string{"k": "v"} }, func(e *sentinelv1.ErrorEvent) { e.Tags = map[string]string{"k": ""} }},
		{"error_event.contexts", func(e *sentinelv1.ErrorEvent) { e.Contexts = contextsOf(32) }, func(e *sentinelv1.ErrorEvent) { e.Contexts = contextsOf(33) }},
		{"error_event.contexts", func(e *sentinelv1.ErrorEvent) { e.Contexts = map[string]*structpb.Struct{rep("c", 64): {}} }, func(e *sentinelv1.ErrorEvent) { e.Contexts = map[string]*structpb.Struct{rep("c", 65): {}} }},
		{"error_event.contexts", func(e *sentinelv1.ErrorEvent) { e.Contexts = map[string]*structpb.Struct{"c": {}} }, func(e *sentinelv1.ErrorEvent) { e.Contexts = map[string]*structpb.Struct{"": {}} }},
		{"sdk.bounds", func(e *sentinelv1.ErrorEvent) { e.Sdk = &sentinelv1.Sdk{Name: rep("n", 128), Version: rep("v", 64)} }, func(e *sentinelv1.ErrorEvent) { e.Sdk = &sentinelv1.Sdk{Name: rep("n", 129)} }},
		{"sdk.bounds", func(e *sentinelv1.ErrorEvent) { e.Sdk = &sentinelv1.Sdk{Version: rep("v", 64)} }, func(e *sentinelv1.ErrorEvent) { e.Sdk = &sentinelv1.Sdk{Version: rep("v", 65)} }},
		{"user.bounds", func(e *sentinelv1.ErrorEvent) { e.User = &sentinelv1.User{Id: rep("i", 255)} }, func(e *sentinelv1.ErrorEvent) { e.User = &sentinelv1.User{Id: rep("i", 256)} }},
		{"user.bounds", func(e *sentinelv1.ErrorEvent) { e.User = &sentinelv1.User{Username: rep("u", 128)} }, func(e *sentinelv1.ErrorEvent) { e.User = &sentinelv1.User{Username: rep("u", 129)} }},
		{"user.bounds", func(e *sentinelv1.ErrorEvent) { e.User = &sentinelv1.User{Email: rep("e", 254)} }, func(e *sentinelv1.ErrorEvent) { e.User = &sentinelv1.User{Email: rep("e", 255)} }},
		{"user.bounds", func(e *sentinelv1.ErrorEvent) { e.User = &sentinelv1.User{IpAddress: rep("1", 45)} }, func(e *sentinelv1.ErrorEvent) { e.User = &sentinelv1.User{IpAddress: rep("1", 46)} }},
		{"geo.bounds", func(e *sentinelv1.ErrorEvent) {
			e.User = &sentinelv1.User{Geo: &sentinelv1.Geo{City: rep("c", 128), Region: rep("r", 128), Subdivision: rep("s", 128), CountryCode: rep("X", 8)}}
		}, func(e *sentinelv1.ErrorEvent) { e.User = &sentinelv1.User{Geo: &sentinelv1.Geo{City: rep("c", 129)}} }},
		{"geo.bounds", func(e *sentinelv1.ErrorEvent) { e.User = &sentinelv1.User{Geo: &sentinelv1.Geo{Region: rep("r", 128)}} }, func(e *sentinelv1.ErrorEvent) { e.User = &sentinelv1.User{Geo: &sentinelv1.Geo{Region: rep("r", 129)}} }},
		{"geo.bounds", func(e *sentinelv1.ErrorEvent) {
			e.User = &sentinelv1.User{Geo: &sentinelv1.Geo{Subdivision: rep("s", 128)}}
		}, func(e *sentinelv1.ErrorEvent) {
			e.User = &sentinelv1.User{Geo: &sentinelv1.Geo{Subdivision: rep("s", 129)}}
		}},
		{"geo.bounds", func(e *sentinelv1.ErrorEvent) { e.User = &sentinelv1.User{Geo: &sentinelv1.Geo{CountryCode: "DE"}} }, func(e *sentinelv1.ErrorEvent) {
			e.User = &sentinelv1.User{Geo: &sentinelv1.Geo{CountryCode: rep("X", 9)}}
		}},
		{"request.bounds", func(e *sentinelv1.ErrorEvent) { e.Request = &sentinelv1.Request{Method: rep("M", 16)} }, func(e *sentinelv1.ErrorEvent) { e.Request = &sentinelv1.Request{Method: rep("M", 17)} }},
		{"request.bounds", func(e *sentinelv1.ErrorEvent) { e.Request = &sentinelv1.Request{Url: rep("u", 2048)} }, func(e *sentinelv1.ErrorEvent) { e.Request = &sentinelv1.Request{Url: rep("u", 2049)} }},
		{"request.bounds", func(e *sentinelv1.ErrorEvent) { e.Request = &sentinelv1.Request{QueryString: rep("q", 4096)} }, func(e *sentinelv1.ErrorEvent) { e.Request = &sentinelv1.Request{QueryString: rep("q", 4097)} }},
		{"request.headers", func(e *sentinelv1.ErrorEvent) { e.Request = &sentinelv1.Request{Headers: stringMap(64, "h", "v")} }, func(e *sentinelv1.ErrorEvent) { e.Request = &sentinelv1.Request{Headers: stringMap(65, "h", "v")} }},
		{"request.headers", func(e *sentinelv1.ErrorEvent) {
			e.Request = &sentinelv1.Request{Headers: map[string]string{rep("h", 256): rep("v", 4096)}}
		}, func(e *sentinelv1.ErrorEvent) {
			e.Request = &sentinelv1.Request{Headers: map[string]string{rep("h", 257): "v"}}
		}},
		{"request.headers", func(e *sentinelv1.ErrorEvent) { e.Request = &sentinelv1.Request{Headers: map[string]string{"h": ""}} }, func(e *sentinelv1.ErrorEvent) {
			e.Request = &sentinelv1.Request{Headers: map[string]string{"h": rep("v", 4097)}}
		}},
		{"request.headers", func(e *sentinelv1.ErrorEvent) { e.Request = &sentinelv1.Request{Headers: map[string]string{"h": "v"}} }, func(e *sentinelv1.ErrorEvent) { e.Request = &sentinelv1.Request{Headers: map[string]string{"": "v"}} }},
		{"request.env", func(e *sentinelv1.ErrorEvent) { e.Request = &sentinelv1.Request{Env: stringMap(64, "E", "v")} }, func(e *sentinelv1.ErrorEvent) { e.Request = &sentinelv1.Request{Env: stringMap(65, "E", "v")} }},
		{"request.env", func(e *sentinelv1.ErrorEvent) {
			e.Request = &sentinelv1.Request{Env: map[string]string{rep("E", 256): rep("v", 1024)}}
		}, func(e *sentinelv1.ErrorEvent) {
			e.Request = &sentinelv1.Request{Env: map[string]string{"E": rep("v", 1025)}}
		}},
		{"request.env", func(e *sentinelv1.ErrorEvent) { e.Request = &sentinelv1.Request{Env: map[string]string{"E": "v"}} }, func(e *sentinelv1.ErrorEvent) {
			e.Request = &sentinelv1.Request{Env: map[string]string{rep("E", 257): "v"}}
		}},
		{"breadcrumb.bounds", func(e *sentinelv1.ErrorEvent) { e.Breadcrumbs = []*sentinelv1.Breadcrumb{{Type: rep("t", 64)}} }, func(e *sentinelv1.ErrorEvent) { e.Breadcrumbs = []*sentinelv1.Breadcrumb{{Type: rep("t", 65)}} }},
		{"breadcrumb.bounds", func(e *sentinelv1.ErrorEvent) { e.Breadcrumbs = []*sentinelv1.Breadcrumb{{Category: rep("c", 128)}} }, func(e *sentinelv1.ErrorEvent) { e.Breadcrumbs = []*sentinelv1.Breadcrumb{{Category: rep("c", 129)}} }},
		{"breadcrumb.bounds", func(e *sentinelv1.ErrorEvent) { e.Breadcrumbs = []*sentinelv1.Breadcrumb{{Message: rep("m", 8192)}} }, func(e *sentinelv1.ErrorEvent) { e.Breadcrumbs = []*sentinelv1.Breadcrumb{{Message: rep("m", 8193)}} }},
		{"breadcrumb.bounds", func(e *sentinelv1.ErrorEvent) { e.Breadcrumbs = []*sentinelv1.Breadcrumb{{Level: "debug"}} }, func(e *sentinelv1.ErrorEvent) { e.Breadcrumbs = []*sentinelv1.Breadcrumb{{Level: "Debug"}} }},
		{"exception_value.bounds", func(e *sentinelv1.ErrorEvent) {
			e.Exception = &sentinelv1.ExceptionInterface{Values: []*sentinelv1.ExceptionValue{{Type: rep("T", 255)}}}
		}, func(e *sentinelv1.ErrorEvent) {
			e.Exception = &sentinelv1.ExceptionInterface{Values: []*sentinelv1.ExceptionValue{{Type: rep("T", 256)}}}
		}},
		{"exception_value.bounds", func(e *sentinelv1.ErrorEvent) {
			e.Exception = &sentinelv1.ExceptionInterface{Values: []*sentinelv1.ExceptionValue{{Value: rep("v", 10000)}}}
		}, func(e *sentinelv1.ErrorEvent) {
			e.Exception = &sentinelv1.ExceptionInterface{Values: []*sentinelv1.ExceptionValue{{Value: rep("v", 10001)}}}
		}},
		{"mechanism.bounds", func(e *sentinelv1.ErrorEvent) {
			e.Exception = &sentinelv1.ExceptionInterface{Values: []*sentinelv1.ExceptionValue{{Mechanism: &sentinelv1.Mechanism{Type: rep("m", 64)}}}}
		}, func(e *sentinelv1.ErrorEvent) {
			e.Exception = &sentinelv1.ExceptionInterface{Values: []*sentinelv1.ExceptionValue{{Mechanism: &sentinelv1.Mechanism{Type: rep("m", 65)}}}}
		}},
		{"stack_frame.v2_bounds", func(e *sentinelv1.ErrorEvent) {
			e.Stacktrace = []*sentinelv1.StackFrame{{File: "f", Module: rep("m", 255)}}
		}, func(e *sentinelv1.ErrorEvent) {
			e.Stacktrace = []*sentinelv1.StackFrame{{File: "f", Module: rep("m", 256)}}
		}},
		{"stack_frame.v2_bounds", func(e *sentinelv1.ErrorEvent) { e.Stacktrace = []*sentinelv1.StackFrame{{AbsPath: rep("p", 1024)}} }, func(e *sentinelv1.ErrorEvent) { e.Stacktrace = []*sentinelv1.StackFrame{{AbsPath: rep("p", 1025)}} }},
		{"stack_frame.v2_bounds", func(e *sentinelv1.ErrorEvent) { e.Stacktrace = []*sentinelv1.StackFrame{{ContextLine: rep("c", 1024)}} }, func(e *sentinelv1.ErrorEvent) { e.Stacktrace = []*sentinelv1.StackFrame{{ContextLine: rep("c", 1025)}} }},
		{"stack_frame.v2_bounds", func(e *sentinelv1.ErrorEvent) {
			e.Stacktrace = []*sentinelv1.StackFrame{{PreContext: lines(5, rep("c", 1024))}}
		}, func(e *sentinelv1.ErrorEvent) {
			e.Stacktrace = []*sentinelv1.StackFrame{{PreContext: []string{rep("c", 1025)}}}
		}},
		{"stack_frame.v2_bounds", func(e *sentinelv1.ErrorEvent) {
			e.Stacktrace = []*sentinelv1.StackFrame{{PostContext: lines(5, rep("c", 1024))}}
		}, func(e *sentinelv1.ErrorEvent) {
			e.Stacktrace = []*sentinelv1.StackFrame{{PostContext: []string{rep("c", 1025)}}}
		}},
		// The v2 bounds apply to chain frames too: ExceptionValue.stacktrace reuses StackFrame.
		{"stack_frame.v2_bounds", withChainFrame(&sentinelv1.StackFrame{Module: rep("m", 255)}), withChainFrame(&sentinelv1.StackFrame{Module: rep("m", 256)})},
		// The tripwire spec §4.2's "chain frames" repair exists for: a Sentry-shaped chain frame with in_app
		// and only module/abs_path fails stack_frame.file_required, so the ingestor clears in_app first.
		{"stack_frame.file_required", withChainFrame(&sentinelv1.StackFrame{Module: "app.auth", AbsPath: "/srv/app/auth.py", InApp: false}), withChainFrame(&sentinelv1.StackFrame{Module: "app.auth", AbsPath: "/srv/app/auth.py", InApp: true})},
		// Repeated-count field rules (spec §3.1-§3.2: the same form as stacktrace's max_items).
		{"repeated.max_items", func(e *sentinelv1.ErrorEvent) { e.Breadcrumbs = make([]*sentinelv1.Breadcrumb, 100) }, func(e *sentinelv1.ErrorEvent) { e.Breadcrumbs = make([]*sentinelv1.Breadcrumb, 101) }},
		{"repeated.max_items", func(e *sentinelv1.ErrorEvent) {
			e.Exception = &sentinelv1.ExceptionInterface{Values: make([]*sentinelv1.ExceptionValue, 20)}
		}, func(e *sentinelv1.ErrorEvent) {
			e.Exception = &sentinelv1.ExceptionInterface{Values: make([]*sentinelv1.ExceptionValue, 21)}
		}},
		{"repeated.max_items", func(e *sentinelv1.ErrorEvent) {
			e.Exception = &sentinelv1.ExceptionInterface{Values: []*sentinelv1.ExceptionValue{{Stacktrace: make([]*sentinelv1.StackFrame, 100)}}}
		}, func(e *sentinelv1.ErrorEvent) {
			e.Exception = &sentinelv1.ExceptionInterface{Values: []*sentinelv1.ExceptionValue{{Stacktrace: make([]*sentinelv1.StackFrame, 101)}}}
		}},
		{"repeated.max_items", func(e *sentinelv1.ErrorEvent) {
			e.Stacktrace = []*sentinelv1.StackFrame{{PreContext: lines(5, "l"), PostContext: lines(5, "l")}}
		}, func(e *sentinelv1.ErrorEvent) { e.Stacktrace = []*sentinelv1.StackFrame{{PreContext: lines(6, "l")}} }},
		{"repeated.max_items", func(e *sentinelv1.ErrorEvent) { e.Stacktrace = []*sentinelv1.StackFrame{{PostContext: lines(5, "l")}} }, func(e *sentinelv1.ErrorEvent) { e.Stacktrace = []*sentinelv1.StackFrame{{PostContext: lines(6, "l")}} }},
	}

	// Fill nil list entries so a count row measures only the count.
	fillNil := func(e *sentinelv1.ErrorEvent) {
		for i, b := range e.Breadcrumbs {
			if b == nil {
				e.Breadcrumbs[i] = &sentinelv1.Breadcrumb{}
			}
		}
		if e.Exception != nil {
			for i, ev := range e.Exception.Values {
				if ev == nil {
					e.Exception.Values[i] = &sentinelv1.ExceptionValue{}
					continue
				}
				for j, f := range ev.Stacktrace {
					if f == nil {
						ev.Stacktrace[j] = &sentinelv1.StackFrame{}
					}
				}
			}
		}
	}

	for i, tc := range cases {
		t.Run(fmt.Sprintf("%02d_%s", i, tc.id), func(t *testing.T) {
			accepted := validErrorEvent()
			tc.accept(accepted)
			fillNil(accepted)
			require.NoError(t, v.Validate(accepted), "%s must accept the value at its bound", tc.id)

			rejected := validErrorEvent()
			tc.reject(rejected)
			fillNil(rejected)
			err := v.Validate(rejected)
			require.Error(t, err, "%s must reject the value past its bound", tc.id)
			require.Contains(t, v2RuleIDs(t, err), tc.id)
		})
	}
}

// Every v2 field populated within its bounds validates as one event (no two rules conflict).
func TestEventV2CEL_FullyPopulatedEventValidates(t *testing.T) {
	v, err := protovalidate.New()
	require.NoError(t, err)
	handled := false
	data, err := structpb.NewValue(map[string]interface{}{"password": "p", "items": []interface{}{1.0, "a"}})
	require.NoError(t, err)
	ctx, err := structpb.NewStruct(map[string]interface{}{"model": "Pixel 8", "memory_size": 8.0e9})
	require.NoError(t, err)

	e := validErrorEvent()
	e.Level, e.Logger, e.ServerName, e.Transaction, e.Dist, e.Source = "fatal", "auth", "pod-1", "/login", "412", "onerror"
	e.Sdk = &sentinelv1.Sdk{Name: "sentinel.go", Version: "1.0.0"}
	e.User = &sentinelv1.User{Id: "u1", Username: "alice", Email: "a@b.co", IpAddress: "2001:db8::1", Geo: &sentinelv1.Geo{City: "Berlin", Region: "BE", CountryCode: "DE", Subdivision: "Berlin"}}
	e.Tags = map[string]string{"tenant_id": "t1", "region": "eu"}
	e.Contexts = map[string]*structpb.Struct{"device": ctx}
	e.Request = &sentinelv1.Request{Method: "POST", Url: "https://x/login", QueryString: "a=1", Headers: map[string]string{"Authorization": "Bearer x"}, Data: data, Env: map[string]string{"REMOTE_ADDR": "10.0.0.1"}}
	e.Breadcrumbs = []*sentinelv1.Breadcrumb{{Type: "http", Category: "xhr", Level: "info", Message: "GET /", Data: ctx}}
	e.Exception = &sentinelv1.ExceptionInterface{Values: []*sentinelv1.ExceptionValue{{Type: "Root", Value: "cause", Mechanism: &sentinelv1.Mechanism{Type: "generic", Handled: &handled}, Stacktrace: []*sentinelv1.StackFrame{{Module: "app.auth", AbsPath: "/srv/app/auth.py", ContextLine: "x", PreContext: []string{"a"}, PostContext: []string{"b"}}}}}}
	require.NoError(t, v.Validate(e))
}
