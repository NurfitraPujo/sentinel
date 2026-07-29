package sentinel

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

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
	// Metadata carries user tags and PII-scrubbed context. Previously
	// emitted as "context", which the ingestor never read (VERIFIED_STATE.md
	// S4) - the field the server actually maps is "metadata".
	Metadata map[string]interface{} `json:"metadata,omitempty"`
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

func NewEvent(cfg Config, err error, ctxTags map[string]interface{}) *Event {
	errMsg := ""
	errClass := "Error"
	if err != nil {
		errMsg = err.Error()
		errClass = fmt.Sprintf("%T", err)
	}

	return &Event{
		EventID:        fmt.Sprintf("evt_%d", time.Now().UnixNano()),
		ProjectKey:     cfg.ProjectKey,
		Platform:       "go",
		ErrorClass:     errClass,
		Message:        errMsg,
		Stacktrace:     ExtractStacktrace(3),
		Timestamp:      time.Now().UTC(),
		Environment:    cfg.Environment,
		ReleaseVersion: cfg.ReleaseVersion,
		Metadata:       ScrubPII(ctxTags),
	}
}
