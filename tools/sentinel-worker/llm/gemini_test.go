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

func newTestGeminiChat(t *testing.T, handler http.HandlerFunc) *geminiChat {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := newGeminiChat(Config{Model: "gemini-test", APIKey: "test-key", BaseURL: srv.URL})
	// Plan §8: "injected clocks, no real sleeps" — the 429 path sleeps via c.sleep; a no-op here
	// keeps every test in this file instant regardless of which status path it exercises.
	c.sleep = func(context.Context, time.Duration) {}
	return c
}

func decodeGeminiRequest(t *testing.T, r *http.Request) geminiRequest {
	t.Helper()
	var body geminiRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	return body
}

// --- URL resolution goldens --------------------------------------------------------------------

// TestResolveGeminiBaseURL_Table pins the LLM_BASE_URL convention against the divergence found in
// review: unlike openai.go's resolveOpenAIURLs, this adapter used to append
// "/v1beta/models/{model}:generateContent" to the RAW base with no normalization, so a base already
// ending in "/v1beta" (the documented-elsewhere convention) produced
// ".../v1beta/v1beta/models/..."-style 404s classified Permanent.
func TestResolveGeminiBaseURL_Table(t *testing.T) {
	cases := []struct {
		name     string
		base     string
		wantBase string
	}{
		{"bare host", "https://generativelanguage.googleapis.com", "https://generativelanguage.googleapis.com"},
		{"bare host trailing slash", "https://generativelanguage.googleapis.com/", "https://generativelanguage.googleapis.com"},
		{"host with /v1beta", "https://generativelanguage.googleapis.com/v1beta", "https://generativelanguage.googleapis.com"},
		{"host with /v1beta trailing slash", "https://generativelanguage.googleapis.com/v1beta/", "https://generativelanguage.googleapis.com"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveGeminiBaseURL(tc.base)
			if got != tc.wantBase {
				t.Errorf("resolveGeminiBaseURL(%q) = %q, want %q", tc.base, got, tc.wantBase)
			}
		})
	}
}

// TestGeminiComplete_PostsToResolvedURL proves the resolved URL is what Complete actually POSTs to,
// end to end through an httptest server, not just unit-testing resolveGeminiBaseURL in isolation.
func TestGeminiComplete_PostsToResolvedURL(t *testing.T) {
	cases := []struct {
		name     string
		baseSlug string // appended to the httptest server's own base URL to simulate a caller-supplied suffix
		wantPath string
	}{
		{"bare host", "", "/v1beta/models/gemini-test:generateContent"},
		{"trailing slash", "/", "/v1beta/models/gemini-test:generateContent"},
		{"host with /v1beta", "/v1beta", "/v1beta/models/gemini-test:generateContent"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(geminiResponse{
					Candidates: []geminiCandidate{{
						Content:      geminiContent{Role: "model", Parts: []geminiPart{{Text: "ok"}}},
						FinishReason: "STOP",
					}},
				})
			}))
			defer srv.Close()

			chat := newGeminiChat(Config{Model: "gemini-test", APIKey: "k", BaseURL: srv.URL + tc.baseSlug})
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
// defined in openai_test.go), NOT by decoding the captured body back through geminiRequest —
// decoding through the adapter's own wire structs makes a wrong json tag invisible, since encoding
// and decoding both use the same (possibly wrong) tag. Proven: mutating functionDeclarations' json
// tag to "function_declarations" left the old struct-decode assertions green; these literal-JSON
// goldens catch it.

