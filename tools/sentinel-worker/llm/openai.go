// Package llm — llm/openai.go is the OpenAI-compatible adapter (plan §4.1): THE PRIMARY provider
// per rev 4 ("OpenAI-compatible hosted is the first production provider; harden this one
// hardest"). It talks POST {LLM_BASE_URL}/v1/chat/completions, which — because the base URL is
// configurable and the wire format is a de-facto standard — also covers Ollama, vLLM, LiteLLM,
// OpenRouter, and Gemini's OpenAI-compat endpoint without any provider-specific branching beyond
// this one file (tenet 5: zero provider names outside llm/<provider>.go).
//
// Tenet-5 note: this file is the one place the literal "openai" is expected to appear (it names
// itself when registering with the llm.go registry) — that is the adapter file the carve-out in
// llm.go's doc comment describes, not a violation of it.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/sentinel"
)

func init() {
	RegisterProvider("openai", NewOpenAIChat)
}

// bodyExcerptCap bounds how much of a non-2xx response body is retained in a PermanentError's
// Reason (plan §4.1: "4xx permanent w/ body excerpt") — enough to be diagnosable in a journal
// entry without letting an adversarial or misbehaving backend inflate it unboundedly.
const bodyExcerptCap = 2000

// openaiHTTPClient is the minimal *http.Client surface the adapter needs, so tests can inject a
// httptest-backed RoundTripper without a real network call.
type openaiHTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// openaiChat is the llm.Chat implementation talking to an OpenAI-compatible
// /chat/completions endpoint. sleep is injectable so tests exercise the Retry-After wait
// without a real sleep (plan §8: "injected clocks, no real sleeps").
//
// completionsURL is the fully resolved "<api root>/chat/completions" URL, computed once in
// NewOpenAIChat/newOpenAIChatForTest per the LLM_BASE_URL convention documented there.
type openaiChat struct {
	baseURL        string
	completionsURL string
	apiKey         string
	model          string
	client         openaiHTTPClient
	sleep          sentinel.CtxSleepFunc
}

// NewOpenAIChat builds the OpenAI-compatible adapter from a provider-neutral Config (plan §4.1
// selection surface: LLM_MODEL/API_KEY/BASE_URL).
//
// LLM_BASE_URL convention: BaseURL is the API ROOT, matching every official OpenAI-compatible
// SDK's base_url — i.e. it already includes the "/v1" (or equivalent) segment, and this adapter
// appends only "/chat/completions". BaseURL defaults to "https://api.openai.com/v1" when empty.
// For providers whose published base already ends in a recognized API-root suffix ("/v1" or
// "/openai", case-insensitive — covers OpenAI, Ollama, vLLM, LiteLLM, OpenRouter, and Gemini's
// OpenAI-compat root "https://generativelanguage.googleapis.com/v1beta/openai") that suffix is
// taken at face value; otherwise "/v1" is appended for backward tolerance with a bare host. Set
// LLM_BASE_URL to the provider's full documented base URL, not just its host.
// Model is required — every OpenAI-compatible backend needs one.
func NewOpenAIChat(cfg Config) (Chat, error) {
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, errors.New("llm/openai: Config.Model is required")
	}
	base := cfg.BaseURL
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	base, completionsURL := resolveOpenAIURLs(base)
	return &openaiChat{
		baseURL:        base,
		completionsURL: completionsURL,
		apiKey:         cfg.APIKey,
		model:          cfg.Model,
		client:         &http.Client{Timeout: 120 * time.Second},
		sleep:          sentinel.SleepCtx,
	}, nil
}

// resolveOpenAIURLs normalizes a caller-supplied base URL into (normalized base, completions
// URL) per NewOpenAIChat's documented LLM_BASE_URL convention: treat base as the API root and
// append only "/chat/completions". If base does not already end in a recognized API-root suffix
// ("/v1" or "/openai", case-insensitive) it gets "/v1" appended, for tolerance with a bare host.
func resolveOpenAIURLs(base string) (normalizedBase, completionsURL string) {
	base = strings.TrimRight(base, "/")
	lower := strings.ToLower(base)
	if !strings.HasSuffix(lower, "/v1") && !strings.HasSuffix(lower, "/openai") {
		base += "/v1"
	}
	return base, base + "/chat/completions"
}

