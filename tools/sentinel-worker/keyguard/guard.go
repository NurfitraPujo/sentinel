// This file implements the Guard sidecar (plan §2.5): the goroutine that periodically evaluates
// Evaluate against a KeyStore + RotatingClient, performs unattended rotation with
// persist-before-use, and exposes the on-401 single-retry hook main.go's sentinel.Client wires up.
package keyguard

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// RotatingClient is the seam Guard needs from sentinel.Client: read /self (for key.createdAt /
// key.expiresAt, C13/C6), call the rotate endpoint, and swap the live in-memory key. Kept as an
// interface (rather than importing package sentinel directly) so keyguard has no dependency on
// the HTTP client's wire shapes and stays independently testable.
type RotatingClient interface {
	// SelfKeyInfo calls GET /api/agent/self and returns the key sub-object's createdAt/expiresAt.
	SelfKeyInfo(ctx context.Context) (KeyInfo, error)
	// Rotate calls POST /api/agent/key/rotate and returns the new secret. The caller (Guard) is
	// responsible for persist-before-use; Rotate itself does not touch the store.
	Rotate(ctx context.Context) (newKey string, err error)
	// SetKey swaps the live in-memory credential used for future requests. Called ONLY after the
	// new key has been durably persisted (plan §2.5 "persist before use").
	SetKey(key string)
}

// Guard runs the unattended-rotation sidecar. Construct with NewGuard, then run Run in its own
// goroutine and call Stop on shutdown.
type Guard struct {
	Store             KeyStore
	Client            RotatingClient
	RotateBeforeHours int
	RotateEveryDays   int
	Interval          time.Duration
	Log               *slog.Logger
	Clock             func() time.Time // overridable for tests; defaults to time.Now
	// ReadOnly disables rotation entirely (a mounted read-only volume, no writable store) -- Run
	// logs loudly once at start and never attempts a rotation.
	ReadOnly bool

	mu           sync.Mutex
	rotatedOn401 bool // single-retry latch for the on-401 trigger (plan §2.5 rule (c): "once")
	// orphanedKey holds a key that Client.Rotate already minted server-side but Store.Persist
	// failed to durably store (plan §2.5: "must NOT blind-rotate in a loop ... rotates at most once
	// more and logs an orphaned-key warning" -- the proactive path needs the same bound
	// TriggerOn401Once already gives the reactive path). While set, evaluateAndRotate does NOT call
	// Client.Rotate again (that would mint yet another orphan every tick) -- it retries ONLY
	// Store.Persist on this same already-minted key, until that succeeds or the process restarts.
	orphanedKey string
	stopCh      chan struct{}
	doneCh      chan struct{}
}

// NewGuard builds a Guard with defaults filled in (Interval, Clock).
func NewGuard(store KeyStore, client RotatingClient, rotateBeforeHours, rotateEveryDays int, interval time.Duration, log *slog.Logger) *Guard {
	if interval <= 0 {
		interval = time.Hour
	}
	return &Guard{
		Store:             store,
		Client:            client,
		RotateBeforeHours: rotateBeforeHours,
		RotateEveryDays:   rotateEveryDays,
		Interval:          interval,
		Log:               log,
		Clock:             time.Now,
		stopCh:            make(chan struct{}),
		doneCh:            make(chan struct{}),
	}
}

// Run is the ticker loop. It blocks until ctx is cancelled or Stop is called, and closes doneCh on
// exit so Stop can wait for a clean drain.
func (g *Guard) Run(ctx context.Context) {
	defer close(g.doneCh)
	if g.ReadOnly {
		if g.Log != nil {
			g.Log.Warn("keyguard: key store is read-only, unattended rotation is DISABLED")
		}
		<-ctx.Done()
		return
	}
	ticker := time.NewTicker(g.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-g.stopCh:
			return
		case <-ticker.C:
			g.evaluateAndRotate(ctx)
		}
	}
}

// Stop signals Run to exit and waits for it to finish.
func (g *Guard) Stop() {
	select {
	case <-g.stopCh:
	default:
		close(g.stopCh)
	}
	<-g.doneCh
}

