-- +goose Up
-- +goose StatementBegin
-- sentinel-user-report-issue §4.6 (contract 05b5a109), the migration §8 fixes as "what must land
-- first": the two external-report tables, the four on-call/escalation tables, the report
-- idempotency store §4.1 needs, the four CHECK swaps and the new occurrence index.
--
-- IDEMPOTENCY (A1): one flat migration directory serves several goose ledgers against the SAME
-- physical database, so this file is replayed per target -- every statement below is a no-op on a
-- schema that already has it applied. IF NOT EXISTS on tables/columns/indexes; a pg_constraint
-- catalog guard for every CHECK swap, because Postgres has no ADD CONSTRAINT IF NOT EXISTS. The
-- four swaps below copy 1722900000_add_agents.sql:35-47 verbatim in shape (unconditional DROP
-- inside the guard, then ADD) so a re-run lands on exactly the same constraint definition.
--
-- ORDERING (§8): external_reporters before external_issue_reports (FK); oncall_schedules before
-- its members, its overrides and issue_escalations. The four CHECK swaps are independent of all
-- of it.

-- §4.6: the external reporter. A separate table with its own FK (Decisions taken, OQ-6) -- not a
-- nullable issues/manual_issue_reports reporter, and manual_issue_reports is NOT touched.
-- organization_id is denormalised NOT NULL for the reason attachments.org_id gives in
-- 1722700000: a tenancy check must never depend on walking a join. project_id is the id-space
-- external_id is unique WITHIN -- `u_8f3c41` from two instrumented products is two people.
CREATE TABLE IF NOT EXISTS external_reporters (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    external_id VARCHAR(255) NOT NULL,
    display_name VARCHAR(255),
    -- Stored so a support person escalating out-of-band has something to work with, and so a
    -- repeat reporter is recognisable. NEVER used to send anything (§4.1, MANUAL_ISSUES_DESIGN
    -- §0 Q1 -- email is never a reply channel).
    email VARCHAR(320),
    first_seen_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- The identity rule, and the thing that makes intake an upsert rather than an insert.
    -- Declared inline (not a bare ADD CONSTRAINT) so CREATE TABLE IF NOT EXISTS carries it.
    CONSTRAINT external_reporters_project_external_id_unique UNIQUE (project_id, external_id)
);

CREATE INDEX IF NOT EXISTS idx_external_reporters_org ON external_reporters (organization_id);
-- §12's abuse review: "who reported the most, most recently, in this org".
CREATE INDEX IF NOT EXISTS idx_external_reporters_org_last_seen
  ON external_reporters (organization_id, last_seen_at DESC);

