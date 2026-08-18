package llm

// parity_test.go is the N8b integration-validator's cross-adapter parity table: one set of
// neutral scenarios (plan §4.1's Chat/Request/Response contract), each expressed in every
// adapter's OWN real wire fixture, asserting that the SAME neutral Response field or error class
// comes out regardless of which provider produced it. Individual adapter files already golden-test
// their own wire shapes in detail (openai_test.go/anthropic_test.go/gemini_test.go); this file's
// job is narrower and different: prove the three adapters agree with EACH OTHER on the
// provider-neutral contract, not just with their own provider's documented behavior.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// newParityOpenAI/newParityAnthropic/newParityGemini each stand up an httptest server running
// handler and return a Chat talking to it — the three providers' real transport, not a fake.
func newParityOpenAI(t *testing.T, handler http.HandlerFunc) Chat {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return newOpenAIChatForTest(srv.URL, "test-key", "m", &http.Client{}, func(context.Context, time.Duration) {})
}

func newParityAnthropic(t *testing.T, handler http.HandlerFunc) Chat {
	t.Helper()
	return newTestAnthropicChat(t, handler)
}

func newParityGemini(t *testing.T, handler http.HandlerFunc) Chat {
	t.Helper()
	return newTestGeminiChat(t, handler)
}

// parityAdapters is the fixed provider set every scenario below runs against, in a stable order
// for deterministic subtest names.
var parityAdapters = []string{"openai", "anthropic", "gemini"}

func newParityChat(t *testing.T, provider string, handler http.HandlerFunc) Chat {
	t.Helper()
	switch provider {
	case "openai":
		return newParityOpenAI(t, handler)
	case "anthropic":
		return newParityAnthropic(t, handler)
	case "gemini":
		return newParityGemini(t, handler)
	default:
		t.Fatalf("newParityChat: unknown provider %q", provider)
		return nil
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, v any) {
	t.Helper()
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
}

// --- scenario: plain completion --------------------------------------------------------------

func TestParity_PlainCompletion(t *testing.T) {
	fixtures := map[string]http.HandlerFunc{
		"openai": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, 200, map[string]any{
				"choices": []map[string]any{{
					"message":       map[string]any{"content": "hello"},
					"finish_reason": "stop",
				}},
				"usage": map[string]any{"prompt_tokens": 3, "completion_tokens": 2},
			})
		},
		"anthropic": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, 200, anthropicResponse{
				Content:    []anthropicContentBlock{{Type: "text", Text: "hello"}},
				StopReason: "end_turn",
			})
		},
		"gemini": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, 200, geminiResponse{
				Candidates:    []geminiCandidate{{Content: geminiContent{Parts: []geminiPart{{Text: "hello"}}}, FinishReason: "STOP"}},
				UsageMetadata: geminiUsageMetadata{PromptTokenCount: 3, CandidatesTokenCount: 2},
			})
		},
	}
	for _, provider := range parityAdapters {
		t.Run(provider, func(t *testing.T) {
			chat := newParityChat(t, provider, fixtures[provider])
			resp, err := chat.Complete(context.Background(), Request{Messages: []Msg{{Role: RoleUser, Text: "hi"}}})
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if resp.Text != "hello" {
				t.Errorf("Text = %q, want %q", resp.Text, "hello")
			}
			if resp.StopReason != StopEndTurn {
				t.Errorf("StopReason = %q, want %q", resp.StopReason, StopEndTurn)
			}
			if len(resp.ToolCalls) != 0 {
				t.Errorf("ToolCalls = %+v, want none", resp.ToolCalls)
			}
		})
	}
}

// --- scenario: tool round-trip, driven through each adapter's own emitted ToolCallID -----------

