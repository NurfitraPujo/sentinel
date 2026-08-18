package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestAnthropicChat(t *testing.T, handler http.HandlerFunc) *anthropicChat {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := newAnthropicChat(Config{Model: "claude-test", APIKey: "test-key", BaseURL: srv.URL})
	// Plan §8: "injected clocks, no real sleeps" — the 429 path sleeps via c.sleep; a no-op here
	// keeps every test in this file instant regardless of which status path it exercises.
	c.sleep = func(context.Context, time.Duration) {}
	return c
}

func decodeAnthropicRequest(t *testing.T, r *http.Request) anthropicRequest {
	t.Helper()
	var body anthropicRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	return body
}

// --- URL resolution goldens --------------------------------------------------------------------

// TestResolveAnthropicURL_Table pins the LLM_BASE_URL convention against the divergence found in
// review: unlike openai.go's resolveOpenAIURLs, this adapter used to append "/v1/messages" to the
// RAW base with no normalization, so a base already ending in "/v1" (the documented-elsewhere
// convention, or a value copied from an OpenAI-style config) produced
// ".../v1/v1/messages"-style 404s classified Permanent.
func TestResolveAnthropicURL_Table(t *testing.T) {
	cases := []struct {
		name            string
		base            string
		wantMessagesURL string
	}{
		{"bare host", "https://api.anthropic.com", "https://api.anthropic.com/v1/messages"},
		{"bare host trailing slash", "https://api.anthropic.com/", "https://api.anthropic.com/v1/messages"},
		{"host with /v1", "https://api.anthropic.com/v1", "https://api.anthropic.com/v1/messages"},
		{"host with /v1 trailing slash", "https://api.anthropic.com/v1/", "https://api.anthropic.com/v1/messages"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, gotMessagesURL := resolveAnthropicURL(tc.base)
			if gotMessagesURL != tc.wantMessagesURL {
				t.Errorf("resolveAnthropicURL(%q) messagesURL = %q, want %q", tc.base, gotMessagesURL, tc.wantMessagesURL)
			}
		})
	}
}

// TestAnthropicComplete_PostsToResolvedURL proves the resolved URL is what Complete actually POSTs
// to, end to end through an httptest server, not just unit-testing resolveAnthropicURL in isolation.
func TestAnthropicComplete_PostsToResolvedURL(t *testing.T) {
	cases := []struct {
		name     string
		baseSlug string // appended to the httptest server's own base URL to simulate a caller-supplied suffix
		wantPath string
	}{
		{"bare host", "", "/v1/messages"},
		{"trailing slash", "/", "/v1/messages"},
		{"host with /v1", "/v1", "/v1/messages"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(anthropicResponse{
					Content:    []anthropicContentBlock{{Type: "text", Text: "ok"}},
					StopReason: "end_turn",
				})
			}))
			defer srv.Close()

			chat := newAnthropicChat(Config{Model: "claude-test", APIKey: "k", BaseURL: srv.URL + tc.baseSlug})
			chat.sleep = func(context.Context, time.Duration) {}
			if _, err := chat.Complete(context.Background(), Request{Messages: []Msg{{Role: RoleUser, Text: "hi"}}}); err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if gotPath != tc.wantPath {
				t.Errorf("POSTed path = %q, want %q", gotPath, tc.wantPath)
			}
		})
	}
}

// --- request-shape goldens ----------------------------------------------------------------------
//
// These assert the RAW JSON body against a literal expected JSON string (via assertJSONEqual,
// defined in openai_test.go), NOT by decoding the captured body back through anthropicRequest —
// decoding through the adapter's own wire structs makes a wrong json tag invisible, since encoding
// and decoding both use the same (possibly wrong) tag. Proven: mutating input_schema's json tag to
// "inputSchema" left the old struct-decode assertions green; these literal-JSON goldens catch it.