-- §4.6: the 1:1 companion, parallel to manual_issue_reports. A separate table rather than seven
-- more columns on manual_issue_reports: an org-member report has no page, no release and no
-- brand, and adding them there would pollute a shipped path this feature promises not to touch.
-- No GIN index on `context` -- nothing queries into it in v1 and an unused GIN index is write
-- cost for nothing.
CREATE TABLE IF NOT EXISTS external_issue_reports (
    issue_id UUID PRIMARY KEY REFERENCES issues(id) ON DELETE CASCADE,
    external_reporter_id UUID NOT NULL REFERENCES external_reporters(id),
    body_md TEXT NOT NULL,
    severity VARCHAR(20) NOT NULL DEFAULT 'medium' CHECK (severity IN ('low', 'medium', 'high', 'critical')),
    context JSONB NOT NULL DEFAULT '{}'::jsonb,
    idempotency_key VARCHAR(128),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_external_issue_reports_reporter
  ON external_issue_reports (external_reporter_id);

-- §4.1: the dedupe store. The existing agent_idempotency_keys (1723800000) key space is the
-- CALLING AGENT's; this endpoint's key space is the calling API KEY ROW's id, so it gets its own
-- table rather than a widened one -- widening would make agent_id nullable and turn a unique
-- constraint that means one thing into one that means two. §4.6's table list omits this table;
-- §4.1 requires it, and the repo-side choice of table is explicitly delegated there.
-- request_hash exists for the §7 `409 idempotency_conflict` case: "the same idempotency_key was
-- used with a DIFFERENT body" is only detectable against a stored digest of the first body.
-- issue_id is the original result replayed on a hit (§4.1: "a repeat within the dedupe window
-- returns the first call's result and creates nothing"); no FK on it, for the reason
-- 1723800000 gives for comment_id -- a retry must not resurrect a deleted issue via a fresh
-- write, and the row is aged out by the 7-day reaper instead.
CREATE TABLE IF NOT EXISTS report_idempotency_keys (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    api_key_id UUID NOT NULL REFERENCES project_api_keys(id) ON DELETE CASCADE,
    idempotency_key VARCHAR(128) NOT NULL,
    request_hash VARCHAR(64) NOT NULL,
    issue_id UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT report_idempotency_keys_key_unique UNIQUE (api_key_id, idempotency_key)
);

-- 7-day reaper predicate is `created_at < cutoff` (§8, matching reapExpiredIdempotencyKeys'
-- existing cutoff); a btree on created_at keeps that sweep off a full scan.
CREATE INDEX IF NOT EXISTS idx_report_idempotency_keys_created_at
  ON report_idempotency_keys (created_at);

-- §4.4: the on-call schedule. An organization has AT MOST ONE ACTIVE schedule, enforced by the
-- partial unique index below rather than by a handler.
CREATE TABLE IF NOT EXISTS oncall_schedules (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    name VARCHAR(255) NOT NULL,
    -- IANA zone name. §4.4's rotation arithmetic converts created_at and T into this zone, and
    -- handoff is wall-clock in it (a DST handoff day is 23 or 25 hours long, deliberately).
    timezone VARCHAR(64) NOT NULL DEFAULT 'UTC',
    kind VARCHAR(20) NOT NULL CHECK (kind IN ('daily', 'weekly')),
    handoff_time TIME NOT NULL,
    -- Null unless weekly (§4.6). 0-6, ISO-style with 0 = Sunday, matching the JS getDay() the
    -- dashboard resolves it with. Guarded by oncall_schedules_handoff_weekday_check below.
    handoff_weekday SMALLINT,
    active BOOLEAN NOT NULL DEFAULT true,
    created_by VARCHAR(255) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM pg_constraint
    WHERE conname = 'oncall_schedules_handoff_weekday_check' AND conrelid = 'oncall_schedules'::regclass
  ) THEN
    ALTER TABLE oncall_schedules DROP CONSTRAINT oncall_schedules_handoff_weekday_check;
  END IF;

  ALTER TABLE oncall_schedules ADD CONSTRAINT oncall_schedules_handoff_weekday_check CHECK (
    (kind = 'weekly' AND handoff_weekday IS NOT NULL AND handoff_weekday BETWEEN 0 AND 6)
    OR (kind = 'daily' AND handoff_weekday IS NULL)
  );
END $$;

-- §4.4/§4.6: "at most one active schedule per organization", in the database. Deactivated
-- schedules are kept (DELETE /oncall deactivates, §4.4a) and do not contend for the slot.
CREATE UNIQUE INDEX IF NOT EXISTS idx_oncall_schedules_one_active
  ON oncall_schedules (organization_id) WHERE active;

-- §4.4: the ordered member list. A member whose org role later drops below ISSUE_WRITE_ROLES is
-- SKIPPED at resolve time, not removed here -- eligibility is not a column, it is a live join
-- against organization_members. The composite PK is §4.6's UNIQUE (schedule_id, user_id).
CREATE TABLE IF NOT EXISTS oncall_schedule_members (
    schedule_id UUID NOT NULL REFERENCES oncall_schedules(id) ON DELETE CASCADE,
    user_id TEXT NOT NULL REFERENCES "user"(id) ON DELETE CASCADE,
    position SMALLINT NOT NULL,
    PRIMARY KEY (schedule_id, user_id)
);

-- §4.6's second uniqueness rule: no two members share a rotation position.
CREATE UNIQUE INDEX IF NOT EXISTS idx_oncall_schedule_members_position
  ON oncall_schedule_members (schedule_id, position);

-- §4.4: overrides, in absolute UTC. An override wins over the rotation for its interval.
CREATE TABLE IF NOT EXISTS oncall_overrides (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    schedule_id UUID NOT NULL REFERENCES oncall_schedules(id) ON DELETE CASCADE,
    user_id TEXT NOT NULL REFERENCES "user"(id) ON DELETE CASCADE,
    starts_at TIMESTAMPTZ NOT NULL,
    ends_at TIMESTAMPTZ NOT NULL,
    created_by VARCHAR(255) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM pg_constraint
    WHERE conname = 'oncall_overrides_window_check' AND conrelid = 'oncall_overrides'::regclass
  ) THEN
    ALTER TABLE oncall_overrides DROP CONSTRAINT oncall_overrides_window_check;
  END IF;

  ALTER TABLE oncall_overrides ADD CONSTRAINT oncall_overrides_window_check CHECK (
    ends_at > starts_at
  );
END $$;

-- whoIsOnCall(schedule, members, overrides, T) loads a schedule's overrides ordered by window.
CREATE INDEX IF NOT EXISTS idx_oncall_overrides_schedule
  ON oncall_overrides (schedule_id, starts_at);

-- §4.4: one row per escalation hop on a report. hop 1 is written at intake; the §4.4b timer adds
-- one row per reassignment and stops at ESCALATION_MAX_HOPS with exhausted = true.
CREATE TABLE IF NOT EXISTS issue_escalations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    issue_id UUID NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    schedule_id UUID NOT NULL REFERENCES oncall_schedules(id) ON DELETE CASCADE,
    assignee_id TEXT NOT NULL REFERENCES "user"(id) ON DELETE CASCADE,
    hop SMALLINT NOT NULL,
    escalated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    ack_deadline_at TIMESTAMPTZ NOT NULL,
    acknowledged_at TIMESTAMPTZ,
    exhausted BOOLEAN NOT NULL DEFAULT false,
    -- "Escalation at intake is idempotent per report" (§4.4): the second hop-1 insert loses here.
    CONSTRAINT issue_escalations_issue_hop_unique UNIQUE (issue_id, hop)
);

-- §4.4b's sweep predicate, so the every-minute timer never scans the table.
CREATE INDEX IF NOT EXISTS idx_issue_escalations_due
  ON issue_escalations (ack_deadline_at)
  WHERE acknowledged_at IS NULL AND NOT exhausted;

-- §4.6 CHECK swap 1/4: the `report` key scope. Per §8 this scope IS the feature switch -- no key
-- that exists today carries it, so the endpoint is off for every existing organization on the day
-- it deploys. Shape copied verbatim from 1722900000_add_agents.sql:35-47.
DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM pg_constraint
    WHERE conname = 'project_api_keys_scope_check' AND conrelid = 'project_api_keys'::regclass
  ) THEN
    ALTER TABLE project_api_keys DROP CONSTRAINT project_api_keys_scope_check;
  END IF;

  ALTER TABLE project_api_keys ADD CONSTRAINT project_api_keys_scope_check CHECK (
    scope IN ('ingest', 'read', 'admin', 'agent', 'report')
  );
