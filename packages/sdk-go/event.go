package sentinel

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	SDKName    = "sentinel.go"
	SDKVersion = "0.4.0" // equals the CHANGELOG's newest heading and the tag (spec §7, P4.1)
)

// Levels the ingestor accepts (apps/ingestor-go/validation/bounds.go: validLevels).
const (
	LevelFatal   = "fatal"
	LevelError   = "error"
	LevelWarning = "warning"
	LevelInfo    = "info"
	LevelDebug   = "debug"
)

// SDKInfo identifies the SDK that produced an event.
type SDKInfo struct {
	Name    string `json:"name,omitempty"`
	Version string `json:"version,omitempty"`
}

// UserInfo identifies the user affected by an event. Geo is server-derived
// and deliberately not exposed here.
type UserInfo struct {
	ID        string `json:"id,omitempty"`
	Username  string `json:"username,omitempty"`
	Email     string `json:"email,omitempty"`
	IPAddress string `json:"ip_address,omitempty"`
}

// RequestInfo carries the HTTP request an event was captured for. Data and
// Env are never set by the SDK itself (only a NewEvent caller may set them).
type RequestInfo struct {
	Method      string            `json:"method,omitempty"`
	URL         string            `json:"url,omitempty"`
	QueryString string            `json:"query_string,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Data        interface{}       `json:"data,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
}

// Breadcrumb is one entry of an event's trail of what happened before the
// error. Timestamp is always set by the SDK (never omitempty, unlike every
// other field here).
type Breadcrumb struct {
	Timestamp time.Time              `json:"timestamp"`
	Type      string                 `json:"type,omitempty"`
	Category  string                 `json:"category,omitempty"`
	Level     string                 `json:"level,omitempty"`
	Message   string                 `json:"message,omitempty"`
	Data      map[string]interface{} `json:"data,omitempty"`
}

// NormalizeLevel maps a level name to one the ingestor accepts, or "" when
// there is none. Case-insensitive, surrounding whitespace ignored.
func NormalizeLevel(level string) string {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case LevelFatal:
		return LevelFatal
	case LevelError:
		return LevelError
	case LevelWarning:
		return LevelWarning
	case LevelInfo:
		return LevelInfo
	case LevelDebug:
		return LevelDebug
	case "warn":
		return LevelWarning
	case "panic":
		return LevelFatal
	case "trace":
		return LevelDebug
	default:
		return ""
	}
}

var (
	resolvedHostname     string
	resolvedHostnameOnce sync.Once
)

// hostname resolves os.Hostname() once per process (§3.2).
func hostname() string {
	resolvedHostnameOnce.Do(func() {
		h, err := os.Hostname()
		if err == nil {
			resolvedHostname = h
		}
	})
	return resolvedHostname
}

// goVersionForWire renders runtime.Version() the way docs/sdk-specification.md
// §3's example shows it: a leading "go" is removed when a digit follows
// ("go1.26.4" -> "1.26.4"); any other form ("devel ...") is sent as is.
func goVersionForWire() string {
	v := runtime.Version()
	if strings.HasPrefix(v, "go") && len(v) > 2 && v[2] >= '0' && v[2] <= '9' {
		return v[2:]
	}
	return v
}

type Frame struct {
	Function string `json:"function"`
	File     string `json:"file"`
	Line     int    `json:"line"`
	// InApp is true when this frame belongs to the application's own code,
	// as opposed to the Go standard library, a vendored dependency, or the
	// module cache. The processor's fingerprinting algorithm only considers
	// in-app frames (up to 3 of them); with every frame InApp=false, every
	// error of a given class collapses into a single issue (see
	// docs/memory/VERIFIED_STATE.md S11). Populated by ExtractStacktrace via
	// isInAppFrame - never left permanently false.
	InApp bool `json:"in_app"`
}

