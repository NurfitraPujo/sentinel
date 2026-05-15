# Technical Decisions (`docs/memory/`)

This file stores durable technical and implementation decisions. For governance-level decisions or project standards, see `.specify/memory/DECISIONS.md`.

## Entry Lifecycle

Each decision follows this lifecycle:

```
Active → Needs Review → Superseded → (pruned)
```

- **Active**: The decision is current and must be honored by all features and AI agents.
- **Needs Review**: Implementation reality or new context suggests this decision may be outdated. It should still be honored until reviewed and explicitly changed.
- **Superseded**: A newer decision has replaced this one. Keep it for historical context until the next audit, then consider pruning.
- **Pruned**: During an audit, remove superseded entries that no longer provide historical value. This keeps the file focused.

### When to change status

| Current Status | Change To    | When                                                                                                       |
| -------------- | ------------ | ---------------------------------------------------------------------------------------------------------- |
| Active         | Needs Review | Verified implementation or tests contradict the decision, or recurring features follow a different pattern |
| Active         | Superseded   | A newer decision explicitly replaces this one                                                              |
| Needs Review   | Active       | Team confirms the decision still holds after review                                                        |
| Needs Review   | Superseded   | Team confirms a replacement decision                                                                       |
| Superseded     | _(remove)_   | Audit finds no remaining historical value                                                                  |

### Rules

- Never delete an Active decision without replacing or superseding it.
- Never silently ignore a decision. If it feels wrong, mark it Needs Review and resolve it.
- Keep at most 3–5 Superseded entries for context. Prune older ones during audits.

---

### 2024-05-20 - Graceful Degradation via In-Memory Buffering

**Status**
Active

**Why this is durable**
Sentinel is an observability platform. Losing events during a temporary database outage defeats the purpose of the platform. This decision ensures that short-term infrastructure issues don't lead to permanent data loss.

**Decision**
When the PostgreSQL database is unavailable, the Processor service MUST buffer incoming events in memory up to a limit (MaxBufferSize = 10,000 events). These events MUST be flushed to the database automatically once connection is restored.

**Tradeoffs**
- **Gained**: High availability and data persistence during temporary outages.
- **Made harder**: Memory management in the Processor service. A long-term outage could lead to OOM if the buffer is too large or if backpressure is not applied.
- **Reconsider**: If Sentinel moves to a multi-tenant model where memory limits must be strictly partitioned, or if the buffer size needs to be dynamic.

**Future mistake prevented**
Directly failing or dropping events when the database is down.

**Evidence**
Implementation in `apps/processor-go/degradation/buffer.go`.

**Where to look next**
`apps/processor-go/processor.go` and `apps/processor-go/degradation/buffer.go`.

---

### 2026-05-15 - Magic Link Authentication via Auth.js Email Provider

**Status**
Active

**Why this is durable**
Local development environments may not have access to Google Workspace OIDC. Magic link authentication provides a fallback that doesn't bypass project RBAC.

**Decision**
- Use Auth.js built-in Email provider for magic link support
- Tokens are cryptographically random, expire in 15 minutes, and are single-use (handled by Auth.js)
- Magic link authentication does NOT bypass project RBAC - user must still have project membership
- For local dev, use `smtp://debug` to output email JSON to stdout instead of sending

**Tradeoffs**
- **Gained**: Simple local auth without Google OAuth setup
- **Made harder**: Requires SMTP configuration for production
- **Reconsider**: If magic link deliverability issues arise in production

**Future mistake prevented**
Custom auth implementation that bypasses RBAC or uses insecure token handling.

**Evidence**
Implementation in `apps/dashboard-web/src/lib/auth.ts` and `apps/dashboard-web/src/routes/auth/signin/+page.svelte`.

**Where to look next**
`specs/001-sentinel-error-service/tasks.md` (Phase 5: Local Development Support)
