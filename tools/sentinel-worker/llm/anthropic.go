// Package llm — this file is the Anthropic Messages API adapter (plan §4.1: "llm/anthropic.go:
// /v1/messages, native tools, tool_choice forcing the final decision tool"). Every provider-specific
// name, wire type, and endpoint lives in this file only (tenet 5) — llm.go's Chat/Request/Response
// stay provider-neutral.
//
// Pinned API version: "2023-06-01" (the anthropic-version header). This is Anthropic's stable
// Messages API version as of this adapter's authoring; bump it deliberately (and re-run the golden
// tests) rather than silently, since a version bump can change wire shape.
//
// Native structured-output path (§4.1): when req.JSONSchema is set, this adapter adds a synthetic
// tool (name = req.JSONSchemaName, or a fixed default when unset) whose input_schema IS the
// caller's JSONSchema, and asks the model to call it — "the decision as a forced tool call whose
// input IS the decision". Forcing interacts with req.Tools as follows, a deliberate reading of "the
// FINAL decision tool" given RunLoop (toolloop.go) reuses the same Request across every turn of a
// tool-use loop:
//   - req.Tools empty: this call has nothing else to do but decide, so tool_choice is forced
//     (`{"type":"tool","name":...}`) — the model MUST call the decision tool.
//   - req.Tools non-empty: the decision tool is offered ALONGSIDE the read-tools with
//     tool_choice "auto", so a mid-loop turn can still call get_issue/search_code/etc.; the model
//     calls the decision tool only once it is ready to conclude. Forcing it unconditionally here
//     would make read-tools uncallable for the whole loop whenever JSONSchema is set, which
//     contradicts §4.1's read-tools-for-TRIAGE section.
//
// Either way, when the model's response contains a tool_use block naming the decision tool, this
// adapter does NOT surface it as a ToolCall — it treats the tool_use input as the turn's Response.Text
// (matching the neutral Response doc: "Text ... once JSONSchema-constrained, structured-JSON
// reply") and reports StopReason as StopEndTurn, so toolloop.go's "no ToolCalls => finalize"
// branch and its schema validator run exactly as they do for every other provider. Any OTHER
// tool_use block (a genuine read-tool call) is still surfaced as a ToolCall.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/sentinel"
)

const (
	anthropicDefaultBaseURL = "https://api.anthropic.com"
	anthropicVersion        = "2023-06-01"
	// anthropicDecisionToolName is used when the caller leaves Request.JSONSchemaName unset.
	anthropicDecisionToolName = "submit_decision"
	// anthropicDefaultMaxTokens is emitted when the caller leaves Request.MaxTokens unset (<=0).
	// Anthropic's max_tokens field is required and rejects values below 1 with a 400; unlike
	// openai.go/gemini.go (whose analogous fields use `omitempty` so a zero is simply omitted from
	// an optional field), anthropicRequest.MaxTokens has no omitempty because Anthropic's field is
	// mandatory — so a caller-unset 0 must be defaulted here rather than passed through.
	anthropicDefaultMaxTokens = 4096
)

func init() {
	RegisterProvider("anthropic", func(cfg Config) (Chat, error) {
		if strings.TrimSpace(cfg.Model) == "" {
			return nil, errors.New("llm/anthropic: Config.Model is required")
		}
		return newAnthropicChat(cfg), nil
	})
}

// anthropicChat implements Chat against the Anthropic Messages API.
type anthropicChat struct {
	httpClient  *http.Client
	baseURL     string
	messagesURL string
	apiKey      string
	model       string
	sleep       sentinel.CtxSleepFunc
}

func newAnthropicChat(cfg Config) *anthropicChat {
	base := cfg.BaseURL
	if base == "" {
		base = anthropicDefaultBaseURL
	}
	base, messagesURL := resolveAnthropicURL(base)
	return &anthropicChat{
		httpClient:  &http.Client{Timeout: 120 * time.Second},
		baseURL:     base,
		messagesURL: messagesURL,
		apiKey:      cfg.APIKey,
		model:       cfg.Model,
		sleep:       sentinel.SleepCtx,
	}
}

// resolveAnthropicURL normalizes a caller-supplied base URL onto the Messages endpoint, mirroring
// openai.go's resolveOpenAIURLs. Unlike OpenAI's convention, Anthropic's canonical API root
// ("https://api.anthropic.com") has no "/v1" segment — the "/v1" lives only in the endpoint path
// ("/v1/messages"). main.go's LLMBaseURL doc documents LLM_BASE_URL as the provider's API ROOT,
// which for some callers (or a value copied from another provider's convention) may already end in
// "/v1"; appending "/v1/messages" to that verbatim would produce
// "https://api.anthropic.com/v1/v1/messages" (a 404 classified Permanent). Strip a trailing "/v1"
// (case-insensitive) after trimming a trailing slash, then append the canonical path.
func resolveAnthropicURL(base string) (normalizedBase, messagesURL string) {
	base = strings.TrimRight(base, "/")
	lower := strings.ToLower(base)
	if strings.HasSuffix(lower, "/v1") {
		base = base[:len(base)-len("/v1")]
	}
	return base, base + "/v1/messages"
}