// evaluateAndRotate implements one tick: read /self, Evaluate the two proactive triggers, and
// rotate if either fired.
func (g *Guard) evaluateAndRotate(ctx context.Context) {
	if g.ReadOnly {
		return
	}
	info, err := g.Client.SelfKeyInfo(ctx)
	if err != nil {
		if g.Log != nil {
			g.Log.Error("keyguard: reading /self for rotation evaluation", "error", err)
		}
		return
	}
	if info.CreatedAt == nil && info.ExpiresAt == nil {
		// The server exposed neither createdAt nor expiresAt for this key (C13: createdAt may be
		// null) -- fall back to the FileKeyStore's own local rotatedAt/createdAt record (plan §2.5:
		// "Track rotation age locally when the server exposes no createdAt fallback"), so the age
		// trigger (b) can still fire off a locally-known rotation time instead of silently never
		// rotating a key the server can't or won't date. Only the `file` backend keeps this record;
		// other KeyStore implementations (e.g. K8sKeyStore) simply have no local fallback to offer.
		if fs, ok := g.Store.(FileKeyStore); ok {
			if rec, ok, err := fs.LoadRecord(ctx); err == nil && ok && !rec.CreatedAt.IsZero() {
				createdAt := rec.CreatedAt
				info.CreatedAt = &createdAt
			}
		}
	}
	now := g.now()
	trigger := Evaluate(info, now, g.RotateBeforeHours, g.RotateEveryDays)

	// Finding 7 (core-robustness round 3): the orphaned-key persist retry MUST be checked before
	// the TriggerNone early return below, not after. A prior tick can mint a key server-side, fail
	// to persist it, and latch g.orphanedKey -- but by the time the NEXT tick runs, the server-side
	// rotation already happened, so Evaluate(info, ...) against the freshly-rotated key's own
	// createdAt/expiresAt legitimately returns TriggerNone (the just-minted key isn't due for
	// rotation itself). With the pending check previously sitting AFTER the TriggerNone return,
	// every subsequent tick took that return and the retry branch below was unreachable for the
	// rest of this process's life -- a durably-lost rotated key stayed orphaned until restart.
	g.mu.Lock()
	pending := g.orphanedKey
	g.mu.Unlock()
	if pending != "" {
		// A prior tick already minted this key server-side but failed to persist it -- retry ONLY
		// the persist of the SAME key, never call Client.Rotate again (that would mint yet another
		// orphaned key every tick forever, plan §2.5's "must NOT blind-rotate in a loop").
		if err := g.retryPersist(ctx, pending, trigger); err != nil {
			if g.Log != nil {
				g.Log.Warn("keyguard: a prior rotation minted a new key but failed to persist it (orphaned key); still not durably stored, not calling rotate again until a persist succeeds or the process restarts", "trigger", trigger, "error", err)
			}
		}
		return
	}

	if trigger == TriggerNone {
		return
	}

	if err := g.rotate(ctx, trigger); err != nil {
		if g.Log != nil {
			g.Log.Error("keyguard: rotation failed", "trigger", trigger, "error", err)
		}
	}
}

// retryPersist retries Store.Persist for an already-minted, still-orphaned key, without calling
// Client.Rotate again. On success it completes persist-before-use (SetKey) and clears the latch.
func (g *Guard) retryPersist(ctx context.Context, key string, trigger Trigger) error {
	if err := g.Store.Persist(ctx, key); err != nil {
		return err
	}
	g.Client.SetKey(key)
	g.mu.Lock()
	g.orphanedKey = ""
	g.mu.Unlock()
	if g.Log != nil {
		g.Log.Info("keyguard: previously orphaned key persisted successfully, now live", "trigger", trigger)
	}
	return nil
}

// TriggerOn401Once implements plan §2.5 rule (c): rotate on a 401, but only ONCE per process --
// a second 401 after an on-401 rotation has already fired this run means rotation did not fix
// authentication (an orphaned/never-persisted key, or a server-side problem), so blind-looping
// would just hammer the rotate endpoint. It logs a WARN and returns without rotating again.
// Successful non-401 traffic does NOT reset the latch (it fires once per process lifetime, not
// once per outage) -- a fresh attempt happens naturally on restart (the crash/restart orphan path
// documented on Guard).
func (g *Guard) TriggerOn401Once(ctx context.Context) {
	if g.ReadOnly {
		return
	}
	g.mu.Lock()
	if g.rotatedOn401 {
		g.mu.Unlock()
		if g.Log != nil {
			g.Log.Warn("keyguard: received a second 401 after an on-401 rotation already fired this run; not rotating again (orphaned key or server-side auth problem)")
		}
		return
	}
	g.rotatedOn401 = true
	g.mu.Unlock()

	if err := g.rotate(ctx, TriggerOn401); err != nil {
		if g.Log != nil {
			g.Log.Error("keyguard: on-401 rotation failed", "error", err)
		}
	}
}

// rotate performs one rotation: call the rotate endpoint, durably persist the new secret via the
// store, THEN swap it into the live client (persist-before-use, plan §2.5). If Persist fails, the
// live client's key is left untouched -- the old key is still valid during the 24h grace window
// (C6), so a failed persist here is safe to retry on the next tick/401 rather than swap in a
// secret nothing durable remembers.
func (g *Guard) rotate(ctx context.Context, trigger Trigger) error {
	newKey, err := g.Client.Rotate(ctx)
	if err != nil {
		return fmt.Errorf("calling rotate endpoint: %w", err)
	}
	if err := g.Store.Persist(ctx, newKey); err != nil {
		// PERSIST-BEFORE-USE: do not swap. The new secret is now live server-side but nothing
		// durable remembers it; the old key remains valid (24h grace, C6). Latch orphanedKey so the
		// PROACTIVE path (evaluateAndRotate) does not blind-loop calling Client.Rotate again every
		// tick -- each such call would mint yet another orphaned server-side key. Log the WARN
		// here, once, at the moment the orphan is created (not just on the next tick's skip) so an
		// operator sees it immediately. Never log the secret itself.
		g.mu.Lock()
		g.orphanedKey = newKey
		g.mu.Unlock()
		if g.Log != nil {
			g.Log.Warn("keyguard: rotated key persist failed; a new key is live server-side but not durably stored (orphaned key) -- old key remains in use via the 24h grace window; proactive rotation is now latched off, retrying persist only, until it succeeds or the process restarts", "trigger", trigger, "error", err)
		}
		return fmt.Errorf("persisting rotated key (old key remains in use, will retry): %w", err)
	}
	g.Client.SetKey(newKey)
	g.mu.Lock()
	g.orphanedKey = ""
	g.mu.Unlock()
	if g.Log != nil {
		g.Log.Info("keyguard: rotated agent key", "trigger", trigger)
	}
	return nil
}

func (g *Guard) now() time.Time {
	if g.Clock != nil {
		return g.Clock()
	}
	return time.Now()
}