func TestGeminiComplete_RequestShape_Completion(t *testing.T) {
	var gotPath, gotAPIKey string
	var gotRawBody []byte
	chat := newTestGeminiChat(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAPIKey = r.Header.Get("x-goog-api-key")
		var err error
		gotRawBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("reading request body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(geminiResponse{
			Candidates: []geminiCandidate{{
				Content:      geminiContent{Role: "model", Parts: []geminiPart{{Text: "hello"}}},
				FinishReason: "STOP",
			}},
			UsageMetadata: geminiUsageMetadata{PromptTokenCount: 4, CandidatesTokenCount: 2},
		})
	})

	resp, err := chat.Complete(context.Background(), Request{
		System:    "you are a triage bot",
		Messages:  []Msg{{Role: RoleUser, Text: "hi"}},
		MaxTokens: 256,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if gotPath != "/v1beta/models/gemini-test:generateContent" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotAPIKey != "test-key" {
		t.Fatalf("x-goog-api-key = %q, want test-key", gotAPIKey)
	}
	assertJSONEqual(t, `{
		"contents": [
			{"role":"user","parts":[{"text":"hi"}]}
		],
		"generationConfig": {"maxOutputTokens":256},
		"systemInstruction": {"parts":[{"text":"you are a triage bot"}]}
	}`, string(gotRawBody))

	if resp.Text != "hello" || resp.StopReason != StopEndTurn {
		t.Fatalf("resp = %+v", resp)
	}
	if resp.Usage != (Usage{InputTokens: 4, OutputTokens: 2}) {
		t.Fatalf("usage = %+v", resp.Usage)
	}
}

func TestGeminiComplete_RequestShape_ToolRoundTrip(t *testing.T) {
	var gotRawBody []byte
	chat := newTestGeminiChat(t, func(w http.ResponseWriter, r *http.Request) {
		var err error
		gotRawBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("reading request body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(geminiResponse{
			Candidates: []geminiCandidate{{
				Content: geminiContent{Role: "model", Parts: []geminiPart{
					{FunctionCall: &geminiFunctionCall{Name: "get_issue", Args: json.RawMessage(`{"id":"1"}`)}},
				}},
				FinishReason: "STOP",
			}},
		})
	})

	tools := []ToolDef{{Name: "get_issue", Description: "fetch an issue", Params: Schema{Type: "object"}}}
	msgs := []Msg{
		{Role: RoleUser, Text: "triage this"},
		// ID mirrors the synthesized shape fromGeminiResponse actually produces ("<name>-<ordinal>"),
		// not an arbitrary opaque string — the wire regression this test guards against (Gemini
		// functionResponse.name must echo the declared function name, not this correlation ID) only
		// shows up with a realistic ID.
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "get_issue-0", Name: "get_issue", Arguments: `{"id":"1"}`}}},
		{Role: RoleTool, ToolResults: []ToolResult{{ToolCallID: "get_issue-0", Content: "issue body"}}},
	}

	resp, err := chat.Complete(context.Background(), Request{Messages: msgs, Tools: tools, MaxTokens: 100})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	assertJSONEqual(t, `{
		"contents": [
			{"role":"user","parts":[{"text":"triage this"}]},
			{"role":"model","parts":[{"functionCall":{"name":"get_issue","args":{"id":"1"}}}]},
			{"role":"user","parts":[{"functionResponse":{"name":"get_issue","response":{"content":"issue body"}}}]}
		],
		"tools": [
			{"functionDeclarations":[{"name":"get_issue","description":"fetch an issue","parameters":{"type":"OBJECT"}}]}
		],
		"generationConfig": {"maxOutputTokens":100}
	}`, string(gotRawBody))

	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "get_issue" || resp.ToolCalls[0].Arguments != `{"id":"1"}` {
		t.Fatalf("resp.ToolCalls = %+v", resp.ToolCalls)
	}
	if resp.StopReason != StopToolUse {
		t.Fatalf("StopReason = %q, want tool_use", resp.StopReason)
	}
}

