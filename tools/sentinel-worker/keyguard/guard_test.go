package keyguard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeStore is an in-memory KeyStore for Guard tests, with hooks to force Persist to fail.
type fakeStore struct {
	mu           sync.Mutex
	persisted    string
	persistErr   error
	persistCalls int
}

func (s *fakeStore) Load(ctx context.Context) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.persisted, s.persisted != "", nil
}

func (s *fakeStore) Persist(ctx context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.persistCalls++
	if s.persistErr != nil {
		return s.persistErr
	}
	s.persisted = key
	return nil
}

// fakeClient is an in-memory RotatingClient for Guard tests.
type fakeClient struct {
	mu          sync.Mutex
	info        KeyInfo
	infoErr     error
	rotateErr   error
	rotateCalls int
	nextKey     string
	liveKey     string
	setKeyCalls []string
}

func (c *fakeClient) SelfKeyInfo(ctx context.Context) (KeyInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.info, c.infoErr
}

func (c *fakeClient) Rotate(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rotateCalls++
	if c.rotateErr != nil {
		return "", c.rotateErr
	}
	return c.nextKey, nil
}

func (c *fakeClient) SetKey(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.liveKey = key
	c.setKeyCalls = append(c.setKeyCalls, key)
}

func newTestLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, nil))
}

// TestGuard_ExpiryNearTriggerRotates proves trigger (a) fires evaluateAndRotate end to end.
func TestGuard_ExpiryNearTriggerRotates(t *testing.T) {
	now := time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC)
	expiresAt := now.Add(48 * time.Hour)
	store := &fakeStore{}
	client := &fakeClient{info: KeyInfo{ExpiresAt: &expiresAt}, nextKey: "new-key-1", liveKey: "old-key"}
	g := NewGuard(store, client, 72, 30, time.Hour, nil)
	g.Clock = func() time.Time { return now }

	g.evaluateAndRotate(context.Background())

	if client.rotateCalls != 1 {
		t.Fatalf("expected 1 rotate call, got %d", client.rotateCalls)
	}
	if store.persisted != "new-key-1" {
		t.Fatalf("expected persisted new-key-1, got %q", store.persisted)
	}
	if client.liveKey != "new-key-1" {
		t.Fatalf("expected live key swapped to new-key-1, got %q", client.liveKey)
	}
}

// TestGuard_AgeTriggerViaCreatedAtRotates proves trigger (b), the null-expiry age fallback.
func TestGuard_AgeTriggerViaCreatedAtRotates(t *testing.T) {
	now := time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC)
	createdAt := now.Add(-31 * 24 * time.Hour)
	store := &fakeStore{}
	client := &fakeClient{info: KeyInfo{CreatedAt: &createdAt}, nextKey: "new-key-2"}
	g := NewGuard(store, client, 72, 30, time.Hour, nil)
	g.Clock = func() time.Time { return now }

	g.evaluateAndRotate(context.Background())

	if client.rotateCalls != 1 {
		t.Fatalf("expected 1 rotate call, got %d", client.rotateCalls)
	}
	if store.persisted != "new-key-2" {
		t.Fatalf("expected persisted new-key-2, got %q", store.persisted)
	}
}

// TestGuard_NoTriggerDoesNotRotate proves a young, non-expiring key is left alone.
func TestGuard_NoTriggerDoesNotRotate(t *testing.T) {
	now := time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC)
	createdAt := now.Add(-5 * 24 * time.Hour)
	store := &fakeStore{}
	client := &fakeClient{info: KeyInfo{CreatedAt: &createdAt}}
	g := NewGuard(store, client, 72, 30, time.Hour, nil)
	g.Clock = func() time.Time { return now }

	g.evaluateAndRotate(context.Background())

	if client.rotateCalls != 0 {
		t.Fatalf("expected no rotate call, got %d", client.rotateCalls)
	}
}

// TestGuard_PersistBeforeUse_FailedPersistLeavesOldKeyLive is the mutation-proved ordering test:
// when Persist fails, SetKey must never be called -- the old key stays live.
func TestGuard_PersistBeforeUse_FailedPersistLeavesOldKeyLive(t *testing.T) {
	now := time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC)
	expiresAt := now.Add(1 * time.Hour)
	store := &fakeStore{persistErr: errors.New("disk full")}
	client := &fakeClient{info: KeyInfo{ExpiresAt: &expiresAt}, nextKey: "new-key-3", liveKey: "old-key"}
	g := NewGuard(store, client, 72, 30, time.Hour, nil)
	g.Clock = func() time.Time { return now }

	g.evaluateAndRotate(context.Background())

	if client.rotateCalls != 1 {
		t.Fatalf("expected the rotate endpoint to still be called, got %d", client.rotateCalls)
	}
	if len(client.setKeyCalls) != 0 {
		t.Fatalf("expected SetKey to NEVER be called on a failed persist, got calls: %v", client.setKeyCalls)
	}
	if client.liveKey != "old-key" {
		t.Fatalf("expected old key to remain live, got %q", client.liveKey)
	}
	if store.persistCalls != 1 {
		t.Fatalf("expected exactly 1 persist attempt, got %d", store.persistCalls)
	}
}

