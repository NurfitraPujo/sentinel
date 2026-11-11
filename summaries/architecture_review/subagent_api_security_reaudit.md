> [!WARNING]
> **Superseded 2026-08-01.** This report predates a set of independent code reviews that found the
> features audited here (member management, invitations) merge clean and pass their own tests but do
> not run correctly in several respects — see `docs/plans/UI_PARITY_REMEDIATION_PLAN.md` (defect
> register D01–D47) for the authoritative, current findings. Do not treat this file's verdict below
> as current; re-verify against `docs/memory/VERIFIED_STATE.md` instead.

# API Endpoints & RBAC Security Logic Re-Audit Report

**Target Files:**
1. `apps/dashboard-web/src/routes/api/organizations/[orgId]/members/[memberId]/+server.ts`
2. `apps/dashboard-web/src/routes/api/organizations/[orgId]/invitations/+server.ts`

**Status:** ✅ **PASSED (100% Compliant)**

---

## Executive Summary
Re-audit of the target API endpoints confirmed that **all 4 identified security & validation issues have been successfully resolved**. The logic enforces safe input parsing, strict type checking, robust RBAC role hierarchy controls, sole owner protections, and self-revocation guards.

---

## Detailed Audit Findings

### 1. Safe JSON Body Parsing
* **Status:** PASS
* **Verification:**
  * In `members/[memberId]/+server.ts` (`PATCH`): `request.json()` is wrapped in `try/catch` and throws `error(400, 'Invalid JSON body')`.
  * In `invitations/+server.ts` (`POST`): `request.json()` is wrapped in `try/catch` and throws `error(400, 'Invalid JSON body')`.
* **Impact:** Malformed or truncated JSON payloads now gracefully return HTTP 400 Bad Request instead of throwing unhandled server errors (HTTP 500).

### 2. Strict Role Enum Validation
* **Status:** PASS
* **Verification:**
  * Both endpoints define `VALID_ROLES = ['owner', 'admin', 'engineer', 'support', 'viewer'] as const`.
  * In `invitations/+server.ts` (`POST`): Ensures `typeof role === 'string'` and `VALID_ROLES.includes(role as Role)` before proceeding; throws `error(400, ...)` if invalid.
  * In `members/[memberId]/+server.ts` (`PATCH`): Validates `VALID_ROLES.includes(role as Role)`; throws `error(400, ...)` if invalid.
* **Impact:** Arbitrary or invalid role strings cannot be injected into invitations or membership updates.

### 3. Email Type & Format Validation Prior to `.trim()`
* **Status:** PASS
* **Verification:**
  * In `invitations/+server.ts` (`POST`): Verifies `typeof email === 'string'` **before** calling `email.trim().toLowerCase()`.
  * Validates the normalized email against `EMAIL_REGEX = /^[^\s@]+@[^\s@]+\.[^\s@]+$/` and throws `error(400, 'Invalid email address format')` if validation fails.
* **Impact:** Prevents runtime `TypeError` crashes when non-string types (e.g., objects, arrays, numbers) are passed in the JSON payload, and ensures syntactically valid email formats before database queries or email dispatches.

### 4. Hierarchy Guards, Sole Owner Protection & Self-Revocation Logic
* **Status:** PASS
* **Verification:**
  * **RBAC Hierarchy Guards:**
    * Caller must hold `owner` or `admin` role (`requireOrgMembership`).
    * Admins cannot grant `owner` role, alter an existing `owner`'s role, revoke an `owner`'s membership, or issue `owner` invitations (all throw `error(403, ...)`).
  * **Sole Owner Protection:**
    * `PATCH` (Demotion): Checks `organizationMembers.role === 'owner'` count before demoting an owner. Throws `error(400, 'Cannot demote the sole owner of an organization')` if `count <= 1`.
    * `DELETE` (Revocation): Checks `organizationMembers.role === 'owner'` count before revoking an owner. Throws `error(400, 'Cannot revoke the sole owner of an organization')` if `count <= 1`.
  * **Self-Revocation Protection:**
    * `DELETE`: Checks `targetMember.userId === session.user.id` and throws `error(400, 'Cannot revoke your own organization access')`.
* **Impact:** Prevents organizational lockout, privileges escalation by `admin` users, and accidental self-revocation.

---

## Conclusion
The security logic across both files meets all requirements and safety guidelines. No further remediations are necessary for these endpoints.