func TestGeminiComplete_RequestShape_StructuredDecision(t *testing.T) {
	schema := Schema{
		Type:     "object",
		Required: []string{"disposition"},
		Properties: map[string]Schema{
			"disposition": {Type: "string", Enum: []string{"comment_only", "fixable"}},
			"duplicateOf": {Type: "string", Nullable: true},
		},
	}

	var gotRawBody []byte
	chat := newTestGeminiChat(t, func(w http.ResponseWriter, r *http.Request) {
		var err error
		gotRawBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("reading request body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(geminiResponse{
			Candidates: []geminiCandidate{{
				Content:      geminiContent{Role: "model", Parts: []geminiPart{{Text: `{"disposition":"comment_only"}`}}},
				FinishReason: "STOP",
			}},
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
		"contents": [
			{"role":"user","parts":[{"text":"decide"}]}
		],
		"generationConfig": {
			"maxOutputTokens": 100,
			"responseMimeType": "application/json",
			"responseSchema": {
				"type": "OBJECT",
				"properties": {
					"disposition": {"type":"STRING","enum":["comment_only","fixable"]},
					"duplicateOf": {"type":"STRING","nullable":true}
				},
				"required": ["disposition"]
			}
		}
	}`, string(gotRawBody))

	if resp.Text != `{"disposition":"comment_only"}` {
		t.Fatalf("Text = %q", resp.Text)
	}
	if len(resp.ToolCalls) != 0 {
		t.Fatalf("ToolCalls = %+v, want none for a plain structured-text response", resp.ToolCalls)
	}
}

// --- response parsing / usage / finish-reason mapping ----------------------------------------------

func TestGeminiComplete_FinishReasonMapping(t *testing.T) {
	cases := []struct {
		wire        string
		hasFuncCall bool
		want        string
	}{
		{"STOP", false, StopEndTurn},
		{"STOP", true, StopToolUse},
		{"MAX_TOKENS", false, StopMaxTokens},
		{"SAFETY", false, StopError},
	}
	for _, tc := range cases {
		parts := []geminiPart{}
		if tc.hasFuncCall {
			parts = append(parts, geminiPart{FunctionCall: &geminiFunctionCall{Name: "get_issue"}})
		} else {
			parts = append(parts, geminiPart{Text: "x"})
		}
		chat := newTestGeminiChat(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(geminiResponse{
				Candidates: []geminiCandidate{{Content: geminiContent{Parts: parts}, FinishReason: tc.wire}},
			})
		})
		resp, err := chat.Complete(context.Background(), Request{Messages: []Msg{{Role: RoleUser, Text: "hi"}}})
		if err != nil {
			t.Fatalf("Complete: %v", err)
		}
		if resp.StopReason != tc.want {
			t.Errorf("wire %q hasFuncCall=%v => %q, want %q", tc.wire, tc.hasFuncCall, resp.StopReason, tc.want)
		}
	}
}

// TestGeminiComplete_NoCandidatesRetriesOnceThenPermanent pins the unified "no usable content"
// semantic (finding: openai's malformed-2xx-body policy is the baseline all three adapters now
// share) — zero candidates used to be reported as a SUCCESSFUL Response{StopReason: StopError},
// letting RunLoop treat a content-less turn as a normal (if odd) final decision instead of
// retrying/failing it. It must now retry once immediately, then report *PermanentError.
func TestGeminiComplete_NoCandidatesRetriesOnceThenPermanent(t *testing.T) {
	calls := 0
	chat := newTestGeminiChat(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(geminiResponse{})
	})
	_, err := chat.Complete(context.Background(), Request{Messages: []Msg{{Role: RoleUser, Text: "hi"}}})
	if calls != 2 {
		t.Fatalf("calls = %d, want 2 (one immediate retry)", calls)
	}
	var permanent *PermanentError
	if !errors.As(err, &permanent) {
		t.Fatalf("expected *PermanentError, got %v (%T)", err, err)
	}
}

// --- error tables ---------------------------------------------------------------------------------

func TestGeminiComplete_ErrorClassification(t *testing.T) {
	cases := []struct {
		status        int
		wantTransient bool
	}{
		{429, true},
		{500, true},
		{503, true},
		{400, false},
		{401, false},
		{403, false},
		{404, false},
	}
	for _, tc := range cases {
		chat := newTestGeminiChat(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_ = json.NewEncoder(w).Encode(geminiErrorEnvelope{Error: struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
				Status  string `json:"status"`
			}{Code: tc.status, Message: "boom", Status: "SOME_STATUS"}})
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

// TestGeminiComplete_429HonorsRetryAfter proves the 429 path sleeps the header's Retry-After
// duration (via the injected sleep, no real sleep) before reporting Transient, matching openai.go.
func TestGeminiComplete_429HonorsRetryAfter(t *testing.T) {
	chat := newTestGeminiChat(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "9")
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(geminiErrorEnvelope{})
	})
	var gotWait time.Duration
	chat.sleep = func(_ context.Context, d time.Duration) { gotWait = d }

	_, err := chat.Complete(context.Background(), Request{Messages: []Msg{{Role: RoleUser, Text: "hi"}}})
	var transient *TransientError
	if !errors.As(err, &transient) {
		t.Fatalf("expected TransientError, got %v (%T)", err, err)
	}
	if gotWait != 9*time.Second {
		t.Fatalf("sleep duration = %v, want 9s", gotWait)
	}
}

// TestGeminiComplete_MalformedBodyRetriesOnceThenPermanent mirrors openai.go's doOnce retry
// policy: a non-JSON 2xx body is retried once immediately, and a *PermanentError if the retry is
// malformed too.
func TestGeminiComplete_MalformedBodyRetriesOnceThenPermanent(t *testing.T) {
	calls := 0
	chat := newTestGeminiChat(t, func(w http.ResponseWriter, r *http.Request) {
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
	if !strings.HasPrefix(permanent.Reason, "llm/gemini:") {
		t.Fatalf("PermanentError.Reason = %q, want prefix %q", permanent.Reason, "llm/gemini:")
	}
}

// TestGeminiToGeminiContents_SkipsEmptyMessage guards against wiring `"parts":[]`, which Gemini
// rejects with a hard 400 — reachable via toolloop.go's finalizeDecision appending
// Msg{Role: RoleAssistant, Text: ""} after a safety/MAX_TOKENS turn.
func TestGeminiToGeminiContents_SkipsEmptyMessage(t *testing.T) {
	out := toGeminiContents([]Msg{
		{Role: RoleUser, Text: "hi"},
		{Role: RoleAssistant},
		{Role: RoleTool},
	}, nil)
	for _, c := range out {
		if len(c.Parts) == 0 {
			t.Fatalf("emitted a zero-parts content: %+v", out)
		}
	}
	if len(out) != 1 {
		t.Fatalf("out = %+v, want only the non-empty user content", out)
	}
}

// TestGeminiComplete_RequestShape_ToolsAndStructuredDecision covers the combination the Gemini
// generateContent API rejects when sent natively (400 INVALID_ARGUMENT, "Function calling with a
// response mime type: 'application/json' is unsupported") — exactly the request RunLoop
// (toolloop.go) builds for every §4.1 TRIAGE turn. The two paths must never both be present:
// responseSchema/responseMimeType stay unset, and the decision is an extra functionDeclaration.
func TestGeminiComplete_RequestShape_ToolsAndStructuredDecision(t *testing.T) {
	schema := Schema{
		Type:     "object",
		Required: []string{"disposition"},
		Properties: map[string]Schema{
			"disposition": {Type: "string", Enum: []string{"comment_only", "fixable"}},
		},
	}
	var gotBody geminiRequest
	chat := newTestGeminiChat(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody = decodeGeminiRequest(t, r)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(geminiResponse{
			Candidates: []geminiCandidate{{
				Content: geminiContent{Role: "model", Parts: []geminiPart{
					{FunctionCall: &geminiFunctionCall{Name: geminiDecisionFunctionName, Args: json.RawMessage(`{"disposition":"comment_only"}`)}},
				}},
				FinishReason: "STOP",
			}},
		})
	})

	tools := []ToolDef{{Name: "get_issue", Description: "fetch an issue", Params: Schema{Type: "object"}}}
	resp, err := chat.Complete(context.Background(), Request{
		Messages:   []Msg{{Role: RoleUser, Text: "triage this"}},
		Tools:      tools,
		MaxTokens:  100,
		JSONSchema: &schema,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if gotBody.GenerationConfig != nil {
		if gotBody.GenerationConfig.ResponseSchema != nil || gotBody.GenerationConfig.ResponseMimeType != "" {
			t.Fatalf("responseSchema/responseMimeType must be unset when Tools is also set, got %+v", gotBody.GenerationConfig)
		}
	}
	if len(gotBody.Tools) != 1 {
		t.Fatalf("tools = %+v", gotBody.Tools)
	}
	decls := gotBody.Tools[0].FunctionDeclarations
	if len(decls) != 2 {
		t.Fatalf("functionDeclarations = %+v, want read-tool + decision function", decls)
	}
	foundDecision := false
	for _, d := range decls {
		if d.Name == geminiDecisionFunctionName {
			foundDecision = true
		}
	}
	if !foundDecision {
		t.Fatalf("expected a %q functionDeclaration among %+v", geminiDecisionFunctionName, decls)
	}

	// The decision functionCall must be unwrapped into Text, not surfaced as a ToolCall, so
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
}

// TestGeminiComplete_DecisionWithProseText_TextPartBeforeDecision covers a Gemini response that
// emits a narration text part alongside the decision functionCall part, text first. The decision
// unwrap must WIN over any prose so Response.Text stays valid, schema-checkable JSON — see the
// blocker this regresses: resp.Text used to accumulate ("+=") the decision args onto the prose.
func TestGeminiComplete_DecisionWithProseText_TextPartBeforeDecision(t *testing.T) {
	schema := Schema{Type: "object", Required: []string{"disposition"}, Properties: map[string]Schema{
		"disposition": {Type: "string", Enum: []string{"comment_only", "fixable"}},
	}}
	chat := newTestGeminiChat(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(geminiResponse{
			Candidates: []geminiCandidate{{
				Content: geminiContent{Role: "model", Parts: []geminiPart{
					{Text: "Sure, here is my decision."},
					{FunctionCall: &geminiFunctionCall{Name: geminiDecisionFunctionName, Args: json.RawMessage(`{"disposition":"comment_only"}`)}},
				}},
				FinishReason: "STOP",
			}},
		})
	})
	resp, err := chat.Complete(context.Background(), Request{
		Messages:   []Msg{{Role: RoleUser, Text: "triage"}},
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

// TestGeminiComplete_DecisionWithProseText_TextPartAfterDecision is the mirror order: the
// decision functionCall part arrives first, then a trailing prose text part.
func TestGeminiComplete_DecisionWithProseText_TextPartAfterDecision(t *testing.T) {
	schema := Schema{Type: "object", Required: []string{"disposition"}, Properties: map[string]Schema{
		"disposition": {Type: "string", Enum: []string{"comment_only", "fixable"}},
	}}
	chat := newTestGeminiChat(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(geminiResponse{
			Candidates: []geminiCandidate{{
				Content: geminiContent{Role: "model", Parts: []geminiPart{
					{FunctionCall: &geminiFunctionCall{Name: geminiDecisionFunctionName, Args: json.RawMessage(`{"disposition":"comment_only"}`)}},
					{Text: "Done."},
				}},
				FinishReason: "STOP",
			}},
		})
	})
	resp, err := chat.Complete(context.Background(), Request{
		Messages:   []Msg{{Role: RoleUser, Text: "triage"}},
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

func TestNewGeminiChat_RequiresModel(t *testing.T) {
	if _, err := New("gemini", Config{APIKey: "k"}); err == nil {
		t.Fatal("expected error for empty Config.Model")
	}
}

// TestGeminiToContents_MalformedToolCallArguments ensures a malformed ToolCall.Arguments string
// (as openai's unvalidated wire string can carry) does not hard-fail request encoding — it must
// fall back to "{}" rather than making json.Marshal fail with a *PermanentError, which would
// poison every RunLoop fallback turn after openai's circuit opens.
func TestGeminiToContents_MalformedToolCallArguments(t *testing.T) {
	contents := toGeminiContents([]Msg{
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "1", Name: "get_issue", Arguments: `{"id": }`}}},
	}, nil)
	b, err := json.Marshal(contents)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !json.Valid(b) {
		t.Fatalf("marshaled contents not valid JSON: %s", b)
	}
}

// TestGeminiComplete_ThoughtsTokenCountSummedIntoOutputTokens pins the finding: Gemini bills
// thinking tokens as output but excludes them from candidatesTokenCount (unlike openai's
// completion_tokens / anthropic's output_tokens, which already fold reasoning in), so a caller
// summing only CandidatesTokenCount under-reports spend and under-enforces
// Caps.MaxOutputTokens. Golden: {prompt:10, candidates:5, thoughts:40} => Usage{10,45}.
func TestGeminiComplete_ThoughtsTokenCountSummedIntoOutputTokens(t *testing.T) {
	chat := newTestGeminiChat(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(geminiResponse{
			Candidates: []geminiCandidate{{Content: geminiContent{Parts: []geminiPart{{Text: "ok"}}}, FinishReason: "STOP"}},
			UsageMetadata: geminiUsageMetadata{
				PromptTokenCount:     10,
				CandidatesTokenCount: 5,
				ThoughtsTokenCount:   40,
			},
		})
	})
	resp, err := chat.Complete(context.Background(), Request{Messages: []Msg{{Role: RoleUser, Text: "hi"}}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Usage != (Usage{InputTokens: 10, OutputTokens: 45}) {
		t.Fatalf("Usage = %+v, want {10 45}", resp.Usage)
	}
}

// TestGeminiFunctionDeclaration_ParametersOmittedWhenEmpty pins the fix for a dead `omitempty` on
// a non-pointer struct field: a ToolDef with the zero-value Schema (no params) must not wire an
// always-present `"parameters":{}` — the field should be entirely absent.
func TestGeminiFunctionDeclaration_ParametersOmittedWhenEmpty(t *testing.T) {
	var gotBody geminiRequest
	chat := newTestGeminiChat(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody = decodeGeminiRequest(t, r)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(geminiResponse{
			Candidates: []geminiCandidate{{Content: geminiContent{Parts: []geminiPart{{Text: "ok"}}}, FinishReason: "STOP"}},
		})
	})
	req := Request{
		Messages: []Msg{{Role: RoleUser, Text: "hi"}},
		Tools:    []ToolDef{{Name: "ping"}}, // deliberately no Params
	}
	if _, err := chat.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(gotBody.Tools) != 1 || len(gotBody.Tools[0].FunctionDeclarations) != 1 {
		t.Fatalf("gotBody.Tools = %+v", gotBody.Tools)
	}
	if gotBody.Tools[0].FunctionDeclarations[0].Parameters != nil {
		t.Fatalf("Parameters = %+v, want nil for a ToolDef with no declared params", gotBody.Tools[0].FunctionDeclarations[0].Parameters)
	}
}

// TestGeminiFunctionNameFromID_ResolvesAgainstDeclaredNames pins the fix for a tool literally
// named with a trailing dash-digit ("resolve-2"): a blind "strip the last -<digits>" heuristic
// mis-parses that name itself as "resolve" + a spurious ordinal, corrupting the wire function
// name. Resolving against the turn's declared function names must recover it exactly.
func TestGeminiFunctionNameFromID_ResolvesAgainstDeclaredNames(t *testing.T) {
	declared := []string{"resolve-2", "get_issue"}

	cases := []struct {
		id   string
		want string
	}{
		// Exact match: an ID that IS a declared name verbatim (no synthesized ordinal) must resolve
		// to itself, not be corrupted by the dash-digit heuristic (old behavior: "resolve").
		{"resolve-2", "resolve-2"},
		// Synthesized shape ("<name>-<ordinal>") against the same declared name.
		{"resolve-2-0", "resolve-2"},
		{"resolve-2-11", "resolve-2"},
		// A normal declared name's synthesized ID still resolves correctly.
		{"get_issue-0", "get_issue"},
		// An ID this turn's declaredNames doesn't explain falls back to the dash-split heuristic.
		{"unknown_tool-3", "unknown_tool"},
	}
	for _, tc := range cases {
		if got := geminiFunctionNameFromID(tc.id, declared); got != tc.want {
			t.Errorf("geminiFunctionNameFromID(%q, %v) = %q, want %q", tc.id, declared, got, tc.want)
		}
	}
}

// TestGeminiComplete_ToolResultRoundTrip_TrailingDashDigitName drives the fix through the real
// Complete round trip rather than the unit-level resolver alone: a declared tool named
// "resolve-2" is called, and the harness's ToolResult (correlated back by the synthesized
// ToolCall.ID) must be sent back under the exact declared function name.
func TestGeminiComplete_ToolResultRoundTrip_TrailingDashDigitName(t *testing.T) {
	var gotBody geminiRequest
	chat := newTestGeminiChat(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody = decodeGeminiRequest(t, r)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(geminiResponse{
			Candidates: []geminiCandidate{{Content: geminiContent{Parts: []geminiPart{{Text: "ok"}}}, FinishReason: "STOP"}},
		})
	})
	req := Request{
		Tools: []ToolDef{{Name: "resolve-2"}},
		Messages: []Msg{
			{Role: RoleUser, Text: "hi"},
			{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "resolve-2-0", Name: "resolve-2", Arguments: `{}`}}},
			{Role: RoleTool, ToolResults: []ToolResult{{ToolCallID: "resolve-2-0", Content: "done"}}},
		},
	}
	if _, err := chat.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	var gotName string
	for _, c := range gotBody.Contents {
		for _, p := range c.Parts {
			if p.FunctionResponse != nil {
				gotName = p.FunctionResponse.Name
			}
		}
	}
	if gotName != "resolve-2" {
		t.Fatalf("functionResponse.name = %q, want %q", gotName, "resolve-2")
	}
}

// TestGeminiComplete_CancelledContextIsWrappedContextError mirrors openai.go's ctx-cancel
// handling (finding: anthropic/gemini used to return a bare *TransientError on a cancelled ctx,
// while openai wrapped context.Canceled so errors.Is(err, context.Canceled) held).
func TestGeminiComplete_CancelledContextIsWrappedContextError(t *testing.T) {
	chat := newTestGeminiChat(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(geminiResponse{
			Candidates: []geminiCandidate{{Content: geminiContent{Parts: []geminiPart{{Text: "ok"}}}, FinishReason: "STOP"}},
		})
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

// TestGeminiComplete_3xxIsTransient pins the shared rule (finding: openai already treated 3xx as
// Transient; anthropic/gemini fell through to Permanent) — a 3xx response is not an authoritative
// failure worth giving up on.
func TestGeminiComplete_3xxIsTransient(t *testing.T) {
	chat := newTestGeminiChat(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMovedPermanently)
		_, _ = w.Write([]byte(`{}`))
	})
	_, err := chat.Complete(context.Background(), Request{Messages: []Msg{{Role: RoleUser, Text: "hi"}}})
	var transient *TransientError
	if !errors.As(err, &transient) {
		t.Fatalf("want *TransientError for 3xx, got %v (%T)", err, err)
	}
}

// TestGeminiComplete_JSONSchemaNameNormalized pins the hoisted normalizer (finding: only
// openai.go sanitized Request.JSONSchemaName; gemini passed it RAW as a wire functionDeclaration
// name, so a spaced/unicode name that worked against openai 400'd here). A conforming wire
// function name must be emitted regardless.
func TestGeminiComplete_JSONSchemaNameNormalized(t *testing.T) {
	var gotBody geminiRequest
	chat := newTestGeminiChat(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody = decodeGeminiRequest(t, r)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(geminiResponse{
			Candidates: []geminiCandidate{{Content: geminiContent{Parts: []geminiPart{{Text: `{}`}}}, FinishReason: "STOP"}},
		})
	})
	schema := Schema{Type: "object"}
	req := Request{
		Messages:       []Msg{{Role: RoleUser, Text: "hi"}},
		Tools:          []ToolDef{{Name: "get_issue"}}, // forces the tools+JSONSchema decision-function path
		JSONSchema:     &schema,
		JSONSchemaName: "décision spatiale!",
	}
	if _, err := chat.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(gotBody.Tools) != 1 || len(gotBody.Tools[0].FunctionDeclarations) != 2 {
		t.Fatalf("Tools = %+v, want one tool group with 2 declarations", gotBody.Tools)
	}
	decisionName := gotBody.Tools[0].FunctionDeclarations[1].Name
	if !jsonSchemaNameRe.MatchString(decisionName) {
		t.Fatalf("wire function name %q does not conform to %s", decisionName, jsonSchemaNameRe.String())
	}
}

func TestGeminiComplete_NetworkErrorIsTransient(t *testing.T) {
	c := newGeminiChat(Config{Model: "m", APIKey: "k", BaseURL: "http://127.0.0.1:1"})
	_, err := c.Complete(context.Background(), Request{Messages: []Msg{{Role: RoleUser, Text: "hi"}}})
	if err == nil {
		t.Fatal("expected error")
	}
	var transient *TransientError
	if !errors.As(err, &transient) {
		t.Fatalf("expected TransientError, got %v (%T)", err, err)
	}
}