func TestAnthropicComplete_RequestShape_Completion(t *testing.T) {
	var gotPath, gotAPIKey, gotVersion string
	var gotRawBody []byte
	chat := newTestAnthropicChat(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAPIKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		var err error
		gotRawBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("reading request body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(anthropicResponse{
			Content:    []anthropicContentBlock{{Type: "text", Text: "hello"}},
			StopReason: "end_turn",
			Usage: struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			}{InputTokens: 3, OutputTokens: 2},
		})
	})

	resp, err := chat.Complete(context.Background(), Request{
		System:    "you are a triage bot",
		Messages:  []Msg{{Role: RoleUser, Text: "hi"}},
		MaxTokens: 512,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if gotPath != "/v1/messages" {
		t.Fatalf("path = %q, want /v1/messages", gotPath)
	}
	if gotAPIKey != "test-key" {
		t.Fatalf("x-api-key = %q, want test-key", gotAPIKey)
	}
	if gotVersion != anthropicVersion {
		t.Fatalf("anthropic-version = %q, want %q", gotVersion, anthropicVersion)
	}
	assertJSONEqual(t, `{
		"model": "claude-test",
		"max_tokens": 512,
		"system": "you are a triage bot",
		"messages": [
			{"role":"user","content":[{"type":"text","text":"hi"}]}
		]
	}`, string(gotRawBody))

	if resp.Text != "hello" || resp.StopReason != StopEndTurn {
		t.Fatalf("resp = %+v, want text=hello stop=end_turn", resp)
	}
	if resp.Usage != (Usage{InputTokens: 3, OutputTokens: 2}) {
		t.Fatalf("usage = %+v", resp.Usage)
	}
}

// TestAnthropicComplete_RequestShape_MaxTokensDefaulted guards against emitting "max_tokens":0 when
// a caller leaves Request.MaxTokens unset: Anthropic's max_tokens is required and rejects values
// below 1 with a hard 400, so a zero must never reach the wire.
func TestAnthropicComplete_RequestShape_MaxTokensDefaulted(t *testing.T) {
	var gotBody anthropicRequest
	chat := newTestAnthropicChat(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody = decodeAnthropicRequest(t, r)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(anthropicResponse{
			Content:    []anthropicContentBlock{{Type: "text", Text: "hello"}},
			StopReason: "end_turn",
		})
	})

	if _, err := chat.Complete(context.Background(), Request{Messages: []Msg{{Role: RoleUser, Text: "hi"}}}); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if gotBody.MaxTokens < 1 {
		t.Fatalf("max_tokens = %d, want a positive default when Request.MaxTokens is unset", gotBody.MaxTokens)
	}
	if gotBody.MaxTokens != anthropicDefaultMaxTokens {
		t.Fatalf("max_tokens = %d, want anthropicDefaultMaxTokens (%d)", gotBody.MaxTokens, anthropicDefaultMaxTokens)
	}
}

func TestAnthropicComplete_RequestShape_ToolRoundTrip(t *testing.T) {
	var gotRawBody []byte
	chat := newTestAnthropicChat(t, func(w http.ResponseWriter, r *http.Request) {
		var err error
		gotRawBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("reading request body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(anthropicResponse{
			Content: []anthropicContentBlock{
				{Type: "tool_use", ID: "call1", Name: "get_issue", Input: json.RawMessage(`{"id":"123"}`)},
			},
			StopReason: "tool_use",
		})
	})

	tools := []ToolDef{{Name: "get_issue", Description: "fetch an issue", Params: Schema{Type: "object"}}}
	msgs := []Msg{
		{Role: RoleUser, Text: "triage this"},
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "prev1", Name: "get_issue", Arguments: `{"id":"1"}`}}},
		{Role: RoleTool, ToolResults: []ToolResult{{ToolCallID: "prev1", Content: "issue body"}}},
	}

	resp, err := chat.Complete(context.Background(), Request{Messages: msgs, Tools: tools, MaxTokens: 100})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	assertJSONEqual(t, `{
		"model": "claude-test",
		"max_tokens": 100,
		"messages": [
			{"role":"user","content":[{"type":"text","text":"triage this"}]},
			{"role":"assistant","content":[{"type":"tool_use","id":"prev1","name":"get_issue","input":{"id":"1"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"prev1","content":"issue body"}]}
		],
		"tools": [
			{"name":"get_issue","description":"fetch an issue","input_schema":{"type":"object"}}
		]
	}`, string(gotRawBody))

	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ID != "call1" || resp.ToolCalls[0].Name != "get_issue" ||
		resp.ToolCalls[0].Arguments != `{"id":"123"}` {
		t.Fatalf("resp.ToolCalls = %+v", resp.ToolCalls)
	}
	if resp.StopReason != StopToolUse {
		t.Fatalf("StopReason = %q, want tool_use", resp.StopReason)
	}
}