// Event is the wire payload sent to POST {endpoint}[/batch]. Field names and
// JSON tags here MUST match packages/proto/sentinel/v1/error_event.proto and
// apps/ingestor-go/validation.ErrorPayload exactly - there is no compiler
// checking this boundary (see docs/memory/ARCHITECTURE.md B5). If you rename
// a field here, rename it on the ingestor side in the same change.
type Event struct {
	// EventID is the client-side idempotency key (docs/plans/IDEMPOTENCY_PLAN.md D-a/D-f): a UUIDv4
	// minted fresh per event by newEventID. Must be identical across every delivery of the SAME event
	// for the ingestor-side dedup to work, and different across different events - see NewEvent below.
	EventID string `json:"event_id"`
	// ProjectKey is the target project's UNIQUE NAME (projects.name), not a credential — the secret
	// travels in the X-API-Key header (Config.APIKey). It is how an organization-wide key selects a
	// project. The server VALIDATES this against the authenticated credential rather than trusting
	// it, and answers a mismatch with 403, so it is not a tenancy bypass (VERIFIED_STATE.md S6).
	ProjectKey string `json:"project_key"`
	// Platform identifies the SDK/language that produced this event. The
	// ingestor requires it (regex ^[a-z0-9]+$); this SDK always sends "go".
	Platform   string `json:"platform"`
	ErrorClass string `json:"error_class"`
	// Message is the human-readable error message. Previously emitted as
	// "error_message", which the ingestor never read (VERIFIED_STATE.md S4)
	// - the field the server actually maps is "message".
	Message     string    `json:"message"`
	Stacktrace  []Frame   `json:"stacktrace"`
	Timestamp   time.Time `json:"timestamp"`
	Environment string    `json:"environment"`
	// ReleaseVersion is a first-class field (proto field 15), not smuggled
	// through metadata - metadata passes through Normalize()'s version-regex
	// rewrite server-side and would otherwise permanently disable regression
	// detection (VERIFIED_STATE.md S5).
	ReleaseVersion string `json:"release_version,omitempty"`
	TraceID        string `json:"trace_id,omitempty"`
	SpanID         string `json:"span_id,omitempty"`
	// TraceFlags is the W3C trace-flags byte from the originating span context —
	// bit 0 is "sampled". The ingestor accepts it (validation.ErrorPayload.TraceFlags),
	// the mapper forwards it and the processor stores it, so dropping it here was the
	// only reason SDK-originated events arrived with sampled-ness lost. Not omitempty:
	// 0 is a meaningful value (an unsampled span), distinct from "no trace context",
	// which is already expressed by TraceID being empty.
	TraceFlags uint32 `json:"trace_flags"`
	// Fingerprint is an optional custom grouping key. Leave it empty — the normal
	// case — and the server computes one from the error class, the stacktrace and
	// the environment. Set it and the server stores it VERBATIM as the issue's
	// fingerprint, so two events sharing a value group together and two differing
	// values do not, whatever their stacktraces look like.
	//
	// "Group together" means WITHIN ONE PROJECT AND ONE ENVIRONMENT, and a custom
	// fingerprint does not change that. Issue identity is the whole tuple
	// (project_id, environment, fingerprint) — a UNIQUE constraint on the issues
	// table, not something the fingerprint carries — so the same custom value sent
	// from production and from staging is TWO issues, each with its own status and
	// its own count. That is deliberate: resolving a bug in staging must not mark
	// it resolved in production. If you want one issue across environments, this
	// field is not the way to get it.
	//
	// Maximum 64 characters: it is written unhashed into issues.fingerprint
	// VARCHAR(64), and the ingestor rejects the event with a 400 rather than
	// truncating, because a shortened grouping key would silently merge issues
	// that were meant to stay apart.
	//
	// The proto's companion `fingerprint_override` is deliberately NOT exposed
	// here. Its effect is the reverse of what the name suggests — it forces the
	// SERVER to recompute and discards whatever Fingerprint was sent with it
	// (apps/processor-go/event/event.go:177) — so an SDK field for it would be a
	// switch whose only function is to throw away the value next to it.
	Fingerprint string `json:"fingerprint,omitempty"`
	// Metadata carries user tags and PII-scrubbed context. Previously
	// emitted as "context", which the ingestor never read (VERIFIED_STATE.md
	// S4) - the field the server actually maps is "metadata".
	Metadata map[string]interface{} `json:"metadata,omitempty"`

	Level       string                            `json:"level,omitempty"`
	SDK         *SDKInfo                          `json:"sdk,omitempty"`
	ServerName  string                            `json:"server_name,omitempty"`
	Transaction string                            `json:"transaction,omitempty"`
	User        *UserInfo                         `json:"user,omitempty"`
	Tags        map[string]string                 `json:"tags,omitempty"`
	Contexts    map[string]map[string]interface{} `json:"contexts,omitempty"`
	Request     *RequestInfo                      `json:"request,omitempty"`
	Breadcrumbs []Breadcrumb                      `json:"breadcrumbs,omitempty"`

	// preOptionsErrorClass and preOptionsFingerprint record what NewEvent
	// itself computed for ErrorClass/Fingerprint, before opts ran, together
	// with whether the SDK's own WithEventErrorClass/WithEventFingerprint
	// options were used. ApplyContext's metadata row reads these so that an
	// option always wins over the context's metadata keys on both fields
	// (spec §3.2, §4.3). Never marshalled.
	preOptionsErrorClass  string
	preOptionsFingerprint string
	optionSetErrorClass   bool
	optionSetFingerprint  bool
	builtByNewEvent       bool
}