func TestParity_ToolRoundTrip(t *testing.T) {
	fixtures := map[string]func() http.HandlerFunc{
		"openai": func() http.HandlerFunc {
			calls := 0
			return func(w http.ResponseWriter, r *http.Request) {
				calls++
				if calls == 1 {
					writeJSON(t, w, 200, map[string]any{
						"choices": []map[string]any{{
							"message": map[string]any{"tool_calls": []map[string]any{{
								"id":       "call_abc123",
								"function": map[string]any{"name": "get_issue", "arguments": `{"id":1}`},
							}}},
							"finish_reason": "tool_calls",
						}},
					})
					return
				}
				var body chatCompletionRequest
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatalf("decode: %v", err)
				}
				var sawToolMsg bool
				for _, m := range body.Messages {
					if m.Role == "tool" && m.ToolCallID == "call_abc123" {
						sawToolMsg = true
					}
				}
				if !sawToolMsg {
					t.Errorf("second request did not echo the adapter's own tool_call_id %q: %+v", "call_abc123", body.Messages)
				}
				writeJSON(t, w, 200, map[string]any{
					"choices": []map[string]any{{"message": map[string]any{"content": "done"}, "finish_reason": "stop"}},
				})
			}
		},
		"anthropic": func() http.HandlerFunc {
			calls := 0
			return func(w http.ResponseWriter, r *http.Request) {
				calls++
				if calls == 1 {
					writeJSON(t, w, 200, anthropicResponse{
						Content:    []anthropicContentBlock{{Type: "tool_use", ID: "toolu_xyz", Name: "get_issue", Input: json.RawMessage(`{"id":1}`)}},
						StopReason: "tool_use",
					})
					return
				}
				var body anthropicRequest
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatalf("decode: %v", err)
				}
				var sawToolResult bool
				for _, m := range body.Messages {
					for _, b := range m.Content {
						if b.Type == "tool_result" && b.ToolUseID == "toolu_xyz" {
							sawToolResult = true
						}
					}
				}
				if !sawToolResult {
					t.Errorf("second request did not echo the adapter's own tool_use id %q: %+v", "toolu_xyz", body.Messages)
				}
				writeJSON(t, w, 200, anthropicResponse{
					Content:    []anthropicContentBlock{{Type: "text", Text: "done"}},
					StopReason: "end_turn",
				})
			}
		},
		"gemini": func() http.HandlerFunc {
			calls := 0
			return func(w http.ResponseWriter, r *http.Request) {
				calls++
				if calls == 1 {
					writeJSON(t, w, 200, geminiResponse{
						Candidates: []geminiCandidate{{
							Content:      geminiContent{Parts: []geminiPart{{FunctionCall: &geminiFunctionCall{Name: "get_issue", Args: json.RawMessage(`{"id":1}`)}}}},
							FinishReason: "STOP",
						}},
					})
					return
				}
				body := decodeGeminiRequest(t, r)
				var sawFunctionResponse bool
				for _, c := range body.Contents {
					for _, p := range c.Parts {
						if p.FunctionResponse != nil && p.FunctionResponse.Name == "get_issue" {
							sawFunctionResponse = true
						}
					}
				}
				if !sawFunctionResponse {
					t.Errorf("second request did not echo the adapter's own declared function name %q: %+v", "get_issue", body.Contents)
				}
				writeJSON(t, w, 200, geminiResponse{
					Candidates: []geminiCandidate{{Content: geminiContent{Parts: []geminiPart{{Text: "done"}}}, FinishReason: "STOP"}},
				})
			}
		},
	}
	for _, provider := range parityAdapters {
		t.Run(provider, func(t *testing.T) {
			chat := newParityChat(t, provider, fixtures[provider]())
			req := Request{
				Tools:    []ToolDef{{Name: "get_issue"}},
				Messages: []Msg{{Role: RoleUser, Text: "look into it"}},
			}
			first, err := chat.Complete(context.Background(), req)
			if err != nil {
				t.Fatalf("first Complete: %v", err)
			}
			if len(first.ToolCalls) != 1 {
				t.Fatalf("first ToolCalls = %+v, want exactly one", first.ToolCalls)
			}
			if first.StopReason != StopToolUse {
				t.Errorf("first StopReason = %q, want %q", first.StopReason, StopToolUse)
			}
			tc := first.ToolCalls[0]
			req.Messages = append(req.Messages,
				Msg{Role: RoleAssistant, ToolCalls: first.ToolCalls},
				Msg{Role: RoleTool, ToolResults: []ToolResult{{ToolCallID: tc.ID, Content: `{"status":"open"}`}}},
			)
			second, err := chat.Complete(context.Background(), req)
			if err != nil {
				t.Fatalf("second Complete: %v", err)
			}
			if second.Text != "done" {
				t.Errorf("second Text = %q, want %q", second.Text, "done")
			}
			if second.StopReason != StopEndTurn {
				t.Errorf("second StopReason = %q, want %q", second.StopReason, StopEndTurn)
			}
		})
	}
}

// --- scenario: structured decision -------------------------------------------------------------

var parityDecisionSchema = Schema{
	Type:     "object",
	Required: []string{"disposition"},
	Properties: map[string]Schema{
		"disposition": {Type: "string", Enum: []string{"comment_only", "fixable"}},
	},
}

