package sentinelzerolog

import (
	"errors"

	"github.com/rs/zerolog"
	sentinel "github.com/NurfitraPujo/sentinel/packages/sdk-go"
)

// LevelFromZerolog maps a zerolog level to a Sentinel level: Trace and
// Debug (and any numeric level below Trace) -> debug, Info -> info, Warn ->
// warning, Error -> error, Fatal and Panic -> fatal, NoLevel and Disabled ->
// "" (spec §3.6). It reads the zerolog.Level enum, never Level.String(),
// because String() returns the mutable package globals
// zerolog.LevelTraceValue ... LevelPanicValue (T31): a program that renames
// one of them would silently change what this maps.
func LevelFromZerolog(l zerolog.Level) string {
	switch {
	case l == zerolog.NoLevel || l == zerolog.Disabled:
		return ""
	case l == zerolog.FatalLevel || l == zerolog.PanicLevel:
		return sentinel.LevelFatal
	case l == zerolog.ErrorLevel:
		return sentinel.LevelError
	case l == zerolog.WarnLevel:
		return sentinel.LevelWarning
	case l == zerolog.InfoLevel:
		return sentinel.LevelInfo
	default: // TraceLevel, DebugLevel, and anything below Trace
		return sentinel.LevelDebug
	}
}

type Hook struct{}

func NewHook() Hook {
	return Hook{}
}

// Run fires for Error, Fatal and Panic only (D2: NoLevel and Disabled lines,
// which v0.3.x captured as errors, are no longer captured). It captures
// with the logger's context, so a log line written with .Ctx(ctx) carries
// that context's user, tags and breadcrumbs (spec §3.6).
func (h Hook) Run(e *zerolog.Event, level zerolog.Level, msg string) {
	if level != zerolog.ErrorLevel && level != zerolog.FatalLevel && level != zerolog.PanicLevel {
		return
	}
	err := errors.New(msg)
	ctx := sentinel.WithLevel(e.GetCtx(), LevelFromZerolog(level))
	sentinel.CaptureErrorContext(ctx, err, map[string]interface{}{
		"zerolog_level": level.String(),
	})
}
