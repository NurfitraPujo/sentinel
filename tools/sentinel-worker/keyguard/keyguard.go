// Package keyguard implements sentinel-worker's Agent-key rotation (plan §2.5): expiry-driven
// rotation now that key expiry is real (C6), a null-expiry age fallback, on-401 rotation, and a
// two-backend key store (file | kubernetes-secret). N8a defines the KeyStore seam and the trigger
// evaluation logic only — the k8s-secret backend and live rotation HTTP call ship in N8e.
package keyguard

import (
	"context"
	"time"
)

// KeyStore is the plan §2.5 "two-backend interface" (WORKER_KEYSTORE = file | kubernetes-secret).
// Implementations MUST persist before Swap is observed by callers ("persist before use" — the new
// secret is durably stored, then and only then swapped in memory).
type KeyStore interface {
	// Load returns the currently persisted key, or ("", false) if the store has never been
	// written (bootstrap: SENTINEL_AGENT_KEY from env is used instead).
	Load(ctx context.Context) (key string, ok bool, err error)
	// Persist durably stores a newly rotated key. Must complete before the caller swaps it into
	// the live Client (plan §2.5's persist-before-use rule).
	Persist(ctx context.Context, key string) error
}

// KeyInfo mirrors the subset of GET /api/agent/self's `key` object keyguard needs (C13: createdAt
// is ISO or null; C6: expiresAt is now real).
type KeyInfo struct {
	CreatedAt *time.Time
	ExpiresAt *time.Time
}

// Trigger names which of the plan §2.5 rotation rules fired, for logging/metrics.
type Trigger string

const (
	TriggerNone       Trigger = ""
	TriggerExpiryNear Trigger = "expiry-near" // (a) expiresAt within WORKER_ROTATE_BEFORE_HOURS
	TriggerAge        Trigger = "age"         // (b) null-expiry key age >= WORKER_ROTATE_EVERY_DAYS
	TriggerOn401      Trigger = "on-401"      // (c) reactive, on-401, once
)

// Evaluate implements the plan §2.5 trigger priority order (a) expiry-near, (b) age-for-null-expiry,
// returning TriggerNone when neither fires. on401 rotation is a separate explicit call site (the
// caller invokes rotation directly on a 401, per plan §2.4's Auth row), not evaluated here.
func Evaluate(info KeyInfo, now time.Time, rotateBeforeHours int, rotateEveryDays int) Trigger {
	if info.ExpiresAt != nil {
		if now.Add(time.Duration(rotateBeforeHours) * time.Hour).After(*info.ExpiresAt) {
			return TriggerExpiryNear
		}
		return TriggerNone
	}
	// Null-expiry key: age-based fallback, unless disabled (0 = off, plan §5).
	if rotateEveryDays <= 0 || info.CreatedAt == nil {
		return TriggerNone
	}
	age := now.Sub(*info.CreatedAt)
	if age >= time.Duration(rotateEveryDays)*24*time.Hour {
		return TriggerAge
	}
	return TriggerNone
}

// FileKeyStore is the `file` backend (compose/VM): a local path, atomic tmp+rename write. N8a
// stubs its shape; state/journal.go and state/cursor.go already establish the tmp+rename pattern
// this will reuse when N8e implements Persist for real.
type FileKeyStore struct {
	Path string
}