func TestParity_StructuredDecision(t *testing.T) {
	const decisionJSON = `{"disposition":"comment_only"}`
	fixtures := map[string]http.HandlerFunc{
		"openai": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, 200, map[string]any{
				"choices": []map[string]any{{"message": map[string]any{"content": decisionJSON}, "finish_reason": "stop"}},
			})
		},
		"anthropic": func(w http.ResponseWriter, r *http.Request) {
			var body anthropicRequest
			_ = json.NewDecoder(r.Body).Decode(&body)
			// Anthropic has no native structured-output mode: the caller's own tool_choice forces
			// a tool_use block naming the decision tool, whose input IS the decision.
			var toolName string
			if body.ToolChoice != nil {
				toolName = body.ToolChoice.Name
			}
			writeJSON(t, w, 200, anthropicResponse{
				Content:    []anthropicContentBlock{{Type: "tool_use", ID: "toolu_1", Name: toolName, Input: json.RawMessage(decisionJSON)}},
				StopReason: "tool_use",
			})
		},
		"gemini": func(w http.ResponseWriter, r *http.Request) {
			// No req.Tools here, so gemini takes the native responseSchema path: the model's plain
			// text IS the schema-shaped JSON, no decision-function unwrap involved.
			writeJSON(t, w, 200, geminiResponse{
				Candidates: []geminiCandidate{{Content: geminiContent{Parts: []geminiPart{{Text: decisionJSON}}}, FinishReason: "STOP"}},
			})
		},
	}
	for _, provider := range parityAdapters {
		t.Run(provider, func(t *testing.T) {
			chat := newParityChat(t, provider, fixtures[provider])
			req := Request{
				Messages:       []Msg{{Role: RoleUser, Text: "decide"}},
				JSONSchema:     &parityDecisionSchema,
				JSONSchemaName: "triage_decision",
			}
			resp, err := chat.Complete(context.Background(), req)
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if resp.StopReason != StopEndTurn {
				t.Errorf("StopReason = %q, want %q", resp.StopReason, StopEndTurn)
			}
			if len(resp.ToolCalls) != 0 {
				t.Errorf("ToolCalls = %+v, want none — the decision must be unwrapped into Text", resp.ToolCalls)
			}
			if verr := validateAgainstSchema(resp.Text, parityDecisionSchema); verr != nil {
				t.Errorf("Text %q does not validate against the schema: %v", resp.Text, verr)
			}
		})
	}
}

// --- scenario: max-tokens truncation -------------------------------------------------------------

func TestParity_MaxTokensTruncation(t *testing.T) {
	fixtures := map[string]http.HandlerFunc{
		"openai": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, 200, map[string]any{
				"choices": []map[string]any{{"message": map[string]any{"content": "cut off mid-sen"}, "finish_reason": "length"}},
			})
		},
		"anthropic": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, 200, anthropicResponse{
				Content:    []anthropicContentBlock{{Type: "text", Text: "cut off mid-sen"}},
				StopReason: "max_tokens",
			})
		},
		"gemini": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, 200, geminiResponse{
				Candidates: []geminiCandidate{{Content: geminiContent{Parts: []geminiPart{{Text: "cut off mid-sen"}}}, FinishReason: "MAX_TOKENS"}},
			})
		},
	}
	for _, provider := range parityAdapters {
		t.Run(provider, func(t *testing.T) {
			chat := newParityChat(t, provider, fixtures[provider])
			resp, err := chat.Complete(context.Background(), Request{Messages: []Msg{{Role: RoleUser, Text: "hi"}}})
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if resp.StopReason != StopMaxTokens {
				t.Errorf("StopReason = %q, want %q", resp.StopReason, StopMaxTokens)
			}
		})
	}
}

// --- scenario: safety / content-block ------------------------------------------------------------

func TestParity_SafetyContentBlock(t *testing.T) {
	fixtures := map[string]http.HandlerFunc{
		"openai": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, 200, map[string]any{
				"choices": []map[string]any{{"message": map[string]any{"content": ""}, "finish_reason": "content_filter"}},
			})
		},
		"anthropic": func(w http.ResponseWriter, r *http.Request) {
			// Anthropic has no dedicated safety-block finish reason on the public Messages API as
			// of this adapter's pinned version; an unrecognized stop_reason maps to StopError via
			// mapAnthropicStopReason's default case, same neutral outcome as openai/gemini's
			// dedicated safety signals.
			writeJSON(t, w, 200, anthropicResponse{
				Content:    []anthropicContentBlock{{Type: "text", Text: ""}},
				StopReason: "refusal",
			})
		},
		"gemini": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, 200, geminiResponse{
				Candidates: []geminiCandidate{{Content: geminiContent{}, FinishReason: "SAFETY"}},
			})
		},
	}
	for _, provider := range parityAdapters {
		t.Run(provider, func(t *testing.T) {
			chat := newParityChat(t, provider, fixtures[provider])
			resp, err := chat.Complete(context.Background(), Request{Messages: []Msg{{Role: RoleUser, Text: "hi"}}})
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if resp.StopReason != StopError {
				t.Errorf("StopReason = %q, want %q", resp.StopReason, StopError)
			}
		})
	}
}

// --- scenario: empty content ("no usable content" — unified onto openai's semantic) --------------