func TestAnthropicComplete_RequestShape_StructuredDecision(t *testing.T) {
	schema := Schema{
		Type:     "object",
		Required: []string{"disposition"},
		Properties: map[string]Schema{
			"disposition": {Type: "string", Enum: []string{"comment_only", "fixable"}},
			// Nullable, per plan §4.2 fields like severity/duplicateOf/causedBy/question/fixBrief —
			// toAnthropicSchema must map this onto standard-JSON-Schema nullability, not drop it.
			"severity": {Type: "string", Enum: []string{"low", "high"}, Nullable: true},
		},
	}

	// schemaJSON is the literal input_schema shared by both decision-tool entries below.
	const schemaJSON = `{
		"type": "object",
		"properties": {
			"disposition": {"type":"string","enum":["comment_only","fixable"]},
			"severity": {"type":["string","null"],"enum":["low","high",null]}
		},
		"required": ["disposition"]
	}`

	t.Run("no other tools forces the decision tool", func(t *testing.T) {
		var gotRawBody []byte
		chat := newTestAnthropicChat(t, func(w http.ResponseWriter, r *http.Request) {
			var err error
			gotRawBody, err = io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("reading request body: %v", err)
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(anthropicResponse{
				Content: []anthropicContentBlock{
					{Type: "tool_use", ID: "d1", Name: "submit_decision", Input: json.RawMessage(`{"disposition":"comment_only"}`)},
				},
				StopReason: "tool_use",
			})
		})

		resp, err := chat.Complete(context.Background(), Request{
			Messages:   []Msg{{Role: RoleUser, Text: "decide"}},
			MaxTokens:  100,
			JSONSchema: &schema,
		})
		if err != nil {
			t.Fatalf("Complete: %v", err)
		}

		assertJSONEqual(t, `{
			"model": "claude-test",
			"max_tokens": 100,
			"messages": [
				{"role":"user","content":[{"type":"text","text":"decide"}]}
			],
			"tools": [
				{
					"name": "submit_decision",
					"description": "Submit the final structured decision for this turn.",
					"input_schema": `+schemaJSON+`
				}
			],
			"tool_choice": {"type":"tool","name":"submit_decision"}
		}`, string(gotRawBody))

		// The forced tool_use must be unwrapped into Text, not surfaced as a ToolCall, so
		// toolloop.go's "no ToolCalls => finalize" branch fires and validates it against the schema.
		if len(resp.ToolCalls) != 0 {
			t.Fatalf("ToolCalls = %+v, want none (decision unwrapped into Text)", resp.ToolCalls)
		}
		if resp.Text != `{"disposition":"comment_only"}` {
			t.Fatalf("Text = %q", resp.Text)
		}
		if resp.StopReason != StopEndTurn {
			t.Fatalf("StopReason = %q, want end_turn", resp.StopReason)
		}
	})

	t.Run("named schema and other tools present uses auto choice", func(t *testing.T) {
		var gotRawBody []byte
		chat := newTestAnthropicChat(t, func(w http.ResponseWriter, r *http.Request) {
			var err error
			gotRawBody, err = io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("reading request body: %v", err)
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(anthropicResponse{
				Content:    []anthropicContentBlock{{Type: "tool_use", ID: "c1", Name: "get_issue", Input: json.RawMessage(`{}`)}},
				StopReason: "tool_use",
			})
		})

		_, err := chat.Complete(context.Background(), Request{
			Messages:       []Msg{{Role: RoleUser, Text: "decide"}},
			Tools:          []ToolDef{{Name: "get_issue", Params: Schema{Type: "object"}}},
			MaxTokens:      100,
			JSONSchema:     &schema,
			JSONSchemaName: "triage_decision",
		})
		if err != nil {
			t.Fatalf("Complete: %v", err)
		}

		assertJSONEqual(t, `{
			"model": "claude-test",
			"max_tokens": 100,
			"messages": [
				{"role":"user","content":[{"type":"text","text":"decide"}]}
			],
			"tools": [
				{"name":"get_issue","input_schema":{"type":"object"}},
				{
					"name": "triage_decision",
					"description": "Submit the final structured decision for this turn.",
					"input_schema": `+schemaJSON+`
				}
			],
			"tool_choice": {"type":"auto"}
		}`, string(gotRawBody))
	})
}

// --- response parsing / usage / stop mapping -----------------------------------------------------

