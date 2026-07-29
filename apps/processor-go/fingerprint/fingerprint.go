package fingerprint

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

const (
	MaxAppFrames = 3
)

type FingerprintConfig struct {
	CustomFingerprint string
	ErrorClass        string
	Stacktrace        []StackFrame
}

type StackFrame struct {
	File     string
	Line     int32
	Function string
	InApp    bool
}

func Compute(cfg FingerprintConfig) string {
	var input string
	if cfg.CustomFingerprint != "" {
		return cfg.CustomFingerprint
	}

	var appFrames []string
	for _, frame := range cfg.Stacktrace {
		if frame.InApp {
			appFrames = append(appFrames, fmt.Sprintf("%s:%s", frame.File, frame.Function))
			if len(appFrames) >= MaxAppFrames {
				break
			}
		}
	}

	// P4-3 / VERIFIED_STATE.md S11: when a client marks no frames in_app, the hash input used to
	// degenerate to ErrorClass alone, collapsing every error of a class in a project into ONE issue.
	// Clients that do not set in_app (anything other than the Go SDK today) still deserve grouping, so
	// fall back to the top frames regardless of the flag. Frames are still capped at MaxAppFrames, so
	// the fingerprint stays stable as deeper stack detail changes.
	if len(appFrames) == 0 {
		for _, frame := range cfg.Stacktrace {
			appFrames = append(appFrames, fmt.Sprintf("%s:%s", frame.File, frame.Function))
			if len(appFrames) >= MaxAppFrames {
				break
			}
		}
	}

	input = cfg.ErrorClass
	if len(appFrames) > 0 {
		input += "|" + strings.Join(appFrames, "|")
	}

	hash := sha256.Sum256([]byte(input))
	return hex.EncodeToString(hash[:])[:16]
}
