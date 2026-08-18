// Package llm — this file is the Google Gemini (GenAI) adapter (plan §4.1: "llm/gemini.go:
// generateContent, functionDeclarations + responseSchema"). Every provider-specific name, wire
// type, and endpoint lives in this file only (tenet 5) — llm.go's Chat/Request/Response stay
// provider-neutral.
//
// Endpoint: POST {base}/v1beta/models/{model}:generateContent. Auth: this adapter sends the API
// key as the `x-goog-api-key` header (Gemini also accepts `?key=` on the URL — the header is used
// here deliberately so the key never lands in a URL that gets logged/cached by an intermediary;
// document this choice if a later stage adds the query-param form for compatibility).
//
// Native structured-output path: unlike Anthropic (which has no dedicated JSON-response mode and
// so needs a forced tool call, see anthropic.go), Gemini supports a native structured-output mode
// directly on generationConfig: `responseSchema` + `responseMimeType: "application/json"`. When
// req.JSONSchema is set, this adapter sets both on every call, alongside any req.Tools as
// functionDeclarations — the model's final text is then already the schema-shaped JSON that
// toolloop.go validates, with no response-shape unwrapping needed (contrast fromAnthropicResponse's
// decision-tool unwrap).
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/sentinel"
)

const geminiDefaultBaseURL = "https://generativelanguage.googleapis.com"

// geminiDecisionFunctionName is the synthetic functionDeclaration name used to carry the final
// structured decision when req.Tools is also set — Gemini's generateContent API rejects a request
// that sets both functionDeclarations and generationConfig.responseSchema/responseMimeType (400
// INVALID_ARGUMENT, "Function calling with a response mime type: ... is unsupported"), so in that
// combination (every §4.1 TRIAGE turn, since RunLoop reuses the same Request across the loop —
// see toolloop.go RunLoop/finalizeDecision) the decision is instead modeled as a forced-by-prompt
// extra function declaration whose args ARE the decision, mirroring anthropic.go's forced-tool
// approach. The native responseSchema path is reserved for tool-free turns.
const geminiDecisionFunctionName = "submit_decision"

func init() {
	RegisterProvider("gemini", func(cfg Config) (Chat, error) {
		if strings.TrimSpace(cfg.Model) == "" {
			return nil, errors.New("llm/gemini: Config.Model is required")
		}
		return newGeminiChat(cfg), nil
	})
}

// geminiChat implements Chat against the Gemini generateContent API.
type geminiChat struct {
	httpClient *http.Client
	baseURL    string
	apiKey     string
	model      string
	sleep      sentinel.CtxSleepFunc
}

func newGeminiChat(cfg Config) *geminiChat {
	base := cfg.BaseURL
	if base == "" {
		base = geminiDefaultBaseURL
	}
	return &geminiChat{
		httpClient: &http.Client{Timeout: 120 * time.Second},
		baseURL:    resolveGeminiBaseURL(base),
		apiKey:     cfg.APIKey,
		model:      cfg.Model,
		sleep:      sentinel.SleepCtx,
	}
}

// resolveGeminiBaseURL normalizes a caller-supplied base URL for the generateContent path, mirroring
// openai.go's resolveOpenAIURLs. Gemini's endpoint path already begins with "/v1beta"
// ("/v1beta/models/{model}:generateContent"); main.go's LLMBaseURL doc documents LLM_BASE_URL as the
// provider's API ROOT, which for some callers (or a value copied from another provider's convention)
// may already end in "/v1beta". Appending the path verbatim in that case would produce
// "https://generativelanguage.googleapis.com/v1beta/v1beta/models/..." (a 404 classified Permanent).
// Strip a trailing "/v1beta" (case-insensitive) after trimming a trailing slash.
func resolveGeminiBaseURL(base string) string {
	base = strings.TrimRight(base, "/")
	lower := strings.ToLower(base)
	if strings.HasSuffix(lower, "/v1beta") {
		base = base[:len(base)-len("/v1beta")]
	}
	return base
}

// --- wire types (Gemini-specific; never referenced outside this file) -----------------------------

type geminiPart struct {
	Text string `json:"text,omitempty"`

	FunctionCall *geminiFunctionCall `json:"functionCall,omitempty"`

	FunctionResponse *geminiFunctionResponse `json:"functionResponse,omitempty"`
}

type geminiFunctionCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

type geminiFunctionResponse struct {
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"` // "user" | "model"
	Parts []geminiPart `json:"parts"`
}