func TestAnthropicComplete_StopReasonMapping(t *testing.T) {
	cases := []struct {
		wire string
		want string
	}{
		{"end_turn", StopEndTurn},
		{"stop_sequence", StopEndTurn},
		{"tool_use", StopToolUse},
		{"max_tokens", StopMaxTokens},
		{"something_new", StopError},
	}
	for _, tc := range cases {
		chat := newTestAnthropicChat(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(anthropicResponse{
				Content:    []anthropicContentBlock{{Type: "text", Text: "x"}},
				StopReason: tc.wire,
			})
		})
		resp, err := chat.Complete(context.Background(), Request{Messages: []Msg{{Role: RoleUser, Text: "hi"}}})
		if err != nil {
			t.Fatalf("Complete: %v", err)
		}
		if resp.StopReason != tc.want {
			t.Errorf("wire %q => %q, want %q", tc.wire, resp.StopReason, tc.want)
		}
	}
}

// --- error tables ---------------------------------------------------------------------------------

func TestAnthropicComplete_ErrorClassification(t *testing.T) {
	cases := []struct {
		status        int
		wantTransient bool
	}{
		{429, true},
		{529, true},
		{500, true},
		{503, true},
		{400, false},
		{401, false},
		{403, false},
		{404, false},
		{422, false},
	}
	for _, tc := range cases {
		chat := newTestAnthropicChat(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_ = json.NewEncoder(w).Encode(anthropicErrorEnvelope{
				Type: "error",
				Error: struct {
					Type    string `json:"type"`
					Message string `json:"message"`
				}{Type: "some_error", Message: "boom"},
			})
		})
		_, err := chat.Complete(context.Background(), Request{Messages: []Msg{{Role: RoleUser, Text: "hi"}}})
		if err == nil {
			t.Fatalf("status %d: expected error", tc.status)
		}
		var transient *TransientError
		isTransient := errors.As(err, &transient)
		if isTransient != tc.wantTransient {
			t.Errorf("status %d: transient = %v, want %v (err=%v)", tc.status, isTransient, tc.wantTransient, err)
		}
		// Exact class, not just the transient boolean: non-transient must be *PermanentError
		// (PermanentError's doc: "Callers (jobs/*) journal this as failed, not retried"), never a
		// bare error that RunLoop's dispatcher (toolloop.go) cannot classify.
		var permanent *PermanentError
		isPermanent := errors.As(err, &permanent)
		if tc.wantTransient && isPermanent {
			t.Errorf("status %d: got *PermanentError, want *TransientError only", tc.status)
		}
		if !tc.wantTransient && !isPermanent {
			t.Errorf("status %d: err = %v (%T), want *PermanentError", tc.status, err, err)
		}
	}
}

// TestAnthropicComplete_429HonorsRetryAfter proves the 429 path sleeps the header's Retry-After
// duration (via the injected sleep, no real sleep) before reporting Transient, matching openai.go.
func TestAnthropicComplete_429HonorsRetryAfter(t *testing.T) {
	chat := newTestAnthropicChat(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(anthropicErrorEnvelope{})
	})
	var gotWait time.Duration
	chat.sleep = func(_ context.Context, d time.Duration) { gotWait = d }

	_, err := chat.Complete(context.Background(), Request{Messages: []Msg{{Role: RoleUser, Text: "hi"}}})
	var transient *TransientError
	if !errors.As(err, &transient) {
		t.Fatalf("expected TransientError, got %v (%T)", err, err)
	}
	if gotWait != 7*time.Second {
		t.Fatalf("sleep duration = %v, want 7s", gotWait)
	}
}

// TestAnthropicComplete_MalformedBodyRetriesOnceThenPermanent mirrors openai.go's doOnce retry
// policy: a non-JSON 2xx body is retried once immediately, and a *PermanentError if the retry is
// malformed too.
func TestAnthropicComplete_MalformedBodyRetriesOnceThenPermanent(t *testing.T) {
	calls := 0
	chat := newTestAnthropicChat(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not json"))
	})
	_, err := chat.Complete(context.Background(), Request{Messages: []Msg{{Role: RoleUser, Text: "hi"}}})
	var permanent *PermanentError
	if !errors.As(err, &permanent) {
		t.Fatalf("expected *PermanentError, got %v (%T)", err, err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2 (one retry)", calls)
	}
	// malformedJSONError used to hardcode "llm/openai:" into Error() regardless of which adapter
	// produced it (it lived in openai.go and anthropic.go/gemini.go reused it verbatim) — every
	// malformed-2xx from this adapter misreported as an OpenAI error.
	if !strings.HasPrefix(permanent.Reason, "llm/anthropic:") {
		t.Fatalf("PermanentError.Reason = %q, want prefix %q", permanent.Reason, "llm/anthropic:")
	}
}