END $$;

-- §4.6 CHECK swap 2/4: issues.source_channel gains 'user_report_api'. The constraint is declared
-- INLINE on the ADD COLUMN in 1721900000_add_issue_lifecycle_and_relations.sql, so Postgres named
-- it issues_source_channel_check; the guard finds it under that name.
DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM pg_constraint
    WHERE conname = 'issues_source_channel_check' AND conrelid = 'issues'::regclass
  ) THEN
    ALTER TABLE issues DROP CONSTRAINT issues_source_channel_check;
  END IF;

  ALTER TABLE issues ADD CONSTRAINT issues_source_channel_check CHECK (
    source_channel IN ('ingestion_sdk', 'manual_support', 'api', 'user_report_api')
  );
END $$;

-- §4.6 CHECK swap 3/4: attachments.uploader_type gains 'external_reporter' -- §4.5's draft upload
-- is made by a report API key on an external reporter's behalf, which is neither a user nor an
-- agent. Also inline-declared (1722700000), hence the same constraint name.
DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM pg_constraint
    WHERE conname = 'attachments_uploader_type_check' AND conrelid = 'attachments'::regclass
  ) THEN
    ALTER TABLE attachments DROP CONSTRAINT attachments_uploader_type_check;
  END IF;

  ALTER TABLE attachments ADD CONSTRAINT attachments_uploader_type_check CHECK (
    uploader_type IN ('user', 'agent', 'external_reporter')
  );