type geminiSchema struct {
	Type        string                   `json:"type,omitempty"`
	Properties  map[string]*geminiSchema `json:"properties,omitempty"`
	Required    []string                 `json:"required,omitempty"`
	Enum        []string                 `json:"enum,omitempty"`
	Items       *geminiSchema            `json:"items,omitempty"`
	Description string                   `json:"description,omitempty"`
	Nullable    bool                     `json:"nullable,omitempty"`
}

type geminiFunctionDeclaration struct {
	Name        string        `json:"name"`
	Description string        `json:"description,omitempty"`
	Parameters  *geminiSchema `json:"parameters,omitempty"`
}

type geminiTool struct {
	FunctionDeclarations []geminiFunctionDeclaration `json:"functionDeclarations"`
}

type geminiGenerationConfig struct {
	MaxOutputTokens  int           `json:"maxOutputTokens,omitempty"`
	ResponseMimeType string        `json:"responseMimeType,omitempty"`
	ResponseSchema   *geminiSchema `json:"responseSchema,omitempty"`
}

type geminiSystemInstruction struct {
	Parts []geminiPart `json:"parts"`
}

type geminiRequest struct {
	Contents          []geminiContent          `json:"contents"`
	Tools             []geminiTool             `json:"tools,omitempty"`
	GenerationConfig  *geminiGenerationConfig  `json:"generationConfig,omitempty"`
	SystemInstruction *geminiSystemInstruction `json:"systemInstruction,omitempty"`
}

type geminiCandidate struct {
	Content      geminiContent `json:"content"`
	FinishReason string        `json:"finishReason"`
}

type geminiUsageMetadata struct {
	PromptTokenCount     int `json:"promptTokenCount"`
	CandidatesTokenCount int `json:"candidatesTokenCount"`
	// ThoughtsTokenCount is Gemini's thinking-token count: billed as output but, unlike openai's
	// completion_tokens and anthropic's output_tokens (which both fold reasoning into their single
	// output figure), EXCLUDED from CandidatesTokenCount — so a caller summing only
	// CandidatesTokenCount into Usage.OutputTokens under-reports real spend and under-enforces
	// Caps.MaxOutputTokens on Gemini specifically. fromGeminiResponse sums it into OutputTokens.
	ThoughtsTokenCount int `json:"thoughtsTokenCount"`
}

type geminiResponse struct {
	Candidates    []geminiCandidate   `json:"candidates"`
	UsageMetadata geminiUsageMetadata `json:"usageMetadata"`
}

type geminiErrorEnvelope struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

// --- Chat implementation ----------------------------------------------------------------------

func (c *geminiChat) Complete(ctx context.Context, req Request) (Response, error) {
	decisionFn := ""
	if req.JSONSchema != nil {
		decisionFn = normalizeJSONSchemaName(req.JSONSchemaName, geminiDecisionFunctionName)
	}

	decls := make([]geminiFunctionDeclaration, 0, len(req.Tools)+1)
	for _, t := range req.Tools {
		decls = append(decls, geminiFunctionDeclaration{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  geminiFunctionParameters(t.Params),
		})
	}

	genCfg := &geminiGenerationConfig{MaxOutputTokens: req.MaxTokens}
	if decisionFn != "" {
		if len(req.Tools) > 0 {
			// Tools+JSONSchema together: responseSchema/responseMimeType is unsupported alongside
			// functionDeclarations (see geminiDecisionFunctionName doc) — declare the decision as an
			// extra function instead, and rely on the system/user prompt to ask the model to call it
			// when ready to conclude (toolloop.go's RunLoop still only finalizes once ToolCalls is
			// empty, so a decision-function call round-trips through the normal tool-call path and
			// fromGeminiResponse unwraps it below).
			decls = append(decls, geminiFunctionDeclaration{
				Name:        decisionFn,
				Description: "Call this with the final structured decision for this turn when ready to conclude.",
				Parameters:  geminiFunctionParameters(*req.JSONSchema),
			})
		} else {
			schema := toGeminiSchema(*req.JSONSchema)
			genCfg.ResponseSchema = &schema
			genCfg.ResponseMimeType = "application/json"
		}
	}

	// declaredNames is every functionDeclaration name this call is offering the model, so
	// toGeminiContents can resolve a prior turn's synthesized ToolCall.ID back to the exact
	// declared function name a functionResponse must echo (see geminiFunctionNameFromID) rather
	// than guessing from the ID's shape alone.
	declaredNames := make([]string, 0, len(decls))
	for _, d := range decls {
		declaredNames = append(declaredNames, d.Name)
	}

	body := geminiRequest{
		Contents: toGeminiContents(req.Messages, declaredNames),
	}
	if req.System != "" {
		body.SystemInstruction = &geminiSystemInstruction{Parts: []geminiPart{{Text: req.System}}}
	}
	if len(decls) > 0 {
		body.Tools = []geminiTool{{FunctionDeclarations: decls}}
	}
	body.GenerationConfig = genCfg

	payload, err := json.Marshal(body)
	if err != nil {
		return Response{}, &PermanentError{Reason: "llm/gemini: encode request: " + err.Error()}
	}

	url := fmt.Sprintf("%s/v1beta/models/%s:generateContent", c.baseURL, c.model)

	for attempt := 1; ; attempt++ {
		resp, err := c.doOnce(ctx, url, payload, decisionFn)
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
func (c *geminiChat) doOnce(ctx context.Context, url string, payload []byte, decisionFn string) (Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return Response{}, &PermanentError{Reason: "llm/gemini: build request: " + err.Error()}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-goog-api-key", c.apiKey)

	httpResp, err := c.httpClient.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return Response{}, fmt.Errorf("llm/gemini: context ended: %w", ctx.Err())
		}
		return Response{}, &TransientError{Err: fmt.Errorf("llm/gemini: request failed: %w", err)}
	}
	defer httpResp.Body.Close()

	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return Response{}, &TransientError{Err: fmt.Errorf("llm/gemini: read response: %w", err)}
	}

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return Response{}, classifyGeminiError(ctx, c.sleep, httpResp.StatusCode, httpResp.Header, respBody)
	}

	var parsed geminiResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return Response{}, &malformedJSONError{Provider: "gemini", err: err}
	}
	if len(parsed.Candidates) == 0 {
		// Unify "no usable content" semantics across adapters (openai's is the baseline: a 2xx body
		// with nothing to work with is treated as malformed — retried once, then Permanent — not
		// silently reported as a successful StopError turn).
		return Response{}, &malformedJSONError{Provider: "gemini", err: errors.New("no candidates in response")}
	}

	return fromGeminiResponse(parsed, decisionFn), nil
}

