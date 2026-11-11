# TODO 08: Connect API Key Management UI in Dashboard to Real Backend Endpoints

## Priority: High
## Status: Completed

### Overview
The API Key management endpoints (`/api/organizations/[orgId]/keys`, `.../rotate`, and `.../revoke`) are fully implemented in SvelteKit server routes with PostgreSQL persistence and NATS cache invalidation. However, the frontend pages (`[orgSlug]/settings/keys/+page.svelte` and `[orgSlug]/projects/[projectId]/settings/keys/+page.svelte`) currently use hardcoded client-side mock arrays (`keys = [...]`) and mock handlers (`handleCreate`, `handleRotate`, `handleRevoke`).

### Requirements
1. **Server Loader Integration**:
   - Create `+page.server.ts` loaders for org-level and project-level key settings pages to fetch active and revoked API keys from `/api/organizations/[orgId]/keys`.
2. **Real Mutation Handlers**:
   - Update `handleCreate` to call `POST /api/organizations/[orgId]/keys` with target scope, project ID, and rate-limit overrides.
   - Update `handleRotate` to call `POST /api/organizations/[orgId]/keys/[keyId]/rotate` and display the newly generated secret key once.
   - Update `handleRevoke` to call `DELETE /api/organizations/[orgId]/keys/[keyId]`.
3. **Secret Token Display & Security**:
   - Ensure the raw secret token is only displayed to the user upon creation/rotation modal and never stored unhashed in local UI state.

### Affected Files
- `apps/dashboard-web/src/routes/[orgSlug]/settings/keys/+page.svelte`
- `apps/dashboard-web/src/routes/[orgSlug]/projects/[projectId]/settings/keys/+page.svelte`
- `apps/dashboard-web/src/routes/[orgSlug]/settings/keys/+page.server.ts` (New)
- `apps/dashboard-web/src/routes/[orgSlug]/projects/[projectId]/settings/keys/+page.server.ts` (New)

### Acceptance Criteria
- Creating a key from the UI inserts a record into `api_keys` via the server API.
- Rotating a key in the UI invalidates the old key via NATS and returns a new raw secret.
- Revoking a key removes it from active list and triggers ingestor cache invalidation.
