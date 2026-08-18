package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// --- test plumbing ---------------------------------------------------------------------------

// scriptedHTTP plays a fixed sequence of HTTP responses, one per call, asserting the exact
// request body sent against a golden JSON string (plan §8: "request-shape GOLDENS (exact JSON
// bodies)"). Calling past the end of the script fails the test.
type scriptedHTTP struct {
	t     *testing.T
	steps []httpStep
	calls int
	// recordedHeaders captures every call's headers, in order, for auth-header assertions.
	recordedHeaders []http.Header
}

type httpStep struct {
	wantBodyJSON string // "" skips the body assertion
	status       int
	body         string
	header       http.Header
}

func (s *scriptedHTTP) Do(req *http.Request) (*http.Response, error) {
	s.t.Helper()
	if s.calls >= len(s.steps) {
		s.t.Fatalf("scriptedHTTP: unexpected call #%d past end of script", s.calls+1)
	}
	step := s.steps[s.calls]
	s.calls++

	if req.URL.Path != "/v1/chat/completions" {
		s.t.Fatalf("unexpected path: %s", req.URL.Path)
	}
	s.recordedHeaders = append(s.recordedHeaders, req.Header.Clone())

	got, err := io.ReadAll(req.Body)
	if err != nil {
		s.t.Fatalf("reading request body: %v", err)
	}
	if step.wantBodyJSON != "" {
		assertJSONEqual(s.t, step.wantBodyJSON, string(got))
	}

	h := step.header
	if h == nil {
		h = http.Header{}
	}
	return &http.Response{
		StatusCode: step.status,
		Body:       io.NopCloser(strings.NewReader(step.body)),
		Header:     h,
	}, nil
}

// assertJSONEqual compares two JSON strings for semantic equality (key order doesn't matter for
// the assertion even though buildRequestBody's own map-key ordering is deterministic).
func assertJSONEqual(t *testing.T, wantJSON, gotJSON string) {
	t.Helper()
	var want, got any
	if err := json.Unmarshal([]byte(wantJSON), &want); err != nil {
		t.Fatalf("invalid want JSON: %v\n%s", err, wantJSON)
	}
	if err := json.Unmarshal([]byte(gotJSON), &got); err != nil {
		t.Fatalf("invalid got JSON: %v\n%s", err, gotJSON)
	}
	wantCanon, _ := json.Marshal(want)
	gotCanon, _ := json.Marshal(got)
	if string(wantCanon) != string(gotCanon) {
		t.Errorf("request body mismatch:\n want: %s\n  got: %s", wantCanon, gotCanon)
	}
}

func noSleep(context.Context, time.Duration) {}

// --- URL resolution goldens --------------------------------------------------------------------

// TestResolveOpenAIURLs_ProviderTable pins the LLM_BASE_URL convention (NewOpenAIChat's doc
// comment) against every provider the package doc/plan §4.1 enumerates as covered: BaseURL is
// the API root (as published by each provider), and the adapter appends only "/chat/completions".
func TestResolveOpenAIURLs_ProviderTable(t *testing.T) {
	cases := []struct {
		name           string
		base           string
		wantCompletion string
	}{
		{"openai", "https://api.openai.com/v1", "https://api.openai.com/v1/chat/completions"},
		{"ollama", "http://localhost:11434/v1", "http://localhost:11434/v1/chat/completions"},
		{"vllm", "http://localhost:8000/v1", "http://localhost:8000/v1/chat/completions"},
		{"litellm", "http://localhost:4000/v1", "http://localhost:4000/v1/chat/completions"},
		{"openrouter", "https://openrouter.ai/api/v1", "https://openrouter.ai/api/v1/chat/completions"},
		{"gemini-compat", "https://generativelanguage.googleapis.com/v1beta/openai", "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions"},
		{"gemini-compat trailing slash", "https://generativelanguage.googleapis.com/v1beta/openai/", "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions"},
		{"bare host tolerated", "https://api.example.com", "https://api.example.com/v1/chat/completions"},
		{"bare host trailing slash", "https://api.example.com/", "https://api.example.com/v1/chat/completions"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, gotCompletion := resolveOpenAIURLs(tc.base)
			if gotCompletion != tc.wantCompletion {
				t.Errorf("resolveOpenAIURLs(%q) completions = %q, want %q", tc.base, gotCompletion, tc.wantCompletion)
			}
		})
	}
}