func toGeminiContents(msgs []Msg, declaredNames []string) []geminiContent {
	out := make([]geminiContent, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case RoleUser:
			parts := []geminiPart{}
			if m.Text != "" {
				parts = append(parts, geminiPart{Text: m.Text})
			}
			for _, tr := range m.ToolResults {
				parts = append(parts, toolResultToGeminiPart(tr, declaredNames))
			}
			if len(parts) == 0 {
				continue // Gemini rejects an empty parts array with a hard 400 — see below.
			}
			out = append(out, geminiContent{Role: "user", Parts: parts})
		case RoleTool:
			// Gemini has no dedicated "tool" role; functionResponse parts are carried in a "user"
			// content entry, mirroring the anthropic.go tool_result-in-user-message convention.
			parts := make([]geminiPart, 0, len(m.ToolResults))
			for _, tr := range m.ToolResults {
				parts = append(parts, toolResultToGeminiPart(tr, declaredNames))
			}
			if len(parts) == 0 {
				continue
			}
			out = append(out, geminiContent{Role: "user", Parts: parts})
		case RoleAssistant:
			parts := []geminiPart{}
			if m.Text != "" {
				parts = append(parts, geminiPart{Text: m.Text})
			}
			for _, tc := range m.ToolCalls {
				args := json.RawMessage(tc.Arguments)
				if len(args) == 0 || !json.Valid(args) {
					// openai carries ToolCall.Arguments as a raw, unvalidated model-produced wire
					// string; a malformed value must not hard-fail request encoding here, or one
					// bad tool-call argument poisons every subsequent RunLoop fallback turn.
					args = json.RawMessage("{}")
				}
				parts = append(parts, geminiPart{FunctionCall: &geminiFunctionCall{Name: tc.Name, Args: args}})
			}
			// A message with no text and no tool calls (e.g. toolloop.go's finalizeDecision
			// appending Msg{Role: RoleAssistant, Text: ""} after a safety/MAX_TOKENS turn) would
			// otherwise wire as `"parts":[]`, which Gemini rejects with a hard 400 — turning a
			// recoverable schema-validation re-ask into a non-transient provider error. Skip it.
			if len(parts) == 0 {
				continue
			}
			out = append(out, geminiContent{Role: "model", Parts: parts})
		}
	}
	return out
}

