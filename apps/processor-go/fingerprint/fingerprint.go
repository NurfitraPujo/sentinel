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
	// Environment is folded into the hash input (sentinel-project-environments §4.9) so the
	// fingerprint VALUE itself is environment-distinct, and idx_issues_fingerprint — a
	// fingerprint-only, non-unique index from 1716508800_init.sql — cannot be read as
	// cross-environment identity by a future query. It is defence in depth, not the mechanism:
	// the mechanism is the UNIQUE (project_id, environment, fingerprint) constraint.
	Environment string
	ErrorClass  string
	Stacktrace  []StackFrame
}

type StackFrame struct {
	File     string
	Line     int32
	Function string
	InApp    bool
}

func Compute(cfg FingerprintConfig) string {
	var input string
	// §4.9: the CustomFingerprint early return is deliberately NOT environment-prefixed. Prefixing a
	// client-chosen fingerprint would change identity for exactly the clients who chose it, which is
	// the one thing a custom fingerprint exists to prevent. Two environments sending the SAME custom
	// fingerprint still get two issue rows, because the unique key is what splits identity.
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

	input = cfg.Environment + "|" + cfg.ErrorClass
	if len(appFrames) > 0 {
		input += "|" + strings.Join(appFrames, "|")
	}

	hash := sha256.Sum256([]byte(input))
	return hex.EncodeToString(hash[:])[:16]
}