// newOpenAIChatForTest builds an adapter with an injected HTTP client and sleep func, bypassing
// NewOpenAIChat's Config validation — this package's own tests use it against an httptest server.
// baseURL follows the same LLM_BASE_URL convention as NewOpenAIChat (API root, not bare host).
func newOpenAIChatForTest(baseURL, apiKey, model string, client openaiHTTPClient, sleep sentinel.CtxSleepFunc) *openaiChat {
	if sleep == nil {
		sleep = sentinel.SleepCtx
	}
	base, completionsURL := resolveOpenAIURLs(baseURL)
	return &openaiChat{baseURL: base, completionsURL: completionsURL, apiKey: apiKey, model: model, client: client, sleep: sleep}
}

// --- wire request shapes (field order here IS the golden JSON key order) --------------------

type chatCompletionRequest struct {
	Model          string              `json:"model"`
	Messages       []wireMessage       `json:"messages"`
	Tools          []wireTool          `json:"tools,omitempty"`
	ResponseFormat *wireResponseFormat `json:"response_format,omitempty"`
	MaxTokens      int                 `json:"max_tokens,omitempty"`
}

type wireMessage struct {
	Role       string         `json:"role"`
	Content    *string        `json:"content,omitempty"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type wireToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function wireFunctionCall `json:"function"`
}

type wireFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type wireTool struct {
	Type     string       `json:"type"`
	Function wireFunction `json:"function"`
}

type wireFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters"`
}

type wireResponseFormat struct {
	Type       string         `json:"type"`
	JSONSchema wireJSONSchema `json:"json_schema"`
}

type wireJSONSchema struct {
	Name   string         `json:"name"`
	Schema map[string]any `json:"schema"`
	Strict bool           `json:"strict"`
}

// buildRequestBody maps an llm.Request onto the OpenAI-compatible wire shape (plan §4.1: "Maps
// llm.Request fully: system message, messages incl. tool results (role tool + tool_call_id),
// tools as {type:function,function:{name,description,parameters}}, response_format
// {type:json_schema,json_schema:{name,schema,strict:true}} when JSONSchema set, max_tokens").
func buildRequestBody(model string, req Request) chatCompletionRequest {
	body := chatCompletionRequest{Model: model, MaxTokens: req.MaxTokens}

	if req.System != "" {
		sys := req.System
		body.Messages = append(body.Messages, wireMessage{Role: "system", Content: &sys})
	}
	for _, m := range req.Messages {
		// Tool results always wire as their own role:"tool" message(s), one per result, regardless
		// of which Role the Msg itself carries (llm.go's Msg doc: RoleUser may carry ToolResults
		// "when replying to prior tool calls"; RoleTool's whole payload IS ToolResults).
		for _, tr := range m.ToolResults {
			content := tr.Content
			body.Messages = append(body.Messages, wireMessage{
				Role:       "tool",
				Content:    &content,
				ToolCallID: tr.ToolCallID,
			})
		}
		if m.Text != "" || len(m.ToolCalls) > 0 {
			wm := wireMessage{Role: string(m.Role)}
			if m.Text != "" {
				text := m.Text
				wm.Content = &text
			}
			for _, tc := range m.ToolCalls {
				wm.ToolCalls = append(wm.ToolCalls, wireToolCall{
					ID:   tc.ID,
					Type: "function",
					Function: wireFunctionCall{
						Name:      tc.Name,
						Arguments: tc.Arguments,
					},
				})
			}
			body.Messages = append(body.Messages, wm)
		}
	}

	for _, t := range req.Tools {
		body.Tools = append(body.Tools, wireTool{
			Type: "function",
			Function: wireFunction{
				Name:        t.Name,
				Description: t.Description,
				// Tool function.parameters are NOT strict mode — keep the caller's Required list
				// verbatim rather than forcing every property in (that forcing is a strict-only rule).
				Parameters: schemaToWire(t.Params, false),
			},
		})
	}

	if req.JSONSchema != nil {
		body.ResponseFormat = &wireResponseFormat{
			Type: "json_schema",
			JSONSchema: wireJSONSchema{
				Name: normalizeJSONSchemaName(req.JSONSchemaName, jsonSchemaNameDefault),
				// strict:true (below) requires every schemaToWire object node's `required` to list
				// every key in `properties` — pass strict=true so schemaToWire enforces that itself.
				Schema: schemaToWire(*req.JSONSchema, true),
				Strict: true,
			},
		}
	}

	return body
}

