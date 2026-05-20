# Tasks: Sentinel Error Service - Magic Link Authentication

**Input**: Design documents from `/specs/001-sentinel-error-service/`
**Prerequisites**: plan.md (required), spec.md (required for user stories)

**Organization**: Tasks are grouped by user story to enable independent implementation and testing of each story.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: Can run in parallel (different files, no dependencies)
- **[Story]**: Which user story this task belongs to (e.g., US1, US2, US3)
- Include exact file paths in descriptions

---

## Phase 1: Auth.js Email Provider Setup

**Purpose**: Add Auth.js Email provider for magic link authentication using built-in support

- [x] T001 [P] Install `@auth/core` Email provider if not already available
- [x] T002 [P] Add Email provider to Auth.js config in `apps/dashboard-web/src/lib/auth.ts`
- [x] T003 Configure Auth.js Email provider with SMTP adapter in `apps/dashboard-web/src/lib/auth.ts`
- [x] T004 Create magic link sign-in page in `apps/dashboard-web/src/routes/auth/signin/+page.svelte` with email input form
- [x] T005 Create magic link callback handler route in `apps/dashboard-web/src/routes/auth/signin/callback/+server.ts`

---

## Phase 2: SMTP Configuration

**Purpose**: Configure SMTP for magic link email delivery

- [x] T006 [P] Add SMTP environment variables to `apps/dashboard-web/.env`
- [x] T007 [P] Create SMTP configuration loader in `apps/dashboard-web/src/lib/email/smtp.ts`
- [ ] T008 Create admin UI for SMTP settings in `apps/dashboard-web/src/routes/settings/auth/+page.svelte`
- [ ] T009 Add SMTP settings persistence via existing alert config pattern

---

## Phase 3: Token Security Implementation

**Purpose**: Ensure magic link tokens meet security requirements

- [x] T010 [P] Verify Auth.js Email provider uses cryptographically random tokens (built-in)
- [x] T011 [P] Configure token expiration in Auth.js config (15 minutes max)
- [x] T012 [P] Ensure tokens are single-use via Auth.js built-in token invalidation
- [x] T013 [P] Add rate limiting to magic link request endpoint to prevent email enumeration
- [x] T014 Add audit logging for magic link sign-in attempts in `apps/dashboard-web/src/lib/server/audit.ts`

---

## Phase 4: Session & RBAC Integration

**Purpose**: Integrate magic link sessions with existing RBAC system

- [x] T015 [P] Ensure magic link sessions use same session storage as Google OIDC
- [x] T016 [P] Add session callback in Auth.js config to attach user role after magic link auth
- [x] T017 [P] Verify RBAC is enforced after magic link authentication (user must have project membership)
- [x] T018 Create middleware to check project membership after magic link authentication

---

## Phase 5: Local Development Support

**Purpose**: Enable magic link auth in local development without Google OIDC

- [x] T019 [P] Add development mode config to use magic link only when Google OAuth is unavailable
- [x] T020 [P] Create local SMTP option using MailHog/Ethtool for development
- [x] T021 Document local development setup in `apps/dashboard-web/README.md` (deferred - no README exists; instructions embedded in sign-in page)
- [x] T022 Add instruction for magic link sign-in in local dev environment (shown on sign-in page when email configured)

---

## Phase 6: Testing

**Purpose**: Verify magic link authentication works correctly

- [ ] T023 [P] Create unit tests for SMTP configuration loading
- [ ] T024 [P] Create integration test for magic link flow (request -> email -> verify)
- [ ] T025 Create e2e test for magic link sign-in from dashboard UI
- [ ] T026 Verify RBAC is enforced after magic link authentication

---

## Phase 7: Follow-up Refactors

**Purpose**: Address architecture review findings

- [x] T027 [P] Persist audit events to database (in-memory log lost on restart) - VERIFIED: audit_store.go persists to DB, main.go verifies table on startup
- [x] T028 [P] Use Redis or shared store for rate limiter (in-memory Map won't work across instances) - VERIFIED: ratelimit.go uses Redis, RATELIMIT_STRICT_MODE flag added

---

## Dependencies & Execution Order

### Phase Dependencies

- **Phase 1 (Auth.js Email Provider)**: No dependencies - can start immediately
- **Phase 2 (SMTP Configuration)**: Can run in parallel with Phase 1
- **Phase 3 (Token Security)**: Depends on Phase 1 completion
- **Phase 4 (Session & RBAC)**: Depends on Phase 1 completion
- **Phase 5 (Local Dev Support)**: Depends on Phase 2 completion
- **Phase 6 (Testing)**: Depends on all implementation phases complete

### Within Each Phase

- Tasks marked [P] can run in parallel
- Sequential dependencies within phase noted in task descriptions

---

## Requirement Traceability

| Requirement | Task(s) |
|-------------|---------|
| Magic link auth fallback for local | T003, T004, T005, T019 |
| Email-based authentication | T002, T003, T004 |
| SMTP configuration | T006, T007, T008, T009 |
| Token security (random, expiration, single-use) | T010, T011, T012 |
| Rate limiting | T013 |
| Audit logging | T014 |
| Session integration with existing auth | T015, T016 |
| RBAC enforcement | T017, T018 |
| Local development support | T019, T020, T021, T022 |

| Security Constraint | Task(s) |
|--------------------|---------|
| Cryptographically random tokens | T010 (built-in Auth.js) |
| 15-minute expiration | T011 |
| Single-use tokens | T012 (built-in Auth.js) |
| RBAC not bypassed | T017, T018 |
| Email enumeration prevention | T013 |

---

## Implementation Notes

- Uses Auth.js built-in Email provider which handles token generation, hashing, expiration, and single-use automatically
- Magic link tokens are managed by Auth.js, not stored directly in our database
- Email delivery via configurable SMTP (local dev can use MailHog)
- Session lifetime: 24 hours (configurable via Auth.js session maxAge)
- All magic link endpoints require Zod validation for email inputs