// toolResultToGeminiPart wraps a neutral ToolResult in Gemini's functionResponse shape, which
// requires a JSON object (not a bare string) as `response` — we wrap the tool's (possibly
// truncated, possibly plain-text) Content under a stable "content" key, and note an error via a
// sibling "isError" key so a failed tool run round-trips distinguishably, same intent as
// anthropic.go's IsError block field.
//
// Gemini matches functionResponse.name against the functionCall/functionDeclaration name, NOT
// against any call-correlation ID — so this must NOT be tr.ToolCallID verbatim. ToolResult (llm.go)
// carries only ToolCallID, which fromGeminiResponse synthesizes as "<function-name>-<ordinal>"
// (see below); geminiFunctionNameFromID reverses that to recover the declared function name,
// resolving against declaredNames (this turn's own functionDeclaration names) rather than a blind
// dash-split heuristic.
func toolResultToGeminiPart(tr ToolResult, declaredNames []string) geminiPart {
	wrapped, _ := json.Marshal(struct {
		Content string `json:"content"`
		IsError bool   `json:"isError,omitempty"`
	}{Content: tr.Content, IsError: tr.IsError})
	return geminiPart{FunctionResponse: &geminiFunctionResponse{Name: geminiFunctionNameFromID(tr.ToolCallID, declaredNames), Response: wrapped}}
}

// geminiFunctionNameFromID recovers the declared Gemini function name a functionResponse.name
// must echo, from a ToolCallID that fromGeminiResponse synthesized as "<function-name>-<ordinal>".
//
// A blind "strip the last -<digits>" heuristic corrupts a tool whose DECLARED NAME itself ends in
// a dash-digit (e.g. "resolve-2"): an ID that happens to equal that declared name verbatim (no
// ordinal appended — e.g. constructed directly rather than round-tripped through
// fromGeminiResponse) gets mis-parsed as name "resolve" + a spurious ordinal "2", corrupting the
// wire function name. Resolving against declaredNames fixes this: an exact match against a
// declared name always wins outright; otherwise the longest declared name that id is
// "<name>-<ordinal>"-shaped against wins (longest, so a declared "resolve-2" is preferred over a
// declared "resolve" for id "resolve-2-0"). Only when neither matches does this fall back to the
// old dash-split heuristic, so an ID from another adapter leaking through (or any other ID this
// turn's declaredNames doesn't explain) still gets a best-effort name instead of being dropped.
func geminiFunctionNameFromID(id string, declaredNames []string) string {
	for _, name := range declaredNames {
		if id == name {
			return name
		}
	}
	best := ""
	for _, name := range declaredNames {
		if strings.HasPrefix(id, name+"-") && len(name) > len(best) {
			best = name
		}
	}
	if best != "" {
		return best
	}
	i := strings.LastIndexByte(id, '-')
	if i < 0 || i == len(id)-1 {
		return id
	}
	if _, err := strconv.Atoi(id[i+1:]); err != nil {
		return id
	}
	return id[:i]
}

// geminiFunctionParameters returns s wrapped as a *geminiSchema for geminiFunctionDeclaration.
// Parameters, or nil when s is the neutral Schema zero value (a ToolDef with no declared
// parameters) — Parameters used to be a plain geminiSchema struct value with a dead `omitempty`
// (encoding/json never treats a non-pointer struct as "empty", so an always-present
// `"parameters":{}` was silently emitted for every declaration regardless of ToolDef.Params);
// making the field a pointer set only here restores the documented omitempty behavior.
func geminiFunctionParameters(s Schema) *geminiSchema {
	if reflect.DeepEqual(s, Schema{}) {
		return nil
	}
	out := toGeminiSchema(s)
	return &out
}

func toGeminiSchema(s Schema) geminiSchema {
	out := geminiSchema{
		Type:        geminiSchemaType(s.Type),
		Required:    s.Required,
		Enum:        s.Enum,
		Description: s.Description,
		Nullable:    s.Nullable,
	}
	if s.Properties != nil {
		out.Properties = make(map[string]*geminiSchema, len(s.Properties))
		for name, prop := range s.Properties {
			p := toGeminiSchema(prop)
			out.Properties[name] = &p
		}
	}
	if s.Items != nil {
		items := toGeminiSchema(*s.Items)
		out.Items = &items
	}
	return out
}

// geminiSchemaType uppercases the neutral Schema.Type into Gemini's OpenAPI-style type enum
// (STRING/NUMBER/BOOLEAN/OBJECT/ARRAY); Gemini has no bare "null" type (nullability is the separate
// `nullable` flag), so a neutral "null" node (only ever a leaf under Nullable, per llm.go's Schema
// doc) maps to no explicit type — Gemini treats a schema with `nullable: true` and no type as
// unconstrained-but-nullable, matching the neutral node's own leaf-only usage.
func geminiSchemaType(t string) string {
	switch t {
	case "object":
		return "OBJECT"
	case "string":
		return "STRING"
	case "number":
		return "NUMBER"
	case "boolean":
		return "BOOLEAN"
	case "array":
		return "ARRAY"
	default:
		return ""
	}
}