// jsonSchemaNameDefault is used when Request.JSONSchemaName is empty — OpenAI's Structured
// Outputs requires json_schema.name to be present (plan-driven default per validator finding:
// json_schema.name must be non-empty and match ^[a-zA-Z0-9_-]{1,64}$). The sanitizer itself
// (normalizeJSONSchemaName) now lives in llm.go so anthropic.go/gemini.go can share it.
const jsonSchemaNameDefault = "decision"

// schemaToWire converts the provider-neutral llm.Schema into a plain map suitable for
// json.Marshal as an OpenAI-compatible JSON-schema fragment. Property/key order in the marshaled
// output is alphabetical (encoding/json sorts map[string]any keys), which is what this package's
// golden tests assert against.
func schemaToWire(s Schema, strict bool) map[string]any {
	m := map[string]any{}
	if s.Nullable && s.Type != "" && s.Type != "null" {
		m["type"] = []string{s.Type, "null"}
	} else if s.Type != "" {
		m["type"] = s.Type
	}
	if s.Description != "" {
		m["description"] = s.Description
	}
	if len(s.Enum) > 0 {
		if s.Nullable {
			// Under strict Structured Outputs the model is grammar-constrained to `enum`; if `type`
			// admits null but `enum` doesn't, the intersection excludes null and a schema-conformant
			// null value becomes unrepresentable on the wire (validator finding: §4.2's `severity`).
			enumWithNull := make([]any, 0, len(s.Enum)+1)
			for _, v := range s.Enum {
				enumWithNull = append(enumWithNull, v)
			}
			enumWithNull = append(enumWithNull, nil)
			m["enum"] = enumWithNull
		} else {
			m["enum"] = s.Enum
		}
	}
	switch s.Type {
	case "object":
		props := map[string]any{}
		for name, propSchema := range s.Properties {
			props[name] = schemaToWire(propSchema, strict)
		}
		m["properties"] = props
		if strict {
			// OpenAI Structured Outputs under strict:true requires `required` to list EVERY key in
			// `properties` — optionality is expressed only via a null union in `type` (validator
			// blocker). Build the full sorted property-name list rather than trusting s.Required.
			required := make([]string, 0, len(s.Properties))
			for name := range s.Properties {
				required = append(required, name)
			}
			sort.Strings(required)
			m["required"] = required
		} else {
			required := s.Required
			if required == nil {
				required = []string{}
			}
			m["required"] = required
		}
		if s.AdditionalProperties != nil {
			m["additionalProperties"] = *s.AdditionalProperties
		} else {
			// strict json_schema mode (plan §4.1: "strict:true") requires this explicit on every
			// object node; false is the safe default absent an adapter override.
			m["additionalProperties"] = false
		}
	case "array":
		if s.Items != nil {
			m["items"] = schemaToWire(*s.Items, strict)
		}
	}
	return m
}

// --- wire response shapes ------------------------------------------------------------------

