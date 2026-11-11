---
title: Organization Member Management & Invitation RBAC Controls
date: 2026-08-01
category: docs/solutions/security-issues/
module: dashboard-web
problem_type: security_issue
component: authentication
symptoms:
  - "Unauthenticated access to organization member list"
  - "Missing role enum validation on invitation creation API"
  - "Unescaped HTML string interpolation in invitation email templates"
root_cause: missing_permission
resolution_type: code_fix
severity: high
tags: organization-members, rbac-authorization, invitations, sveltekit, email-sanitization
---

# Organization Member Management & Invitation RBAC Controls

## Problem
Organization member management and invitation actions in `dashboard-web` contained missing authorization checks in server load handlers (`+page.server.ts`), unvalidated role parameters on invitation creation (`POST /invitations`), unescaped HTML template parameters in invitation email dispatches, and unhandled `request.json()` parse exceptions.

## Symptoms
- Unauthenticated users could access `/[orgSlug]/settings/members` and fetch member names, IDs, and email addresses.
- `POST /api/organizations/[orgId]/invitations` accepted arbitrary unvalidated `role` strings and saved them to the database.
- Dynamic `organizationName` and `inviteUrl` values were interpolated directly into email HTML strings without entity escaping.
- Malformed JSON payloads triggered unhandled `SyntaxError` crashes (`500 Internal Server Error`) instead of returning `400 Bad Request`.

## What Didn't Work
- Relying solely on layout hooks for server route protection: `hooks.server.ts` resolves active organization context for authenticated sessions but skips resolution for unauthenticated requests, allowing page `load()` handlers to execute without authentication unless explicitly gated.
- Naked `await request.json()` calls: In SvelteKit API handlers, accessing `request.json()` on an invalid or empty request body throws a `SyntaxError` before route handler logic runs.

## Solution
1. **Server Load Session & Org Authorization**:
   In `+page.server.ts`, explicitly assert session authentication and verify `locals.currentOrg.slug === params.orgSlug`:
   ```typescript
   export const load: PageServerLoad = async ({ locals, params }) => {
     const session = await locals.auth();
     if (!session?.user?.email) {
       throw error(401, 'Unauthorized session');
     }

     const currentOrg = locals.currentOrg;
     if (!currentOrg || currentOrg.slug !== params.orgSlug) {
       throw error(403, 'Forbidden: Unauthorized access to organization');
     }
     // ... fetch members scoped strictly to currentOrg.id
   };
   ```

2. **Strict API Input & Role Enum Validation**:
   In `POST /invitations` and `PATCH /members/[memberId]`, wrap JSON parsing in `try/catch` blocks and validate `role` against the valid enum array:
   ```typescript
   const VALID_ROLES = ['owner', 'admin', 'engineer', 'support', 'viewer'] as const;

   let body: any;
   try {
     body = await request.json();
   } catch {
     throw error(400, 'Invalid JSON body');
   }

   const { email, role } = body ?? {};
   if (!role || !VALID_ROLES.includes(role)) {
     throw error(400, `Invalid role. Must be one of: ${VALID_ROLES.join(', ')}`);
   }
   ```

3. **HTML Entity Escaping in Email Templates**:
   Implement `escapeHtml` in `email.ts` to sanitize dynamic template variables:
   ```typescript
   function escapeHtml(str: string): string {
     return str
       .replace(/&/g, '&amp;')
       .replace(/</g, '&lt;')
       .replace(/>/g, '&gt;')
       .replace(/"/g, '&quot;')
       .replace(/'/g, '&#039;');
   }

   const safeOrgName = escapeHtml(organizationName.replace(/[\r\n]/g, ' '));
   const safeInviteUrl = escapeHtml(inviteUrl);
   ```

4. **Sole Owner Protection & Self-Revocation Guards**:
   In `PATCH` and `DELETE` handlers, query `organizationMembers` to verify that demoting or revoking a member will not reduce active `owner` count to 0, and prevent callers from revoking their own access.

## Why This Works
- Explicit `locals.auth()` checks in `PageServerLoad` guarantee that SvelteKit SSR loads enforce server-side authentication even if hook routing changes.
- Safe JSON parsing and enum validation ensure only valid payloads pass to database queries, eliminating 500 errors and invalid data persistence.
- HTML entity escaping prevents XSS and HTML injection vulnerabilities in HTML email clients.
- Sole owner guards protect organizations against accidental lockout.

## Prevention
- **Always gate `PageServerLoad`**: Every `+page.server.ts` route must check `locals.auth()` before executing database queries.
- **Wrap `request.json()`**: Always wrap `request.json()` in a `try/catch` block in SvelteKit API endpoints.
- **Sanitize HTML inputs**: Never interpolate raw user input or database strings into HTML email templates without entity escaping.
- **Mock isolation in Vitest**: Always use `vi.resetAllMocks()` in `beforeEach` blocks to prevent mock queue leaks between test cases.

## Related Issues
- [12-organization-member-management-actions.md](file:///home/fitrapujo/oss/sentinel/docs/todos/12-organization-member-management-actions.md)