func TestParity_EmptyContentRetriesOnceThenPermanent(t *testing.T) {
	fixtures := map[string]func() http.HandlerFunc{
		"openai": func() http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) {
				writeJSON(t, w, 200, map[string]any{"choices": []map[string]any{}})
			}
		},
		"anthropic": func() http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) {
				writeJSON(t, w, 200, anthropicResponse{Content: []anthropicContentBlock{}})
			}
		},
		"gemini": func() http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) {
				writeJSON(t, w, 200, geminiResponse{})
			}
		},
	}
	for _, provider := range parityAdapters {
		t.Run(provider, func(t *testing.T) {
			calls := 0
			handler := fixtures[provider]()
			counting := func(w http.ResponseWriter, r *http.Request) {
				calls++
				handler(w, r)
			}
			chat := newParityChat(t, provider, counting)
			_, err := chat.Complete(context.Background(), Request{Messages: []Msg{{Role: RoleUser, Text: "hi"}}})
			var permanent *PermanentError
			if !errors.As(err, &permanent) {
				t.Fatalf("expected *PermanentError, got %v (%T)", err, err)
			}
			if calls != 2 {
				t.Fatalf("calls = %d, want 2 (one immediate retry, matching openai's malformed-body policy)", calls)
			}
		})
	}
}

// --- scenario: 429 -----------------------------------------------------------------------------

func TestParity_429IsTransient(t *testing.T) {
	for _, provider := range parityAdapters {
		t.Run(provider, func(t *testing.T) {
			chat := newParityChat(t, provider, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
			})
			_, err := chat.Complete(context.Background(), Request{Messages: []Msg{{Role: RoleUser, Text: "hi"}}})
			var transient *TransientError
			if !errors.As(err, &transient) {
				t.Fatalf("want *TransientError for 429, got %v (%T)", err, err)
			}
		})
	}
}

// --- scenario: 5xx -----------------------------------------------------------------------------

func TestParity_5xxIsTransient(t *testing.T) {
	for _, provider := range parityAdapters {
		t.Run(provider, func(t *testing.T) {
			chat := newParityChat(t, provider, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"error":{"message":"overloaded"}}`))
			})
			_, err := chat.Complete(context.Background(), Request{Messages: []Msg{{Role: RoleUser, Text: "hi"}}})
			var transient *TransientError
			if !errors.As(err, &transient) {
				t.Fatalf("want *TransientError for 5xx, got %v (%T)", err, err)
			}
		})
	}
}

// --- scenario: 4xx -----------------------------------------------------------------------------

func TestParity_4xxIsPermanent(t *testing.T) {
	for _, provider := range parityAdapters {
		t.Run(provider, func(t *testing.T) {
			chat := newParityChat(t, provider, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":{"message":"invalid request"}}`))
			})
			_, err := chat.Complete(context.Background(), Request{Messages: []Msg{{Role: RoleUser, Text: "hi"}}})
			var permanent *PermanentError
			if !errors.As(err, &permanent) {
				t.Fatalf("want *PermanentError for 4xx, got %v (%T)", err, err)
			}
		})
	}
}

// --- scenario: 3xx -----------------------------------------------------------------------------

// TestParity_3xxIsTransient pins the finding: openai already treated an unfollowed 3xx as
// Transient; anthropic/gemini fell through to their catch-all Permanent. One rule now: <200 ||
// 3xx => Transient, for all three.
func TestParity_3xxIsTransient(t *testing.T) {
	for _, provider := range parityAdapters {
		t.Run(provider, func(t *testing.T) {
			chat := newParityChat(t, provider, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusMovedPermanently)
				_, _ = w.Write([]byte(`{}`))
			})
			_, err := chat.Complete(context.Background(), Request{Messages: []Msg{{Role: RoleUser, Text: "hi"}}})
			var transient *TransientError
			if !errors.As(err, &transient) {
				t.Fatalf("want *TransientError for 3xx, got %v (%T)", err, err)
			}
		})
	}
}

// --- scenario: ctx-cancel -----------------------------------------------------------------------

// TestParity_CtxCancelIsWrappedContextError pins the finding: anthropic/gemini used to return a
// bare *TransientError on a cancelled ctx, while openai wrapped context.Canceled so
// errors.Is(err, context.Canceled) held and the failure was NOT recorded against the circuit
// breaker as a transient provider failure. All three now agree.
func TestParity_CtxCancelIsWrappedContextError(t *testing.T) {
	for _, provider := range parityAdapters {
		t.Run(provider, func(t *testing.T) {
			chat := newParityChat(t, provider, func(w http.ResponseWriter, r *http.Request) {
				// Never actually reached: ctx is cancelled before Complete's transport call fires.
				writeJSON(t, w, 200, map[string]any{})
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
		})
	}
}