type chatCompletionResponse struct {
	Choices []struct {
		Message struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// mapFinishReason normalizes an OpenAI-compatible finish_reason onto the plan §4.1 neutral
// StopReason set. Backends outside the big providers sometimes emit reasons this package has
// never seen (custom vLLM builds, etc.) — those fall back to StopError rather than propagating an
// unrecognized string, so callers (toolloop.go) never have to branch on provider-specific text.
func mapFinishReason(reason string) string {
	switch reason {
	case "stop":
		return StopEndTurn
	case "tool_calls", "function_call":
		return StopToolUse
	case "length":
		return StopMaxTokens
	case "content_filter":
		return StopError
	case "":
		return StopEndTurn
	default:
		return StopError
	}
}

// Complete implements llm.Chat (plan §4.1). See the package doc for the error-mapping table this
// enforces: 429 sleeps Retry-After (via sentinel.RetryAfter/SleepCtx) then reports transient;
// 5xx reports transient; 4xx reports *PermanentError with a body excerpt; a malformed (non-JSON)
// 2xx body is retried once immediately, and reports *PermanentError if the retry is malformed too.
func (a *openaiChat) Complete(ctx context.Context, req Request) (Response, error) {
	wireReq := buildRequestBody(a.model, req)
	payload, err := json.Marshal(wireReq)
	if err != nil {
		return Response{}, &PermanentError{Reason: "llm/openai: encoding request: " + err.Error()}
	}

	for attempt := 1; ; attempt++ {
		resp, classifyErr := a.doOnce(ctx, payload)
		if classifyErr == nil {
			return resp, nil
		}
		var malformed *malformedJSONError
		if errors.As(classifyErr, &malformed) {
			if attempt == 1 {
				continue // "malformed JSON -> transient once then permanent": one immediate retry
			}
			return Response{}, &PermanentError{Reason: malformed.Error()}
		}
		return Response{}, classifyErr
	}
}

// doOnce performs exactly one HTTP round trip and classifies the outcome. A malformed-body result
// is returned as *malformedJSONError (unwrapped) so Complete can distinguish "retry once" from
// "already transient/permanent, return as-is"; every other failure path already returns the final
// *TransientError / *PermanentError.
func (a *openaiChat) doOnce(ctx context.Context, payload []byte) (Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.completionsURL, bytes.NewReader(payload))
	if err != nil {
		return Response{}, &PermanentError{Reason: "llm/openai: building request: " + err.Error()}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if a.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+a.apiKey)
	}

	httpResp, err := a.client.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return Response{}, fmt.Errorf("llm/openai: context ended: %w", ctx.Err())
		}
		return Response{}, &TransientError{Err: fmt.Errorf("llm/openai: request failed: %w", err)}
	}
	defer httpResp.Body.Close()
	body, readErr := io.ReadAll(httpResp.Body)
	if readErr != nil {
		return Response{}, &TransientError{Err: fmt.Errorf("llm/openai: reading response body: %w", readErr)}
	}

	switch {
	case httpResp.StatusCode == http.StatusTooManyRequests:
		wait := sentinel.RetryAfter(httpResp.Header, 60*time.Second)
		a.sleep(ctx, wait)
		return Response{}, &TransientError{Err: fmt.Errorf("llm/openai: rate limited (429), waited %s", wait)}
	case httpResp.StatusCode >= 500:
		return Response{}, &TransientError{Err: fmt.Errorf("llm/openai: server error %d: %s", httpResp.StatusCode, excerpt(body))}
	case httpResp.StatusCode >= 400:
		return Response{}, &PermanentError{Reason: fmt.Sprintf("llm/openai: client error %d: %s", httpResp.StatusCode, excerpt(body))}
	case isTransientNonSuccessStatus(httpResp.StatusCode):
		// Any other non-2xx (3xx redirects an http.Client did not itself follow, etc.) — treat as
		// transient rather than silently succeeding on a body that was never a real completion.
		return Response{}, &TransientError{Err: fmt.Errorf("llm/openai: unexpected status %d: %s", httpResp.StatusCode, excerpt(body))}
	}

	var wireResp chatCompletionResponse
	if err := json.Unmarshal(body, &wireResp); err != nil {
		return Response{}, &malformedJSONError{Provider: "openai", err: err}
	}
	if len(wireResp.Choices) == 0 {
		return Response{}, &malformedJSONError{Provider: "openai", err: errors.New("no choices in response")}
	}

	choice := wireResp.Choices[0]
	resp := Response{
		Text:       choice.Message.Content,
		Usage:      Usage{InputTokens: wireResp.Usage.PromptTokens, OutputTokens: wireResp.Usage.CompletionTokens},
		StopReason: mapFinishReason(choice.FinishReason),
	}
	for _, tc := range choice.Message.ToolCalls {
		resp.ToolCalls = append(resp.ToolCalls, ToolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		})
	}
	return resp, nil
}

// excerpt truncates a non-2xx response body to bodyExcerptCap bytes for inclusion in an error
// message (plan §4.1: "4xx permanent w/ body excerpt") — never the full body, which could be
// arbitrarily large or contain content not meant for a journal entry.
func excerpt(body []byte) string {
	s := strings.TrimSpace(string(body))
	if len(s) > bodyExcerptCap {
		return s[:bodyExcerptCap] + "...[truncated]"
	}
	return s
}
