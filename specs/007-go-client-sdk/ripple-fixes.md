# Ripple Fixes — Session 2026-07-25

## R-001: Trailing Slash / Endpoint Path Concatenation Discrepancy [WARNING]

**Strategy**: Option A — Trim Trailing Slash

**Files to modify**:
- `packages/sdk-go/transport.go` — Update `sendBatch()` to sanitize `t.cfg.Endpoint` with `strings.TrimSuffix`.

**Key steps**:
1. In `sendBatch()`, compute `baseEndpoint := strings.TrimSuffix(t.cfg.Endpoint, "/")`.
2. Format batch endpoint as `fmt.Sprintf("%s/batch", baseEndpoint)`.

**Verification**: Run `go test -v ./...` in `packages/sdk-go`.

---

## R-002: Channel Drain Lockup Risk During Flush Timeout Expiry [INFO]

**Strategy**: Option A — Context Timeout Guard

**Files to modify**:
- `packages/sdk-go/transport.go` — Add `context.WithTimeout(context.Background(), 5*time.Second)` to `sendBatch()`.

**Key steps**:
1. In `sendBatch()`, derive request context with 5s timeout.
2. Ensure `defer cancel()` releases context resources.

**Verification**: Run `go test -v ./...` in `packages/sdk-go`.