// --- wire types (Anthropic-specific; never referenced outside this file) --------------------------

type anthropicContentBlock struct {
	Type string `json:"type"`

	// type "text"
	Text string `json:"text,omitempty"`

	// type "tool_use" (assistant turn asking for a tool call, or our synthetic decision tool)
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// type "tool_result" (our reply to a prior tool_use)
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
}

type anthropicMessage struct {
	Role    string                  `json:"role"` // "user" | "assistant"
	Content []anthropicContentBlock `json:"content"`
}

// Type is `any` rather than `string` so toAnthropicSchema can emit standard-JSON-Schema
// nullability (["<type>","null"]) for a Nullable neutral Schema, mirroring schemaToWire's
// (openai.go) []string{s.Type, "null"} form — Anthropic's input_schema is plain JSON Schema, so
// this wire shape is honored the same way OpenAI's is.
// Enum is []any (not []string) so a Nullable node can append a real JSON null member — see
// toAnthropicSchema. The neutral validator (§4.2) treats the wire string "null" as an ordinary
// enum value, not the JSON null literal, so emitting it as a string makes a schema-conformant
// null decision unrepresentable on the wire (validator finding, mirrored from schemaToWire in
// openai.go).
type anthropicSchema struct {
	Type                 any                         `json:"type,omitempty"`
	Properties           map[string]*anthropicSchema `json:"properties,omitempty"`
	Required             []string                    `json:"required,omitempty"`
	Enum                 []any                       `json:"enum,omitempty"`
	Items                *anthropicSchema            `json:"items,omitempty"`
	Description          string                      `json:"description,omitempty"`
	AdditionalProperties *bool                       `json:"additionalProperties,omitempty"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema anthropicSchema `json:"input_schema"`
}

type anthropicToolChoice struct {
	Type string `json:"type"` // "auto" | "tool"
	Name string `json:"name,omitempty"`
}

type anthropicRequest struct {
	Model      string               `json:"model"`
	MaxTokens  int                  `json:"max_tokens"`
	System     string               `json:"system,omitempty"`
	Messages   []anthropicMessage   `json:"messages"`
	Tools      []anthropicTool      `json:"tools,omitempty"`
	ToolChoice *anthropicToolChoice `json:"tool_choice,omitempty"`
}

type anthropicResponse struct {
	ID         string                  `json:"id"`
	Type       string                  `json:"type"`
	Role       string                  `json:"role"`
	Content    []anthropicContentBlock `json:"content"`
	Model      string                  `json:"model"`
	StopReason string                  `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

type anthropicErrorEnvelope struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// --- Chat implementation ----------------------------------------------------------------------

func (c *anthropicChat) Complete(ctx context.Context, req Request) (Response, error) {
	decisionTool := ""
	if req.JSONSchema != nil {
		decisionTool = normalizeJSONSchemaName(req.JSONSchemaName, anthropicDecisionToolName)
	}

	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = anthropicDefaultMaxTokens
	}

	body := anthropicRequest{
		Model:     c.model,
		MaxTokens: maxTokens,
		System:    req.System,
		Messages:  toAnthropicMessages(req.Messages),
	}
	for _, t := range req.Tools {
		body.Tools = append(body.Tools, anthropicTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: toAnthropicSchema(t.Params),
		})
	}
	if decisionTool != "" {
		body.Tools = append(body.Tools, anthropicTool{
			Name:        decisionTool,
			Description: "Submit the final structured decision for this turn.",
			InputSchema: toAnthropicSchema(*req.JSONSchema),
		})
		if len(req.Tools) == 0 {
			// Nothing else this turn can do but decide (see file doc comment).
			body.ToolChoice = &anthropicToolChoice{Type: "tool", Name: decisionTool}
		} else {
			body.ToolChoice = &anthropicToolChoice{Type: "auto"}
		}
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return Response{}, &PermanentError{Reason: "llm/anthropic: encode request: " + err.Error()}
	}

	for attempt := 1; ; attempt++ {
		resp, err := c.doOnce(ctx, payload, decisionTool)
		if err == nil {
			return resp, nil
		}
		var malformed *malformedJSONError
		if errors.As(err, &malformed) {
			if attempt == 1 {
				continue // malformed 2xx body: one immediate retry, then Permanent
			}
			return Response{}, &PermanentError{Reason: malformed.Error()}
		}
		return Response{}, err
	}
}