// errorClassWasSet reports whether something other than NewEvent's own
// default set ev.ErrorClass (spec §3.2): the SDK's WithEventErrorClass
// option was used, or the value changed from what NewEvent computed. An
// event not built by NewEvent counts a non-empty class as set.
func (ev *Event) errorClassWasSet() bool {
	if !ev.builtByNewEvent {
		return ev.ErrorClass != ""
	}
	return ev.optionSetErrorClass || ev.ErrorClass != ev.preOptionsErrorClass
}

// fingerprintWasSet reports whether something other than NewEvent's own
// default set ev.Fingerprint (spec §3.2). See errorClassWasSet.
func (ev *Event) fingerprintWasSet() bool {
	if !ev.builtByNewEvent {
		return ev.Fingerprint != ""
	}
	return ev.optionSetFingerprint || ev.Fingerprint != ev.preOptionsFingerprint
}

func ExtractStacktrace(skip int) []Frame {
	var frames []Frame
	pcs := make([]uintptr, 32)
	n := runtime.Callers(skip, pcs)
	if n == 0 {
		return frames
	}

	callers := runtime.CallersFrames(pcs[:n])
	for {
		frame, more := callers.Next()
		if !strings.Contains(frame.Function, "runtime.") && !strings.Contains(frame.Function, "sentinel.") {
			frames = append(frames, Frame{
				Function: frame.Function,
				File:     frame.File,
				Line:     frame.Line,
				InApp:    isInAppFrame(frame.File),
			})
		}
		if !more {
			break
		}
	}
	return frames
}

// isInAppFrame reports whether file belongs to the application's own code
// rather than the Go standard library, a vendored dependency, or the module
// cache. It is a heuristic based on the file's path on disk, since that is
// all runtime.CallersFrames gives us - there is no portable way for a
// library to learn the importing application's module root at runtime.
//
// A frame is treated as in-app when it is NOT under:
//   - GOROOT (the Go standard library)
//   - the module cache (any GOPATH's pkg/mod, where all downloaded
//     dependencies - including this SDK itself - are compiled from)
//   - a vendor/ directory (vendored dependencies)
func isInAppFrame(file string) bool {
	if file == "" {
		return false
	}
	slashFile := filepath.ToSlash(file)

	if goroot := runtime.GOROOT(); goroot != "" {
		if slashGoroot := filepath.ToSlash(goroot); strings.HasPrefix(slashFile, slashGoroot+"/") {
			return false
		}
	}
	if strings.Contains(slashFile, "/pkg/mod/") {
		return false
	}
	if strings.Contains(slashFile, "/vendor/") {
		return false
	}
	return true
}

// newEventID mints a UUIDv4 using crypto/rand. It replaces the previous evt_<UnixNano> scheme
// (docs/plans/IDEMPOTENCY_PLAN.md D-f): UnixNano collides across goroutines and process restarts
// within a project, which under the event_id-based dedup design would silently merge distinct events
// into one. packages/sdk-go is a separate Go module with no existing UUID dependency (unlike the root
// module's github.com/google/uuid, used by apps/processor-go), so this is generated by hand rather
// than adding one just for a 16-byte random value with two bits fixed.
func newEventID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand.Read failing means the OS entropy source is broken - there is no fallback that
		// preserves the uniqueness guarantee this id exists for. This mirrors crypto/rand's own doc
		// comment ("Read ... always returns len(b) and a nil error" in practice) by refusing to hand
		// back a degraded, possibly-colliding id.
		panic(fmt.Sprintf("sentinel: crypto/rand.Read failed while minting event_id: %v", err))
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// ClassifiedError wraps an underlying error with an explicit error class.
type ClassifiedError struct {
	Class   string
	Message string
	Cause   error
}

func (e *ClassifiedError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	if e.Cause != nil {
		return e.Cause.Error()
	}
	return e.Class
}

func (e *ClassifiedError) Unwrap() error {
	return e.Cause
}

func (e *ClassifiedError) ErrorClass() string {
	return e.Class
}

// Classify wraps err with an explicit error class for Sentinel.
func Classify(err error, class string) error {
	if err == nil {
		return nil
	}
	return &ClassifiedError{
		Class: class,
		Cause: err,
	}
}