// TestComplete_PostsToResolvedURL proves the resolved URL is what Complete actually POSTs to, end
// to end through an httptest server keyed on the provider's real path — not just unit-testing
// resolveOpenAIURLs in isolation.
func TestComplete_PostsToResolvedURL(t *testing.T) {
	cases := []struct {
		name     string
		base     string
		wantPath string
	}{
		{"openai", "https://api.openai.com/v1", "/v1/chat/completions"},
		{"openrouter", "https://openrouter.ai/api/v1", "/api/v1/chat/completions"},
		{"gemini-compat", "https://generativelanguage.googleapis.com/v1beta/openai", "/v1beta/openai/chat/completions"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{}}`)
			}))
			defer srv.Close()

			// Swap the provider's real host for the httptest server's, keeping the provider's path.
			providerURL, err := url.Parse(tc.base)
			if err != nil {
				t.Fatalf("parsing provider base URL: %v", err)
			}
			srvURL, err := url.Parse(srv.URL)
			if err != nil {
				t.Fatalf("parsing httptest server URL: %v", err)
			}
			providerURL.Scheme, providerURL.Host = srvURL.Scheme, srvURL.Host
			chat := newOpenAIChatForTest(providerURL.String(), "k", "m", &http.Client{}, noSleep)
			if _, err := chat.Complete(context.Background(), Request{Messages: []Msg{{Role: RoleUser, Text: "hi"}}}); err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if gotPath != tc.wantPath {
				t.Errorf("POSTed path = %q, want %q", gotPath, tc.wantPath)
			}
		})
	}
}

// --- request-shape goldens ---------------------------------------------------------------------

func TestComplete_SimpleCompletionGolden(t *testing.T) {
	http := &scriptedHTTP{t: t, steps: []httpStep{
		{
			wantBodyJSON: `{
				"model": "gpt-4o-mini",
				"messages": [
					{"role":"system","content":"You are a triage assistant."},
					{"role":"user","content":"hello"}
				],
				"max_tokens": 500
			}`,
			status: 200,
			body: `{
				"choices":[{"message":{"content":"hi there"},"finish_reason":"stop"}],
				"usage":{"prompt_tokens":12,"completion_tokens":3}
			}`,
		},
	}}
	chat := newOpenAIChatForTest("https://api.example.com", "sk-test", "gpt-4o-mini", http, noSleep)

	resp, err := chat.Complete(context.Background(), Request{
		System:    "You are a triage assistant.",
		Messages:  []Msg{{Role: RoleUser, Text: "hello"}},
		MaxTokens: 500,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "hi there" {
		t.Errorf("Text = %q", resp.Text)
	}
	if resp.Usage.InputTokens != 12 || resp.Usage.OutputTokens != 3 {
		t.Errorf("Usage = %+v", resp.Usage)
	}
	if resp.StopReason != StopEndTurn {
		t.Errorf("StopReason = %q", resp.StopReason)
	}
	if got := http.recordedHeaders[0].Get("Authorization"); got != "Bearer sk-test" {
		t.Errorf("Authorization header = %q", got)
	}
	if http.calls != 1 {
		t.Fatalf("expected exactly 1 HTTP call, got %d", http.calls)
	}
}

func TestComplete_OmitsAuthHeaderWhenAPIKeyEmpty(t *testing.T) {
	http := &scriptedHTTP{t: t, steps: []httpStep{
		{status: 200, body: `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{}}`},
	}}
	chat := newOpenAIChatForTest("http://localhost:11434/v1", "", "llama3", http, noSleep)

	if _, err := chat.Complete(context.Background(), Request{Messages: []Msg{{Role: RoleUser, Text: "hi"}}}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got := http.recordedHeaders[0].Values("Authorization"); len(got) != 0 {
		t.Errorf("Authorization header should be absent for empty API key, got %v", got)
	}
}

func TestComplete_ToolRoundTripGolden(t *testing.T) {
	// Turn 1: the harness sends a user turn plus tool defs; the fake server returns a tool call.
	// Turn 2: the harness feeds back the assistant's tool_calls turn + the tool result, and the
	// fake server returns the final decision.
	scripted := &scriptedHTTP{t: t, steps: []httpStep{
		{
			wantBodyJSON: `{
				"model": "gpt-4o-mini",
				"messages": [{"role":"user","content":"triage this issue"}],
				"tools": [{
					"type":"function",
					"function":{
						"name":"get_issue",
						"description":"fetch the issue",
						"parameters":{"type":"object","properties":{},"required":[],"additionalProperties":false}
					}
				}]
			}`,
			status: 200,
			body: `{
				"choices":[{"message":{"content":"","tool_calls":[
					{"id":"call_1","type":"function","function":{"name":"get_issue","arguments":"{}"}}
				]},"finish_reason":"tool_calls"}],
				"usage":{"prompt_tokens":20,"completion_tokens":8}
			}`,
		},
		{
			wantBodyJSON: `{
				"model": "gpt-4o-mini",
				"messages": [
					{"role":"user","content":"triage this issue"},
					{"role":"assistant","tool_calls":[
						{"id":"call_1","type":"function","function":{"name":"get_issue","arguments":"{}"}}
					]},
					{"role":"tool","tool_call_id":"call_1","content":"{\"title\":\"boom\"}"}
				],
				"tools": [{
					"type":"function",
					"function":{
						"name":"get_issue",
						"description":"fetch the issue",
						"parameters":{"type":"object","properties":{},"required":[],"additionalProperties":false}
					}
				}]
			}`,
			status: 200,
			body: `{
				"choices":[{"message":{"content":"done"},"finish_reason":"stop"}],
				"usage":{"prompt_tokens":30,"completion_tokens":4}
			}`,
		},
	}}
	chat := newOpenAIChatForTest("https://api.example.com", "sk-test", "gpt-4o-mini", scripted, noSleep)

	tools := []ToolDef{{Name: "get_issue", Description: "fetch the issue", Params: Schema{Type: "object"}}}

	// First call: model asks for a tool.
	resp1, err := chat.Complete(context.Background(), Request{
		Messages: []Msg{{Role: RoleUser, Text: "triage this issue"}},
		Tools:    tools,
	})
	if err != nil {
		t.Fatalf("Complete #1: %v", err)
	}
	if len(resp1.ToolCalls) != 1 || resp1.ToolCalls[0].ID != "call_1" || resp1.ToolCalls[0].Name != "get_issue" {
		t.Fatalf("ToolCalls = %+v", resp1.ToolCalls)
	}

	// Second call: the harness appends the assistant tool-call turn plus the tool result.
	resp2, err := chat.Complete(context.Background(), Request{
		Messages: []Msg{
			{Role: RoleUser, Text: "triage this issue"},
			{Role: RoleAssistant, ToolCalls: resp1.ToolCalls},
			{Role: RoleTool, ToolResults: []ToolResult{{ToolCallID: "call_1", Content: `{"title":"boom"}`}}},
		},
		Tools: tools,
	})
	if err != nil {
		t.Fatalf("Complete #2: %v", err)
	}
	if resp2.Text != "done" || resp2.StopReason != StopEndTurn {
		t.Fatalf("resp2 = %+v", resp2)
	}
	if scripted.calls != 2 {
		t.Fatalf("expected 2 HTTP calls, got %d", scripted.calls)
	}
}

func TestComplete_JSONSchemaRequestGolden(t *testing.T) {
	schema := Schema{
		Type:     "object",
		Required: []string{"disposition"},
		Properties: map[string]Schema{
			"disposition": {Type: "string", Enum: []string{"comment_only", "fixable"}},
			"confidence":  {Type: "number"},
			"duplicateOf": {Type: "string", Nullable: true},
		},
	}
	http := &scriptedHTTP{t: t, steps: []httpStep{
		{
			// strict:true requires `required` to list EVERY property (validator blocker) — the
			// caller's Required (["disposition"] only) is NOT what goes on the wire under strict mode.
			wantBodyJSON: `{
				"model": "gpt-4o-mini",
				"messages": [{"role":"user","content":"decide"}],
				"response_format": {
					"type":"json_schema",
					"json_schema": {
						"name":"triage_decision",
						"strict": true,
						"schema": {
							"type":"object",
							"required":["confidence","disposition","duplicateOf"],
							"additionalProperties": false,
							"properties":{
								"disposition":{"type":"string","enum":["comment_only","fixable"]},
								"confidence":{"type":"number"},
								"duplicateOf":{"type":["string","null"]}
							}
						}
					}
				}
			}`,
			status: 200,
			body:   `{"choices":[{"message":{"content":"{\"disposition\":\"fixable\"}"},"finish_reason":"stop"}],"usage":{}}`,
		},
	}}
	chat := newOpenAIChatForTest("https://api.example.com", "sk-test", "gpt-4o-mini", http, noSleep)

	_, err := chat.Complete(context.Background(), Request{
		Messages:       []Msg{{Role: RoleUser, Text: "decide"}},
		JSONSchema:     &schema,
		JSONSchemaName: "triage_decision",
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
}

// TestBuildRequestBody_JSONSchemaNameDefaultsWhenEmpty guards against the empty-name passthrough
// (validator major finding): OpenAI 400s on an empty json_schema.name.
func TestBuildRequestBody_JSONSchemaNameDefaultsWhenEmpty(t *testing.T) {
	schema := Schema{Type: "object", Properties: map[string]Schema{"x": {Type: "string"}}}
	body := buildRequestBody("gpt-4o-mini", Request{
		Messages:   []Msg{{Role: RoleUser, Text: "decide"}},
		JSONSchema: &schema,
		// JSONSchemaName intentionally unset.
	})
	if body.ResponseFormat == nil || body.ResponseFormat.JSONSchema.Name == "" {
		t.Fatalf("json_schema.name must never be emitted empty, got %+v", body.ResponseFormat)
	}
	if body.ResponseFormat.JSONSchema.Name != jsonSchemaNameDefault {
		t.Errorf("Name = %q, want default %q", body.ResponseFormat.JSONSchema.Name, jsonSchemaNameDefault)
	}
}

// TestBuildRequestBody_JSONSchemaNameSanitized guards the ^[a-zA-Z0-9_-]{1,64}$ constraint.
func TestBuildRequestBody_JSONSchemaNameSanitized(t *testing.T) {
	schema := Schema{Type: "object"}
	body := buildRequestBody("gpt-4o-mini", Request{
		Messages:       []Msg{{Role: RoleUser, Text: "decide"}},
		JSONSchema:     &schema,
		JSONSchemaName: "triage decision!",
	})
	name := body.ResponseFormat.JSONSchema.Name
	if !jsonSchemaNameRe.MatchString(name) {
		t.Errorf("sanitized name %q does not match %s", name, jsonSchemaNameRe.String())
	}
}

// TestSchemaToWire_StrictRequiresEveryProperty is the direct unit-level guard for the validator
// blocker: under strict:true, `required` must list every key in `properties`, never the caller's
// possibly-partial Schema.Required.
func TestSchemaToWire_StrictRequiresEveryProperty(t *testing.T) {
	s := Schema{
		Type:     "object",
		Required: []string{"disposition"}, // caller only marks one required — must be ignored under strict
		Properties: map[string]Schema{
			"disposition": {Type: "string"},
			"question":    {Type: "string", Nullable: true},
			"fixBrief":    {Type: "string", Nullable: true},
		},
	}
	wire := schemaToWire(s, true)
	required, _ := wire["required"].([]string)
	if len(required) != len(s.Properties) {
		t.Fatalf("strict required = %v, want one entry per property (%d)", required, len(s.Properties))
	}
	for name := range s.Properties {
		found := false
		for _, r := range required {
			if r == name {
				found = true
			}
		}
		if !found {
			t.Errorf("strict required %v missing property %q", required, name)
		}
	}
}

// TestSchemaToWire_NonStrictKeepsCallerRequired proves tool function.parameters (non-strict) is
// unaffected by the strict-mode forcing above.
func TestSchemaToWire_NonStrictKeepsCallerRequired(t *testing.T) {
	s := Schema{
		Type:       "object",
		Required:   []string{"disposition"},
		Properties: map[string]Schema{"disposition": {Type: "string"}, "optional": {Type: "string"}},
	}
	wire := schemaToWire(s, false)
	required, _ := wire["required"].([]string)
	if len(required) != 1 || required[0] != "disposition" {
		t.Errorf("non-strict required = %v, want caller's [\"disposition\"] untouched", required)
	}
}

// TestSchemaToWire_NullableEnumIncludesNull is the direct unit-level guard for the validator major
// finding: a nullable enum field (plan §4.2's `severity`) must admit null in BOTH `type` and
// `enum`, or the intersection under strict grammar-constrained decoding excludes null entirely.
func TestSchemaToWire_NullableEnumIncludesNull(t *testing.T) {
	s := Schema{Type: "string", Nullable: true, Enum: []string{"low", "medium", "high", "critical"}}
	wire := schemaToWire(s, true)
	enum, ok := wire["enum"].([]any)
	if !ok {
		t.Fatalf("enum = %#v (%T), want []any including nil", wire["enum"], wire["enum"])
	}
	if len(enum) != 5 || enum[4] != nil {
		t.Errorf("enum = %#v, want [low medium high critical <nil>]", enum)
	}
}

// TestComplete_NullableEnumGolden is the wire-level golden for the §4.2-shaped `severity` field.
func TestComplete_NullableEnumGolden(t *testing.T) {
	schema := Schema{
		Type:     "object",
		Required: []string{"severity"},
		Properties: map[string]Schema{
			"severity": {Type: "string", Nullable: true, Enum: []string{"low", "medium", "high", "critical"}},
		},
	}
	http := &scriptedHTTP{t: t, steps: []httpStep{
		{
			wantBodyJSON: `{
				"model": "gpt-4o-mini",
				"messages": [{"role":"user","content":"decide"}],
				"response_format": {
					"type":"json_schema",
					"json_schema": {
						"name":"triage",
						"strict": true,
						"schema": {
							"type":"object",
							"required":["severity"],
							"additionalProperties": false,
							"properties":{
								"severity":{"type":["string","null"],"enum":["low","medium","high","critical",null]}
							}
						}
					}
				}
			}`,
			status: 200,
			body:   `{"choices":[{"message":{"content":"{}"}, "finish_reason":"stop"}],"usage":{}}`,
		},
	}}
	chat := newOpenAIChatForTest("https://api.example.com", "sk-test", "gpt-4o-mini", http, noSleep)
	_, err := chat.Complete(context.Background(), Request{
		Messages:       []Msg{{Role: RoleUser, Text: "decide"}},
		JSONSchema:     &schema,
		JSONSchemaName: "triage",
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
}

// --- response parsing ---------------------------------------------------------------------------

func TestComplete_ParsesUsageAndFinishReasons(t *testing.T) {
	cases := []struct {
		finishReason string
		want         string
	}{
		{"stop", StopEndTurn},
		{"tool_calls", StopToolUse},
		{"function_call", StopToolUse},
		{"length", StopMaxTokens},
		{"content_filter", StopError},
		{"something_new_and_weird", StopError},
		{"", StopEndTurn},
	}
	for _, tc := range cases {
		t.Run(tc.finishReason, func(t *testing.T) {
			http := &scriptedHTTP{t: t, steps: []httpStep{
				{status: 200, body: fmt.Sprintf(`{"choices":[{"message":{"content":"x"},"finish_reason":%q}],"usage":{"prompt_tokens":1,"completion_tokens":2}}`, tc.finishReason)},
			}}
			chat := newOpenAIChatForTest("https://api.example.com", "k", "m", http, noSleep)
			resp, err := chat.Complete(context.Background(), Request{Messages: []Msg{{Role: RoleUser, Text: "hi"}}})
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if resp.StopReason != tc.want {
				t.Errorf("finish_reason %q -> StopReason = %q, want %q", tc.finishReason, resp.StopReason, tc.want)
			}
			if resp.Usage.InputTokens != 1 || resp.Usage.OutputTokens != 2 {
				t.Errorf("Usage = %+v", resp.Usage)
			}
		})
	}
}

// --- schema-ignored fallback path ------------------------------------------------------------

func TestComplete_SchemaIgnoredByBackend_ReturnsTextForToolloopToValidate(t *testing.T) {
	// Some local models ignore response_format entirely and just answer in prose. The adapter
	// must NOT error or try to enforce the schema itself — plan §4.1: "the toolloop's
	// validate-and-re-ask is the enforcement". Complete simply returns whatever text came back.
	schema := Schema{Type: "object", Required: []string{"disposition"}}
	http := &scriptedHTTP{t: t, steps: []httpStep{
		{status: 200, body: `{"choices":[{"message":{"content":"Sure! I think this is fixable."},"finish_reason":"stop"}],"usage":{}}`},
	}}
	chat := newOpenAIChatForTest("https://api.example.com", "k", "m", http, noSleep)

	resp, err := chat.Complete(context.Background(), Request{
		Messages:   []Msg{{Role: RoleUser, Text: "decide"}},
		JSONSchema: &schema,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "Sure! I think this is fixable." {
		t.Errorf("Text = %q, adapter must pass through non-conforming text unchanged", resp.Text)
	}
}

// --- error mapping table -----------------------------------------------------------------------

func TestComplete_ErrorMapping(t *testing.T) {
	t.Run("5xx is transient", func(t *testing.T) {
		http := &scriptedHTTP{t: t, steps: []httpStep{{status: 503, body: "upstream down"}}}
		chat := newOpenAIChatForTest("https://api.example.com", "k", "m", http, noSleep)
		_, err := chat.Complete(context.Background(), Request{Messages: []Msg{{Role: RoleUser, Text: "hi"}}})
		var transient *TransientError
		if !errors.As(err, &transient) {
			t.Fatalf("want *TransientError, got %v (%T)", err, err)
		}
	})

	t.Run("4xx is permanent with body excerpt", func(t *testing.T) {
		http := &scriptedHTTP{t: t, steps: []httpStep{{status: 400, body: `{"error":"bad request: missing field foo"}`}}}
		chat := newOpenAIChatForTest("https://api.example.com", "k", "m", http, noSleep)
		_, err := chat.Complete(context.Background(), Request{Messages: []Msg{{Role: RoleUser, Text: "hi"}}})
		var perm *PermanentError
		if !errors.As(err, &perm) {
			t.Fatalf("want *PermanentError, got %v (%T)", err, err)
		}
		if !strings.Contains(perm.Reason, "missing field foo") {
			t.Errorf("Reason should carry the body excerpt, got %q", perm.Reason)
		}
	})

	t.Run("401 is permanent", func(t *testing.T) {
		http := &scriptedHTTP{t: t, steps: []httpStep{{status: 401, body: `{"error":"invalid api key"}`}}}
		chat := newOpenAIChatForTest("https://api.example.com", "k", "m", http, noSleep)
		_, err := chat.Complete(context.Background(), Request{Messages: []Msg{{Role: RoleUser, Text: "hi"}}})
		var perm *PermanentError
		if !errors.As(err, &perm) {
			t.Fatalf("want *PermanentError, got %v (%T)", err, err)
		}
	})

	t.Run("body excerpt is capped", func(t *testing.T) {
		big := strings.Repeat("x", bodyExcerptCap+500)
		http := &scriptedHTTP{t: t, steps: []httpStep{{status: 422, body: big}}}
		chat := newOpenAIChatForTest("https://api.example.com", "k", "m", http, noSleep)
		_, err := chat.Complete(context.Background(), Request{Messages: []Msg{{Role: RoleUser, Text: "hi"}}})
		var perm *PermanentError
		if !errors.As(err, &perm) {
			t.Fatalf("want *PermanentError, got %v (%T)", err, err)
		}
		if len(perm.Reason) > bodyExcerptCap+200 {
			t.Errorf("Reason not capped: %d bytes", len(perm.Reason))
		}
	})

	t.Run("malformed JSON retried once then permanent", func(t *testing.T) {
		http := &scriptedHTTP{t: t, steps: []httpStep{
			{status: 200, body: `not json at all`},
			{status: 200, body: `also not json`},
		}}
		chat := newOpenAIChatForTest("https://api.example.com", "k", "m", http, noSleep)
		_, err := chat.Complete(context.Background(), Request{Messages: []Msg{{Role: RoleUser, Text: "hi"}}})
		var perm *PermanentError
		if !errors.As(err, &perm) {
			t.Fatalf("want *PermanentError after retry, got %v (%T)", err, err)
		}
		if http.calls != 2 {
			t.Fatalf("expected exactly 2 HTTP calls (1 retry), got %d", http.calls)
		}
	})

	t.Run("malformed JSON succeeds on retry", func(t *testing.T) {
		http := &scriptedHTTP{t: t, steps: []httpStep{
			{status: 200, body: `not json at all`},
			{status: 200, body: `{"choices":[{"message":{"content":"recovered"},"finish_reason":"stop"}],"usage":{}}`},
		}}
		chat := newOpenAIChatForTest("https://api.example.com", "k", "m", http, noSleep)
		resp, err := chat.Complete(context.Background(), Request{Messages: []Msg{{Role: RoleUser, Text: "hi"}}})
		if err != nil {
			t.Fatalf("Complete: %v", err)
		}
		if resp.Text != "recovered" {
			t.Errorf("Text = %q", resp.Text)
		}
		if http.calls != 2 {
			t.Fatalf("expected exactly 2 HTTP calls, got %d", http.calls)
		}
	})

	t.Run("transport failure is transient", func(t *testing.T) {
		client := errHTTPClient{err: errors.New("dial tcp: connection refused")}
		chat := newOpenAIChatForTest("https://api.example.com", "k", "m", client, noSleep)
		_, err := chat.Complete(context.Background(), Request{Messages: []Msg{{Role: RoleUser, Text: "hi"}}})
		var transient *TransientError
		if !errors.As(err, &transient) {
			t.Fatalf("want *TransientError, got %v (%T)", err, err)
		}
	})

	t.Run("cancelled context is neither transient nor permanent", func(t *testing.T) {
		client := errHTTPClient{err: errors.New("context canceled")}
		chat := newOpenAIChatForTest("https://api.example.com", "k", "m", client, noSleep)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := chat.Complete(ctx, Request{Messages: []Msg{{Role: RoleUser, Text: "hi"}}})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("want errors.Is(err, context.Canceled), got %v (%T)", err, err)
		}
		var transient *TransientError
		var perm *PermanentError
		if errors.As(err, &transient) || errors.As(err, &perm) {
			t.Fatalf("cancelled context must not be classified as *TransientError/*PermanentError, got %T", err)
		}
	})

	t.Run("3xx is transient", func(t *testing.T) {
		http := &scriptedHTTP{t: t, steps: []httpStep{{status: 302, body: "found"}}}
		chat := newOpenAIChatForTest("https://api.example.com", "k", "m", http, noSleep)
		_, err := chat.Complete(context.Background(), Request{Messages: []Msg{{Role: RoleUser, Text: "hi"}}})
		var transient *TransientError
		if !errors.As(err, &transient) {
			t.Fatalf("want *TransientError, got %v (%T)", err, err)
		}
	})

	t.Run("empty choices does not panic and is permanent after retry", func(t *testing.T) {
		http := &scriptedHTTP{t: t, steps: []httpStep{
			{status: 200, body: `{"choices":[],"usage":{}}`},
			{status: 200, body: `{"choices":[],"usage":{}}`},
		}}
		chat := newOpenAIChatForTest("https://api.example.com", "k", "m", http, noSleep)
		_, err := chat.Complete(context.Background(), Request{Messages: []Msg{{Role: RoleUser, Text: "hi"}}})
		var perm *PermanentError
		if !errors.As(err, &perm) {
			t.Fatalf("want *PermanentError, got %v (%T)", err, err)
		}
		if http.calls != 2 {
			t.Fatalf("expected exactly 2 HTTP calls (1 retry), got %d", http.calls)
		}
	})
}

// errHTTPClient is an openaiHTTPClient whose Do always fails with a fixed error, for exercising
// doOnce's transport-failure classification branch without a real network call.
type errHTTPClient struct{ err error }

func (c errHTTPClient) Do(req *http.Request) (*http.Response, error) { return nil, c.err }

func TestNewOpenAIChat_ClientHasTimeout(t *testing.T) {
	chat, err := NewOpenAIChat(Config{Model: "gpt-4o-mini", APIKey: "sk-test"})
	if err != nil {
		t.Fatalf("NewOpenAIChat: %v", err)
	}
	oc, ok := chat.(*openaiChat)
	if !ok {
		t.Fatalf("NewOpenAIChat returned %T, want *openaiChat", chat)
	}
	hc, ok := oc.client.(*http.Client)
	if !ok {
		t.Fatalf("client is %T, want *http.Client", oc.client)
	}
	if hc.Timeout <= 0 {
		t.Fatalf("client.Timeout = %v, want a positive bound", hc.Timeout)
	}
}

func TestComplete_RetryAfterHonored(t *testing.T) {
	var slept time.Duration
	var sleptCalled bool
	sleep := func(_ context.Context, d time.Duration) {
		slept = d
		sleptCalled = true
	}
	h := http.Header{"Retry-After": []string{"7"}}
	scripted := &scriptedHTTP{t: t, steps: []httpStep{
		{status: 429, body: "slow down", header: h},
	}}
	chat := newOpenAIChatForTest("https://api.example.com", "k", "m", scripted, sleep)

	_, err := chat.Complete(context.Background(), Request{Messages: []Msg{{Role: RoleUser, Text: "hi"}}})
	var transient *TransientError
	if !errors.As(err, &transient) {
		t.Fatalf("want *TransientError for 429, got %v (%T)", err, err)
	}
	if !sleptCalled {
		t.Fatal("expected sleep to be invoked for Retry-After")
	}
	if slept != 7*time.Second {
		t.Errorf("slept %v, want 7s", slept)
	}
}

func TestComplete_RetryAfterDefaultWhenHeaderAbsent(t *testing.T) {
	var slept time.Duration
	sleep := func(_ context.Context, d time.Duration) { slept = d }
	scripted := &scriptedHTTP{t: t, steps: []httpStep{{status: 429, body: "slow down"}}}
	chat := newOpenAIChatForTest("https://api.example.com", "k", "m", scripted, sleep)

	if _, err := chat.Complete(context.Background(), Request{Messages: []Msg{{Role: RoleUser, Text: "hi"}}}); err == nil {
		t.Fatal("expected an error for 429")
	}
	if slept != 60*time.Second {
		t.Errorf("slept %v, want default 60s", slept)
	}
}

// --- registry wiring --------------------------------------------------------------------------

func TestNewOpenAIChat_RegisteredUnderOpenAIName(t *testing.T) {
	chat, err := New("openai", Config{Model: "gpt-4o-mini", APIKey: "sk-test"})
	if err != nil {
		t.Fatalf("New(\"openai\", ...): %v", err)
	}
	if _, ok := chat.(*openaiChat); !ok {
		t.Fatalf("New(\"openai\", ...) returned %T, want *openaiChat", chat)
	}
}

func TestNewOpenAIChat_RequiresModel(t *testing.T) {
	if _, err := NewOpenAIChat(Config{}); err == nil {
		t.Fatal("expected an error when Model is empty")
	}
}

func TestNewOpenAIChat_DefaultsBaseURL(t *testing.T) {
	chat, err := NewOpenAIChat(Config{Model: "gpt-4o-mini"})
	if err != nil {
		t.Fatalf("NewOpenAIChat: %v", err)
	}
	oc := chat.(*openaiChat)
	if oc.baseURL != "https://api.openai.com/v1" {
		t.Errorf("baseURL = %q", oc.baseURL)
	}
	if oc.completionsURL != "https://api.openai.com/v1/chat/completions" {
		t.Errorf("completionsURL = %q", oc.completionsURL)
	}
}

// httptest network smoke test: confirms the adapter really speaks HTTP against a listening
// server (all other tests use the injected openaiHTTPClient fake, per plan §8: "httptest fake
// providers only — no network").
func TestComplete_AgainstHTTPTestServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"served"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer srv.Close()

	chat, err := NewOpenAIChat(Config{Model: "gpt-4o-mini", APIKey: "sk-test", BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("NewOpenAIChat: %v", err)
	}
	resp, err := chat.Complete(context.Background(), Request{Messages: []Msg{{Role: RoleUser, Text: "hi"}}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "served" {
		t.Errorf("Text = %q", resp.Text)
	}
}
