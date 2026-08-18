package keyguard

import (
	"testing"
	"time"
)

func TestEvaluate_ExpiryNearTriggers(t *testing.T) {
	now := time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC)
	expiresAt := now.Add(48 * time.Hour) // within the 72h rotate-before window
	info := KeyInfo{ExpiresAt: &expiresAt}
	if got := Evaluate(info, now, 72, 30); got != TriggerExpiryNear {
		t.Fatalf("expected TriggerExpiryNear, got %q", got)
	}
}

func TestEvaluate_ExpiryFarDoesNotTrigger(t *testing.T) {
	now := time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC)
	expiresAt := now.Add(200 * time.Hour) // well outside the 72h window
	info := KeyInfo{ExpiresAt: &expiresAt}
	if got := Evaluate(info, now, 72, 30); got != TriggerNone {
		t.Fatalf("expected TriggerNone, got %q", got)
	}
}

// TestEvaluate_NullExpiryAgeFallback proves the plan §2.5 rule (b): a null-expiry key rotates on
// age alone, via key.createdAt (C13).
func TestEvaluate_NullExpiryAgeFallback(t *testing.T) {
	now := time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC)
	createdAt := now.Add(-31 * 24 * time.Hour) // older than the 30-day default
	info := KeyInfo{ExpiresAt: nil, CreatedAt: &createdAt}
	if got := Evaluate(info, now, 72, 30); got != TriggerAge {
		t.Fatalf("expected TriggerAge, got %q", got)
	}
}

func TestEvaluate_NullExpiryYoungKeyDoesNotTrigger(t *testing.T) {
	now := time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC)
	createdAt := now.Add(-5 * 24 * time.Hour)
	info := KeyInfo{ExpiresAt: nil, CreatedAt: &createdAt}
	if got := Evaluate(info, now, 72, 30); got != TriggerNone {
		t.Fatalf("expected TriggerNone, got %q", got)
	}
}

// TestEvaluate_AgeFallbackDisabledWhenZero proves WORKER_ROTATE_EVERY_DAYS=0 disables trigger (b)
// entirely (plan §5: "0 disables").
func TestEvaluate_AgeFallbackDisabledWhenZero(t *testing.T) {
	now := time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC)
	createdAt := now.Add(-999 * 24 * time.Hour)
	info := KeyInfo{ExpiresAt: nil, CreatedAt: &createdAt}
	if got := Evaluate(info, now, 72, 0); got != TriggerNone {
		t.Fatalf("expected TriggerNone with rotateEveryDays=0, got %q", got)
	}
}

func TestEvaluate_NullExpiryNoCreatedAtNeverTriggers(t *testing.T) {
	now := time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC)
	info := KeyInfo{}
	if got := Evaluate(info, now, 72, 30); got != TriggerNone {
		t.Fatalf("expected TriggerNone with no createdAt, got %q", got)
	}
}