// TestAnthropicComplete_EmptyContentRetriesOnceThenPermanent pins the unified "no usable content"
// semantic (finding: openai's malformed-2xx-body policy is the baseline all three adapters now
// share) — a 2xx body with a zero-length content array used to succeed with an empty Response.Text
// instead of being treated as unusable. It must now retry once immediately, then *PermanentError,
// exactly like a malformed body.
func TestAnthropicComplete_EmptyContentRetriesOnceThenPermanent(t *testing.T) {
	calls := 0
	chat := newTestAnthropicChat(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(anthropicResponse{Content: []anthropicContentBlock{}})
	})
	_, err := chat.Complete(context.Background(), Request{Messages: []Msg{{Role: RoleUser, Text: "hi"}}})
	var permanent *PermanentError
	if !errors.As(err, &permanent) {
		t.Fatalf("expected *PermanentError, got %v (%T)", err, err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2 (one retry)", calls)
	}
}

// TestAnthropicComplete_CancelledContextIsWrappedContextError mirrors openai.go's ctx-cancel
// handling (finding: anthropic/gemini used to return a bare *TransientError on a cancelled ctx,
// while openai wrapped context.Canceled so errors.Is(err, context.Canceled) held).
func TestAnthropicComplete_CancelledContextIsWrappedContextError(t *testing.T) {
	chat := newTestAnthropicChat(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(anthropicResponse{Content: []anthropicContentBlock{{Type: "text", Text: "ok"}}})
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := chat.Complete(ctx, Request{Messages: []Msg{{Role: RoleUser, Text: "hi"}}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want errors.Is(err, context.Canceled), got %v (%T)", err, err)
	}
	var transient *TransientError
	if errors.As(err, &transient) {
		t.Fatalf("ctx-cancel must not be classified *TransientError, got %v", err)
	}
}

// TestAnthropicComplete_3xxIsTransient pins the shared rule (finding: openai already treated 3xx
// as Transient; anthropic/gemini fell through to Permanent) — a 3xx response is not an
// authoritative failure worth giving up on.
func TestAnthropicComplete_3xxIsTransient(t *testing.T) {
	chat := newTestAnthropicChat(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMovedPermanently)
		_, _ = w.Write([]byte(`{}`))
	})
	_, err := chat.Complete(context.Background(), Request{Messages: []Msg{{Role: RoleUser, Text: "hi"}}})
	var transient *TransientError
	if !errors.As(err, &transient) {
		t.Fatalf("want *TransientError for 3xx, got %v (%T)", err, err)
	}
}

// TestAnthropicComplete_JSONSchemaNameNormalized pins the hoisted normalizer (finding: only
// openai.go sanitized Request.JSONSchemaName; anthropic passed it RAW as a wire tool name, so a
// spaced/unicode name that worked against openai 400'd here). A conforming wire tool name must be
// emitted regardless.
func TestAnthropicComplete_JSONSchemaNameNormalized(t *testing.T) {
	var gotBody anthropicRequest
	chat := newTestAnthropicChat(t, func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(anthropicResponse{Content: []anthropicContentBlock{{Type: "text", Text: "ok"}}})
	})
	schema := Schema{Type: "object"}
	req := Request{
		Messages:       []Msg{{Role: RoleUser, Text: "hi"}},
		JSONSchema:     &schema,
		JSONSchemaName: "décision spatiale!",
	}
	if _, err := chat.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(gotBody.Tools) != 1 {
		t.Fatalf("Tools = %+v, want exactly one decision tool", gotBody.Tools)
	}
	if !jsonSchemaNameRe.MatchString(gotBody.Tools[0].Name) {
		t.Fatalf("wire tool name %q does not conform to %s", gotBody.Tools[0].Name, jsonSchemaNameRe.String())
	}
}

// TestAnthropicToAnthropicMessages_SkipsEmptyMessage guards against wiring `"content":[]`, which
// Anthropic rejects with a hard 400 — reachable via toolloop.go's finalizeDecision appending
// Msg{Role: RoleAssistant, Text: ""} after a safety/MAX_TOKENS turn.
func TestAnthropicToAnthropicMessages_SkipsEmptyMessage(t *testing.T) {
	out := toAnthropicMessages([]Msg{
		{Role: RoleUser, Text: "hi"},
		{Role: RoleAssistant},
		{Role: RoleTool},
	})
	for _, m := range out {
		if len(m.Content) == 0 {
			t.Fatalf("emitted a zero-content message: %+v", out)
		}
	}
	if len(out) != 1 {
		t.Fatalf("out = %+v, want only the non-empty user message", out)
	}
}

