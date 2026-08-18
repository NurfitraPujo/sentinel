// Package llm is the transport the Advisor (the in-worker LLM decision layer, per root
// CONTEXT.md) sits on. It defines provider-neutral request/response types (plan §4.1), the
// Chat interface every provider adapter implements, and a registry/factory that selects an
// adapter by name. N8b ships this package self-contained: llm.go + toolloop.go + budget.go, plus
// the openai, anthropic, and gemini adapters (llm/openai.go, llm/anthropic.go, llm/gemini.go) —
// openai is the primary, hardened-hardest adapter per rev 4. All three recognized provider names
// now have a registered adapter; New returns a typed not-implemented error only for a future
// recognized-but-unregistered name, which is currently unreachable via New for the three known
// names. Registering an adapter is the only provider-specific code any llm/<provider>.go file
// needs to add (tenet 5: provider-agnosticism).
//
// jobs.Advisor (tools/sentinel-worker/jobs/jobs.go) stays a stub through N8b/c; nothing in
// main.go imports this package yet — it is a documented unwired seam until N8d wires
// llm.RunLoop into a real Advisor implementation (see AGENT_WORKER_PLAN.md §9's N8a-unwired-seams
// note, which this package continues).
//
// Tenet-5 carve-out: plan §4.1's tenet 5 ("provider-agnosticism") is enforced as "zero provider
// names outside adapter files (llm/<provider>.go)" — but the three literal strings "openai",
// "anthropic", "gemini" in this file's knownProviders map (and UnknownProviderError's message,
// and New's provider-name parameter documentation) are a deliberate, accepted exception, not a
// violation. LLM_PROVIDER=openai|anthropic|gemini IS the §4.1-mandated selection surface: New must
// be able to tell "not implemented yet" (a recognized name, no adapter registered) apart from
// "unknown provider" (a typo) before any adapter file exists, which means the recognized-name set
// has to live somewhere that isn't an adapter file. This package (llm.go specifically) is that one
// documented exception; no other file in this package, and no adapter file's own logic, should
// duplicate or branch on these three strings outside what New/knownProviders already does here.
package llm

import (
	"context"
	"regexp"
	"strings"
	"sync"
)

// Role identifies who authored one Msg in a Request's conversation history.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ToolCall is one tool invocation an assistant turn asked the harness to execute (plan §4.1's
// `Response.ToolCalls`). ID correlates it to the ToolResult carried back in the next Msg.
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // raw JSON object, provider-neutral (adapters decode/encode per wire format)
}

// ToolResult is the harness's answer to one ToolCall, carried in a RoleTool Msg.
type ToolResult struct {
	ToolCallID string `json:"toolCallId"`
	Content    string `json:"content"` // truncated per §4.1/toolloop.go before it ever reaches here
	// IsError marks a tool execution failure (as opposed to a normal result) so the adapter can
	// signal it distinctly on the wire where the provider supports that (harmless to ignore
	// otherwise — Content already carries the error text).
	IsError bool `json:"isError,omitempty"`
}

// Msg is one turn in a Request's conversation history. Exactly one of Text, ToolCalls, or
// ToolResults is populated, matching Role:
//   - RoleUser:      Text (plus optional ToolResults when replying to prior tool calls)
//   - RoleAssistant: Text and/or ToolCalls (an assistant turn that only calls tools may have empty Text)
//   - RoleTool:      ToolResults (one or more, if a turn issued multiple parallel calls)
type Msg struct {
	Role        Role         `json:"role"`
	Text        string       `json:"text,omitempty"`
	ToolCalls   []ToolCall   `json:"toolCalls,omitempty"`
	ToolResults []ToolResult `json:"toolResults,omitempty"`
}

// Schema is a minimal JSON-schema fragment: object type, named properties (each itself a Schema),
// which of those are required, and (for string/enum-shaped leaves) an explicit enum membership
// list. This is intentionally not a full JSON-schema implementation — see toolloop.go's validator
// docstring for exactly which subset toolloop.go enforces against Response.Text when a Request
// carries a JSONSchema.
type Schema struct {
	Type       string            `json:"type"` // "object" | "string" | "number" | "boolean" | "array" | "null"
	Properties map[string]Schema `json:"properties,omitempty"`
	Required   []string          `json:"required,omitempty"`
	Enum       []string          `json:"enum,omitempty"`
	// Items describes the element schema when Type == "array".
	Items *Schema `json:"items,omitempty"`
	// Description is a natural-language description of this schema node. Adapters that translate
	// to wire tool/response schemas (OpenAI tools[].function.parameters, Anthropic
	// tools[].input_schema, Gemini functionDeclarations[].parameters) all carry per-property
	// descriptions, and tool-calling quality depends on them — this is provider-neutral, not
	// adapter-specific (tenet 5).
	Description string `json:"description,omitempty"`
	// Nullable makes null an explicitly valid value for this node, independent of Type — e.g.
	// §4.2's TRIAGE decision has `severity: low|medium|high|critical|null` and
	// `duplicateOf: issueId|null`. toolloop.go's validator only treats null as valid where Nullable
	// is set; a required field is never satisfied by null unless Nullable is also true. Adapters
	// map this onto their own wire nullability (OpenAI strict json_schema `type:["string","null"]`,
	// Gemini responseSchema `nullable: true`).
	Nullable bool `json:"nullable,omitempty"`
	// AdditionalProperties, when non-nil, is emitted by adapters that need it explicit (OpenAI
	// strict json_schema mode requires `additionalProperties: false` on every object node). nil
	// means "adapter default" rather than "true" or "false" — left unset here in the
	// provider-neutral type, set explicitly by whichever adapter needs the strict-mode default.
	AdditionalProperties *bool `json:"additionalProperties,omitempty"`
}

