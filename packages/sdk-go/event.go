package sentinel

import (
	"fmt"
	"runtime"
	"strings"
	"time"
)

type Frame struct {
	Function string `json:"function"`
	File     string `json:"file"`
	Line     int    `json:"line"`
}

type Event struct {
	EventID        string                 `json:"event_id"`
	ProjectKey     string                 `json:"project_key"`
	ErrorClass     string                 `json:"error_class"`
	ErrorMessage   string                 `json:"error_message"`
	Stacktrace     []Frame                `json:"stacktrace"`
	Timestamp      time.Time              `json:"timestamp"`
	Environment    string                 `json:"environment"`
	ReleaseVersion string                 `json:"release_version,omitempty"`
	TraceID        string                 `json:"trace_id,omitempty"`
	SpanID         string                 `json:"span_id,omitempty"`
	Context        map[string]interface{} `json:"context,omitempty"`
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
			})
		}
		if !more {
			break
		}
	}
	return frames
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
		ErrorClass:     errClass,
		ErrorMessage:   errMsg,
		Stacktrace:     ExtractStacktrace(3),
		Timestamp:      time.Now().UTC(),
		Environment:    cfg.Environment,
		ReleaseVersion: cfg.ReleaseVersion,
		Context:        ScrubPII(ctxTags),
	}
}
