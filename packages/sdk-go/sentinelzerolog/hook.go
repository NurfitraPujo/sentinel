package sentinelzerolog

import (
	"errors"

	"github.com/rs/zerolog"
	sentinel "github.com/tuju-ai/sentinel/packages/sdk-go"
)

type Hook struct{}

func NewHook() Hook {
	return Hook{}
}

func (h Hook) Run(e *zerolog.Event, level zerolog.Level, msg string) {
	if level >= zerolog.ErrorLevel {
		err := errors.New(msg)
		sentinel.CaptureError(err, map[string]interface{}{
			"zerolog_level": level.String(),
		})
	}
}