// TestGuard_ProactiveRotate_OrphanedKeyDoesNotBlindLoop proves the proactive path (evaluateAndRotate)
// does not re-call Rotate every tick when Persist keeps failing: rotate() has already minted a
// new server-side key on the FIRST failed attempt, so a second attempt without the latch would
// mint yet another orphaned key indefinitely. It must instead log an orphaned-key WARN and refuse
// to call Rotate again until a persist succeeds.
func TestGuard_ProactiveRotate_OrphanedKeyDoesNotBlindLoop(t *testing.T) {
	var buf bytes.Buffer
	now := time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC)
	expiresAt := now.Add(1 * time.Hour)
	store := &fakeStore{persistErr: errors.New("secret patch RBAC denied")}
	client := &fakeClient{info: KeyInfo{ExpiresAt: &expiresAt}, nextKey: "new-key-orphan", liveKey: "old-key"}
	g := NewGuard(store, client, 72, 30, time.Hour, newTestLogger(&buf))
	g.Clock = func() time.Time { return now }

	// First tick: rotate() calls Rotate, Persist fails, latch is set.
	g.evaluateAndRotate(context.Background())
	if client.rotateCalls != 1 {
		t.Fatalf("expected 1 rotate call after first failed persist, got %d", client.rotateCalls)
	}
	if !strings.Contains(buf.String(), "orphaned") {
		t.Fatalf("expected an orphaned-key WARN to be logged on the failed persist, got: %s", buf.String())
	}

	// Further ticks (still failing to persist, trigger still firing) must NOT call Rotate again --
	// that would mint another orphaned key every hour forever.
	g.evaluateAndRotate(context.Background())
	g.evaluateAndRotate(context.Background())
	if client.rotateCalls != 1 {
		t.Fatalf("expected still 1 rotate call after further ticks (orphaned latch), got %d", client.rotateCalls)
	}
	if client.liveKey != "old-key" {
		t.Fatalf("expected old key to remain live throughout, got %q", client.liveKey)
	}

	// Once Persist starts succeeding again, the NEXT trigger must persist the SAME already-minted
	// key (never calling Client.Rotate again) and clear the latch.
	store.mu.Lock()
	store.persistErr = nil
	store.mu.Unlock()
	g.evaluateAndRotate(context.Background())
	if client.rotateCalls != 1 {
		t.Fatalf("expected Client.Rotate to never be called again while orphaned (retry persists the same key), got %d rotate calls", client.rotateCalls)
	}
	if client.liveKey != "new-key-orphan" {
		t.Fatalf("expected the orphaned key to go live once persist succeeded, got %q", client.liveKey)
	}

	// The latch is now clear: a FRESH trigger should mint and persist a new key normally.
	client.nextKey = "new-key-after-recovery"
	g.evaluateAndRotate(context.Background())
	if client.rotateCalls != 2 {
		t.Fatalf("expected a fresh rotate call once the orphan cleared, got %d", client.rotateCalls)
	}
	if client.liveKey != "new-key-after-recovery" {
		t.Fatalf("expected the fresh key to go live, got %q", client.liveKey)
	}
}

// TestGuard_TriggerOn401Once_RotatesOnce proves rule (c): the first 401 rotates, a second 401
// after that does NOT rotate again, and logs a WARN instead.
func TestGuard_TriggerOn401Once_RotatesOnce(t *testing.T) {
	var buf bytes.Buffer
	store := &fakeStore{}
	client := &fakeClient{nextKey: "new-key-401"}
	g := NewGuard(store, client, 72, 30, time.Hour, newTestLogger(&buf))

	g.TriggerOn401Once(context.Background())
	if client.rotateCalls != 1 {
		t.Fatalf("expected 1 rotate call after first 401, got %d", client.rotateCalls)
	}

	g.TriggerOn401Once(context.Background())
	if client.rotateCalls != 1 {
		t.Fatalf("expected still 1 rotate call after second 401 (single-retry latch), got %d", client.rotateCalls)
	}
	if !strings.Contains(buf.String(), "WARN") {
		t.Fatalf("expected a WARN log for the orphaned second 401, got: %s", buf.String())
	}
}

