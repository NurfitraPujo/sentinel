# Phase 1 Data Model: Organization Management Layer

## Entities & Relationships

```mermaid
erDiagram
    USER ||--o{ ORGANIZATION_MEMBER : belongs_to
    ORGANIZATION ||--o{ ORGANIZATION_MEMBER : has_members
    ORGANIZATION ||--o{ ORGANIZATION_INVITATION : issues_invites
    ORGANIZATION ||--o{ PROJECT : owns
    PROJECT ||--o{ PROJECT_MEMBER : has_overrides
    USER ||--o{ PROJECT_MEMBER : receives_override
    USER ||--o| USER_SESSION_PREFERENCE : has_preference
    ORGANIZATION ||--o| USER_SESSION_PREFERENCE : active_org

    ORGANIZATION {
        uuid id PK
        string name
        string slug UK
        string avatar_url
        timestamp created_at
        timestamp updated_at
    }

    ORGANIZATION_MEMBER {
        uuid id PK
        uuid organization_id FK
        string user_id FK
        string role "owner | admin | engineer | support | viewer"
        timestamp joined_at
    }

    ORGANIZATION_INVITATION {
        uuid id PK
        uuid organization_id FK
        string email
        string role "owner | admin | engineer | support | viewer"
        string token UK
        string status "pending | accepted | revoked | expired"
        timestamp expires_at
        timestamp created_at
    }

    PROJECT_MEMBER {
        uuid id PK
        uuid project_id FK
        string user_id FK
        string role "admin | engineer | support | viewer"
        timestamp created_at
    }

    USER_SESSION_PREFERENCE {
        string user_id PK
        uuid last_active_organization_id FK
        timestamp updated_at
    }

    PROJECT {
        uuid id PK
        uuid organization_id FK
        string name
        string api_key
        string api_key_hash
        timestamp created_at
    }
```

## Schema Definitions (TypeScript / Drizzle & SQL)

### 1. `organizations` Table
```typescript
export const organizations = pgTable('organizations', {
  id: uuid('id').primaryKey().defaultRandom(),
  name: varchar('name', { length: 255 }).notNull(),
  slug: varchar('slug', { length: 255 }).notNull().unique(),
  avatarUrl: text('avatar_url'),
  createdAt: timestamp('created_at').defaultNow(),
  updatedAt: timestamp('updated_at').defaultNow(),
});
```

### 2. `organization_members` Table
```typescript
export const organizationMembers = pgTable('organization_members', {
  id: uuid('id').primaryKey().defaultRandom(),
  organizationId: uuid('organization_id').notNull().references(() => organizations.id, { onDelete: 'cascade' }),
  userId: varchar('user_id', { length: 255 }).notNull(),
  role: varchar('role', { length: 20 }).notNull().default('viewer'), // owner, admin, engineer, support, viewer
  joinedAt: timestamp('joined_at').defaultNow(),
}, (table) => ({
  uniqueUserOrg: index('org_members_user_org_unique').on(table.userId, table.organizationId),
}));
```

### 3. `organization_invitations` Table
```typescript
export const organizationInvitations = pgTable('organization_invitations', {
  id: uuid('id').primaryKey().defaultRandom(),
  organizationId: uuid('organization_id').notNull().references(() => organizations.id, { onDelete: 'cascade' }),
  email: varchar('email', { length: 255 }).notNull(),
  role: varchar('role', { length: 20 }).notNull().default('viewer'),
  token: varchar('token', { length: 128 }).notNull().unique(),
  status: varchar('status', { length: 20 }).notNull().default('pending'), // pending, accepted, revoked, expired
  expiresAt: timestamp('expires_at').notNull(),
  createdAt: timestamp('created_at').defaultNow(),
});
```

### 4. `user_session_preferences` Table
```typescript
export const userSessionPreferences = pgTable('user_session_preferences', {
  userId: varchar('user_id', { length: 255 }).primaryKey(),
  lastActiveOrganizationId: uuid('last_active_organization_id').references(() => organizations.id, { onDelete: 'set null' }),
  updatedAt: timestamp('updated_at').defaultNow(),
});
```

### 5. Updated `projects` Table
```typescript
// Updated projects schema to include organization_id
export const projects = pgTable('projects', {
  id: uuid('id').primaryKey().defaultRandom(),
  organizationId: uuid('organization_id').notNull().references(() => organizations.id, { onDelete: 'cascade' }),
  name: varchar('name', { length: 255 }).notNull(),
  apiKey: varchar('api_key', { length: 64 }).notNull(),
  apiKeyHash: varchar('api_key_hash', { length: 128 }).notNull(),
  createdAt: timestamp('created_at').defaultNow(),
});
```

## Effective Permission Resolution Logic

```typescript
export function resolveEffectiveProjectRole(
  orgRole: 'owner' | 'admin' | 'engineer' | 'support' | 'viewer',
  projectOverrideRole?: 'admin' | 'engineer' | 'support' | 'viewer' | null
): string {
  // If a project-specific override exists, it takes precedence
  if (projectOverrideRole) {
    return projectOverrideRole;
  }
  // Otherwise inherit organization role
  return orgRole;
}
```
