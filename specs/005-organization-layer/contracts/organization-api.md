# Interface Contract: Organization & Navigation API

This document details the REST / Server Endpoint contracts exposed by Sentinel Dashboard Web for managing Organizations, Organization Members, Invites, and Context Switching.

---

## 1. List User Organizations & Active Context

### `GET /api/organizations`
Returns all organizations the current authenticated user belongs to, along with their active organization preference.

#### Request Headers
- `Cookie` / `Authorization`: Session authentication token

#### Response `200 OK`
```json
{
  "activeOrganizationId": "11111111-2222-3333-4444-555555555555",
  "organizations": [
    {
      "id": "11111111-2222-3333-4444-555555555555",
      "name": "Acme Corp",
      "slug": "acme-corp",
      "avatarUrl": "https://example.com/avatar.png",
      "role": "admin",
      "projectCount": 5
    },
    {
      "id": "66666666-7777-8888-9999-000000000000",
      "name": "Stark Industries",
      "slug": "stark-industries",
      "avatarUrl": null,
      "role": "engineer",
      "projectCount": 2
    }
  ]
}
```

---

## 2. Switch Active Organization Context

### `POST /api/organizations/switch`
Updates the active organization context for the authenticated user and persists their last active preference.

#### Request Body
```json
{
  "organizationId": "66666666-7777-8888-9999-000000000000"
}
```

#### Response `200 OK`
```json
{
  "success": true,
  "activeOrganization": {
    "id": "66666666-7777-8888-9999-000000000000",
    "name": "Stark Industries",
    "slug": "stark-industries"
  },
  "redirectUrl": "/stark-industries/projects"
}
```

---

## 3. Create Organization

### `POST /api/organizations`
Creates a new organization. The creator automatically becomes the `owner`.

#### Request Body
```json
{
  "name": "Wayne Enterprises",
  "slug": "wayne-enterprises"
}
```

#### Response `201 Created`
```json
{
  "id": "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
  "name": "Wayne Enterprises",
  "slug": "wayne-enterprises",
  "role": "owner",
  "createdAt": "2026-07-24T15:20:00Z"
}
```

---

## 4. Invite Organization Member

### `POST /api/organizations/:orgId/invitations`
Invites a new member to the organization with a designated role.

#### Request Body
```json
{
  "email": "engineer@acme.com",
  "role": "engineer"
}
```

#### Response `201 Created`
```json
{
  "id": "inv-9999-8888-7777",
  "email": "engineer@acme.com",
  "role": "engineer",
  "status": "pending",
  "expiresAt": "2026-07-31T15:20:00Z"
}
```

---

## 5. Get Organization Projects (Scoped Navigation)

### `GET /api/organizations/:orgId/projects`
Returns all projects under the active organization.

#### Response `200 OK`
```json
{
  "organization": {
    "id": "11111111-2222-3333-4444-555555555555",
    "name": "Acme Corp",
    "slug": "acme-corp"
  },
  "projects": [
    {
      "id": "p-101",
      "name": "Payment Gateway API",
      "openIssuesCount": 3,
      "userRole": "admin"
    },
    {
      "id": "p-102",
      "name": "Auth Service",
      "openIssuesCount": 0,
      "userRole": "admin"
    }
  ]
}
```