// doOnce performs exactly one HTTP round trip. A malformed 2xx body is returned as
// *malformedJSONError so Complete can retry once before treating it as Permanent; every other
// failure path already returns a final *TransientError / *PermanentError.
func (c *anthropicChat) doOnce(ctx context.Context, payload []byte, decisionTool string) (Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.messagesURL, bytes.NewReader(payload))
	if err != nil {
		return Response{}, &PermanentError{Reason: "llm/anthropic: build request: " + err.Error()}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", c.apiKey)
	httpReq.Header.Set("anthropic-version", anthropicVersion)

	httpResp, err := c.httpClient.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return Response{}, fmt.Errorf("llm/anthropic: context ended: %w", ctx.Err())
		}
		return Response{}, &TransientError{Err: fmt.Errorf("llm/anthropic: request failed: %w", err)}
	}
	defer httpResp.Body.Close()

	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return Response{}, &TransientError{Err: fmt.Errorf("llm/anthropic: read response: %w", err)}
	}

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return Response{}, classifyAnthropicError(ctx, c.sleep, httpResp.StatusCode, httpResp.Header, respBody)
	}

	var parsed anthropicResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return Response{}, &malformedJSONError{Provider: "anthropic", err: err}
	}
	if len(parsed.Content) == 0 {
		// Unify "no usable content" semantics across adapters (openai's is the baseline: a 2xx body
		// with nothing to work with is treated as malformed — retried once, then Permanent — not
		// silently reported as success with an empty Text).
		return Response{}, &malformedJSONError{Provider: "anthropic", err: errors.New("no content blocks in response")}
	}

	return fromAnthropicResponse(parsed, decisionTool), nil
}

func toAnthropicMessages(msgs []Msg) []anthropicMessage {
	out := make([]anthropicMessage, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case RoleUser:
			blocks := []anthropicContentBlock{}
			if m.Text != "" {
				blocks = append(blocks, anthropicContentBlock{Type: "text", Text: m.Text})
			}
			for _, tr := range m.ToolResults {
				blocks = append(blocks, anthropicContentBlock{
					Type:      "tool_result",
					ToolUseID: tr.ToolCallID,
					Content:   tr.Content,
					IsError:   tr.IsError,
				})
			}
			if len(blocks) == 0 {
				continue // Anthropic rejects an empty content array with a hard 400 — see below.
			}
			out = append(out, anthropicMessage{Role: "user", Content: blocks})
		case RoleTool:
			// RunLoop (toolloop.go) puts tool results in a RoleTool Msg; the Anthropic wire format
			// has no "tool" role — tool_result blocks are carried in a "user" message.
			blocks := make([]anthropicContentBlock, 0, len(m.ToolResults))
			for _, tr := range m.ToolResults {
				blocks = append(blocks, anthropicContentBlock{
					Type:      "tool_result",
					ToolUseID: tr.ToolCallID,
					Content:   tr.Content,
					IsError:   tr.IsError,
				})
			}
			if len(blocks) == 0 {
				continue
			}
			out = append(out, anthropicMessage{Role: "user", Content: blocks})
		case RoleAssistant:
			blocks := []anthropicContentBlock{}
			if m.Text != "" {
				blocks = append(blocks, anthropicContentBlock{Type: "text", Text: m.Text})
			}
			for _, tc := range m.ToolCalls {
				input := json.RawMessage(tc.Arguments)
				if len(input) == 0 || !json.Valid(input) {
					// openai carries ToolCall.Arguments as a raw, unvalidated model-produced wire
					// string; a malformed value must not hard-fail request encoding here, or one
					// bad tool-call argument poisons every subsequent RunLoop fallback turn.
					input = json.RawMessage("{}")
				}
				blocks = append(blocks, anthropicContentBlock{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  tc.Name,
					Input: input,
				})
			}
			// A message with no text and no tool calls (e.g. toolloop.go's finalizeDecision
			// appending Msg{Role: RoleAssistant, Text: ""} after a safety/MAX_TOKENS turn) would
			// otherwise wire as `"content":[]`, which Anthropic rejects with a hard 400 — turning a
			// recoverable schema-validation re-ask into a non-transient provider error. Skip it.
			if len(blocks) == 0 {
				continue
			}
			out = append(out, anthropicMessage{Role: "assistant", Content: blocks})
		}
	}
	return out
}