// TestAnthropicComplete_DecisionWithProseText_TextPartBeforeDecision covers a text block emitted
// before the decision tool_use block in the same response (extended-thinking / narration is
// normal). The `=` assignment in fromAnthropicResponse's tool_use branch already wins over prose
// accumulated earlier, but assert it explicitly against the loop's own validator.
func TestAnthropicComplete_DecisionWithProseText_TextPartBeforeDecision(t *testing.T) {
	schema := Schema{Type: "object", Required: []string{"disposition"}, Properties: map[string]Schema{
		"disposition": {Type: "string", Enum: []string{"comment_only", "fixable"}},
	}}
	chat := newTestAnthropicChat(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(anthropicResponse{
			Content: []anthropicContentBlock{
				{Type: "text", Text: "Sure, here is my decision."},
				{Type: "tool_use", ID: "d1", Name: "submit_decision", Input: json.RawMessage(`{"disposition":"comment_only"}`)},
			},
			StopReason: "tool_use",
		})
	})
	resp, err := chat.Complete(context.Background(), Request{
		Messages:   []Msg{{Role: RoleUser, Text: "decide"}},
		MaxTokens:  100,
		JSONSchema: &schema,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != `{"disposition":"comment_only"}` {
		t.Fatalf("Text = %q, want raw decision JSON only", resp.Text)
	}
	if err := validateAgainstSchema(resp.Text, schema); err != nil {
		t.Fatalf("validateAgainstSchema: %v", err)
	}
}

// TestAnthropicComplete_DecisionWithProseText_TextPartAfterDecision covers a trailing text block
// emitted AFTER the decision tool_use block — the bug this regresses: resp.Text used to
// accumulate ("+=") the trailing prose onto the already-assigned decision JSON.
func TestAnthropicComplete_DecisionWithProseText_TextPartAfterDecision(t *testing.T) {
	schema := Schema{Type: "object", Required: []string{"disposition"}, Properties: map[string]Schema{
		"disposition": {Type: "string", Enum: []string{"comment_only", "fixable"}},
	}}
	chat := newTestAnthropicChat(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(anthropicResponse{
			Content: []anthropicContentBlock{
				{Type: "tool_use", ID: "d1", Name: "submit_decision", Input: json.RawMessage(`{"disposition":"comment_only"}`)},
				{Type: "text", Text: "Done."},
			},
			StopReason: "tool_use",
		})
	})
	resp, err := chat.Complete(context.Background(), Request{
		Messages:   []Msg{{Role: RoleUser, Text: "decide"}},
		MaxTokens:  100,
		JSONSchema: &schema,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != `{"disposition":"comment_only"}` {
		t.Fatalf("Text = %q, want raw decision JSON only", resp.Text)
	}
	if err := validateAgainstSchema(resp.Text, schema); err != nil {
		t.Fatalf("validateAgainstSchema: %v", err)
	}
}

func TestNewAnthropicChat_RequiresModel(t *testing.T) {
	if _, err := New("anthropic", Config{APIKey: "k"}); err == nil {
		t.Fatal("expected error for empty Config.Model")
	}
}

// TestAnthropicToMessages_MalformedToolCallArguments ensures a malformed ToolCall.Arguments
// string does not hard-fail request encoding — it must fall back to "{}" rather than making
// json.Marshal fail with a *PermanentError, which would poison every RunLoop fallback turn after
// openai's circuit opens.
func TestAnthropicToMessages_MalformedToolCallArguments(t *testing.T) {
	msgs := toAnthropicMessages([]Msg{
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "1", Name: "get_issue", Arguments: `{"id": }`}}},
	})
	b, err := json.Marshal(msgs)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !json.Valid(b) {
		t.Fatalf("marshaled messages not valid JSON: %s", b)
	}
}

func TestAnthropicComplete_NetworkErrorIsTransient(t *testing.T) {
	// Point at a server that refuses connections, so http.Client.Do itself fails.
	c := newAnthropicChat(Config{Model: "m", APIKey: "k", BaseURL: "http://127.0.0.1:1"})
	_, err := c.Complete(context.Background(), Request{Messages: []Msg{{Role: RoleUser, Text: "hi"}}})
	if err == nil {
		t.Fatal("expected error")
	}
	var transient *TransientError
	if !errors.As(err, &transient) {
		t.Fatalf("expected TransientError, got %v (%T)", err, err)
	}
}