func classifyRootError(root error) string {
	if root == nil {
		return "Error"
	}
	if cp, ok := root.(interface{ ErrorClass() string }); ok {
		if c := cp.ErrorClass(); c != "" {
			return c
		}
	}
	t := fmt.Sprintf("%T", root)
	if t == "*errors.errorString" {
		msg := root.Error()
		if strings.HasPrefix(msg, "handler not found for task") {
			return "TaskHandlerNotFoundError"
		}
	}
	return t
}

// extractErrorClass resolves the error class from err by unwrapping the error
// chain down to the root/innermost cause so that wrapper types such as
// *fmt.wrapError (from fmt.Errorf("%w", err)) or join errors resolve to the
// underlying concrete error type.
func extractErrorClass(err error) string {
	if err == nil {
		return "Error"
	}
	if cp, ok := err.(interface{ ErrorClass() string }); ok {
		if c := cp.ErrorClass(); c != "" {
			return c
		}
	}
	curr := err
	for {
		if cp, ok := curr.(interface{ ErrorClass() string }); ok {
			if c := cp.ErrorClass(); c != "" {
				return c
			}
		}
		switch u := curr.(type) {
		case interface{ Unwrap() error }:
			unwrapped := u.Unwrap()
			if unwrapped == nil {
				return classifyRootError(curr)
			}
			curr = unwrapped
		case interface{ Unwrap() []error }:
			unwrappedList := u.Unwrap()
			var found error
			for _, item := range unwrappedList {
				if item != nil {
					found = item
					break
				}
			}
			if found == nil {
				return classifyRootError(curr)
			}
			curr = found
		default:
			unwrapped := errors.Unwrap(curr)
			if unwrapped == nil {
				return classifyRootError(curr)
			}
			curr = unwrapped
		}
	}
}

// EventOption configures an Event during NewEvent construction.
type EventOption func(*Event)

// WithEventFingerprint sets a custom fingerprint on the Event.
func WithEventFingerprint(fingerprint string) EventOption {
	return func(e *Event) {
		e.Fingerprint = fingerprint
		e.optionSetFingerprint = true
	}
}

// WithEventErrorClass sets a custom error class on the Event.
func WithEventErrorClass(errorClass string) EventOption {
	return func(e *Event) {
		e.ErrorClass = errorClass
		e.optionSetErrorClass = true
	}
}

func NewEvent(cfg Config, err error, ctxTags map[string]interface{}, opts ...EventOption) *Event {
	errMsg := ""
	errClass := "Error"
	if err != nil {
		errMsg = err.Error()
		errClass = extractErrorClass(err)
	}

	var fp string
	if ctxTags != nil {
		if ec, ok := ctxTags["error_class"].(string); ok && ec != "" {
			errClass = ec
		}
		if f, ok := ctxTags["fingerprint"].(string); ok && f != "" {
			fp = f
		}
	}

	serverName := cfg.ServerName
	if serverName == "" {
		serverName = os.Getenv("SENTINEL_SERVER_NAME")
	}
	if serverName == "" {
		serverName = hostname()
	}

	metadata := ScrubPII(ctxTags)
	if metadata == nil {
		// A NewEvent caller with no ctxTags (core's NewEvent(cfg, nil, nil))
		// still gets a non-nil map, so it, an option or BeforeSend can write
		// into it as it can into Tags (spec §3.2). The wire is unchanged:
		// metadata is omitempty, so an empty map is omitted exactly as nil was.
		metadata = make(map[string]interface{})
	}

	ev := &Event{
		EventID:        newEventID(),
		ProjectKey:     cfg.ProjectKey,
		Platform:       "go",
		ErrorClass:     errClass,
		Message:        errMsg,
		Fingerprint:    fp,
		Stacktrace:     ExtractStacktrace(3),
		Timestamp:      time.Now().UTC(),
		Environment:    cfg.Environment,
		ReleaseVersion: cfg.ReleaseVersion,
		Metadata:       metadata,

		Level:      LevelError,
		SDK:        &SDKInfo{Name: SDKName, Version: SDKVersion},
		ServerName: serverName,
		Tags:       make(map[string]string),
		Contexts: map[string]map[string]interface{}{
			"runtime": {"name": "go", "version": goVersionForWire()},
			"os":      {"name": runtime.GOOS},
			"device":  {"arch": runtime.GOARCH},
		},

		preOptionsErrorClass:  errClass,
		preOptionsFingerprint: fp,
		builtByNewEvent:       true,
	}

	for _, opt := range opts {
		if opt != nil {
			opt(ev)
		}
	}

	return ev
}