// TestGuard_ReadOnlyDisablesRotationAndLogsLoudly proves a read-only store disables rotation
// entirely (both the ticker path and the on-401 path) and logs a WARN at Run start.
func TestGuard_ReadOnlyDisablesRotationAndLogsLoudly(t *testing.T) {
	var buf bytes.Buffer
	now := time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC)
	expiresAt := now.Add(1 * time.Hour) // would trigger if not read-only
	store := &fakeStore{}
	client := &fakeClient{info: KeyInfo{ExpiresAt: &expiresAt}, nextKey: "should-not-rotate"}
	g := NewGuard(store, client, 72, 30, time.Hour, newTestLogger(&buf))
	g.Clock = func() time.Time { return now }
	g.ReadOnly = true

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		g.Run(ctx)
		close(done)
	}()
	// Give Run a moment to log and park on <-ctx.Done().
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit after ctx cancellation")
	}

	if !strings.Contains(buf.String(), "read-only") || !strings.Contains(buf.String(), "DISABLED") {
		t.Fatalf("expected a loud read-only disabled log, got: %s", buf.String())
	}

	g.evaluateAndRotate(context.Background())
	g.TriggerOn401Once(context.Background())
	if client.rotateCalls != 0 {
		t.Fatalf("expected no rotate calls while read-only, got %d", client.rotateCalls)
	}
}

// TestGuard_SecretNeverLogged runs a full rotate cycle against a logger capturing everything at
// Debug level and asserts the raw secret values never appear in the log output (grep-style
// assertion, per plan §2.5 "NEVER log/journal/snapshot the secret").
func TestGuard_SecretNeverLogged(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	now := time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC)
	expiresAt := now.Add(1 * time.Hour)
	const oldSecret = "sk-live-OLD-SECRET-do-not-log-me"
	const newSecret = "sk-live-NEW-SECRET-do-not-log-me"
	store := &fakeStore{}
	client := &fakeClient{info: KeyInfo{ExpiresAt: &expiresAt}, nextKey: newSecret, liveKey: oldSecret}
	g := NewGuard(store, client, 72, 30, time.Hour, logger)
	g.Clock = func() time.Time { return now }

	g.evaluateAndRotate(context.Background())
	g.TriggerOn401Once(context.Background())

	out := buf.String()
	if strings.Contains(out, oldSecret) {
		t.Fatalf("log output must never contain the old secret; got: %s", out)
	}
	if strings.Contains(out, newSecret) {
		t.Fatalf("log output must never contain the new secret; got: %s", out)
	}
}

// TestGuard_AgeTriggerFallsBackToLocalCreatedAtWhenServerCreatedAtIsNull proves fileKeyRecord's
// CreatedAt (plan §2.5: "Track rotation age locally when the server exposes no createdAt
// fallback") is actually consulted by evaluateAndRotate: when /self returns neither createdAt nor
// expiresAt, the age trigger must still fire off the FileKeyStore's own local record.
func TestGuard_AgeTriggerFallsBackToLocalCreatedAtWhenServerCreatedAtIsNull(t *testing.T) {
	now := time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	store := FileKeyStore{Path: dir + "/agent-key.json"}

	// Seed a local record whose CreatedAt is old enough (31 days) to trip the age trigger
	// (rotateEveryDays=30), via Persist -- exactly how production writes it.
	if err := store.Persist(context.Background(), "old-key"); err != nil {
		t.Fatalf("seeding local record: %v", err)
	}
	// Persist stamps CreatedAt with time.Now(), which we can't control -- rewrite the file
	// directly to backdate it, since Persist always uses the real clock.
	rec, ok, err := store.LoadRecord(context.Background())
	if err != nil || !ok {
		t.Fatalf("LoadRecord after seeding: ok=%v err=%v", ok, err)
	}
	rec.CreatedAt = now.Add(-31 * 24 * time.Hour)
	data, _ := json.MarshalIndent(rec, "", "  ")
	if err := os.WriteFile(store.Path, data, 0o600); err != nil {
		t.Fatalf("backdating local record: %v", err)
	}

	// The server reports NEITHER createdAt NOR expiresAt for this key (C13 null createdAt case).
	client := &fakeClient{info: KeyInfo{}, nextKey: "new-key-fallback"}
	g := NewGuard(store, client, 72, 30, time.Hour, nil)
	g.Clock = func() time.Time { return now }

	g.evaluateAndRotate(context.Background())

	if client.rotateCalls != 1 {
		t.Fatalf("expected the local-record age fallback to fire rotation, got %d rotate calls", client.rotateCalls)
	}
}

// TestGuard_Stop_ExitsRunPromptly proves the sidecar goroutine drains cleanly on Stop.
func TestGuard_Stop_ExitsRunPromptly(t *testing.T) {
	store := &fakeStore{}
	client := &fakeClient{}
	g := NewGuard(store, client, 72, 30, time.Hour, nil)

	done := make(chan struct{})
	go func() {
		g.Run(context.Background())
		close(done)
	}()
	time.Sleep(10 * time.Millisecond)
	g.Stop()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit after Stop")
	}
}
