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
	if cfg.CustomFingerprint != "" {
		return cfg.CustomFingerprint
	}

	var appFrames []string
	for _, frame := range cfg.Stacktrace {
		if frame.InApp {
			appFrames = append(appFrames, fmt.Sprintf("%s:%d", frame.File, frame.Line))
			if len(appFrames) >= MaxAppFrames {
				break
			}
		}
	}

	input := cfg.ErrorClass
	if len(appFrames) > 0 {
		input += "|" + strings.Join(appFrames, "|")
	}

	hash := sha256.Sum256([]byte(input))
	return hex.EncodeToString(hash[:])[:16]
}