// ToolDef describes one tool the model may call: its name, a natural-language description, and a
// JSON-schema of its parameters. Providers translate this into their own function/tool-calling
// wire format (see each llm/<provider>.go adapter).
type ToolDef struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Params      Schema `json:"params"`
}

// Request is one model turn's input (plan §4.1). System is the system/instructions prompt;
// Messages is the full conversation so far; Tools are the tools the model may call this turn;
// MaxTokens bounds the model's output; JSONSchema, when set, asks the provider (where supported)
// to constrain its final text to the schema, and is also what toolloop.go validates the final
// decision against regardless of provider-side enforcement (§4.1: "backends that ignore
// json_schema ... are handled by the loop's validate-and-re-ask, not assumed away").
type Request struct {
	System     string
	Messages   []Msg
	Tools      []ToolDef
	MaxTokens  int
	JSONSchema *Schema
	// JSONSchemaName names the JSONSchema for wire formats that require a schema name alongside
	// the shape itself (OpenAI's response_format json_schema.name is mandatory in strict mode) —
	// provider-neutral so adapters don't each have to invent one from context.
	JSONSchemaName string
}

// Usage is adapter-reported token accounting for one Complete call, summed by toolloop.go across
// a whole RunLoop and by budget.go across a day.
type Usage struct {
	InputTokens  int
	OutputTokens int
}

// StopReason values a Response may report. Adapters normalize their own provider-specific stop
// reasons onto this small provider-neutral set.
const (
	StopEndTurn   = "end_turn"
	StopToolUse   = "tool_use"
	StopMaxTokens = "max_tokens"
	StopError     = "error"
)

// Response is one model turn's output (plan §4.1). Text is the assistant's natural-language (or,
// once JSONSchema-constrained, structured-JSON) reply; ToolCalls is non-empty when the model
// wants the harness to execute one or more tools before it continues; Usage and StopReason are
// adapter-reported.
type Response struct {
	Text       string
	ToolCalls  []ToolCall
	Usage      Usage
	StopReason string
}

// Chat is the transport the Advisor sits on (root CONTEXT.md): one model turn per Complete call.
// The harness — not the adapter — owns the tool-execution loop (toolloop.go's RunLoop).
type Chat interface {
	Complete(ctx context.Context, req Request) (Response, error)
}

// jsonSchemaNameRe is the character set every wire format this package targets accepts for a
// tool/function/schema name (OpenAI json_schema.name, Anthropic tool name, Gemini
// functionDeclaration name all constrain to this same pattern in practice).
var jsonSchemaNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// normalizeJSONSchemaName is neutral naming hygiene (tenet 5: not provider-specific logic, just
// shared string sanitization), hoisted here from openai.go so all three adapters apply it — before
// this hoist, anthropic.go and gemini.go passed Request.JSONSchemaName onto the wire RAW as a
// tool/function name, so a Request with a spaced or unicode JSONSchemaName that worked fine
// against openai (which normalized) 400'd against anthropic/gemini (which didn't).
//
// name is the caller-supplied Request.JSONSchemaName (possibly empty); defaultName is the
// adapter's own fallback name to use when name is empty (each adapter picks its own: openai's
// "decision", anthropic's/gemini's "submit_decision") — defaultName itself is assumed to already
// conform to jsonSchemaNameRe. A non-empty name that already conforms is returned unchanged;
// otherwise every character outside the accepted set is replaced with "_", truncated to 64 runes.
func normalizeJSONSchemaName(name, defaultName string) string {
	if strings.TrimSpace(name) == "" {
		return defaultName
	}
	if jsonSchemaNameRe.MatchString(name) {
		return name
	}
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
		if b.Len() >= 64 {
			break
		}
	}
	out := b.String()
	if out == "" {
		return defaultName
	}
	return out
}

// isTransientNonSuccessStatus reports whether an HTTP status outside the ordinary 429/5xx-handled
// paths should still be classified Transient rather than Permanent: anything below 200, or a 3xx
// redirect the adapter's own http.Client did not follow, is not an authoritative failure response
// worth giving up on — one rule all three adapters apply (openai.go's doOnce catch-all is one
// occurrence of it; anthropic.go's/gemini.go's classify*Error call it explicitly). 429 and 5xx are
// handled by each adapter's own retry-after/overload logic before this ever applies.
func isTransientNonSuccessStatus(status int) bool {
	return status < 200 || (status >= 300 && status < 400)
}