END $$;

-- §4.6 CHECK swap 4/4: notifications.kind gains 'escalated'. §4.4: none of the existing seven
-- means "you were put on this".
DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM pg_constraint
    WHERE conname = 'notifications_kind_check' AND conrelid = 'notifications'::regclass
  ) THEN
    ALTER TABLE notifications DROP CONSTRAINT notifications_kind_check;
  END IF;

  ALTER TABLE notifications ADD CONSTRAINT notifications_kind_check CHECK (
    kind IN (
      'commented', 'claimed', 'status_changed', 'resolved', 'linked', 'progress_update',
      'question_asked', 'escalated'
    )
  );
END $$;

-- §4.3/§4.6: the triage matcher reads a project's recent occurrences newest-first within
-- REPORT_MATCH_WINDOW_MINUTES of the report. idx_occurrences_issue_release (1721900000) is keyed
-- on release, not time, so it cannot serve that scan.
CREATE INDEX IF NOT EXISTS idx_occurrences_issue_created
  ON error_occurrences (issue_id, created_at DESC);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- The four CHECK swaps reverse to their pre-§4.6 value sets, same guarded shape as
-- 1722900000_add_agents.sql's Down.
DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM pg_constraint
    WHERE conname = 'notifications_kind_check' AND conrelid = 'notifications'::regclass
  ) THEN
    ALTER TABLE notifications DROP CONSTRAINT notifications_kind_check;
  END IF;

  ALTER TABLE notifications ADD CONSTRAINT notifications_kind_check CHECK (
    kind IN (
      'commented', 'claimed', 'status_changed', 'resolved', 'linked', 'progress_update',
      'question_asked'
    )
  );
END $$;

DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM pg_constraint
    WHERE conname = 'attachments_uploader_type_check' AND conrelid = 'attachments'::regclass
  ) THEN
    ALTER TABLE attachments DROP CONSTRAINT attachments_uploader_type_check;
  END IF;

  ALTER TABLE attachments ADD CONSTRAINT attachments_uploader_type_check CHECK (
    uploader_type IN ('user', 'agent')
  );
END $$;

DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM pg_constraint
    WHERE conname = 'issues_source_channel_check' AND conrelid = 'issues'::regclass
  ) THEN
    ALTER TABLE issues DROP CONSTRAINT issues_source_channel_check;
  END IF;

  ALTER TABLE issues ADD CONSTRAINT issues_source_channel_check CHECK (
    source_channel IN ('ingestion_sdk', 'manual_support', 'api')
  );
END $$;

DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM pg_constraint
    WHERE conname = 'project_api_keys_scope_check' AND conrelid = 'project_api_keys'::regclass
  ) THEN
    ALTER TABLE project_api_keys DROP CONSTRAINT project_api_keys_scope_check;
  END IF;

  ALTER TABLE project_api_keys ADD CONSTRAINT project_api_keys_scope_check CHECK (
    scope IN ('ingest', 'read', 'admin', 'agent')
  );
END $$;

DROP INDEX IF EXISTS idx_occurrences_issue_created;

DROP TABLE IF EXISTS issue_escalations;
DROP TABLE IF EXISTS oncall_overrides;
DROP TABLE IF EXISTS oncall_schedule_members;
DROP TABLE IF EXISTS oncall_schedules;
DROP TABLE IF EXISTS report_idempotency_keys;
DROP TABLE IF EXISTS external_issue_reports;
DROP TABLE IF EXISTS external_reporters;

-- +goose StatementEnd