// toAnthropicSchema maps the neutral Schema onto Anthropic's plain-JSON-Schema input_schema shape,
// including Nullable (llm.go documents Nullable as adapter-mapped; this file previously dropped
// it silently). A Nullable node with a concrete Type gets `type: [<type>, "null"]`; a Nullable node
// with an Enum gets "null" appended to the enum (both mirroring schemaToWire in openai.go, adapted
// to Anthropic's plain map/struct wire shape rather than OpenAI's map[string]any).
func toAnthropicSchema(s Schema) anthropicSchema {
	var wireType any
	if s.Type != "" {
		if s.Nullable && s.Type != "null" {
			wireType = []string{s.Type, "null"}
		} else {
			wireType = s.Type
		}
	}
	var enum []any
	if len(s.Enum) > 0 {
		enum = make([]any, 0, len(s.Enum)+1)
		for _, v := range s.Enum {
			enum = append(enum, v)
		}
		if s.Nullable {
			// Append a real JSON null, not the string "null" — see the anthropicSchema.Enum doc.
			enum = append(enum, nil)
		}
	}
	out := anthropicSchema{
		Type:                 wireType,
		Required:             s.Required,
		Enum:                 enum,
		Description:          s.Description,
		AdditionalProperties: s.AdditionalProperties,
	}
	if s.Properties != nil {
		out.Properties = make(map[string]*anthropicSchema, len(s.Properties))
		for name, prop := range s.Properties {
			p := toAnthropicSchema(prop)
			out.Properties[name] = &p
		}
	}
	if s.Items != nil {
		items := toAnthropicSchema(*s.Items)
		out.Items = &items
	}
	return out
}

// fromAnthropicResponse converts the wire response to the neutral Response. decisionTool, when
// non-empty, names the synthetic decision tool this Complete call offered — a tool_use block with
// that name is unwrapped into Response.Text (its input, verbatim JSON) rather than surfaced as a
// ToolCall, per the file doc comment.
func fromAnthropicResponse(r anthropicResponse, decisionTool string) Response {
	resp := Response{
		Usage: Usage{InputTokens: r.Usage.InputTokens, OutputTokens: r.Usage.OutputTokens},
	}
	sawDecision := false
	for _, block := range r.Content {
		switch block.Type {
		case "text":
			if sawDecision {
				// A decision tool_use block already won; discard prose emitted before or after
				// it so Response.Text stays the raw decision JSON (see the doc comment above).
				continue
			}
			resp.Text += block.Text
		case "tool_use":
			if decisionTool != "" && block.Name == decisionTool {
				resp.Text = string(block.Input)
				sawDecision = true
				continue
			}
			resp.ToolCalls = append(resp.ToolCalls, ToolCall{
				ID:        block.ID,
				Name:      block.Name,
				Arguments: string(block.Input),
			})
		}
	}
	if sawDecision {
		resp.StopReason = StopEndTurn
	} else {
		resp.StopReason = mapAnthropicStopReason(r.StopReason)
	}
	return resp
}

func mapAnthropicStopReason(sr string) string {
	switch sr {
	case "end_turn", "stop_sequence":
		return StopEndTurn
	case "tool_use":
		return StopToolUse
	case "max_tokens":
		return StopMaxTokens
	default:
		return StopError
	}
}

// classifyAnthropicError maps an HTTP error response to the neutral error taxonomy (plan §2.4),
// matching openai.go's doOnce mapping: 429 honors Retry-After (sleeping via sleep, capped by
// sentinel.RetryAfter) then reports Transient; 5xx (including Anthropic's 529 "overloaded") is
// Transient; everything else (400 invalid request, 401/403 auth, 404 not found, 422) is a
// *PermanentError with a body excerpt — PermanentError's doc: "Callers (jobs/*) journal this as
// failed, not retried."
func classifyAnthropicError(ctx context.Context, sleep sentinel.CtxSleepFunc, status int, header http.Header, body []byte) error {
	var env anthropicErrorEnvelope
	_ = json.Unmarshal(body, &env)
	msg := env.Error.Message
	if msg == "" {
		msg = excerpt(body)
	}
	if status == 429 {
		wait := sentinel.RetryAfter(header, 60*time.Second)
		sleep(ctx, wait)
		return &TransientError{Err: fmt.Errorf("llm/anthropic: rate limited (429), waited %s", wait)}
	}
	if status == 529 || status >= 500 || isTransientNonSuccessStatus(status) {
		return &TransientError{Err: fmt.Errorf("llm/anthropic: status %d: %s", status, msg)}
	}
	return &PermanentError{Reason: fmt.Sprintf("llm/anthropic: status %d: %s", status, msg)}
}