// malformedJSONError marks a 2xx response body that failed to json.Unmarshal, distinct from an
// HTTP-status-classified failure, so each adapter's Complete retry-once policy can target it
// specifically (retry once immediately, then report *PermanentError). Provider names the adapter
// that produced it ("openai" | "anthropic" | "gemini") so Error() attributes correctly — this type
// used to live in llm/openai.go with "llm/openai:" hardcoded into Error(), so a malformed 2xx from
// anthropic.go or gemini.go (which reused the same type) was misreported as an OpenAI error.
type malformedJSONError struct {
	Provider string
	err      error
}

func (e *malformedJSONError) Error() string {
	return "llm/" + e.Provider + ": malformed response body: " + e.err.Error()
}
func (e *malformedJSONError) Unwrap() error { return e.err }

// Config carries the provider-neutral settings New needs to build a Chat: which model to talk to,
// the API key, and (for OpenAI-compatible backends: Ollama, vLLM, LiteLLM, OpenRouter,
// Gemini-compat) an optional override base URL (plan §4.1).
type Config struct {
	Model   string
	APIKey  string
	BaseURL string
}

// NotImplementedError reports that a provider named to New has no adapter registered yet. All
// three currently recognized provider names (openai, anthropic, gemini) have registered adapters
// as of this stage, so this error is presently unreachable via New for a known name; it is
// retained for a future recognized-but-unregistered provider name, distinct from an outright
// unknown-provider error, so callers/tests can tell "not implemented yet" apart from "typo'd the
// provider name".
type NotImplementedError struct {
	Provider string
}

func (e *NotImplementedError) Error() string {
	return "llm: provider " + e.Provider + " is not implemented yet (adapter lands in a later stage)"
}

// UnknownProviderError reports a provider name New does not recognize at all.
type UnknownProviderError struct {
	Provider string
}

func (e *UnknownProviderError) Error() string {
	return "llm: unknown provider " + e.Provider + " (want one of: openai, anthropic, gemini)"
}

// knownProviders is the recognized provider-name set (plan §4.1's LLM_PROVIDER values). All three
// now have adapters registered (llm/openai.go, llm/anthropic.go, llm/gemini.go); this set remains
// distinct from providerFactories so a future recognized-but-unregistered name still resolves to
// NotImplementedError rather than UnknownProviderError, giving a caller a clear "not yet" instead
// of "did you mean...".
var knownProviders = map[string]bool{
	"openai":    true,
	"anthropic": true,
	"gemini":    true,
}

// providerFactories holds registered adapter constructors, keyed by provider name. It is
// populated from each llm/<provider>.go's init() call to RegisterProvider (llm/openai.go,
// llm/anthropic.go, llm/gemini.go each do this). Exported via RegisterProvider so adapter files,
// and tests, can register/override without New itself needing provider-specific code (tenet 5).
//
// providerFactoriesMu guards both the map and every read/write of it: RegisterProvider is exported
// and package-global state mutated from an exported func is reachable from concurrent callers —
// adapter package init()s can run during package initialization in any order/goroutine per Go's
// own init ordering across imported packages, and tests (this package's own and, later, jobs/*'s)
// may call RegisterProvider and New concurrently with -race. A bare map here would be exactly the
// kind of unsynchronized package-global mutation this repo's own conventions call out (see B7/B4
// for the analogous "typed accessor, not a bare read" pattern this mirrors for writes).
var (
	providerFactoriesMu sync.RWMutex
	providerFactories   = map[string]func(Config) (Chat, error){}
)

// RegisterProvider adds (or, in tests, overrides) a provider factory. Adapter packages call this
// from their own init() in a later stage; N8b's tests use it to install fakes. Safe for concurrent
// use, including concurrently with New.
func RegisterProvider(name string, factory func(Config) (Chat, error)) {
	providerFactoriesMu.Lock()
	defer providerFactoriesMu.Unlock()
	providerFactories[name] = factory
}

// New builds a Chat for the given provider name and config (plan §4.1: "Selection:
// LLM_PROVIDER=openai|anthropic|gemini + LLM_MODEL/API_KEY/BASE_URL"). Returns UnknownProviderError
// for a name outside the recognized set, or NotImplementedError for a recognized name with no
// adapter registered yet — the current state of every recognized name in N8b.
func New(provider string, cfg Config) (Chat, error) {
	providerFactoriesMu.RLock()
	factory, ok := providerFactories[provider]
	providerFactoriesMu.RUnlock()
	if ok {
		return factory(cfg)
	}
	if !knownProviders[provider] {
		return nil, &UnknownProviderError{Provider: provider}
	}
	return nil, &NotImplementedError{Provider: provider}
}