// fromGeminiResponse converts the wire response to the neutral Response. decisionFn, when
// non-empty, names the synthetic decision function this Complete call declared for the
// Tools+JSONSchema combination (see geminiDecisionFunctionName) — a functionCall part with that
// name is unwrapped into Response.Text (its args, verbatim JSON) rather than surfaced as a
// ToolCall, mirroring fromAnthropicResponse's decision-tool unwrap.
func fromGeminiResponse(r geminiResponse, decisionFn string) Response {
	resp := Response{
		// ThoughtsTokenCount is billed as output but excluded from CandidatesTokenCount (see the
		// field's doc comment) — sum it in so Usage.OutputTokens matches openai's completion_tokens
		// and anthropic's output_tokens, both of which already fold reasoning into one figure.
		Usage: Usage{
			InputTokens:  r.UsageMetadata.PromptTokenCount,
			OutputTokens: r.UsageMetadata.CandidatesTokenCount + r.UsageMetadata.ThoughtsTokenCount,
		},
	}
	// doOnce now guards len(r.Candidates)==0 as malformed (retry once, then Permanent) before this
	// is ever called, unifying "no usable content" onto openai's semantic — see doOnce.
	cand := r.Candidates[0]
	sawDecision := false
	for _, part := range cand.Content.Parts {
		switch {
		case part.FunctionCall != nil:
			args := part.FunctionCall.Args
			if len(args) == 0 {
				args = json.RawMessage("{}")
			}
			if decisionFn != "" && part.FunctionCall.Name == decisionFn {
				// Assign, don't append: prose emitted alongside the decision call (before or
				// after, in the same turn) must not corrupt the decision JSON the loop validates.
				resp.Text = string(args)
				sawDecision = true
				continue
			}
			resp.ToolCalls = append(resp.ToolCalls, ToolCall{
				// Gemini function calls carry no call ID on the wire; toolloop.go correlates a
				// ToolResult back by ToolCallID, so we synthesize one from the function name plus
				// its ordinal position among this turn's calls (stable within one Response).
				ID:        fmt.Sprintf("%s-%d", part.FunctionCall.Name, len(resp.ToolCalls)),
				Name:      part.FunctionCall.Name,
				Arguments: string(args),
			})
		case part.Text != "":
			if sawDecision {
				continue
			}
			resp.Text += part.Text
		}
	}
	if sawDecision {
		resp.StopReason = StopEndTurn
	} else {
		resp.StopReason = mapGeminiFinishReason(cand.FinishReason, len(resp.ToolCalls) > 0)
	}
	return resp
}

func mapGeminiFinishReason(fr string, hadToolCalls bool) string {
	switch fr {
	case "STOP":
		if hadToolCalls {
			return StopToolUse
		}
		return StopEndTurn
	case "MAX_TOKENS":
		return StopMaxTokens
	case "":
		if hadToolCalls {
			return StopToolUse
		}
		return StopEndTurn
	default:
		return StopError
	}
}

// classifyGeminiError maps an HTTP error response to the neutral error taxonomy (plan §2.4),
// matching openai.go's doOnce mapping: 429 (RESOURCE_EXHAUSTED) honors Retry-After (sleeping via
// sleep, capped by sentinel.RetryAfter) then reports Transient; 5xx is Transient; everything else
// (400 INVALID_ARGUMENT, 401/403 PERMISSION_DENIED/UNAUTHENTICATED, 404 NOT_FOUND) is a
// *PermanentError with a body excerpt — PermanentError's doc: "Callers (jobs/*) journal this as
// failed, not retried."
func classifyGeminiError(ctx context.Context, sleep sentinel.CtxSleepFunc, status int, header http.Header, body []byte) error {
	var env geminiErrorEnvelope
	_ = json.Unmarshal(body, &env)
	msg := env.Error.Message
	if msg == "" {
		msg = excerpt(body)
	}
	if status == 429 {
		wait := sentinel.RetryAfter(header, 60*time.Second)
		sleep(ctx, wait)
		return &TransientError{Err: fmt.Errorf("llm/gemini: rate limited (429), waited %s", wait)}
	}
	if status >= 500 || isTransientNonSuccessStatus(status) {
		return &TransientError{Err: fmt.Errorf("llm/gemini: status %d: %s", status, msg)}
	}
	return &PermanentError{Reason: fmt.Sprintf("llm/gemini: status %d: %s", status, msg)}
}
